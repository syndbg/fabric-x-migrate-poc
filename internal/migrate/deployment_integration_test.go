//go:build integration

package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	cb "github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/common/crypto"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/syndbg/fabric-x-migrate-poc/internal/integrationtest"
)

func TestDeployment(t *testing.T) {
	if os.Getenv("FABRIC_X_MIGRATION_TEST_DEPLOYMENT") != "1" {
		t.Skip("set FABRIC_X_MIGRATION_TEST_DEPLOYMENT=1 after make acceptance-binaries")
	}
	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	for _, name := range []string{"arma", "armageddon", "committer"} {
		require.FileExists(t, filepath.Join(repository, "artifacts", "runtime", "bin", name))
	}
	sources := integrationtest.CaptureSources(t, repository, "goleveldb", "assets", "securities")
	for _, backend := range []struct{ name, dsn, container string }{
		{"PostgreSQL", os.Getenv("FABRIC_X_MIGRATION_TEST_DATABASE_URL"), os.Getenv("FABRIC_X_MIGRATION_TEST_POSTGRES_CONTAINER")},
		{"YugabyteDB", os.Getenv("FABRIC_X_MIGRATION_TEST_YUGABYTE_DATABASE_URL"), os.Getenv("FABRIC_X_MIGRATION_TEST_YUGABYTE_CONTAINER")},
	} {
		require.NotEmpty(t, backend.dsn, "%s database is required for deployment acceptance", backend.name)
		require.NotEmpty(t, backend.container, "%s container is required for native backup tools", backend.name)
		t.Run(backend.name, func(t *testing.T) {
			t.Log("prepare four Arma parties and import both organization databases")
			arma := prepareArmaDeployment(t, repository)
			block, err := protoutil.ReadBlockFromFile(arma.blockPath)
			require.NoError(t, err)
			target, err := decodeChannelConfig(marshal(t, block))
			require.NoError(t, err)
			for _, name := range []string{"SnapshotEndorsement", "CheckpointEndorsement"} {
				target.application().Policies[name] = proto.Clone(target.application().Policies["Endorsement"]).(*cb.ConfigPolicy)
			}
			encoded, err := target.encode()
			require.NoError(t, err)
			block.Data.Data[0] = encoded
			writeDeploymentBlock(t, arma.blockPath, block)

			options := Options{Snapshots: map[string]string{}, SourceConfigs: map[string]string{}, MappingPath: filepath.Join(t.TempDir(), "mapping.json"), TargetConfigPath: arma.blockPath}
			mapping := MappingConfig{TargetNetwork: target.channel}
			expected := map[string][]stateRow{}
			var privateKeys [][]byte
			for _, channel := range []string{"assets", "securities"} {
				options.Snapshots[channel], options.SourceConfigs[channel] = sources[channel].Snapshot, sources[channel].ConfigBlock
				mapping.Mappings = append(mapping.Mappings, Mapping{SourceChannel: channel, SourceNamespace: "singleprivate", TargetNamespace: channel, TargetHashNamespace: channel + "_hashes"})
				expected[channel] = []stateRow{{Key: []byte(channel + "-public-key"), Value: []byte(channel + "-public-value")}}
				key := []byte(channel + "-private-key")
				keyHash, valueHash := sha256.Sum256(key), sha256.Sum256([]byte(channel+"-private-value"))
				expected[channel+"_hashes"] = []stateRow{{Key: keyHash[:], Value: valueHash[:]}}
				privateKeys = append(privateKeys, key)
			}
			require.NoError(t, os.WriteFile(options.MappingPath, marshalJSON(t, mapping), 0o600))
			firstDB, secondDB := testDatabase(t, backend.dsn), testDatabase(t, backend.dsn)
			cli := filepath.Join(repository, "artifacts", "bin", "fabric-x-migrate")
			first := importUsingCLI(t, cli, firstDB, options)
			second := importUsingCLI(t, cli, secondDB, options)
			require.Equal(t, first, second)
			block.Data.Data[0] = first.TargetConfigEnvelope
			writeDeploymentBlock(t, arma.blockPath, block)
			arma.start()

			t.Log("start both committers and verify reads and authorization")
			firstOrg := startDeploymentCommitter(t, repository, firstDB, arma, 1, "")
			secondOrg := startDeploymentCommitter(t, repository, secondDB, arma, 2, "")
			assertDeploymentState(t, firstOrg, expected, privateKeys)
			assertDeploymentState(t, secondOrg, expected, privateKeys)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
			defer cancel()
			broadcaster, err := adapters.NewBroadcastStream(ctx, &ordererdial.Config{FaultToleranceLevel: ordererdial.BFT,
				LatestKnownConfigBlockPath: arma.blockPath, TLS: connection.TLSConfig{Mode: connection.NoneTLSMode}})
			require.NoError(t, err)
			defer connection.CloseConnectionsLog(broadcaster)

			updated := deploymentWrites(expected)
			firstWrite := deploymentTransaction(t, target.channel, sources["assets"].PeerMSPDir, expected, updated)
			submitDeploymentTransaction(t, broadcaster, firstWrite, committerpb.Status_COMMITTED, firstOrg, secondOrg)
			assertDeploymentState(t, firstOrg, updated, privateKeys)
			assertDeploymentState(t, secondOrg, updated, privateKeys)
			for _, ns := range sortedKeys(updated) {
				rows := map[string][]stateRow{ns: updated[ns]}
				denied := deploymentTransaction(t, target.channel, sources["assets"].AdminMSPDir, rows, deploymentWrites(rows))
				submitDeploymentTransaction(t, broadcaster, denied, committerpb.Status_ABORTED_SIGNATURE_INVALID, firstOrg, secondOrg)
			}

			secondOrg.stop(t)
			t.Log("back up the stopped organization after target writes")
			backupLedger := filepath.Join(t.TempDir(), "ledger")
			require.NoError(t, os.CopyFS(backupLedger, os.DirFS(secondOrg.ledger)))
			conn := secondDB.Config().ConnConfig
			dump := deploymentDatabaseCommand(t, backend.container, backend.name == "YugabyteDB", conn.User, conn.Password, conn.Database, "dump")
			var backup, dumpErrors bytes.Buffer
			dump.Stdout, dump.Stderr = &backup, &dumpErrors
			dumpErr := dump.Run()
			require.NoError(t, dumpErr, "%s\n%s", dumpErrors.String(), backup.String())

			whileStopped := deploymentWrites(updated)
			missedWrite := deploymentTransaction(t, target.channel, sources["assets"].PeerMSPDir, updated, whileStopped)
			submitDeploymentTransaction(t, broadcaster, missedWrite, committerpb.Status_COMMITTED, firstOrg)
			assertDeploymentState(t, firstOrg, whileStopped, privateKeys)
			for ns, rows := range updated {
				require.ElementsMatch(t, rows, readRows(t, secondDB, ns), "stopped organization must not see later writes")
			}

			restoredDB := testDatabase(t, backend.dsn)
			t.Log("restore into a fresh database and catch up from Arma")
			restore := deploymentDatabaseCommand(t, backend.container, backend.name == "YugabyteDB", conn.User, conn.Password, restoredDB.Config().ConnConfig.Database, "restore")
			restore.Stdin = bytes.NewReader(backup.Bytes())
			runDeploymentCommand(t, restore)
			for ns, rows := range updated {
				require.ElementsMatch(t, rows, readRows(t, restoredDB, ns), "backup must include post-migration writes")
			}
			assertDeploymentStatus(t, restoredDB, firstWrite.Id, committerpb.Status_COMMITTED)
			recovered := startDeploymentCommitter(t, repository, restoredDB, arma, 2, backupLedger)
			assertDeploymentStatus(t, restoredDB, missedWrite.Id, committerpb.Status_COMMITTED)
			assertDeploymentState(t, recovered, whileStopped, privateKeys)

			afterRestore := deploymentWrites(whileStopped)
			t.Log("verify both organizations continue accepting writes after restore")
			finalWrite := deploymentTransaction(t, target.channel, sources["assets"].PeerMSPDir, whileStopped, afterRestore)
			submitDeploymentTransaction(t, broadcaster, finalWrite, committerpb.Status_COMMITTED, firstOrg, recovered)
			for _, ns := range sortedKeys(afterRestore) {
				rows := map[string][]stateRow{ns: afterRestore[ns]}
				denied := deploymentTransaction(t, target.channel, sources["assets"].AdminMSPDir, rows, deploymentWrites(rows))
				submitDeploymentTransaction(t, broadcaster, denied, committerpb.Status_ABORTED_SIGNATURE_INVALID, firstOrg, recovered)
			}
			assertDeploymentState(t, firstOrg, afterRestore, privateKeys)
			assertDeploymentState(t, recovered, afterRestore, privateKeys)
			for _, ns := range first.Namespaces {
				require.Equal(t, readRows(t, firstDB, ns.Namespace), readRows(t, restoredDB, ns.Namespace))
			}
		})
	}
}

