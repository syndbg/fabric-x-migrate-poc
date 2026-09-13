package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	cb "github.com/hyperledger/fabric-protos-go-apiv2/common"
	mb "github.com/hyperledger/fabric-protos-go-apiv2/msp"
	pb "github.com/hyperledger/fabric-protos-go-apiv2/peer"
	lb "github.com/hyperledger/fabric-protos-go-apiv2/peer/lifecycle"
	fxpolicy "github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/common/policies"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
	"google.golang.org/protobuf/proto"

	"github.com/syndbg/fabric-x-migrate-poc/internal/fabricsnapshot"
)

type channelConfig struct {
	envelope *cb.Envelope
	payload  *cb.Payload
	config   *cb.ConfigEnvelope
	channel  string
	block    *uint64
}

// Both a config block from configtxgen/peer and its CONFIG envelope are standard
// Fabric formats. Source config blocks retain their checkpoint for validation.
func decodeChannelConfig(data []byte) (*channelConfig, error) {
	block := new(cb.Block)
	var blockNumber *uint64
	if err := proto.Unmarshal(data, block); err == nil && block.Header != nil && block.Data != nil {
		if len(block.Data.Data) != 1 {
			return nil, errors.New("config block must contain exactly one envelope")
		}
		data = block.Data.Data[0]
		blockNumber = &block.Header.Number
	}
	c := &channelConfig{envelope: new(cb.Envelope), payload: new(cb.Payload), config: new(cb.ConfigEnvelope), block: blockNumber}
	if err := proto.Unmarshal(data, c.envelope); err != nil {
		return nil, err
	}
	if err := proto.Unmarshal(c.envelope.Payload, c.payload); err != nil {
		return nil, err
	}
	if c.payload.Header == nil {
		return nil, errors.New("config envelope has no header")
	}
	header := new(cb.ChannelHeader)
	if err := proto.Unmarshal(c.payload.Header.ChannelHeader, header); err != nil {
		return nil, err
	}
	if header.Type != int32(cb.HeaderType_CONFIG) || header.ChannelId == "" {
		return nil, errors.New("expected CONFIG envelope with a channel ID")
	}
	c.channel = header.ChannelId
	if err := proto.Unmarshal(c.payload.Data, c.config); err != nil {
		return nil, err
	}
	if c.config.GetConfig().GetChannelGroup().GetGroups()["Application"] == nil {
		return nil, errors.New("config has no Application group")
	}
	return c, nil
}

func (c *channelConfig) bundle() (*channelconfig.Bundle, error) {
	return channelconfig.NewBundle(c.channel, c.config.Config, factory.GetDefault())
}

func (c *channelConfig) application() *cb.ConfigGroup {
	return c.config.Config.ChannelGroup.Groups["Application"]
}

func (c *channelConfig) encode() ([]byte, error) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(c.config)
	if err != nil {
		return nil, err
	}
	payload := proto.Clone(c.payload).(*cb.Payload)
	payload.Data = data
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return nil, err
	}
	// Resolved configuration is an offline initial CONFIG envelope, not a signed
	// update from the old channel. Do not retain a signature over changed bytes.
	return proto.MarshalOptions{Deterministic: true}.Marshal(&cb.Envelope{Payload: encoded})
}

func publicMSP(group *cb.ConfigGroup) (string, *mb.MSPConfig, error) {
	value := group.Values["MSP"]
	if value == nil {
		return "", nil, errors.New("organization has no MSP definition")
	}
	config := new(mb.MSPConfig)
	if err := proto.Unmarshal(value.Value, config); err != nil {
		return "", nil, err
	}
	switch config.Type {
	case 0:
		msp := new(mb.FabricMSPConfig)
		if err := proto.Unmarshal(config.Config, msp); err != nil {
			return "", nil, err
		}
		if msp.Name == "" || msp.SigningIdentity != nil {
			return "", nil, errors.New("MSP must contain an ID and public verification material only")
		}
		return msp.Name, config, nil
	case 1:
		msp := new(mb.IdemixMSPConfig)
		if err := proto.Unmarshal(config.Config, msp); err != nil {
			return "", nil, err
		}
		if msp.Name == "" || msp.Signer != nil {
			return "", nil, errors.New("Idemix MSP must contain an ID and public verification material only")
		}
		return msp.Name, config, nil
	default:
		return "", nil, fmt.Errorf("unsupported MSP type %d", config.Type)
	}
}

