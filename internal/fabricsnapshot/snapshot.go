package fabricsnapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const formatVersion = byte(1)

type Snapshot struct {
	Metadata              Metadata
	AdditionalMetadata    AdditionalMetadata
	Files                 []File
	PublicNamespaces      []Namespace
	PublicRecords         []Record
	PrivateHashNamespaces []Namespace
	ConfigHistoryRecords  uint64
	TransactionIDs        []string
}

func Read(directory string) (*Snapshot, error) {
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

	contents := map[string][]byte{
		"_snapshot_signable_metadata.json":   signable,
		"_snapshot_additional_metadata.json": additional,
	}
	allowed := map[string]bool{
		"_snapshot_signable_metadata.json":   true,
		"_snapshot_additional_metadata.json": true,
	}
	for name, expected := range metadata.SnapshotFilesRawHashes {
		if name == "" || filepath.Base(name) != name {
			return nil, fmt.Errorf("invalid snapshot file name %q", name)
		}
		data, err := readRegular(filepath.Join(directory, name))
		if err != nil {
			return nil, err
		}
		if hash(data) != expected {
			return nil, fmt.Errorf("snapshot file hash mismatch: %s", name)
		}
		contents[name] = data
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

	publicNamespaces, publicRecords, err := decodeRecords("public_state", contents)
	if err != nil {
		return nil, err
	}
	privateNamespaces, _, err := decodeOptionalRecords("private_state_hashes", contents)
	if err != nil {
		return nil, err
	}
	configHistoryRecords, err := decodeConfigHistory(contents)
	if err != nil {
		return nil, err
	}
	txIDs, err := decodeTxIDs(contents["txids.data"], contents["txids.metadata"])
	if err != nil {
		return nil, err
	}

	files := make([]File, 0, len(contents))
	for name, data := range contents {
		file := File{Name: name, Size: int64(len(data)), SHA256: hash(data)}
		if strings.HasSuffix(name, ".data") || strings.HasSuffix(name, ".metadata") {
			if len(data) == 0 {
				return nil, fmt.Errorf("empty snapshot file %s", name)
			}
			format := uint8(data[0])
			file.FormatByte = &format
		}
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return &Snapshot{
		Metadata:              metadata,
		AdditionalMetadata:    extra,
		Files:                 files,
		PublicNamespaces:      publicNamespaces,
		PublicRecords:         publicRecords,
		PrivateHashNamespaces: privateNamespaces,
		ConfigHistoryRecords:  configHistoryRecords,
		TransactionIDs:        txIDs,
	}, nil
}

func decodeConfigHistory(files map[string][]byte) (uint64, error) {
	data, dataOK := files["confighistory.data"]
	metadata, metadataOK := files["confighistory.metadata"]
	if !dataOK && !metadataOK {
		return 0, nil
	}
	if !dataOK || !metadataOK {
		return 0, errors.New("confighistory requires data and metadata files")
	}
	if len(metadata) == 0 || metadata[0] != formatVersion {
		return 0, errors.New("confighistory.metadata: unsupported format byte")
	}
	count, offset, err := consumeUvarint(metadata, 1)
	if err != nil {
		return 0, fmt.Errorf("confighistory.metadata: %w", err)
	}
	if offset != len(metadata) {
		return 0, fmt.Errorf("confighistory.metadata: %d trailing bytes", len(metadata)-offset)
	}
	if len(data) == 0 || data[0] != formatVersion {
		return 0, errors.New("confighistory.data: unsupported format byte")
	}
	offset = 1
	for range count {
		if _, offset, err = sized(data, offset); err != nil {
			return 0, fmt.Errorf("confighistory.data key: %w", err)
		}
		if _, offset, err = sized(data, offset); err != nil {
			return 0, fmt.Errorf("confighistory.data value: %w", err)
		}
	}
	if offset != len(data) {
		return 0, fmt.Errorf("confighistory.data: %d trailing bytes", len(data)-offset)
	}
	return count, nil
}

func decodeOptionalRecords(prefix string, files map[string][]byte) ([]Namespace, []Record, error) {
	data, dataOK := files[prefix+".data"]
	metadata, metadataOK := files[prefix+".metadata"]
	if !dataOK && !metadataOK {
		return nil, nil, nil
	}
	if !dataOK || !metadataOK {
		return nil, nil, fmt.Errorf("snapshot has incomplete %s file pair", prefix)
	}
	return decodeRecordBytes(prefix, data, metadata)
}

func decodeRecords(prefix string, files map[string][]byte) ([]Namespace, []Record, error) {
	data, dataOK := files[prefix+".data"]
	metadata, metadataOK := files[prefix+".metadata"]
	if !dataOK || !metadataOK {
		return nil, nil, fmt.Errorf("snapshot is missing %s file pair", prefix)
	}
	return decodeRecordBytes(prefix, data, metadata)
}

func decodeRecordBytes(prefix string, data, metadata []byte) ([]Namespace, []Record, error) {
	namespaces, err := decodeNamespaces(prefix+".metadata", metadata)
	if err != nil {
		return nil, nil, err
	}
	if len(data) == 0 || data[0] != formatVersion {
		return nil, nil, fmt.Errorf("%s.data: unsupported format byte", prefix)
	}
	offset := 1
	var records []Record
	for _, namespace := range namespaces {
		for range namespace.Records {
			raw, next, err := sized(data, offset)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.data: %w", prefix, err)
			}
			offset = next
			fields, err := protobufBytes(raw)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.data: %w", prefix, err)
			}
			key, keyOK := fields[1]
			value := fields[2]
			version, versionOK := fields[4]
			if !keyOK || !versionOK || len(key) == 0 {
				return nil, nil, fmt.Errorf("%s.data: incomplete snapshot record", prefix)
			}
			block, transaction, err := decodeHeight(version)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.data: %w", prefix, err)
			}
			records = append(records, Record{
				Namespace: namespace.Name, Key: key, Value: value, Metadata: fields[3], Version: version,
				BlockNumber: block, TransactionNumber: transaction,
			})
		}
	}
	if offset != len(data) {
		return nil, nil, fmt.Errorf("%s.data: %d trailing bytes", prefix, len(data)-offset)
	}
	return namespaces, records, nil
}

