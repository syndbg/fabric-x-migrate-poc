package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cb "github.com/hyperledger/fabric-protos-go-apiv2/common"
	mb "github.com/hyperledger/fabric-protos-go-apiv2/msp"
	pb "github.com/hyperledger/fabric-protos-go-apiv2/peer"
	lb "github.com/hyperledger/fabric-protos-go-apiv2/peer/lifecycle"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/syndbg/fabric-x-migrate-poc/internal/fabricsnapshot"
)

func TestMappingsAndPolicies(t *testing.T) {
	options := testOptions(t, false)
	file, err := readInput(options.MappingPath)
	require.NoError(t, err)
	config, err := readMappings(file)
	require.NoError(t, err)
	snapshots := map[string]*fabricsnapshot.SnapshotStream{}
	sources := map[string]*channelConfig{}
	for channel, path := range options.Snapshots {
		snapshots[channel], err = fabricsnapshot.OpenStream(path)
		require.NoError(t, err)
		data, err := os.ReadFile(options.SourceConfigs[channel])
		require.NoError(t, err)
		sources[channel], err = decodeChannelConfig(data)
		require.NoError(t, err)
	}
	counts, err := mappingCounts(config, snapshots)
	require.NoError(t, err)
	require.Equal(t, map[string]uint64{"basic": 4}, counts)
	target := testChannel(t, "network1")
	// The CLI must populate missing public membership before target validation.
	target.application().Groups = nil
	policies, err := preparePolicies(config, snapshots, sources, target)
	require.NoError(t, err)
	require.Len(t, policies, 1)
	require.NotEmpty(t, target.application().Groups)
	encoded, err := target.encode()
	require.NoError(t, err)
	for range 10 {
		repeated, err := preparePolicies(config, snapshots, sources, target)
		require.NoError(t, err)
		require.Equal(t, policies, repeated)
		configBytes, err := target.encode()
		require.NoError(t, err)
		require.Equal(t, encoded, configBytes)
	}
	policy := new(applicationpb.NamespacePolicy)
	require.NoError(t, proto.Unmarshal(policies["basic"], policy))
	signature := new(cb.SignaturePolicyEnvelope)
	require.NoError(t, proto.Unmarshal(policy.GetMspRule(), signature))
	require.NotEmpty(t, signature.Identities)

	t.Run("conflicting membership", func(t *testing.T) {
		conflicting := testChannel(t, "network1")
		for _, group := range conflicting.application().Groups {
			msp := new(mb.MSPConfig)
			require.NoError(t, proto.Unmarshal(group.Values["MSP"].Value, msp))
			fabric := new(mb.FabricMSPConfig)
			require.NoError(t, proto.Unmarshal(msp.Config, fabric))
			fabric.RevocationList = [][]byte{[]byte("different")}
			msp.Config = marshal(t, fabric)
			group.Values["MSP"].Value = marshal(t, msp)
		}
		require.ErrorContains(t, reuseMSPs(conflicting, sources), "conflicting MSP")
	})
	t.Run("duplicate source and reserved destination", func(t *testing.T) {
		bad := config
		bad.Mappings = append(append([]Mapping{}, config.Mappings...), config.Mappings[0])
		_, err := readMappings(inputFile{data: marshalJSON(t, bad)})
		require.ErrorContains(t, err, "more than once")
		bad.Mappings = []Mapping{{SourceChannel: "assets", SourceNamespace: "basic", TargetNamespace: "_config"}}
		_, err = readMappings(inputFile{data: marshalJSON(t, bad)})
		require.ErrorContains(t, err, "reserved")
	})
}

func TestCanonicalPolicyPreservesThresholdAndIgnoresOrdering(t *testing.T) {
	a, b := &mb.MSPPrincipal{Principal: []byte("A")}, &mb.MSPPrincipal{Principal: []byte("B")}
	first := &cb.SignaturePolicyEnvelope{Identities: []*mb.MSPPrincipal{a, b}, Rule: nOutOf(2, signedBy(0), signedBy(1))}
	second := &cb.SignaturePolicyEnvelope{Identities: []*mb.MSPPrincipal{b, a}, Rule: nOutOf(2, signedBy(0), signedBy(1))}
	x, err := canonicalPolicy(first)
	require.NoError(t, err)
	y, err := canonicalPolicy(second)
	require.NoError(t, err)
	require.Equal(t, x, y)
	second.Rule = nOutOf(1, signedBy(0), signedBy(1))
	y, err = canonicalPolicy(second)
	require.NoError(t, err)
	require.NotEqual(t, x, y)
}

func signedBy(index int32) *cb.SignaturePolicy {
	return &cb.SignaturePolicy{Type: &cb.SignaturePolicy_SignedBy{SignedBy: index}}
}
func nOutOf(n int32, rules ...*cb.SignaturePolicy) *cb.SignaturePolicy {
	return &cb.SignaturePolicy{Type: &cb.SignaturePolicy_NOutOf_{NOutOf: &cb.SignaturePolicy_NOutOf{N: n, Rules: rules}}}
}

func testChannel(t *testing.T, channel string) *channelConfig {
	t.Helper()
	data, err := os.ReadFile("testdata/source.block")
	require.NoError(t, err)
	config, err := decodeChannelConfig(data)
	require.NoError(t, err)
	header := new(cb.ChannelHeader)
	require.NoError(t, proto.Unmarshal(config.payload.Header.ChannelHeader, header))
	header.ChannelId = channel
	config.payload.Header.ChannelHeader = marshal(t, header)
	config.channel = channel
	for _, name := range []string{"SnapshotEndorsement", "CheckpointEndorsement"} {
		config.application().Policies[name] = proto.Clone(config.application().Policies["LifecycleEndorsement"]).(*cb.ConfigPolicy)
	}
	return config
}