func writeDeploymentBlock(t *testing.T, path string, block *cb.Block) {
	t.Helper()
	var err error
	block.Header.DataHash, err = protoutil.BlockDataHash(block.Data)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, marshal(t, block), 0o600))
}

type deploymentCommitter struct {
	db        *pgxpool.Pool
	query     committerpb.QueryServiceClient
	ledger    string
	processes []*deploymentProcess
}

func (c *deploymentCommitter) stop(t *testing.T) {
	t.Helper()
	for i := len(c.processes) - 1; i >= 0; i-- {
		c.processes[i].stop(t)
	}
}

func startDeploymentCommitter(t *testing.T, repository string, db *pgxpool.Pool, arma armaDeployment, organization int, ledger string) *deploymentCommitter {
	t.Helper()
	if ledger == "" {
		ledger = t.TempDir()
	}
	conn := db.Config().ConnConfig
	insecure := connection.TLSConfig{Mode: connection.NoneTLSMode}
	system := config.SystemConfig{
		ClientTLS: insecure, LedgerPath: ledger,
		Policy: &workload.PolicyProfile{ArtifactsPath: filepath.Dir(arma.blockPath)},
		DB: config.DatabaseConfig{Name: conn.Database, Username: conn.User, Password: conn.Password,
			Endpoints: []*connection.Endpoint{{Host: conn.Host, Port: int(conn.Port)}}, TLS: statedb.TLSConfig{Mode: connection.NoneTLSMode}},
		VCMinTransactionBatchSize: 1, VCTimeoutForMinTransactionBatchSize: 100 * time.Millisecond,
		VerifierBatchTimeCutoff: 10 * time.Millisecond,
	}
	system.Logging.LogSpec = "warn"
	type service struct {
		name, template string
		config         config.ServiceConfig
		release        func()
	}
	var services []service
	for _, entry := range []struct{ name, template string }{
		{"verifier", config.TemplateVerifier}, {"vc", config.TemplateVC}, {"coordinator", config.TemplateCoordinator},
		{"query", config.TemplateQueryService}, {"sidecar", config.TemplateSidecar},
	} {
		address, release := reserveDeploymentAddress(t)
		host, port, err := net.SplitHostPort(address)
		require.NoError(t, err)
		number, err := strconv.Atoi(port)
		require.NoError(t, err)
		cfg := config.ServiceConfig{GrpcEndpoint: &connection.Endpoint{Host: host, Port: number}, HTTPEndpoint: &connection.Endpoint{Host: "127.0.0.1", Port: 0}, GrpcTLS: insecure, HTTPTLS: insecure}
		services = append(services, service{entry.name, entry.template, cfg, release})
		switch entry.name {
		case "verifier":
			system.Services.Verifier = []config.ServiceConfig{cfg}
		case "vc":
			system.Services.VCService = []config.ServiceConfig{cfg}
		case "coordinator":
			system.Services.Coordinator = cfg
		case "query":
			system.Services.Query = cfg
		case "sidecar":
			system.Services.Sidecar = cfg
		}
	}
	c := &deploymentCommitter{db: db, ledger: ledger}
	for _, service := range services {
		system.ThisService = service.config
		path := config.CreateTempConfigFromTemplate(t, service.template, &system)
		if service.name == "sidecar" {
			editDeploymentYAML(t, path, func(value map[string]any) {
				orderer := value["orderer"].(map[string]any)
				orderer["latest-known-config-block-path"] = arma.blockPath
				orderer["identity"] = map[string]any{"msp-id": fmt.Sprintf("org%d", organization), "msp-dir": filepath.Join(arma.root, "crypto", "ordererOrganizations", fmt.Sprintf("org%d", organization), "orderers", fmt.Sprintf("party%d", organization), "consenter", "msp")}
			})
		}
		service.release()
		process := startDeploymentProcess(t, filepath.Join(repository, "artifacts", "runtime", "bin", "committer"), "start", service.name, "--config", path)
		c.processes = append(c.processes, process)
		waitDeploymentAddress(t, service.config.GrpcEndpoint.Address(), process)
	}
	c.query = committerpb.NewQueryServiceClient(test.NewSecuredConnection(t, system.Services.Query.GrpcEndpoint, insecure))
	return c
}

