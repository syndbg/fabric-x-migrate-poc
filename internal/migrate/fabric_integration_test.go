//go:build integration

package migrate

import (
	"os"
	"path/filepath"
	"testing"

	cb "github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/syndbg/fabric-x-migrate-poc/internal/integrationtest"
)

func TestFabricSnapshots(t *testing.T) {
	if os.Getenv("FABRIC_X_MIGRATION_TEST_FABRIC") != "1" {
		t.Skip("set FABRIC_X_MIGRATION_TEST_FABRIC=1 to capture live peer snapshots")
	}
	dsn := os.Getenv("FABRIC_X_MIGRATION_TEST_DATABASE_URL")
	require.NotEmpty(t, dsn)
	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	for _, backend := range []string{"goleveldb", "CouchDB"} {
		t.Run(backend, func(t *testing.T) {
			sources := integrationtest.CaptureSources(t, repository, backend, "assets", "securities")
			options := Options{Snapshots: map[string]string{}, SourceConfigs: map[string]string{}, MappingPath: filepath.Join(t.TempDir(), "mapping.json"), TargetConfigPath: filepath.Join(t.TempDir(), "network1.config")}
			mapping := MappingConfig{TargetNetwork: "network1"}
			for _, channel := range []string{"assets", "securities"} {
				options.Snapshots[channel] = sources[channel].Snapshot
				options.SourceConfigs[channel] = sources[channel].ConfigBlock
				mapping.Mappings = append(mapping.Mappings, Mapping{SourceChannel: channel, SourceNamespace: "basic", TargetNamespace: channel + "_basic"}, Mapping{SourceChannel: channel, SourceNamespace: "singleprivate", TargetNamespace: channel + "_private"})
			}
			require.NoError(t, os.WriteFile(options.MappingPath, marshalJSON(t, mapping), 0o600))
			data, err := os.ReadFile(sources["assets"].ConfigBlock)
			require.NoError(t, err)
			target, err := decodeChannelConfig(data)
			require.NoError(t, err)
			header := new(cb.ChannelHeader)
			require.NoError(t, proto.Unmarshal(target.payload.Header.ChannelHeader, header))
			header.ChannelId = "network1"
			target.channel = "network1"
			target.payload.Header.ChannelHeader = marshal(t, header)
			for _, name := range []string{"SnapshotEndorsement", "CheckpointEndorsement"} {
				target.application().Policies[name] = proto.Clone(target.application().Policies["LifecycleEndorsement"]).(*cb.ConfigPolicy)
			}
			encoded, err := target.encode()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(options.TargetConfigPath, encoded, 0o600))
			firstDB, secondDB := testDatabase(t, dsn), testDatabase(t, dsn)
			first, err := Import(t.Context(), firstDB, options)
			require.NoError(t, err)
			second, err := Import(t.Context(), secondDB, options)
			require.NoError(t, err)
			require.Equal(t, first, second)
			require.Equal(t, uint64(16), first.RecordCount)
			for _, namespace := range first.Namespaces {
				require.Equal(t, readRows(t, firstDB, namespace.Namespace), readRows(t, secondDB, namespace.Namespace))
			}
			// The unmodified Fabric sample stores the same private key in two
			// collections. Its identical key hashes must collide, not be overwritten.
			mapping.Mappings = []Mapping{{SourceChannel: "assets", SourceNamespace: "private", TargetNamespace: "private_hashes"}}
			require.NoError(t, os.WriteFile(options.MappingPath, marshalJSON(t, mapping), 0o600))
			collisionDB := testDatabase(t, dsn)
			_, err = Import(t.Context(), collisionDB, options)
			require.ErrorContains(t, err, "duplicate key")
			requireNoTables(t, collisionDB)
		})
	}
}
