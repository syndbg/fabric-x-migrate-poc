package fabricsnapshot

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenStreamWalksOneRecordAndDetectsChangedInput(t *testing.T) {
	directory := validSnapshot(t, "SimpleKeyValueDB")
	snapshot, err := OpenStream(directory)
	require.NoError(t, err)
	require.Equal(t, []Namespace{{Name: "basic", Records: 1}}, snapshot.PublicNamespaces)

	var records []Record
	require.NoError(t, snapshot.WalkPublicRecords(func(record Record) error {
		records = append(records, record)
		return nil
	}))
	require.Len(t, records, 1)
	require.Equal(t, "basic", records[0].Namespace)
	require.Equal(t, []byte("asset"), records[0].Key)

	dataPath := filepath.Join(directory, "public_state.data")
	data, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, append(data, 1), 0o644))
	require.ErrorContains(t, snapshot.WalkPublicRecords(nil), "trailing bytes")
}

func TestHashStreams(t *testing.T) {
	for _, backend := range []string{"SimpleKeyValueDB", "CouchDB"} {
		t.Run(backend, func(t *testing.T) {
			directory := validSnapshot(t, backend)
			keys := [][]byte{bytes.Repeat([]byte{0}, 32), bytes.Repeat([]byte{0xff}, 32)}
			if backend == "CouchDB" {
				keys[0], keys[1] = keys[1], keys[0]
			}
			data := []byte{1}
			for _, key := range keys {
				record := appendField(nil, 1, key)
				record = appendField(record, 2, bytes.Repeat([]byte{42}, 32))
				record = appendField(record, 4, []byte{1, 3, 0})
				data = appendSized(data, record)
			}
			addSnapshotFiles(t, directory, map[string][]byte{
				"private_state_hashes.data":     data,
				"private_state_hashes.metadata": binary.AppendUvarint(appendSized([]byte{1, 1}, []byte("basic$$howners")), 2),
			})
			snapshot, err := OpenStream(directory)
			require.NoError(t, err)
			var got [][]byte
			require.NoError(t, snapshot.WalkPrivateHashRecords(func(record Record) error {
				got = append(got, record.Key)
				require.Equal(t, bytes.Repeat([]byte{42}, 32), record.Value)
				require.Empty(t, record.Version)
				require.Zero(t, record.BlockNumber)
				return nil
			}))
			require.Equal(t, keys, got)
			require.NoError(t, snapshot.Revalidate())
			write(t, directory, "private_state_hashes.data", append(data, 0))
			require.ErrorContains(t, snapshot.WalkPrivateHashRecords(nil), "trailing bytes")
			require.ErrorContains(t, snapshot.Revalidate(), "hash mismatch")
		})
	}
}

func TestStreamRejectsMalformedOrChangedInputs(t *testing.T) {
	t.Run("incomplete hash pair", func(t *testing.T) {
		dir := validSnapshot(t, "SimpleKeyValueDB")
		addSnapshotFiles(t, dir, map[string][]byte{"private_state_hashes.data": {1}})
		_, err := OpenStream(dir)
		require.ErrorContains(t, err, "incomplete private_state_hashes")
	})
	t.Run("changed metadata and excluded data", func(t *testing.T) {
		for _, name := range []string{"_snapshot_signable_metadata.json", "_snapshot_additional_metadata.json", "txids.data"} {
			dir := validSnapshot(t, "SimpleKeyValueDB")
			s, err := OpenStream(dir)
			require.NoError(t, err)
			write(t, dir, name, []byte("changed"))
			require.ErrorContains(t, s.Revalidate(), "hash mismatch")
		}
	})
	t.Run("untrusted length", func(t *testing.T) {
		_, err := readSized(bufio.NewReader(bytes.NewReader(binary.AppendUvarint(nil, 1<<60))))
		require.Error(t, err)
		_, err = decodeNamespaces("test", binary.AppendUvarint([]byte{1}, 1<<60))
		require.ErrorContains(t, err, "namespace count")
	})
	t.Run("duplicate public key", func(t *testing.T) {
		dir := validSnapshot(t, "SimpleKeyValueDB")
		data, err := os.ReadFile(filepath.Join(dir, "public_state.data"))
		require.NoError(t, err)
		addSnapshotFiles(t, dir, map[string][]byte{
			"public_state.data":     append(data, data[1:]...),
			"public_state.metadata": binary.AppendUvarint(appendSized([]byte{1, 1}, []byte("basic")), 2),
		})
		s, err := OpenStream(dir)
		require.NoError(t, err)
		require.ErrorContains(t, s.WalkPublicRecords(nil), "not strictly ordered")
	})
}

func addSnapshotFiles(t *testing.T, directory string, files map[string][]byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "_snapshot_signable_metadata.json"))
	require.NoError(t, err)
	var metadata Metadata
	require.NoError(t, json.Unmarshal(data, &metadata))
	for name, contents := range files {
		write(t, directory, name, contents)
		metadata.SnapshotFilesRawHashes[name] = digest(contents)
	}
	data, err = json.Marshal(metadata)
	require.NoError(t, err)
	write(t, directory, "_snapshot_signable_metadata.json", data)
	rewriteSnapshotHashes(t, directory)
}