func deploymentWrites(previous map[string][]stateRow) map[string][]stateRow {
	next := map[string][]stateRow{}
	for ns, rows := range previous {
		for _, row := range rows {
			value := sha256.Sum256(append([]byte("updated:"), row.Value...))
			next[ns] = append(next[ns], stateRow{Key: row.Key, Value: value[:], Version: row.Version + 1})
		}
	}
	return next
}

func deploymentTransaction(t *testing.T, channel, mspDir string, previous, next map[string][]stateRow) *servicepb.LoadGenTx {
	t.Helper()
	identity, err := ordererdial.NewIdentitySigner(&ordererdial.IdentityConfig{MspID: "Org1MSP", MSPDir: mspDir})
	require.NoError(t, err)
	endorser, err := testsig.NewNsEndorserFromMsp(testsig.CreatorCertificate, identity)
	require.NoError(t, err)
	nonce := make([]byte, crypto.NonceSize)
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	id := protoutil.ComputeTxID(nonce, nil)
	tx := &applicationpb.Tx{}
	for _, ns := range sortedKeys(previous) {
		writes := &applicationpb.TxNamespace{NsId: ns, NsVersion: 0}
		for i, row := range previous[ns] {
			writes.ReadWrites = append(writes.ReadWrites, &applicationpb.ReadWrite{Key: row.Key, Value: next[ns][i].Value, Version: new(uint64(row.Version))})
		}
		tx.Namespaces = append(tx.Namespaces, writes)
	}
	tx.Endorsements = make([]*applicationpb.Endorsements, len(tx.Namespaces))
	for i := range tx.Namespaces {
		tx.Endorsements[i], err = endorser.EndorseTxNs(id, tx, i)
		require.NoError(t, err)
	}
	builder := &workload.TxBuilder{ChannelID: channel, NonceSource: bytes.NewReader(nonce)}
	return builder.MakeTx(tx)
}

