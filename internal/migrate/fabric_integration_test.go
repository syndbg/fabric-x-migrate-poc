//go:build integration

package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/syndbg/fabric-x-migrate-poc/internal/integrationtest"
)

func TestFabricSnapshots(t *testing.T) {
	if workerInput := os.Getenv("FABRIC_X_MIGRATION_TEST_WORKER"); workerInput != "" {
		var worker snapshotWorker
		require.NoError(t, json.Unmarshal([]byte(workerInput), &worker))
		repository, err := filepath.Abs("../..")
		require.NoError(t, err)
		testFabricSnapshotBackend(t, repository, worker.DSN, worker.StateDB, worker.Sources)
		return
	}
	if os.Getenv("FABRIC_X_MIGRATION_TEST_FABRIC") != "1" {
		t.Skip("set FABRIC_X_MIGRATION_TEST_FABRIC=1 after make build runtime-binaries")
	}
	databases := []snapshotDatabase{
		{name: "PostgreSQL", dsn: os.Getenv("FABRIC_X_MIGRATION_TEST_DATABASE_URL")},
	}
	require.NotEmpty(t, databases[0].dsn)
	if dsn := os.Getenv("FABRIC_X_MIGRATION_TEST_YUGABYTE_DATABASE_URL"); dsn != "" {
		databases = append(databases, snapshotDatabase{name: "YugabyteDB", dsn: dsn})
	}
	testBinary, err := os.Executable()
	require.NoError(t, err)
	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	for _, stateDB := range []string{"goleveldb", "CouchDB"} {
		t.Run(stateDB, func(t *testing.T) {
			sources := integrationtest.CaptureSources(t, repository, stateDB, "assets", "securities")
			runSnapshotWorkers(t, testBinary, databases, stateDB, sources)
		})
	}
}

type snapshotDatabase struct{ name, dsn string }

type snapshotWorker struct {
	DSN     string
	StateDB string
	Sources map[string]integrationtest.Source
}

func runSnapshotWorkers(t *testing.T, testBinary string, databases []snapshotDatabase, stateDB string, sources map[string]integrationtest.Source) {
	t.Helper()
	results := make(chan error, len(databases))
	var wg sync.WaitGroup
	for _, database := range databases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			input, err := json.Marshal(snapshotWorker{DSN: database.dsn, StateDB: stateDB, Sources: sources})
			if err != nil {
				results <- fmt.Errorf("%s: encode worker input: %w", database.name, err)
				return
			}
			command := exec.Command(testBinary, "-test.run=^TestFabricSnapshots$", "-test.v")
			command.Env = append(os.Environ(), "FABRIC_X_MIGRATION_TEST_WORKER="+string(input))
			command.Stdout, command.Stderr = os.Stdout, os.Stderr
			if err := command.Run(); err != nil {
				results <- fmt.Errorf("%s: %w", database.name, err)
				return
			}
			results <- nil
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
}