func decodeNamespaces(name string, data []byte) ([]Namespace, error) {
	if len(data) == 0 || data[0] != formatVersion {
		return nil, fmt.Errorf("%s: unsupported format byte", name)
	}
	count, offset, err := consumeUvarint(data, 1)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	result := make([]Namespace, 0, count)
	for range count {
		namespace, next, err := sized(data, offset)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		offset = next
		records, next, err := consumeUvarint(data, offset)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		offset = next
		result = append(result, Namespace{Name: string(namespace), Records: records})
	}
	if offset != len(data) {
		return nil, fmt.Errorf("%s: %d trailing bytes", name, len(data)-offset)
	}
	return result, nil
}

func decodeTxIDs(data, metadata []byte) ([]string, error) {
	if len(metadata) == 0 || metadata[0] != formatVersion {
		return nil, errors.New("txids.metadata: unsupported format byte")
	}
	count, offset, err := consumeUvarint(metadata, 1)
	if err != nil {
		return nil, fmt.Errorf("txids.metadata: %w", err)
	}
	if offset != len(metadata) {
		return nil, fmt.Errorf("txids.metadata: %d trailing bytes", len(metadata)-offset)
	}
	if len(data) == 0 || data[0] != formatVersion {
		return nil, errors.New("txids.data: unsupported format byte")
	}
	offset = 1
	result := make([]string, 0, count)
	seen := make(map[string]bool, count)
	for range count {
		raw, next, err := sized(data, offset)
		if err != nil {
			return nil, fmt.Errorf("txids.data: %w", err)
		}
		offset = next
		id := string(raw)
		if id == "" || seen[id] {
			return nil, fmt.Errorf("txids.data: empty or duplicate transaction ID %q", id)
		}
		seen[id] = true
		result = append(result, id)
	}
	if offset != len(data) {
		return nil, fmt.Errorf("txids.data: %d trailing bytes", len(data)-offset)
	}
	return result, nil
}

func protobufBytes(data []byte) (map[uint64][]byte, error) {
	fields := map[uint64][]byte{}
	seen := map[uint64]bool{}
	for offset := 0; offset < len(data); {
		tag, next, err := consumeUvarint(data, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		field := tag >> 3
		if tag&7 != 2 || field < 1 || field > 4 || seen[field] {
			return nil, fmt.Errorf("unsupported or duplicate protobuf field %d", field)
		}
		value, next, err := sized(data, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		fields[field] = value
		seen[field] = true
	}
	return fields, nil
}

func decodeHeight(data []byte) (uint64, uint64, error) {
	block, offset, err := orderPreservingUint(data, 0)
	if err != nil {
		return 0, 0, err
	}
	transaction, offset, err := orderPreservingUint(data, offset)
	if err != nil {
		return 0, 0, err
	}
	if offset != len(data) {
		return 0, 0, errors.New("fabric height has trailing bytes")
	}
	return block, transaction, nil
}

func orderPreservingUint(data []byte, offset int) (uint64, int, error) {
	size, next, err := consumeUvarint(data, offset)
	if err != nil {
		return 0, 0, err
	}
	if size > 8 || size > uint64(len(data)-next) {
		return 0, 0, errors.New("invalid Fabric order-preserving uint")
	}
	end := next + int(size)
	var padded [8]byte
	copy(padded[8-int(size):], data[next:end])
	return binary.BigEndian.Uint64(padded[:]), end, nil
}

func sized(data []byte, offset int) ([]byte, int, error) {
	size, next, err := consumeUvarint(data, offset)
	if err != nil {
		return nil, 0, err
	}
	if size > uint64(len(data)-next) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	end := next + int(size)
	return bytes.Clone(data[next:end]), end, nil
}

func consumeUvarint(data []byte, offset int) (uint64, int, error) {
	if offset < 0 || offset >= len(data) {
		return 0, 0, io.ErrUnexpectedEOF
	}
	value, read := binary.Uvarint(data[offset:])
	if read <= 0 {
		return 0, 0, errors.New("invalid varint")
	}
	return value, offset + read, nil
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("snapshot entry is not a regular file: %s", path)
	}
	return os.ReadFile(path)
}

func hash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