func submitDeploymentTransaction(t *testing.T, broadcaster adapters.Broadcaster, tx *servicepb.LoadGenTx, expected committerpb.Status, organizations ...*deploymentCommitter) {
	t.Helper()
	require.NoError(t, broadcaster.SendBatch(workload.MapToEnvelopeBatch(0, []*servicepb.LoadGenTx{tx})))
	for _, organization := range organizations {
		assertDeploymentStatus(t, organization.db, tx.Id, expected)
	}
}

func assertDeploymentStatus(t *testing.T, db *pgxpool.Pool, id string, expected committerpb.Status) {
	t.Helper()
	var status int32
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		return db.QueryRow(ctx, "SELECT status FROM tx_status WHERE tx_id=$1", []byte(id)).Scan(&status) == nil
	}, 2*time.Minute, 100*time.Millisecond, "transaction %s was not persisted", id)
	require.Equal(t, int32(expected), status)
}

func assertDeploymentState(t *testing.T, c *deploymentCommitter, expected map[string][]stateRow, privateKeys [][]byte) {
	t.Helper()
	query := &committerpb.Query{}
	for _, ns := range sortedKeys(expected) {
		entry := &committerpb.QueryNamespace{NsId: ns, Keys: append([][]byte(nil), privateKeys...)}
		for _, row := range expected[ns] {
			entry.Keys = append(entry.Keys, row.Key)
		}
		query.Namespaces = append(query.Namespaces, entry)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	response, err := c.query.GetRows(ctx, query)
	require.NoError(t, err)
	actual := map[string][]stateRow{}
	for _, ns := range response.Namespaces {
		for _, row := range ns.Rows {
			actual[ns.NsId] = append(actual[ns.NsId], stateRow{Key: row.Key, Value: row.Value, Version: int64(row.Version)})
		}
	}
	require.Len(t, actual, len(expected))
	for ns, rows := range expected {
		require.ElementsMatch(t, rows, actual[ns], "namespace %s", ns)
		require.ElementsMatch(t, rows, readRows(t, c.db, ns))
	}
}