func reuseMSPs(target *channelConfig, sources map[string]*channelConfig) error {
	byID := map[string]*mb.MSPConfig{}
	byName := map[string]string{}
	for name, group := range target.application().Groups {
		id, msp, err := publicMSP(group)
		if err != nil {
			return fmt.Errorf("target organization %s: %w", name, err)
		}
		if _, exists := byID[id]; exists {
			return fmt.Errorf("duplicate target MSP ID %q", id)
		}
		byID[id], byName[name] = msp, id
	}
	channels := sortedKeys(sources)
	for _, channel := range channels {
		for _, name := range sortedKeys(sources[channel].application().Groups) {
			group := sources[channel].application().Groups[name]
			id, msp, err := publicMSP(group)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", channel, name, err)
			}
			if existing, found := byID[id]; found {
				if !proto.Equal(existing, msp) {
					return fmt.Errorf("conflicting MSP definitions for %q", id)
				}
				continue
			}
			if previous, exists := byName[name]; exists {
				return fmt.Errorf("organization name %q refers to both %s and %s", name, previous, id)
			}
			if target.application().Groups == nil {
				target.application().Groups = map[string]*cb.ConfigGroup{}
			}
			target.application().Groups[name] = proto.Clone(group).(*cb.ConfigGroup)
			byID[id], byName[name] = msp, id
		}
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// canonicalPolicy removes ordering differences introduced by implicit-meta
// conversion, whose subpolicies come from Go maps. Thresholds remain unchanged.
func canonicalPolicy(policy *cb.SignaturePolicyEnvelope) ([]byte, error) {
	policy = proto.Clone(policy).(*cb.SignaturePolicyEnvelope)
	identities := map[string]*mb.MSPPrincipal{}
	oldIDs := make([]string, len(policy.Identities))
	for i, principal := range policy.Identities {
		if principal == nil {
			return nil, errors.New("nil policy principal")
		}
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(principal)
		if err != nil {
			return nil, err
		}
		oldIDs[i] = string(data)
		identities[string(data)] = principal
	}
	index := map[string]int32{}
	policy.Identities = nil
	for _, key := range sortedKeys(identities) {
		index[key] = int32(len(policy.Identities))
		policy.Identities = append(policy.Identities, identities[key])
	}
	var normalize func(*cb.SignaturePolicy) error
	normalize = func(rule *cb.SignaturePolicy) error {
		if rule == nil {
			return errors.New("missing signature policy rule")
		}
		switch value := rule.Type.(type) {
		case *cb.SignaturePolicy_SignedBy:
			if value.SignedBy < 0 || int(value.SignedBy) >= len(oldIDs) {
				return errors.New("signature policy references unknown principal")
			}
			value.SignedBy = index[oldIDs[value.SignedBy]]
		case *cb.SignaturePolicy_NOutOf_:
			if value.NOutOf == nil || value.NOutOf.N < 0 || int(value.NOutOf.N) > len(value.NOutOf.Rules) {
				return errors.New("invalid signature policy threshold")
			}
			for _, child := range value.NOutOf.Rules {
				if err := normalize(child); err != nil {
					return err
				}
			}
			sort.SliceStable(value.NOutOf.Rules, func(i, j int) bool {
				a, _ := proto.Marshal(value.NOutOf.Rules[i])
				b, _ := proto.Marshal(value.NOutOf.Rules[j])
				return bytes.Compare(a, b) < 0
			})
		default:
			return errors.New("unsupported signature policy rule")
		}
		return nil
	}
	if err := normalize(policy.Rule); err != nil {
		return nil, err
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(policy)
}

func resolvePolicy(policy *pb.ApplicationPolicy, bundle *channelconfig.Bundle) ([]byte, error) {
	var signature *cb.SignaturePolicyEnvelope
	switch p := policy.GetType().(type) {
	case *pb.ApplicationPolicy_SignaturePolicy:
		signature = p.SignaturePolicy
	case *pb.ApplicationPolicy_ChannelConfigPolicyReference:
		resolved, exists := bundle.PolicyManager().GetPolicy(p.ChannelConfigPolicyReference)
		if !exists {
			return nil, fmt.Errorf("source policy %q does not exist", p.ChannelConfigPolicyReference)
		}
		converter, ok := resolved.(policies.Converter)
		if !ok {
			return nil, fmt.Errorf("source policy %q cannot be represented by a signature policy", p.ChannelConfigPolicyReference)
		}
		var err error
		signature, err = converter.Convert()
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("missing or unsupported application endorsement policy")
	}
	if signature == nil {
		return nil, errors.New("missing signature policy")
	}
	canonical, err := canonicalPolicy(signature)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(&applicationpb.NamespacePolicy{Rule: &applicationpb.NamespacePolicy_MspRule{MspRule: canonical}})
}

type chaincodePolicy struct {
	application *pb.ApplicationPolicy
	collections *pb.CollectionConfigPackage
}

func readChaincodePolicies(s *fabricsnapshot.SnapshotStream, mappings MappingConfig) (map[string]chaincodePolicy, error) {
	fields := map[string][]byte{}
	if err := s.WalkPublicRecords(func(record fabricsnapshot.Record) error {
		if record.Namespace != "_lifecycle" {
			return nil
		}
		for _, mapping := range mappings.Mappings {
			if mapping.SourceChannel != s.Metadata.ChannelName {
				continue
			}
			key := string(record.Key)
			if key == "namespaces/metadata/"+mapping.SourceNamespace || strings.HasPrefix(key, "namespaces/fields/"+mapping.SourceNamespace+"/") {
				fields[key] = record.Value
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	result := map[string]chaincodePolicy{}
	for _, mapping := range mappings.Mappings {
		if mapping.SourceChannel != s.Metadata.ChannelName {
			continue
		}
		namespace := mapping.SourceNamespace
		metadata := new(lb.StateMetadata)
		if err := proto.Unmarshal(fields["namespaces/metadata/"+namespace], metadata); err != nil {
			return nil, err
		}
		if metadata.Datatype != "ChaincodeDefinition" {
			return nil, fmt.Errorf("%s/%s has no committed lifecycle chaincode definition", s.Metadata.ChannelName, namespace)
		}
		sequence := new(lb.StateData)
		if err := proto.Unmarshal(fields["namespaces/fields/"+namespace+"/Sequence"], sequence); err != nil {
			return nil, err
		}
		if sequence.GetInt64() <= 0 {
			return nil, fmt.Errorf("%s/%s has no committed chaincode sequence", s.Metadata.ChannelName, namespace)
		}
		validation := new(lb.ChaincodeValidationInfo)
		field := new(lb.StateData)
		if err := proto.Unmarshal(fields["namespaces/fields/"+namespace+"/ValidationInfo"], field); err != nil {
			return nil, err
		}
		if err := proto.Unmarshal(field.GetBytes(), validation); err != nil {
			return nil, err
		}
		if validation.ValidationPlugin != "vscc" {
			return nil, fmt.Errorf("%s/%s uses unsupported validation plugin %q", s.Metadata.ChannelName, namespace, validation.ValidationPlugin)
		}
		application := new(pb.ApplicationPolicy)
		if err := proto.Unmarshal(validation.ValidationParameter, application); err != nil {
			return nil, err
		}
		collections := new(pb.CollectionConfigPackage)
		field = new(lb.StateData)
		if err := proto.Unmarshal(fields["namespaces/fields/"+namespace+"/Collections"], field); err != nil {
			return nil, err
		}
		if err := proto.Unmarshal(field.GetBytes(), collections); err != nil {
			return nil, err
		}
		result[namespace] = chaincodePolicy{application: application, collections: collections}
	}
	return result, nil
}

func preparePolicies(config MappingConfig, snapshots map[string]*fabricsnapshot.SnapshotStream, sources map[string]*channelConfig, target *channelConfig) (map[string][]byte, error) {
	if err := reuseMSPs(target, sources); err != nil {
		return nil, err
	}
	encoded, err := target.encode()
	if err != nil {
		return nil, err
	}
	if err := fxpolicy.ValidateConfigTx(encoded); err != nil {
		return nil, fmt.Errorf("target configuration: %w", err)
	}
	targetBundle, err := target.bundle()
	if err != nil {
		return nil, err
	}
	result := map[string][]byte{}
	add := func(namespace string, application *pb.ApplicationPolicy, source *channelconfig.Bundle) error {
		encoded, err := resolvePolicy(application, source)
		if err != nil {
			return err
		}
		if previous, exists := result[namespace]; exists && !bytes.Equal(previous, encoded) {
			return fmt.Errorf("conflicting endorsement policies for target namespace %q", namespace)
		}
		if _, err := fxpolicy.CreateNamespaceVerifier(&applicationpb.PolicyItem{Namespace: namespace, Policy: encoded}, targetBundle.MSPManager()); err != nil {
			return err
		}
		result[namespace] = encoded
		return nil
	}
	for _, channel := range sortedKeys(snapshots) {
		s := snapshots[channel]
		bundle, err := sources[channel].bundle()
		if err != nil {
			return nil, fmt.Errorf("source channel %s: %w", channel, err)
		}
		definitions, err := readChaincodePolicies(s, config)
		if err != nil {
			return nil, err
		}
		for namespace, definition := range definitions {
			mapping, _ := mappingFor(config, channel, namespace)
			if err := add(mapping.TargetNamespace, definition.application, bundle); err != nil {
				return nil, fmt.Errorf("%s/%s: %w", channel, namespace, err)
			}
			for _, hashes := range s.PrivateHashNamespaces {
				source, collection, err := fabricsnapshot.SplitHashNamespace(hashes.Name)
				if err != nil {
					return nil, err
				}
				if source != namespace {
					continue
				}
				effective := definition.application
				found := false
				for _, item := range definition.collections.Config {
					c := item.GetStaticCollectionConfig()
					if c == nil {
						return nil, errors.New("unsupported collection configuration")
					}
					if c.Name == collection {
						found = true
						if c.EndorsementPolicy != nil {
							effective = c.EndorsementPolicy
						}
					}
				}
				// Implicit collections use their owning organization's endorsement policy.
				if orgMSP, implicit := strings.CutPrefix(collection, "_implicit_org_"); implicit {
					application, ok := bundle.ApplicationConfig()
					if !ok {
						return nil, errors.New("source channel has no application organizations")
					}
					for orgName, org := range application.Organizations() {
						if org.MSPID() != orgMSP {
							continue
						}
						found = true
						path := "/Channel/Application/" + orgName + "/Endorsement"
						if _, exists := bundle.PolicyManager().GetPolicy(path); exists {
							effective = &pb.ApplicationPolicy{Type: &pb.ApplicationPolicy_ChannelConfigPolicyReference{ChannelConfigPolicyReference: path}}
						} else {
							effective = &pb.ApplicationPolicy{Type: &pb.ApplicationPolicy_SignaturePolicy{SignaturePolicy: policydsl.SignedByAnyMember([]string{orgMSP})}}
						}
					}
				}
				if !found {
					return nil, fmt.Errorf("%s/%s has hashes for unknown collection %q", channel, namespace, collection)
				}
				if err := add(mapping.hashTarget(), effective, bundle); err != nil {
					return nil, fmt.Errorf("%s/%s: %w", channel, hashes.Name, err)
				}
			}
		}
	}
	return result, nil
}
