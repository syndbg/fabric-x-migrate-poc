package genesisdata

import (
	"archive/tar"
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
	"regexp"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"
)

const (
	FormatName        = "fabric-x-genesis-data"
	FormatVersion     = 1
	RecordFormat      = byte(1)
	ManifestFile      = "manifest.json"
	PublicStateFile   = "public_state.data"
	TransactionIDFile = "transaction_ids.data"
)

var archiveTime = time.Unix(0, 0).UTC()

var targetNamespacePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

type File struct {
	Manifest       Manifest
	PublicState    []*StateRecord
	TransactionIDs []*TransactionIDRecord
	MigrationID    string
}

// Write creates one deterministic genesis-data file. It refuses to overwrite
// an existing path so an approved migration artifact cannot be replaced.
func Write(path string, input Input) (*File, error) {
	if path == "" {
		return nil, errors.New("output path is empty")
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("output already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	archive, _, err := build(input)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".fabric-x-genesis-")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(archive); err != nil {
		_ = temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(temporaryPath, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, err
	}
	return Read(path)
}

// VerifyInput checks that input reproduces file byte for byte.
func VerifyInput(file *File, input Input) error {
	if file == nil {
		return errors.New("genesis-data file is nil")
	}
	_, expected, err := build(input)
	if err != nil {
		return err
	}
	if expected.MigrationID != file.MigrationID {
		return errors.New("source snapshot does not reproduce the genesis-data file")
	}
	return nil
}

func build(input Input) ([]byte, *File, error) {
	normalize(&input)
	if err := validateInput(input); err != nil {
		return nil, nil, err
	}

	publicData, err := encodeMessages(len(input.PublicState), func(i int) proto.Message {
		return input.PublicState[i]
	})
	if err != nil {
		return nil, nil, err
	}
	txIDData, err := encodeMessages(len(input.TransactionIDs), func(i int) proto.Message {
		return input.TransactionIDs[i]
	})
	if err != nil {
		return nil, nil, err
	}
	manifest := Manifest{
		Format:            FormatName,
		FormatVersion:     FormatVersion,
		ExporterVersion:   input.ExporterVersion,
		HashAlgorithm:     "SHA-256",
		Source:            input.Source,
		NamespaceMappings: input.NamespaceMappings,
		Parts: []Part{
			{Name: PublicStateFile, Message: "fabricx.migration.v1.StateRecord", FormatVersion: RecordFormat, RecordCount: uint64(len(input.PublicState)), SHA256: hash(publicData)},
			{Name: TransactionIDFile, Message: "fabricx.migration.v1.TransactionIDRecord", FormatVersion: RecordFormat, RecordCount: uint64(len(input.TransactionIDs)), SHA256: hash(txIDData)},
		},
		Exclusions: input.Exclusions,
	}
	manifestData, err := marshalManifest(manifest)
	if err != nil {
		return nil, nil, err
	}
	// ponytail: the PoC materializes one file; switch to staged streaming after
	// representative snapshots establish the required size and batch limits.
	archive, err := encodeArchive(manifestData, publicData, txIDData)
	if err != nil {
		return nil, nil, err
	}
	return archive, &File{
		Manifest: manifest, PublicState: input.PublicState, TransactionIDs: input.TransactionIDs,
		MigrationID: hash(archive),
	}, nil
}

// Read parses a canonical version-1 genesis-data file.
func Read(path string) (*File, error) {
	archive, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	members, err := decodeArchive(archive)
	if err != nil {
		return nil, err
	}
	manifest, err := decodeManifest(members[ManifestFile])
	if err != nil {
		return nil, err
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	if hash(members[PublicStateFile]) != manifest.Parts[0].SHA256 ||
		hash(members[TransactionIDFile]) != manifest.Parts[1].SHA256 {
		return nil, errors.New("genesis-data member hash mismatch")
	}

	stateFrames, err := decodeFrames(PublicStateFile, members[PublicStateFile])
	if err != nil {
		return nil, err
	}
	state := make([]*StateRecord, 0, len(stateFrames))
	for _, frame := range stateFrames {
		record := new(StateRecord)
		if err := unmarshalCanonical(frame, record); err != nil {
			return nil, fmt.Errorf("%s: %w", PublicStateFile, err)
		}
		state = append(state, record)
	}
	txIDFrames, err := decodeFrames(TransactionIDFile, members[TransactionIDFile])
	if err != nil {
		return nil, err
	}
	txIDs := make([]*TransactionIDRecord, 0, len(txIDFrames))
	for _, frame := range txIDFrames {
		record := new(TransactionIDRecord)
		if err := unmarshalCanonical(frame, record); err != nil {
			return nil, fmt.Errorf("%s: %w", TransactionIDFile, err)
		}
		txIDs = append(txIDs, record)
	}
	if uint64(len(state)) != manifest.Parts[0].RecordCount || uint64(len(txIDs)) != manifest.Parts[1].RecordCount {
		return nil, errors.New("genesis-data record count mismatch")
	}
	if err := validateRecords(manifest.NamespaceMappings, state, txIDs); err != nil {
		return nil, err
	}
	canonical, err := encodeArchive(members[ManifestFile], members[PublicStateFile], members[TransactionIDFile])
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(archive, canonical) {
		return nil, errors.New("genesis-data file is not canonically encoded")
	}
	return &File{
		Manifest:       manifest,
		PublicState:    state,
		TransactionIDs: txIDs,
		MigrationID:    hash(archive),
	}, nil
}

func normalize(input *Input) {
	sort.Slice(input.Source.Files, func(i, j int) bool { return input.Source.Files[i].Name < input.Source.Files[j].Name })
	sort.Slice(input.NamespaceMappings, func(i, j int) bool {
		if input.NamespaceMappings[i].Source != input.NamespaceMappings[j].Source {
			return input.NamespaceMappings[i].Source < input.NamespaceMappings[j].Source
		}
		return input.NamespaceMappings[i].Target < input.NamespaceMappings[j].Target
	})
	sort.Slice(input.PublicState, func(i, j int) bool { return compareState(input.PublicState[i], input.PublicState[j]) < 0 })
	sort.Slice(input.TransactionIDs, func(i, j int) bool {
		return input.TransactionIDs[i].TransactionId < input.TransactionIDs[j].TransactionId
	})
	sort.Slice(input.Exclusions, func(i, j int) bool {
		if input.Exclusions[i].Kind != input.Exclusions[j].Kind {
			return input.Exclusions[i].Kind < input.Exclusions[j].Kind
		}
		return input.Exclusions[i].Subject < input.Exclusions[j].Subject
	})
	for i := range input.Exclusions {
		sort.Strings(input.Exclusions[i].SourceFiles)
	}
}

func validateInput(input Input) error {
	if input.ExporterVersion == "" || len(input.PublicState) == 0 {
		return errors.New("exporter version and public state are required")
	}
	return validateRecords(input.NamespaceMappings, input.PublicState, input.TransactionIDs)
}

func validateManifest(manifest Manifest) error {
	if manifest.Format != FormatName || manifest.FormatVersion != FormatVersion ||
		manifest.ExporterVersion == "" || manifest.HashAlgorithm != "SHA-256" {
		return errors.New("unsupported or incomplete genesis-data manifest")
	}
	if manifest.Source.FabricVersion == "" || manifest.Source.Channel == "" || manifest.Source.StateDBType == "" {
		return errors.New("incomplete source checkpoint")
	}
	if len(manifest.Parts) != 2 ||
		manifest.Parts[0].Name != PublicStateFile || manifest.Parts[0].Message != "fabricx.migration.v1.StateRecord" ||
		manifest.Parts[1].Name != TransactionIDFile || manifest.Parts[1].Message != "fabricx.migration.v1.TransactionIDRecord" {
		return errors.New("invalid genesis-data member declaration")
	}
	for _, part := range manifest.Parts {
		if part.FormatVersion != RecordFormat || !validSHA256(part.SHA256) {
			return fmt.Errorf("invalid part metadata for %s", part.Name)
		}
	}
	for i, file := range manifest.Source.Files {
		if file.Name == "" || file.Size < 0 || !validSHA256(file.SHA256) || i > 0 && manifest.Source.Files[i-1].Name >= file.Name {
			return fmt.Errorf("invalid or unordered source file %d", i)
		}
	}
	for i, exclusion := range manifest.Exclusions {
		if exclusion.Kind == "" || exclusion.Subject == "" || exclusion.Reason == "" {
			return fmt.Errorf("invalid exclusion %d", i)
		}
		if i > 0 {
			previous := manifest.Exclusions[i-1]
			if previous.Kind > exclusion.Kind || previous.Kind == exclusion.Kind && previous.Subject >= exclusion.Subject {
				return errors.New("exclusions are not strictly ordered")
			}
		}
		for j, name := range exclusion.SourceFiles {
			if name == "" || j > 0 && exclusion.SourceFiles[j-1] >= name {
				return fmt.Errorf("exclusion %d source files are not strictly ordered", i)
			}
		}
	}
	return nil
}

func validateRecords(mappings []NamespaceMapping, state []*StateRecord, txIDs []*TransactionIDRecord) error {
	if len(mappings) == 0 {
		return errors.New("namespace mapping is empty")
	}
	allowed := make(map[string]string, len(mappings))
	targets := make(map[string]string, len(mappings))
	for i, mapping := range mappings {
		if mapping.Source == "" || mapping.Target == "" || i > 0 && mappings[i-1].Source >= mapping.Source {
			return errors.New("namespace mappings are invalid or unordered")
		}
		if len(mapping.Target) > 60 || !targetNamespacePattern.MatchString(mapping.Target) || mapping.Target == "_meta" || mapping.Target == "_config" {
			return fmt.Errorf("invalid Fabric-X target namespace %q", mapping.Target)
		}
		if previous, exists := targets[mapping.Target]; exists && previous != mapping.Source {
			return fmt.Errorf("target namespace %q has multiple sources", mapping.Target)
		}
		allowed[mapping.Source] = mapping.Target
		targets[mapping.Target] = mapping.Source
	}
	for i, record := range state {
		if record == nil || len(record.Key) == 0 || allowed[record.SourceNamespace] != record.TargetNamespace {
			return fmt.Errorf("invalid public-state record %d", i)
		}
		if i > 0 && compareState(state[i-1], record) >= 0 {
			return errors.New("public-state records are not strictly ordered")
		}
		if i > 0 && state[i-1].TargetNamespace == record.TargetNamespace && bytes.Equal(state[i-1].Key, record.Key) {
			return fmt.Errorf("duplicate mapped key in namespace %q", record.TargetNamespace)
		}
	}
	for i, record := range txIDs {
		if record == nil || record.TransactionId == "" || i > 0 && txIDs[i-1].TransactionId >= record.TransactionId {
			return errors.New("transaction IDs are invalid or unordered")
		}
	}
	return nil
}

func compareState(left, right *StateRecord) int {
	if result := bytes.Compare([]byte(left.TargetNamespace), []byte(right.TargetNamespace)); result != 0 {
		return result
	}
	if result := bytes.Compare(left.Key, right.Key); result != 0 {
		return result
	}
	return bytes.Compare([]byte(left.SourceNamespace), []byte(right.SourceNamespace))
}

func encodeMessages(count int, message func(int) proto.Message) ([]byte, error) {
	data := []byte{RecordFormat}
	marshal := proto.MarshalOptions{Deterministic: true}
	for i := 0; i < count; i++ {
		encoded, err := marshal.Marshal(message(i))
		if err != nil {
			return nil, err
		}
		data = binary.AppendUvarint(data, uint64(len(encoded)))
		data = append(data, encoded...)
	}
	return data, nil
}

func decodeFrames(name string, data []byte) ([][]byte, error) {
	if len(data) == 0 || data[0] != RecordFormat {
		return nil, fmt.Errorf("%s: unsupported record format", name)
	}
	var frames [][]byte
	for offset := 1; offset < len(data); {
		length, n := binary.Uvarint(data[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("%s: invalid record length", name)
		}
		if !bytes.Equal(data[offset:offset+n], binary.AppendUvarint(nil, length)) {
			return nil, fmt.Errorf("%s: non-canonical record length", name)
		}
		offset += n
		if length > uint64(len(data)-offset) {
			return nil, fmt.Errorf("%s: truncated record", name)
		}
		end := offset + int(length)
		frames = append(frames, data[offset:end])
		offset = end
	}
	return frames, nil
}

func unmarshalCanonical(data []byte, message proto.Message) error {
	if err := proto.Unmarshal(data, message); err != nil {
		return err
	}
	canonical, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, canonical) {
		return errors.New("protobuf record is not canonically encoded")
	}
	return nil
}

func encodeArchive(manifest, state, txIDs []byte) ([]byte, error) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, member := range []struct {
		name string
		data []byte
	}{{ManifestFile, manifest}, {PublicStateFile, state}, {TransactionIDFile, txIDs}} {
		header := &tar.Header{
			Name:     member.name,
			Mode:     0o644,
			Size:     int64(len(member.data)),
			ModTime:  archiveTime,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(member.data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func decodeArchive(data []byte) (map[string][]byte, error) {
	reader := tar.NewReader(bytes.NewReader(data))
	expected := []string{ManifestFile, PublicStateFile, TransactionIDFile}
	members := make(map[string][]byte, len(expected))
	for _, name := range expected {
		header, err := reader.Next()
		if err != nil {
			return nil, fmt.Errorf("read archive member %s: %w", name, err)
		}
		if header.Name != name || header.Typeflag != tar.TypeReg || header.Mode != 0o644 ||
			header.Uid != 0 || header.Gid != 0 || !header.ModTime.Equal(archiveTime) {
			return nil, fmt.Errorf("non-canonical archive header for %s", name)
		}
		member, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		members[name] = member
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("unexpected archive member")
		}
		return nil, err
	}
	return members, nil
}

func decodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return manifest, errors.New("manifest contains multiple JSON values")
	}
	canonical, err := marshalManifest(manifest)
	if err != nil {
		return manifest, err
	}
	if !bytes.Equal(data, canonical) {
		return manifest, errors.New("manifest is not canonically encoded")
	}
	return manifest, nil
}

func marshalManifest(manifest Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func hash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == hex.EncodeToString(decoded)
}
