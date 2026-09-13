//go:build integration

package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	cb "github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	fxpolicy "github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/common/crypto"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5/pgxpool"
)

func migrationRuntime(t *testing.T, repository, dsn string) (*runner.CommitterRuntime, *pgxpool.Pool) {
	t.Helper()
	parsed, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	// The upstream runner expects bin/ relative to its test repository root.
	workingRoot := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(repository, "artifacts", "runtime", "bin"), filepath.Join(workingRoot, "bin")))
	workingDir := filepath.Join(workingRoot, "internal", "migrate")
	require.NoError(t, os.MkdirAll(workingDir, 0o700))
	t.Chdir(workingDir)
	c := runner.NewRuntime(t, &runner.Config{
		DBConnection: &testdb.Connection{
			Endpoints: []*connection.Endpoint{{Host: parsed.ConnConfig.Host, Port: int(parsed.ConnConfig.Port)}},
			User:      parsed.ConnConfig.User, Password: parsed.ConnConfig.Password, Database: parsed.ConnConfig.Database,
			TLS: statedb.TLSConfig{Mode: connection.NoneTLSMode},
		},
		BlockSize: 1, BlockTimeout: 100 * time.Millisecond,
		VCMinTransactionBatchSize: 1, VCTimeoutForMinTransactionBatchSize: 100 * time.Millisecond,
		VerifierBatchTimeCutoff: 10 * time.Millisecond,
	})
	// The upstream sample defaults to MSP 1.0. Source peer/admin roles require
	// modern channel capabilities in the administrator-provided target config.
	block, err := protoutil.ReadBlockFromFile(c.OrdererEnv.ConfigBlockPath())
	require.NoError(t, err)
	target, err := decodeChannelConfig(marshal(t, block))
	require.NoError(t, err)
	for _, group := range []*cb.ConfigGroup{target.config.Config.ChannelGroup, target.config.Config.ChannelGroup.Groups["Orderer"], target.application()} {
		group.Values["Capabilities"] = &cb.ConfigValue{Value: marshal(t, &cb.Capabilities{Capabilities: map[string]*cb.Capability{"V2_0": {}}})}
	}
	block.Data.Data[0], err = target.encode()
	require.NoError(t, err)
	block.Header.DataHash, err = protoutil.BlockDataHash(block.Data)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(c.OrdererEnv.ConfigBlockPath(), marshal(t, block), 0o600))
	parsed.ConnConfig.Database = c.DBEnv.DBConf.Database
	pool, err := pgxpool.NewWithConfig(t.Context(), parsed)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// The runner initializes system tables. Remove them only in its newly
	// allocated test database so startup proves the CLI prepared the schema.
	_, err = pool.Exec(t.Context(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	require.NoError(t, err)
	return c, pool
}

func verifyRunningMigration(t *testing.T, c *runner.CommitterRuntime, imported *Result, expected map[string][]stateRow, privateKeys [][]byte, peerMSP, adminMSP string) {
	t.Helper()
	// Build real MSP endorsements. The runner's MakeAndSendTransactionsToOrderer
	// substitutes dummy signatures for expected failures, so send signed TXs directly.
	signer := func(mspDir string) func([]*applicationpb.TxNamespace) *servicepb.LoadGenTx {
		identity, err := ordererdial.NewIdentitySigner(&ordererdial.IdentityConfig{MspID: "Org1MSP", MSPDir: mspDir})
		require.NoError(t, err)
		// A role policy knows the MSP roots, not each peer certificate. Send the
		// full certificate; the load generator's certificate-ID shortcut requires
		// an identity already registered with the verifier.
		endorser, err := testsig.NewNsEndorserFromMsp(testsig.CreatorCertificate, identity)
		require.NoError(t, err)
		return func(namespaces []*applicationpb.TxNamespace) *servicepb.LoadGenTx {
			nonce := make([]byte, crypto.NonceSize)
			_, err := rand.Read(nonce)
			require.NoError(t, err)
			txID := protoutil.ComputeTxID(nonce, nil)
			tx := &applicationpb.Tx{Namespaces: namespaces, Endorsements: make([]*applicationpb.Endorsements, len(namespaces))}
			for i := range namespaces {
				tx.Endorsements[i], err = endorser.EndorseTxNs(txID, tx, i)
				require.NoError(t, err)
			}
			builder := &workload.TxBuilder{ChannelID: runner.TestChannelName, NonceSource: bytes.NewReader(nonce)}
			return builder.MakeTx(tx)
		}
	}
	authorized, unauthorized := signer(peerMSP), signer(adminMSP)
	// No namespace-creation transaction is sent before startup or these writes.
	c.Start(t, runner.FullTxPathWithQuery)
	assertQueryState(t, c, expected, privateKeys)

	updated := make(map[string][]stateRow, len(expected))
	var namespaces []*applicationpb.TxNamespace
	for _, ns := range sortedKeys(expected) {
		writes := &applicationpb.TxNamespace{NsId: ns, NsVersion: 0}
		for _, row := range expected[ns] {
			value := sha256.Sum256(append([]byte("updated:"), row.Value...))
			writes.ReadWrites = append(writes.ReadWrites, &applicationpb.ReadWrite{Key: row.Key, Value: value[:], Version: new(uint64(0))})
			updated[ns] = append(updated[ns], stateRow{Key: row.Key, Value: value[:], Version: 1})
		}
		namespaces = append(namespaces, writes)
	}
	tx := authorized(namespaces)
	resolved, err := decodeChannelConfig(imported.TargetConfigEnvelope)
	require.NoError(t, err)
	bundle, err := resolved.bundle()
	require.NoError(t, err)
	for i, ns := range namespaces {
		for _, result := range imported.Namespaces {
			if result.Namespace != ns.NsId {
				continue
			}
			verifier, err := fxpolicy.CreateNamespaceVerifier(&applicationpb.PolicyItem{Namespace: ns.NsId, Policy: result.Policy}, bundle.MSPManager())
			require.NoError(t, err)
			require.NoError(t, verifier.VerifyNs(tx.Id, tx.Tx, i), "source peer must satisfy imported policy for %s", ns.NsId)
		}
	}
	c.SendTransactionsToOrderer(t, []*servicepb.LoadGenTx{tx}, []committerpb.Status{committerpb.Status_COMMITTED})
	assertQueryState(t, c, updated, privateKeys)

	// Check every namespace separately: one restrictive policy must not hide a
	// weakened policy on another destination, including a separate hash namespace.
	for _, ns := range sortedKeys(updated) {
		row := updated[ns][0]
		tx := unauthorized([]*applicationpb.TxNamespace{{
			NsId: ns, NsVersion: 0, ReadWrites: []*applicationpb.ReadWrite{{Key: row.Key, Value: []byte("unauthorized"), Version: new(uint64(1))}},
		}})
		c.SendTransactionsToOrderer(t, []*servicepb.LoadGenTx{tx}, []committerpb.Status{committerpb.Status_ABORTED_SIGNATURE_INVALID})
	}
	assertQueryState(t, c, updated, privateKeys)
}

func assertQueryState(t *testing.T, c *runner.CommitterRuntime, expected map[string][]stateRow, privateKeys [][]byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	query := &committerpb.Query{}
	for _, ns := range sortedKeys(expected) {
		entry := &committerpb.QueryNamespace{NsId: ns, Keys: append([][]byte(nil), privateKeys...)}
		for _, row := range expected[ns] {
			entry.Keys = append(entry.Keys, row.Key)
		}
		query.Namespaces = append(query.Namespaces, entry)
	}
	response, err := c.QueryServiceClient.GetRows(ctx, query)
	require.NoError(t, err)
	actual := map[string][]stateRow{}
	for _, ns := range response.Namespaces {
		for _, row := range ns.Rows {
			actual[ns.NsId] = append(actual[ns.NsId], stateRow{Key: row.Key, Value: row.Value, Version: int64(row.Version)})
		}
	}
	require.Len(t, actual, len(expected))
	for ns, rows := range expected {
		// Exact rows also prove that no raw private keys or values were returned.
		require.ElementsMatch(t, rows, actual[ns], "namespace %s", ns)
	}
}
