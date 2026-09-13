package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/syndbg/fabric-x-migrate-poc/internal/fabricsnapshot"
)

type Mapping struct {
	SourceChannel       string `json:"source_channel"`
	SourceNamespace     string `json:"source_namespace"`
	TargetNamespace     string `json:"target_namespace"`
	TargetHashNamespace string `json:"target_hash_namespace,omitempty"`
}

type MappingConfig struct {
	TargetNetwork string    `json:"target_network"`
	Mappings      []Mapping `json:"mappings"`
}

type Options struct {
	Snapshots        map[string]string
	SourceConfigs    map[string]string
	MappingPath      string
	TargetConfigPath string
}

type inputFile struct {
	path string
	data []byte
}

func readInput(path string) (inputFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return inputFile{}, err
	}
	if !info.Mode().IsRegular() {
		return inputFile{}, fmt.Errorf("input is not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	return inputFile{path: path, data: data}, err
}

func (f inputFile) revalidate() error {
	current, err := readInput(f.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current.data, f.data) {
		return fmt.Errorf("input changed during import: %s", f.path)
	}
	return nil
}

func (f inputFile) sha256() string {
	sum := sha256.Sum256(f.data)
	return hex.EncodeToString(sum[:])
}

func readMappings(file inputFile) (MappingConfig, error) {
	var config MappingConfig
	decoder := json.NewDecoder(bytes.NewReader(file.data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("mapping: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return config, errors.New("mapping must contain one JSON object")
	}
	if config.TargetNetwork == "" || len(config.Mappings) == 0 {
		return config, errors.New("target_network and at least one mapping are required")
	}
	seen := map[[2]string]bool{}
	for _, mapping := range config.Mappings {
		key := [2]string{mapping.SourceChannel, mapping.SourceNamespace}
		if seen[key] {
			return config, fmt.Errorf("source %s/%s is mapped more than once", key[0], key[1])
		}
		seen[key] = true
		if key[0] == "" || key[1] == "" || strings.HasPrefix(key[1], "_") || strings.Contains(key[1], "$$") || key[1] == "lscc" || key[1] == "cscc" || key[1] == "qscc" {
			return config, fmt.Errorf("invalid application source %s/%s", key[0], key[1])
		}
		if err := validateTargetNamespace(mapping.TargetNamespace); err != nil {
			return config, err
		}
		if mapping.TargetHashNamespace != "" {
			if err := validateTargetNamespace(mapping.TargetHashNamespace); err != nil {
				return config, err
			}
		}
	}
	sort.Slice(config.Mappings, func(i, j int) bool {
		a, b := config.Mappings[i], config.Mappings[j]
		if a.SourceChannel != b.SourceChannel {
			return a.SourceChannel < b.SourceChannel
		}
		return a.SourceNamespace < b.SourceNamespace
	})
	return config, nil
}

func validateTargetNamespace(namespace string) error {
	if committerpb.IsSystemNamespace(namespace) {
		return fmt.Errorf("reserved target namespace %q", namespace)
	}
	if err := policy.ValidateNamespaceID(namespace); err != nil {
		return fmt.Errorf("invalid target namespace %q: %w", namespace, err)
	}
	return nil
}

func (m Mapping) hashTarget() string {
	if m.TargetHashNamespace != "" {
		return m.TargetHashNamespace
	}
	return m.TargetNamespace
}

func mappingFor(config MappingConfig, channel, namespace string) (Mapping, bool) {
	for _, m := range config.Mappings {
		if m.SourceChannel == channel && m.SourceNamespace == namespace {
			return m, true
		}
	}
	return Mapping{}, false
}

func mappingCounts(config MappingConfig, snapshots map[string]*fabricsnapshot.SnapshotStream) (map[string]uint64, error) {
	counts := map[string]uint64{}
	for _, m := range config.Mappings {
		s, ok := snapshots[m.SourceChannel]
		if !ok {
			return nil, fmt.Errorf("missing source channel %q", m.SourceChannel)
		}
		found := false
		counts[m.TargetNamespace] += 0
		for _, ns := range s.PublicNamespaces {
			if ns.Name == m.SourceNamespace {
				counts[m.TargetNamespace] += ns.Records
				found = true
			}
		}
		for _, ns := range s.PrivateHashNamespaces {
			source, _, err := fabricsnapshot.SplitHashNamespace(ns.Name)
			if err != nil {
				return nil, err
			}
			if source == m.SourceNamespace {
				counts[m.hashTarget()] += ns.Records
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("source namespace %s/%s is not in the snapshot", m.SourceChannel, m.SourceNamespace)
		}
	}
	return counts, nil
}
