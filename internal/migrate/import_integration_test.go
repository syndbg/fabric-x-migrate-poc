//go:build integration

package migrate

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"
)

func TestImport(t *testing.T) {
	dsn := os.Getenv("FABRIC_X_MIGRATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set FABRIC_X_MIGRATION_TEST_DATABASE_URL to a disposable PostgreSQL or YugabyteDB server with CREATE DATABASE permission")
	}
	ctx := context.Background()
	t.Run("independent organizations and both hash destinations", func(t *testing.T) {
		for _, separate := range []bool{false, true} {
			options := testOptions(t, separate)
			firstDB, secondDB := testDatabase(t, dsn), testDatabase(t, dsn)
			first, err := Import(ctx, firstDB, options)
			require.NoError(t, err)
			second, err := Import(ctx, secondDB, options)
			require.NoError(t, err)
			require.Equal(t, first, second)
			require.Equal(t, uint64(4), first.RecordCount)
			for _, ns := range first.Namespaces {
				a, b := readRows(t, firstDB, ns.Namespace), readRows(t, secondDB, ns.Namespace)
				require.Equal(t, a, b)
				require.Equal(t, int(ns.RecordCount), len(a))
				for _, row := range a {
					require.Zero(t, row.Version)
				}
			}
			var functions int
			require.NoError(t, firstDB.QueryRow(ctx, "SELECT count(*) FROM pg_proc WHERE proname IN ('insert_ns_basic','update_ns_basic','validate_reads_ns_basic')").Scan(&functions))
			require.Equal(t, 3, functions)
			_, err = Import(ctx, firstDB, options)
			require.ErrorContains(t, err, "is not empty")
		}
	})
	t.Run("colliding identical records roll back all tables", func(t *testing.T) {
		options := testOptions(t, false)
		options.Snapshots["securities"] = testSnapshot(t, "securities", 1)
		pool := testDatabase(t, dsn)
		_, err := Import(ctx, pool, options)
		require.ErrorContains(t, err, "duplicate key")
		requireNoTables(t, pool)
	})
	t.Run("invalid configuration leaves database untouched", func(t *testing.T) {
		options := testOptions(t, false)
		pool := testDatabase(t, dsn)
		require.NoError(t, os.WriteFile(options.TargetConfigPath, []byte("invalid"), 0o600))
		_, err := Import(ctx, pool, options)
		require.Error(t, err)
		requireNoTables(t, pool)
	})
	t.Run("concurrent imports cannot both succeed", func(t *testing.T) {
		options := testOptions(t, false)
		pool := testDatabase(t, dsn)
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for range 2 {
			wg.Go(func() { _, err := Import(ctx, pool, options); results <- err })
		}
		wg.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			}
		}
		require.Equal(t, 1, successes)
		require.Len(t, readRows(t, pool, "basic"), 4)
	})
}

func testDatabase(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	name := fmt.Sprintf("migration_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	config, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize())
		require.NoError(t, err)
		admin.Close()
	})
	return pool
}

type stateRow struct {
	Key     []byte
	Value   []byte
	Version int64
}

func readRows(t *testing.T, pool *pgxpool.Pool, namespace string) []stateRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT key,value,version FROM "+namespaceTable(namespace)+" ORDER BY key")
	require.NoError(t, err)
	result, err := pgx.CollectRows(rows, pgx.RowToStructByPos[stateRow])
	require.NoError(t, err)
	return result
}
func requireNoTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&count))
	require.Zero(t, count)
}
