//go:build integration

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/syndbg/fabric-x-migrate-poc/internal/fabricsnapshot"
	"github.com/syndbg/fabric-x-migrate-poc/internal/integrationtest"
	"github.com/syndbg/fabric-x-migrate-poc/pkg/genesisdata"
)

func TestExport(t *testing.T) {
	_, sourceFile, _, _ := runtime.Caller(0)
	workspace := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))

	for _, version := range []string{"2.5.16", "3.1.5"} {
		t.Run("when source is Fabric "+version+", it exports deterministically", func(t *testing.T) {
			snapshotPath := filepath.Join(workspace, "fabric-x-migration-lab", "artifacts", "snapshots", "fabric-"+version, "block-3")
			snapshot, err := fabricsnapshot.Read(snapshotPath)
			require.NoError(t, err)
			input, err := exportInput(snapshot, version, []genesisdata.NamespaceMapping{{Source: "basic", Target: "migration_basic"}})
			require.NoError(t, err)

			firstPath := filepath.Join(t.TempDir(), "first.fxgenesis")
			secondPath := filepath.Join(t.TempDir(), "second.fxgenesis")
			first, err := genesisdata.Write(firstPath, input)
			require.NoError(t, err)
			second, err := genesisdata.Write(secondPath, input)
			require.NoError(t, err)
			firstBytes, err := os.ReadFile(firstPath)
			require.NoError(t, err)
			secondBytes, err := os.ReadFile(secondPath)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(firstBytes, secondBytes), "repeated export bytes differ")
			assert.Equal(t, first.MigrationID, second.MigrationID)
			assert.Len(t, first.PublicState, 6)
			assert.Len(t, first.TransactionIDs, 4)
			require.Len(t, first.Manifest.NamespaceMappings, 1)
			assert.Equal(t, "basic", first.Manifest.NamespaceMappings[0].Source)
			assert.Equal(t, "migration_basic", first.Manifest.NamespaceMappings[0].Target)
			assert.NotEmpty(t, first.Manifest.Exclusions)

			var output bytes.Buffer
			require.NoError(t, verifySource([]string{"--snapshot", snapshotPath, "--input", firstPath}, &output, &output))
			assert.Contains(t, output.String(), "source_integrity: verified")
		})
	}

	t.Run("when the target namespace is invalid, it rejects the export", func(t *testing.T) {
		snapshot, err := fabricsnapshot.Read(filepath.Join(workspace, "fabric-x-migration-lab", "artifacts", "snapshots", "fabric-2.5.16", "block-3"))
		require.NoError(t, err)
		input, err := exportInput(snapshot, "2.5.16", []genesisdata.NamespaceMapping{{Source: "basic", Target: "migration.basic"}})
		require.NoError(t, err)
		_, err = genesisdata.Write(filepath.Join(t.TempDir(), "invalid.fxgenesis"), input)
		require.Error(t, err)
	})

	t.Run("when Fabric has two channels, it exports one snapshot per channel", func(t *testing.T) {
		snapshots := integrationtest.Capture(t, filepath.Join(workspace, "fabric-x-migrate-poc"), "goleveldb", "payments", "securities")
		migrationIDs := make([]string, 0, len(snapshots))
		for _, channel := range []string{"payments", "securities"} {
			snapshot, err := fabricsnapshot.Read(snapshots[channel])
			require.NoError(t, err)
			assert.Equal(t, channel, snapshot.Metadata.ChannelName)
			assert.Equal(t, "SimpleKeyValueDB", snapshot.Metadata.StateDBType)
			input, err := exportInput(snapshot, integrationtest.Version, []genesisdata.NamespaceMapping{{Source: "basic", Target: channel + "_basic"}})
			require.NoError(t, err)
			bundle, err := genesisdata.Write(filepath.Join(t.TempDir(), channel+".fxgenesis"), input)
			require.NoError(t, err)
			migrationIDs = append(migrationIDs, bundle.MigrationID)
		}
		require.Len(t, migrationIDs, 2)
		assert.NotEqual(t, migrationIDs[0], migrationIDs[1])
	})

	t.Run("when Fabric uses CouchDB, it exports the official snapshot format", func(t *testing.T) {
		snapshotPath := integrationtest.Capture(t, filepath.Join(workspace, "fabric-x-migrate-poc"), "CouchDB", "couchdb")["couchdb"]
		snapshot, err := fabricsnapshot.Read(snapshotPath)
		require.NoError(t, err)
		assert.Equal(t, "CouchDB", snapshot.Metadata.StateDBType)
		assert.NotEmpty(t, snapshot.PrivateHashNamespaces)
		assert.Positive(t, snapshot.ConfigHistoryRecords)
		input, err := exportInput(snapshot, integrationtest.Version, []genesisdata.NamespaceMapping{{Source: "basic", Target: "couchdb_basic"}})
		require.NoError(t, err)
		excluded := make(map[string]uint64, len(input.Exclusions))
		for _, exclusion := range input.Exclusions {
			excluded[exclusion.Kind] += exclusion.RecordCount
		}
		assert.Positive(t, excluded["private_data_hashes"])
		assert.Equal(t, snapshot.ConfigHistoryRecords, excluded["collection_config_history"])
		bundle, err := genesisdata.Write(filepath.Join(t.TempDir(), "couchdb.fxgenesis"), input)
		require.NoError(t, err)
		assert.NotEmpty(t, bundle.PublicState)
		assert.Equal(t, input.Exclusions, bundle.Manifest.Exclusions)
	})
}

func TestVerifySource(t *testing.T) {
	t.Run("when the original snapshot differs, it rejects the bundle", func(t *testing.T) {
		_, sourceFile, _, _ := runtime.Caller(0)
		workspace := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
		firstSnapshotPath := filepath.Join(workspace, "fabric-x-migration-lab", "artifacts", "snapshots", "fabric-2.5.16", "block-3")
		firstSnapshot, err := fabricsnapshot.Read(firstSnapshotPath)
		require.NoError(t, err)
		input, err := exportInput(firstSnapshot, "2.5.16", []genesisdata.NamespaceMapping{{Source: "basic", Target: "migration_basic"}})
		require.NoError(t, err)
		bundlePath := filepath.Join(t.TempDir(), "fabric-2.5.16.fxgenesis")
		_, err = genesisdata.Write(bundlePath, input)
		require.NoError(t, err)

		secondSnapshotPath := filepath.Join(workspace, "fabric-x-migration-lab", "artifacts", "snapshots", "fabric-3.1.5", "block-3")
		err = verifySource([]string{"--snapshot", secondSnapshotPath, "--input", bundlePath}, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
	})
}