func testFabricSnapshotBackend(t *testing.T, repository, dsn, backend string, sources map[string]integrationtest.Source) {
	t.Helper()
	cli := filepath.Join(repository, "artifacts", "bin", "fabric-x-migrate")
	for _, binary := range []string{cli, filepath.Join(repository, "artifacts", "runtime", "bin", "committer"), filepath.Join(repository, "artifacts", "runtime", "bin", "mock")} {
		_, err := os.Stat(binary)
		require.NoError(t, err, "run make build runtime-binaries")
	}
	for _, layout := range []string{"single_channel", "separate_namespaces", "shared_namespace"} {
		for _, hashes := range []string{"public_only", "colocated_hashes", "separate_hashes"} {
			t.Run(layout+"/"+hashes, func(t *testing.T) {
				c, firstDB := migrationRuntime(t, repository, dsn)
				secondDB := testDatabase(t, dsn)
				require.NotEqual(t, firstDB.Config().ConnConfig.Database, secondDB.Config().ConnConfig.Database)
				requireNoTables(t, firstDB)
				requireNoTables(t, secondDB)
				mapping := MappingConfig{TargetNetwork: runner.TestChannelName}
				options := Options{Snapshots: map[string]string{}, SourceConfigs: map[string]string{}, MappingPath: filepath.Join(t.TempDir(), "mapping.json"), TargetConfigPath: c.OrdererEnv.ConfigBlockPath()}
				expected := map[string][]stateRow{}
				var expectedCount uint64
				var privateKeys [][]byte
				channels := []string{"assets"}
				if layout != "single_channel" {
					channels = append(channels, "securities")
				}
				for _, channel := range channels {
					options.Snapshots[channel] = sources[channel].Snapshot
					options.SourceConfigs[channel] = sources[channel].ConfigBlock
					ns := "state"
					if layout == "separate_namespaces" {
						ns = channel + "_state"
					}
					m := Mapping{SourceChannel: channel, SourceNamespace: "singleprivate", TargetNamespace: ns}
					if hashes == "public_only" {
						m.SourceNamespace = "singlepublic"
					}
					hashNS := ns
					if hashes == "separate_hashes" {
						hashNS = ns + "_hashes"
						m.TargetHashNamespace = hashNS
					}
					mapping.Mappings = append(mapping.Mappings, m)
					// The oracle comes from the writes made by the test contract, not
					// the snapshot reader or migration's mapping/record helpers.
					expected[ns] = append(expected[ns], stateRow{Key: []byte(channel + "-public-key"), Value: []byte(channel + "-public-value")})
					expectedCount++
					key := []byte(channel + "-private-key")
					if hashes != "public_only" {
						keyHash := sha256.Sum256(key)
						valueHash := sha256.Sum256([]byte(channel + "-private-value"))
						expected[hashNS] = append(expected[hashNS], stateRow{Key: keyHash[:], Value: valueHash[:]})
						expectedCount++
					}
					privateKeys = append(privateKeys, key)
				}
				require.NoError(t, os.WriteFile(options.MappingPath, marshalJSON(t, mapping), 0o600))
				first := importUsingCLI(t, cli, firstDB, options)
				second := importUsingCLI(t, cli, secondDB, options)
				require.Equal(t, first, second)
				require.Equal(t, expectedCount, first.RecordCount)
				require.Len(t, first.Namespaces, len(expected))
				for _, result := range first.Namespaces {
					require.Contains(t, expected, result.Namespace)
					require.Equal(t, uint64(len(expected[result.Namespace])), result.RecordCount)
				}
				for ns, rows := range expected {
					for _, db := range []*pgxpool.Pool{firstDB, secondDB} {
						// Exact table contents exclude extra rows, private plaintext, and
						// lifecycle state, even if both imports make the same mistake.
						require.ElementsMatch(t, rows, readRows(t, db, ns), "namespace %s", ns)
					}
				}

				if layout == "single_channel" && hashes != "public_only" {
					// The Fabric sample repeats a private key in two collections.
					// Separating hashes from public state cannot resolve that collision.
					bad := mapping
					bad.Mappings = []Mapping{{SourceChannel: "assets", SourceNamespace: "private", TargetNamespace: "state", TargetHashNamespace: mapping.Mappings[0].TargetHashNamespace}}
					badOptions := options
					badOptions.MappingPath = filepath.Join(t.TempDir(), "collision.json")
					require.NoError(t, os.WriteFile(badOptions.MappingPath, marshalJSON(t, bad), 0o600))
					db := testDatabase(t, dsn)
					_, err := Import(t.Context(), db, badOptions)
					require.ErrorContains(t, err, "duplicate key")
					requireNoTables(t, db)
				}

				// Prepare matching startup configuration offline. Keep the generated
				// target orderer settings, adding the source MSPs resolved by the CLI.
				block, err := protoutil.ReadBlockFromFile(c.OrdererEnv.ConfigBlockPath())
				require.NoError(t, err)
				block.Data.Data[0] = first.TargetConfigEnvelope
				block.Header.DataHash, err = protoutil.BlockDataHash(block.Data)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(c.OrdererEnv.ConfigBlockPath(), marshal(t, block), 0o600))
				verifyRunningMigration(t, c, first, expected, privateKeys, sources["assets"].PeerMSPDir, sources["assets"].AdminMSPDir)
			})
		}
	}
}

func importUsingCLI(t *testing.T, binary string, db *pgxpool.Pool, options Options) *Result {
	t.Helper()
	// pgx ConnString retains the original URL, even when the test changes Database.
	dsn, err := url.Parse(db.Config().ConnString())
	require.NoError(t, err)
	dsn.Path = "/" + db.Config().ConnConfig.Database
	args := []string{"import", "--mapping", options.MappingPath, "--target-config", options.TargetConfigPath, "--database-url", dsn.String()}
	for _, channel := range sortedKeys(options.Snapshots) {
		args = append(args, "--snapshot", channel+"="+options.Snapshots[channel], "--source-config", channel+"="+options.SourceConfigs[channel])
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	require.NoError(t, err, "%s", stderr.String())
	result := new(Result)
	require.NoError(t, json.Unmarshal(output, result))
	return result
}