func testOptions(t *testing.T, separateHashes bool) Options {
	t.Helper()
	dir := t.TempDir()
	options := Options{Snapshots: map[string]string{}, SourceConfigs: map[string]string{}, MappingPath: filepath.Join(dir, "mapping.json"), TargetConfigPath: filepath.Join(dir, "target.config")}
	mapping := MappingConfig{TargetNetwork: "network1"}
	for i, channel := range []string{"assets", "securities"} {
		config := testChannel(t, channel)
		envelope, err := config.encode()
		require.NoError(t, err)
		block := &cb.Block{Header: &cb.BlockHeader{}, Data: &cb.BlockData{Data: [][]byte{envelope}}}
		options.SourceConfigs[channel] = filepath.Join(dir, channel+".block")
		require.NoError(t, os.WriteFile(options.SourceConfigs[channel], marshal(t, block), 0o600))
		options.Snapshots[channel] = testSnapshot(t, channel, byte(i+1))
		m := Mapping{SourceChannel: channel, SourceNamespace: "basic", TargetNamespace: "basic"}
		if separateHashes {
			m.TargetHashNamespace = "hashes"
		}
		mapping.Mappings = append(mapping.Mappings, m)
	}
	require.NoError(t, os.WriteFile(options.MappingPath, marshalJSON(t, mapping), 0o600))
	target, err := testChannel(t, "network1").encode()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(options.TargetConfigPath, target, 0o600))
	return options
}

func testSnapshot(t *testing.T, channel string, keyByte byte) string {
	t.Helper()
	dir := t.TempDir()
	application := &pb.ApplicationPolicy{Type: &pb.ApplicationPolicy_ChannelConfigPolicyReference{ChannelConfigPolicyReference: "/Channel/Application/Endorsement"}}
	validation := &lb.ChaincodeValidationInfo{ValidationPlugin: "vscc", ValidationParameter: marshal(t, application)}
	collections := &pb.CollectionConfigPackage{Config: []*pb.CollectionConfig{{Payload: &pb.CollectionConfig_StaticCollectionConfig{StaticCollectionConfig: &pb.StaticCollectionConfig{Name: "owners"}}}}}
	lifecycle := map[string][]byte{
		"namespaces/metadata/basic":              marshal(t, &lb.StateMetadata{Datatype: "ChaincodeDefinition", Fields: []string{"Sequence", "EndorsementInfo", "ValidationInfo", "Collections"}}),
		"namespaces/fields/basic/Sequence":       marshal(t, &lb.StateData{Type: &lb.StateData_Int64{Int64: 1}}),
		"namespaces/fields/basic/ValidationInfo": marshal(t, &lb.StateData{Type: &lb.StateData_Bytes{Bytes: marshal(t, validation)}}),
		"namespaces/fields/basic/Collections":    marshal(t, &lb.StateData{Type: &lb.StateData_Bytes{Bytes: marshal(t, collections)}}),
	}
	data := []byte{1}
	for _, key := range sortedKeys(lifecycle) {
		data = appendRecord(data, []byte(key), lifecycle[key])
	}
	data = appendRecord(data, []byte{keyByte}, []byte("public value"))
	metadata := binary.AppendUvarint([]byte{1}, 2)
	metadata = appendSized(metadata, []byte("_lifecycle"))
	metadata = binary.AppendUvarint(metadata, uint64(len(lifecycle)))
	metadata = appendSized(metadata, []byte("basic"))
	metadata = binary.AppendUvarint(metadata, 1)
	hashData := appendRecord([]byte{1}, bytes.Repeat([]byte{keyByte}, 32), bytes.Repeat([]byte{42}, 32))
	hashMetadata := binary.AppendUvarint(appendSized([]byte{1, 1}, []byte("basic$$howners")), 1)
	writeSnapshot(t, dir, channel, map[string][]byte{"public_state.data": data, "public_state.metadata": metadata, "private_state_hashes.data": hashData, "private_state_hashes.metadata": hashMetadata})
	return dir
}

func writeSnapshot(t *testing.T, dir, channel string, files map[string][]byte) {
	t.Helper()
	hashes := map[string]string{}
	for name, data := range files {
		sum := sha256.Sum256(data)
		hashes[name] = hex.EncodeToString(sum[:])
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	metadata := marshalJSON(t, fabricsnapshot.Metadata{ChannelName: channel, LastBlockNumber: 3, StateDBType: "SimpleKeyValueDB", SnapshotFilesRawHashes: hashes})
	sum := sha256.Sum256(metadata)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "_snapshot_signable_metadata.json"), metadata, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "_snapshot_additional_metadata.json"), marshalJSON(t, fabricsnapshot.AdditionalMetadata{SnapshotHash: hex.EncodeToString(sum[:])}), 0o600))
}

func appendRecord(data, key, value []byte) []byte {
	record := appendField(nil, 1, key)
	record = appendField(record, 2, value)
	record = appendField(record, 4, []byte{1, 3, 0})
	return appendSized(data, record)
}
func appendField(data []byte, field byte, value []byte) []byte {
	return appendSized(append(data, field<<3|2), value)
}
func appendSized(data, value []byte) []byte {
	return append(binary.AppendUvarint(data, uint64(len(value))), value...)
}
func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	require.NoError(t, e)
	return b
}
func marshalJSON(t *testing.T, m any) []byte {
	t.Helper()
	b, e := json.Marshal(m)
	require.NoError(t, e)
	return b
}

// The fixture is a config block from the repository's local Fabric test network.
// It contains public MSP verification material only.
