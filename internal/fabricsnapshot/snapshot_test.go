package fabricsnapshot

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func validSnapshot(t *testing.T, stateDBType string) string {
	t.Helper()
	directory := t.TempDir()
	record := appendField(nil, 1, []byte("asset"))
	record = appendField(record, 2, []byte("owner=alice"))
	record = appendField(record, 4, []byte{1, 3, 0})
	files := map[string][]byte{
		"public_state.data":      appendSized([]byte{1}, record),
		"public_state.metadata":  binary.AppendUvarint(appendSized([]byte{1, 1}, []byte("basic")), 1),
		"confighistory.data":     appendSized(appendSized([]byte{1}, []byte("collection-key")), []byte("collection-value")),
		"confighistory.metadata": {1, 1},
		"txids.data":             appendSized([]byte{1}, []byte("tx-1")),
		"txids.metadata":         {1, 1},
	}
	hashes := map[string]string{}
	for name, data := range files {
		hashes[name] = digest(data)
		write(t, directory, name, data)
	}
	metadata := Metadata{
		ChannelName: "migration", LastBlockNumber: 3, LastBlockHash: digest([]byte("block")),
		PreviousBlockHash: digest([]byte("previous")), SnapshotFilesRawHashes: hashes, StateDBType: stateDBType,
	}
	signable, err := json.Marshal(metadata)
	require.NoError(t, err)
	write(t, directory, "_snapshot_signable_metadata.json", signable)
	additional, err := json.Marshal(AdditionalMetadata{SnapshotHash: digest(signable), LastBlockCommitHash: digest([]byte("commit"))})
	require.NoError(t, err)
	write(t, directory, "_snapshot_additional_metadata.json", additional)
	return directory
}

func rewriteSnapshotHashes(t *testing.T, directory string) {
	t.Helper()
	metadataBytes, err := os.ReadFile(filepath.Join(directory, "_snapshot_signable_metadata.json"))
	require.NoError(t, err)
	var metadata Metadata
	require.NoError(t, json.Unmarshal(metadataBytes, &metadata))
	for name := range metadata.SnapshotFilesRawHashes {
		data, readErr := os.ReadFile(filepath.Join(directory, name))
		require.NoError(t, readErr)
		metadata.SnapshotFilesRawHashes[name] = digest(data)
	}
	metadataBytes, err = json.Marshal(metadata)
	require.NoError(t, err)
	write(t, directory, "_snapshot_signable_metadata.json", metadataBytes)
	additional, err := json.Marshal(AdditionalMetadata{SnapshotHash: digest(metadataBytes), LastBlockCommitHash: digest([]byte("commit"))})
	require.NoError(t, err)
	write(t, directory, "_snapshot_additional_metadata.json", additional)
}

func appendField(output []byte, number byte, value []byte) []byte {
	output = append(output, number<<3|2)
	return appendSized(output, value)
}

func appendSized(output, value []byte) []byte {
	output = binary.AppendUvarint(output, uint64(len(value)))
	return append(output, value...)
}

func digest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func write(t *testing.T, directory, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0o644))
}
