package genesisdata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrite(t *testing.T) {
	t.Run("when input order changes, it writes the same canonical file", func(t *testing.T) {
		firstPath := filepath.Join(t.TempDir(), "first.fxgenesis")
		secondPath := filepath.Join(t.TempDir(), "second.fxgenesis")
		first, err := Write(firstPath, sampleInput(false))
		require.NoError(t, err)
		second, err := Write(secondPath, sampleInput(true))
		require.NoError(t, err)
		assert.Equal(t, first.MigrationID, second.MigrationID)
		const golden = "a3eb8eddc0409558cf4b6cd0f1328503b45d40505327850f590f0a10fffc2585"
		assert.Equal(t, golden, first.MigrationID, "update golden only after format review")
		require.NotEmpty(t, first.PublicState)
		require.NotEmpty(t, first.TransactionIDs)
		assert.Equal(t, "asset-a", string(first.PublicState[0].Key))
		assert.Equal(t, "tx-a", first.TransactionIDs[0].TransactionId)
	})

	t.Run("when two sources map to one target namespace, it rejects the collision", func(t *testing.T) {
		input := sampleInput(false)
		input.NamespaceMappings = []NamespaceMapping{
			{Source: "assets", Target: "shared"},
			{Source: "payments", Target: "shared"},
		}
		input.PublicState = []*StateRecord{
			{SourceNamespace: "assets", TargetNamespace: "shared", Key: []byte("asset"), Value: []byte("value")},
			{SourceNamespace: "payments", TargetNamespace: "shared", Key: []byte("payment"), Value: []byte("value")},
		}

		_, err := Write(filepath.Join(t.TempDir(), "collision.fxgenesis"), input)
		require.ErrorContains(t, err, `target namespace "shared" has multiple sources`)
	})
}

func TestRead(t *testing.T) {
	t.Run("when the file is corrupted, it rejects the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bundle.fxgenesis")
		_, err := Write(path, sampleInput(false))
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data[len(data)/2] ^= 1
		require.NoError(t, os.WriteFile(path, data, 0o644))
		_, err = Read(path)
		require.Error(t, err)
	})
}

func TestVerifyInput(t *testing.T) {
	t.Run("when source-derived input matches, it verifies the bundle", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bundle.fxgenesis")
		file, err := Write(path, sampleInput(false))
		require.NoError(t, err)
		require.NoError(t, VerifyInput(file, sampleInput(true)))
	})

	t.Run("when source-derived state differs, it rejects the bundle", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bundle.fxgenesis")
		file, err := Write(path, sampleInput(false))
		require.NoError(t, err)
		different := sampleInput(false)
		different.PublicState[0].Value = []byte("different")
		require.Error(t, VerifyInput(file, different))
	})
}

func sampleInput(reverse bool) Input {
	format := uint8(1)
	state := []*StateRecord{
		{SourceNamespace: "basic", TargetNamespace: "migration_basic", Key: []byte("asset-b"), Value: []byte("value-b"), SourceVersion: []byte{1, 3, 0}, SourceBlockNumber: 3},
		{SourceNamespace: "basic", TargetNamespace: "migration_basic", Key: []byte("asset-a"), Value: []byte("value-a"), SourceVersion: []byte{1, 3, 0}, SourceBlockNumber: 3},
	}
	txIDs := []*TransactionIDRecord{{TransactionId: "tx-b"}, {TransactionId: "tx-a"}}
	files := []SourceFile{
		{Name: "txids.data", Size: 12, SHA256: strings.Repeat("2", 64), FormatByte: &format},
		{Name: "public_state.data", Size: 34, SHA256: strings.Repeat("1", 64), FormatByte: &format},
	}
	if reverse {
		state[0], state[1] = state[1], state[0]
		txIDs[0], txIDs[1] = txIDs[1], txIDs[0]
		files[0], files[1] = files[1], files[0]
	}
	return Input{
		ExporterVersion: "fabric-x-migrate-poc-v1",
		Source: Source{
			FabricVersion:     "3.1.5",
			Channel:           "migration",
			LastBlockNumber:   3,
			LastBlockHash:     strings.Repeat("a", 64),
			PreviousBlockHash: strings.Repeat("b", 64),
			StateDBType:       "SimpleKeyValueDB",
			Files:             files,
		},
		NamespaceMappings: []NamespaceMapping{{Source: "basic", Target: "migration_basic"}},
		PublicState:       state,
		TransactionIDs:    txIDs,
		Exclusions: []Exclusion{
			{Kind: "private_data_hashes", Subject: "basic$$h_private", RecordCount: 2, SourceFiles: []string{"private_state_hashes.metadata", "private_state_hashes.data"}, Reason: "not migrated"},
			{Kind: "fabric_system_namespace", Subject: "_lifecycle", RecordCount: 5, SourceFiles: []string{"public_state.metadata", "public_state.data"}, Reason: "not application state"},
		},
	}
}
