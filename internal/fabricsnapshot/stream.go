package fabricsnapshot

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SnapshotStream verifies snapshot metadata and file hashes without retaining
// ledger rows in memory. Its walk methods validate and yield one row at a time.
type SnapshotStream struct {
	Directory             string
	Metadata              Metadata
	AdditionalMetadata    AdditionalMetadata
	PublicNamespaces      []Namespace
	PrivateHashNamespaces []Namespace
	fileHashes            map[string]string
}

func OpenStream(directory string) (*SnapshotStream, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("snapshot path is not a directory: %s", directory)
	}
	signable, err := readRegular(filepath.Join(directory, "_snapshot_signable_metadata.json"))
	if err != nil {
		return nil, err
	}
	additional, err := readRegular(filepath.Join(directory, "_snapshot_additional_metadata.json"))
	if err != nil {
		return nil, err
	}
	var metadata Metadata
	if err := decodeJSON(signable, &metadata); err != nil {
		return nil, fmt.Errorf("decode signable metadata: %w", err)
	}
	var extra AdditionalMetadata
	if err := decodeJSON(additional, &extra); err != nil {
		return nil, fmt.Errorf("decode additional metadata: %w", err)
	}
	if metadata.ChannelName == "" || metadata.StateDBType == "" || len(metadata.SnapshotFilesRawHashes) == 0 {
		return nil, errors.New("incomplete snapshot metadata")
	}
	if metadata.StateDBType != "SimpleKeyValueDB" && metadata.StateDBType != "CouchDB" {
		return nil, fmt.Errorf("unsupported Fabric state database %q", metadata.StateDBType)
	}
	if hash(signable) != extra.SnapshotHash {
		return nil, errors.New("snapshot signable metadata hash mismatch")
	}
	if _, ok := metadata.SnapshotFilesRawHashes["public_state.data"]; !ok {
		return nil, errors.New("snapshot is missing public_state.data")
	}
	if _, ok := metadata.SnapshotFilesRawHashes["public_state.metadata"]; !ok {
		return nil, errors.New("snapshot is missing public_state.metadata")
	}
	_, hashData := metadata.SnapshotFilesRawHashes["private_state_hashes.data"]
	_, hashMetadata := metadata.SnapshotFilesRawHashes["private_state_hashes.metadata"]
	if hashData != hashMetadata {
		return nil, errors.New("snapshot has incomplete private_state_hashes file pair")
	}

	allowed := map[string]bool{
		"_snapshot_signable_metadata.json":   true,
		"_snapshot_additional_metadata.json": true,
	}
	for name := range metadata.SnapshotFilesRawHashes {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.HasPrefix(name, "_snapshot_") {
			return nil, fmt.Errorf("invalid snapshot file name %q", name)
		}
		allowed[name] = true
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return nil, fmt.Errorf("unexpected snapshot entry %q", entry.Name())
		}
	}

	hashes := make(map[string]string, len(metadata.SnapshotFilesRawHashes))
	names := make([]string, 0, len(metadata.SnapshotFilesRawHashes))
	for name, expected := range metadata.SnapshotFilesRawHashes {
		hashes[name] = expected
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		actual, err := hashRegularFile(filepath.Join(directory, name))
		if err != nil {
			return nil, err
		}
		if actual != hashes[name] {
			return nil, fmt.Errorf("snapshot file hash mismatch: %s", name)
		}
	}

	hashes["_snapshot_signable_metadata.json"] = hash(signable)
	hashes["_snapshot_additional_metadata.json"] = hash(additional)
	s := &SnapshotStream{
		Directory: directory, Metadata: metadata, AdditionalMetadata: extra, fileHashes: hashes,
	}
	s.PublicNamespaces, err = s.readNamespaces("public_state")
	if err != nil {
		return nil, err
	}
	if hashData {
		s.PrivateHashNamespaces, err = s.readNamespaces("private_state_hashes")
		if err != nil {
			return nil, err
		}
		for _, ns := range s.PrivateHashNamespaces {
			if _, _, err := SplitHashNamespace(ns.Name); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// SplitHashNamespace decodes Fabric's existing namespace$$hcollection name.
func SplitHashNamespace(name string) (string, string, error) {
	namespace, collection, ok := strings.Cut(name, "$$h")
	if !ok || namespace == "" || collection == "" || strings.Contains(collection, "$$") {
		return "", "", fmt.Errorf("invalid private hash namespace %q", name)
	}
	return namespace, collection, nil
}

func (s *SnapshotStream) readNamespaces(prefix string) ([]Namespace, error) {
	name := prefix + ".metadata"
	data, err := readRegular(filepath.Join(s.Directory, name))
	if err != nil {
		return nil, err
	}
	if hash(data) != s.fileHashes[name] {
		return nil, fmt.Errorf("snapshot file hash mismatch: %s", name)
	}
	namespaces, err := decodeNamespaces(name, data)
	if err != nil {
		return nil, err
	}
	for i, ns := range namespaces {
		if i > 0 && namespaces[i-1].Name >= ns.Name {
			return nil, fmt.Errorf("%s: namespaces are not strictly ordered", name)
		}
	}
	return namespaces, nil
}

// Revalidate checks every input again, including excluded files and both metadata
// files. Call immediately before committing the target transaction.
func (s *SnapshotStream) Revalidate() error {
	entries, err := os.ReadDir(s.Directory)
	if err != nil {
		return err
	}
	if len(entries) != len(s.fileHashes) {
		return errors.New("snapshot file set changed after validation")
	}
	for _, entry := range entries {
		expected, ok := s.fileHashes[entry.Name()]
		if !ok {
			return fmt.Errorf("unexpected snapshot entry %q", entry.Name())
		}
		actual, err := hashRegularFile(filepath.Join(s.Directory, entry.Name()))
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("snapshot file hash mismatch: %s", entry.Name())
		}
	}
	return nil
}

func (s *SnapshotStream) WalkPublicRecords(yield func(Record) error) error {
	return s.walkRecords("public_state", yield)
}

func (s *SnapshotStream) WalkPrivateHashRecords(yield func(Record) error) error {
	if _, ok := s.fileHashes["private_state_hashes.data"]; !ok {
		return nil
	}
	return s.walkRecords("private_state_hashes", yield)
}

func (s *SnapshotStream) walkRecords(prefix string, yield func(Record) error) (returnErr error) {
	namespaces, err := s.readNamespaces(prefix)
	if err != nil {
		return err
	}
	name := prefix + ".data"
	file, err := openRegular(filepath.Join(s.Directory, name))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	hasher := sha256.New()
	reader := bufio.NewReader(io.TeeReader(file, hasher))
	version, err := reader.ReadByte()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if version != formatVersion {
		return fmt.Errorf("%s: unsupported format byte", name)
	}
	for _, namespace := range namespaces {
		var previousKey []byte
		for range namespace.Records {
			raw, err := readSized(reader)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fields, err := protobufBytes(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			key, keyOK := fields[1]
			sourceVersion, versionOK := fields[4]
			if !keyOK || !versionOK || len(key) == 0 {
				return fmt.Errorf("%s: incomplete snapshot record", name)
			}
			orderKey := key
			if prefix == "private_state_hashes" {
				if len(key) != sha256.Size || len(fields[2]) != sha256.Size {
					return fmt.Errorf("%s: key and value must be SHA-256 hashes", name)
				}
				// CouchDB sorts the Base64 keys before Fabric decodes them for export.
				if s.Metadata.StateDBType == "CouchDB" {
					orderKey = []byte(base64.StdEncoding.EncodeToString(key))
				}
			}
			if previousKey != nil && bytes.Compare(previousKey, orderKey) >= 0 {
				return fmt.Errorf("%s: keys are not strictly ordered in namespace %q", name, namespace.Name)
			}
			previousKey = orderKey
			block, _, err := decodeHeight(sourceVersion)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if block > s.Metadata.LastBlockNumber {
				return fmt.Errorf("%s: key version is after the snapshot checkpoint", name)
			}
			// Source versions are validated above, then discarded.
			record := Record{Namespace: namespace.Name, Key: key, Value: fields[2], Metadata: fields[3]}
			if yield != nil {
				if err := yield(record); err != nil {
					return err
				}
			}
		}
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s: trailing bytes", name)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != s.fileHashes[name] {
		return fmt.Errorf("snapshot file hash mismatch: %s", name)
	}
	return nil
}

func readSized(reader *bufio.Reader) ([]byte, error) {
	size, err := binary.ReadUvarint(reader)
	if err != nil {
		return nil, err
	}
	if size > uint64(^uint(0)>>1) {
		return nil, errors.New("record is too large")
	}
	// Grow only as bytes arrive; an untrusted length must not allocate terabytes.
	var data bytes.Buffer
	if _, err := io.CopyN(&data, reader, int64(size)); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func hashRegularFile(path string) (digest string, returnErr error) {
	file, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
