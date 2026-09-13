package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/syndbg/fabric-x-migrate-poc/internal/fabricsnapshot"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"
)

const insertBatchSize = 512

type NamespaceResult struct {
	Namespace   string `json:"namespace"`
	RecordCount uint64 `json:"record_count"`
	Policy      []byte `json:"policy"`
}

type SourceResult struct {
	Channel      string `json:"channel"`
	Block        uint64 `json:"block"`
	SnapshotHash string `json:"snapshot_hash"`
	ConfigSHA256 string `json:"config_sha256"`
}

// Result reports database counts and agreed inputs. Database digest verification
// is a separate Fabric-X feature; no migration-specific digest is computed.
type Result struct {
	Sources              []SourceResult    `json:"sources"`
	Mapping              MappingConfig     `json:"mapping"`
	TargetConfigEnvelope []byte            `json:"target_config_envelope"`
	RecordCount          uint64            `json:"record_count"`
	Namespaces           []NamespaceResult `json:"namespaces"`
}

func Import(ctx context.Context, pool *pgxpool.Pool, options Options) (*Result, error) {
	if pool == nil {
		return nil, errors.New("target database pool is required")
	}
	mappingFile, err := readInput(options.MappingPath)
	if err != nil {
		return nil, err
	}
	config, err := readMappings(mappingFile)
	if err != nil {
		return nil, err
	}
	targetFile, err := readInput(options.TargetConfigPath)
	if err != nil {
		return nil, err
	}
	target, err := decodeChannelConfig(targetFile.data)
	if err != nil {
		return nil, fmt.Errorf("target configuration: %w", err)
	}
	if target.channel != config.TargetNetwork {
		return nil, fmt.Errorf("target config network %q does not match mapping network %q", target.channel, config.TargetNetwork)
	}
	if len(options.Snapshots) == 0 || len(options.Snapshots) != len(options.SourceConfigs) {
		return nil, errors.New("each snapshot requires a source configuration for the same channel")
	}
	inputs := []inputFile{mappingFile, targetFile}
	snapshots := map[string]*fabricsnapshot.SnapshotStream{}
	sources := map[string]*channelConfig{}
	result := &Result{Mapping: config}
	for _, channel := range sortedKeys(options.Snapshots) {
		s, err := fabricsnapshot.OpenStream(options.Snapshots[channel])
		if err != nil {
			return nil, err
		}
		if s.Metadata.ChannelName != channel {
			return nil, fmt.Errorf("snapshot channel %q does not match --snapshot channel %q", s.Metadata.ChannelName, channel)
		}
		file, err := readInput(options.SourceConfigs[channel])
		if err != nil {
			return nil, fmt.Errorf("source config %s: %w", channel, err)
		}
		source, err := decodeChannelConfig(file.data)
		if err != nil {
			return nil, fmt.Errorf("source config %s: %w", channel, err)
		}
		if source.channel != channel {
			return nil, fmt.Errorf("source config channel %q does not match %q", source.channel, channel)
		}
		if source.block == nil || *source.block > s.Metadata.LastBlockNumber {
			return nil, fmt.Errorf("source config for %q must be a config block at or before the snapshot checkpoint", channel)
		}
		snapshots[channel], sources[channel] = s, source
		inputs = append(inputs, file)
		result.Sources = append(result.Sources, SourceResult{Channel: channel, Block: s.Metadata.LastBlockNumber, SnapshotHash: s.AdditionalMetadata.SnapshotHash, ConfigSHA256: file.sha256()})
	}
	counts, err := mappingCounts(config, snapshots)
	if err != nil {
		return nil, err
	}
	policies, err := preparePolicies(config, snapshots, sources, target)
	if err != nil {
		return nil, err
	}
	result.TargetConfigEnvelope, err = target.encode()
	if err != nil {
		return nil, err
	}

	walk := func(yield func(string, fabricsnapshot.Record) error) error {
		for _, channel := range sortedKeys(snapshots) {
			snapshot := snapshots[channel]
			if err := snapshot.WalkPublicRecords(func(record fabricsnapshot.Record) error {
				mapping, selected := mappingFor(config, channel, record.Namespace)
				if !selected {
					return nil
				}
				if len(record.Metadata) != 0 {
					return fmt.Errorf("%s/%s contains unsupported key-level metadata", channel, record.Namespace)
				}
				return yield(mapping.TargetNamespace, record)
			}); err != nil {
				return err
			}
			if err := snapshot.WalkPrivateHashRecords(func(record fabricsnapshot.Record) error {
				source, _, err := fabricsnapshot.SplitHashNamespace(record.Namespace)
				if err != nil {
					return err
				}
				mapping, selected := mappingFor(config, channel, source)
				if !selected {
					return nil
				}
				if len(record.Metadata) != 0 {
					return fmt.Errorf("%s/%s contains unsupported key-level metadata", channel, record.Namespace)
				}
				return yield(mapping.hashTarget(), record)
			}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(func(string, fabricsnapshot.Record) error { return ctx.Err() }); err != nil {
		return nil, fmt.Errorf("validate source records: %w", err)
	}
	revalidate := func() error {
		for _, input := range inputs {
			if err := input.revalidate(); err != nil {
				return err
			}
		}
		for _, snapshot := range snapshots {
			if err := snapshot.Revalidate(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := revalidate(); err != nil {
		return nil, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("begin target import: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, namespace := range []string{committerpb.ConfigNamespaceID, committerpb.MetaNamespaceID} {
		if _, err := tx.Exec(ctx, statedb.MakeNsTablesQuery(namespace, 0)); err != nil {
			return nil, fmt.Errorf("create system namespace %s: %w", namespace, err)
		}
	}
	// The normal configuration row serializes imports without a migration table
	// or PostgreSQL-specific advisory locks. It also binds the target network.
	if err := ensureEntry(ctx, tx, committerpb.ConfigNamespaceID, committerpb.ConfigKey, result.TargetConfigEnvelope); err != nil {
		return nil, err
	}
	for _, namespace := range sortedKeys(counts) {
		if _, err := tx.Exec(ctx, statedb.MakeNsTablesQuery(namespace, 0)); err != nil {
			return nil, fmt.Errorf("create namespace %q: %w", namespace, err)
		}
		var hasRows bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+namespaceTable(namespace)+")").Scan(&hasRows); err != nil {
			return nil, err
		}
		if hasRows {
			return nil, fmt.Errorf("target namespace %q is not empty", namespace)
		}
		if err := ensureEntry(ctx, tx, committerpb.MetaNamespaceID, namespace, policies[namespace]); err != nil {
			return nil, err
		}
	}
	batch := stateBatch{tx: tx}
	if err := walk(func(target string, record fabricsnapshot.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if batch.target != "" && batch.target != target {
			if err := batch.flush(ctx); err != nil {
				return err
			}
		}
		batch.target = target
		batch.keys = append(batch.keys, record.Key)
		value := record.Value
		if value == nil {
			value = []byte{}
		}
		batch.values = append(batch.values, value)
		if len(batch.keys) == insertBatchSize {
			return batch.flush(ctx)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("stream source snapshot into target: %w", err)
	}
	if err := batch.flush(ctx); err != nil {
		return nil, err
	}
	for _, namespace := range sortedKeys(counts) {
		var count, wrongVersion uint64
		if err := tx.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE version <> 0) FROM "+namespaceTable(namespace)).Scan(&count, &wrongVersion); err != nil {
			return nil, err
		}
		if count != counts[namespace] || wrongVersion != 0 {
			return nil, fmt.Errorf("namespace %q: expected %d records at version 0; found %d records and %d wrong versions", namespace, counts[namespace], count, wrongVersion)
		}
		result.RecordCount += count
		result.Namespaces = append(result.Namespaces, NamespaceResult{Namespace: namespace, RecordCount: count, Policy: policies[namespace]})
	}
	if err := revalidate(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit target import: %w", err)
	}
	return result, nil
}

func ensureEntry(ctx context.Context, tx pgx.Tx, namespace, key string, value []byte) error {
	if len(value) == 0 {
		return fmt.Errorf("missing configuration for %s/%s", namespace, key)
	}
	table := namespaceTable(namespace)
	if _, err := tx.Exec(ctx, "INSERT INTO "+table+" (key, value, version) VALUES ($1,$2,0) ON CONFLICT (key) DO NOTHING", []byte(key), value); err != nil {
		return err
	}
	var existing []byte
	var version uint64
	if err := tx.QueryRow(ctx, "SELECT value, version FROM "+table+" WHERE key=$1 FOR UPDATE", []byte(key)).Scan(&existing, &version); err != nil {
		return err
	}
	if !bytes.Equal(existing, value) || version != 0 {
		return fmt.Errorf("existing configuration conflicts at %s/%s", namespace, key)
	}
	return nil
}

func namespaceTable(namespace string) string {
	return pgx.Identifier{statedb.TableName(namespace)}.Sanitize()
}

type stateBatch struct {
	tx     pgx.Tx
	target string
	keys   [][]byte
	values [][]byte
}

func (batch *stateBatch) flush(ctx context.Context) error {
	if len(batch.keys) == 0 {
		return nil
	}
	query := "INSERT INTO " + namespaceTable(batch.target) + ` (key, value, version)
SELECT key, value, 0 FROM unnest($1::bytea[], $2::bytea[]) AS rows(key, value)`
	result, err := batch.tx.Exec(ctx, query, batch.keys, batch.values)
	if err != nil {
		return fmt.Errorf("insert into namespace %q: %w", batch.target, err)
	}
	if uint64(result.RowsAffected()) != uint64(len(batch.keys)) {
		return fmt.Errorf("insert into namespace %q wrote %d rows; expected %d", batch.target, result.RowsAffected(), len(batch.keys))
	}
	batch.keys = batch.keys[:0]
	batch.values = batch.values[:0]
	return nil
}
