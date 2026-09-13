//go:build integration

package migrate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	cb "github.com/hyperledger/fabric-protos-go-apiv2/common"
	pb "github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestNormalRuntimeAfterImport(t *testing.T) {
	if os.Getenv("FABRIC_X_MIGRATION_TEST_RUNTIME") != "1" {
		t.Skip("set FABRIC_X_MIGRATION_TEST_RUNTIME=1 after make runtime-binaries")
	}
	dsn := os.Getenv("FABRIC_X_MIGRATION_TEST_DATABASE_URL")
	require.NotEmpty(t, dsn)
	parsed, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	binaryDir := filepath.Join(root, "artifacts", "runtime", "bin")
	for _, name := range []string{"committer", "mock"} {
		_, err := os.Stat(filepath.Join(binaryDir, name))
		require.NoError(t, err, "run make runtime-binaries")
	}
	// The upstream runner expects bin/ relative to its test repository root.
	workingRoot := t.TempDir()
	require.NoError(t, os.Symlink(binaryDir, filepath.Join(workingRoot, "bin")))
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
	})
	c.SystemConfig.Policy.NamespacePolicies["basic"] = &workload.Policy{Scheme: workload.PolicySchemeMSP}
	c.TxBuilder, err = workload.NewTxBuilderFromPolicy(c.SystemConfig.Policy, nil)
	require.NoError(t, err)
	signature := new(cb.SignaturePolicyEnvelope)
	require.NoError(t, proto.Unmarshal(c.TxBuilder.TxEndorser.VerificationPolicies()["basic"].GetMspRule(), signature))
	application := &pb.ApplicationPolicy{Type: &pb.ApplicationPolicy_SignaturePolicy{SignaturePolicy: signature}}
	snapshot := testSnapshot(t, runner.TestChannelName, 1, application)
	mappingPath := filepath.Join(t.TempDir(), "mapping.json")
	mapping := MappingConfig{TargetNetwork: runner.TestChannelName, Mappings: []Mapping{{SourceChannel: runner.TestChannelName, SourceNamespace: "basic", TargetNamespace: "basic"}}}
	require.NoError(t, os.WriteFile(mappingPath, marshalJSON(t, mapping), 0o600))
	dbConfig := parsed.Copy()
	dbConfig.ConnConfig.Database = c.DBEnv.DBConf.Database
	pool, err := pgxpool.NewWithConfig(t.Context(), dbConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	result, err := Import(t.Context(), pool, Options{
		Snapshots:     map[string]string{runner.TestChannelName: snapshot},
		SourceConfigs: map[string]string{runner.TestChannelName: c.OrdererEnv.ConfigBlockPath()},
		MappingPath:   mappingPath, TargetConfigPath: c.OrdererEnv.ConfigBlockPath(),
	})
	require.NoError(t, err)
	require.Equal(t, uint64(2), result.RecordCount)
	// No namespace-creation transaction is sent. Startup must load the imported
	// policy and preserve the version-zero rows before accepting new traffic.
	c.Start(t, runner.FullTxPathWithQuery)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	query := &committerpb.Query{Namespaces: []*committerpb.QueryNamespace{{NsId: "basic", Keys: [][]byte{{1}, bytes.Repeat([]byte{1}, 32)}}}}
	rows, err := c.QueryServiceClient.GetRows(ctx, query)
	require.NoError(t, err)
	require.Len(t, rows.Namespaces, 1)
	require.Len(t, rows.Namespaces[0].Rows, 2)
	for _, row := range rows.Namespaces[0].Rows {
		require.Zero(t, row.Version)
	}
	update := []*applicationpb.TxNamespace{{NsId: "basic", NsVersion: 0, ReadWrites: []*applicationpb.ReadWrite{{Key: []byte{1}, Value: []byte("updated"), Version: new(uint64(0))}}}}
	c.MakeAndSendTransactionsToOrderer(t, [][]*applicationpb.TxNamespace{update}, []committerpb.Status{committerpb.Status_COMMITTED})
	rows, err = c.QueryServiceClient.GetRows(ctx, query)
	require.NoError(t, err)
	for _, row := range rows.Namespaces[0].Rows {
		if bytes.Equal(row.Key, []byte{1}) {
			require.Equal(t, []byte("updated"), row.Value)
			require.Equal(t, uint64(1), row.Version)
		}
	}
	// Signing the next write with an unrelated key must fail the imported MSP policy.
	c.AddOrUpdateNamespaces(t, "basic")
	update[0].ReadWrites[0].Version = new(uint64(1))
	c.MakeAndSendTransactionsToOrderer(t, [][]*applicationpb.TxNamespace{update}, []committerpb.Status{committerpb.Status_ABORTED_SIGNATURE_INVALID})
}
