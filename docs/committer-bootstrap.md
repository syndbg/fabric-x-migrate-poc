# Fabric-X committer changes for snapshot initialization

This document describes the PoC on the
[Fabric-X committer migration branch](https://github.com/syndbg/fabric-x-committer/tree/feat-add-fabric-to-fabric-x-migration).
It follows the
[migration RFC source](https://github.com/syndbg/fabric-rfcs/blob/feat-add-fabric-to-fabric-x-migration/text/0000-fabric-x-snapshot-migration.md).
The last sections list the work that remains before production use.

## Public CLI contract

Keep the mentorship issue's startup mode:

```sh
committer --init-from-snapshot <snapshot-datafile> [--config <vc.yaml>]
committer --verify-migration <snapshot-datafile> [--config <vc.yaml>]
committer --activate-migration <snapshot-datafile> [--config <vc.yaml>]
```

`cmd/committer/main.go` registers the root `--init-from-snapshot` flag and the
existing `--config` flag. The snapshot flag selects an offline
Validator-Committer initialization path. This path does not start the VC gRPC
server, Coordinator connection, transaction workers, Query Service, Sidecar,
or health endpoint.

The mode is mutually exclusive with `start`, `healthcheck`, and other
subcommands. A missing snapshot argument, unreadable file, or incompatible
configuration is a command error.

Activation is a separate offline operation. It requires the same verified
file, target anchor, namespace map, configuration, policies, state, and
transaction-ID set recorded by import. It changes only the migration-record
status from `VERIFIED` to `ACTIVE`. Repeating activation for the same file is
an idempotent success; every later `--init-from-snapshot` attempt is rejected.
The marker does not itself coordinate activation across organizations or open
application ingress.

Verification is read-only. It revalidates the exact bundle, migration record,
target anchor, configuration, namespace map, policies, public state, and
transaction-ID baseline, then prints the configuration, map, policy, state,
and transaction-ID counts and SHA-256 digests. It works in both `VERIFIED` and
`ACTIVE` state. These fields are the comparison contract between committer
organizations.

## Shared format package

The committer must use the exact reader and generated protobuf types used by
the exporter.

> TODO(schema-ownership): use the PoC module during local development. Before
> merge, move the exporter and shared schema/reader package to the proposed
> production owner, `github.com/hyperledger/fabric-x-common`, release it, and
> update the committer to that version. Do not permanently depend on the
> [personal PoC repository](https://github.com/syndbg/fabric-x-migrate-poc)
> and do not copy the `.proto` into the
> [Fabric-X committer implementation](https://github.com/syndbg/fabric-x-committer/tree/feat-add-fabric-to-fabric-x-migration).

The reader must validate the entire input file before opening a database write
transaction: archive member set and order, fixed tar headers,
manifest encoding, format versions, SHA-256 hashes, protobuf framing,
deterministic protobuf bytes, record counts, canonical ordering, namespace
mapping, duplicate keys and transaction IDs, and PDC exclusions.

## Validator-Committer ownership

The import belongs under `service/vc` because that service owns the Fabric-X
state tables and transaction-ID uniqueness. The package exposes three narrow
entry points:

```go
func InitFromSnapshot(ctx context.Context, config *vc.Config, path string) (*vc.SnapshotInitResult, error)
func VerifySnapshot(ctx context.Context, config *vc.Config, path string) (*vc.SnapshotVerificationResult, error)
func ActivateSnapshot(ctx context.Context, config *vc.Config, path string) (*vc.SnapshotActivationResult, error)
```

The CLI reads the normal VC configuration and calls the selected function. Each
function creates a database pool but does not construct or run
`ValidatorCommitterService`.

## Database schema

`service/vc/init_database_tmpl.sql` adds the migration record and migrated
transaction-ID storage.

### Migration record

The `migration_record` table contains one durable record for the initialized
database:

| Field | Meaning |
|---|---|
| migration ID / file SHA-256 | Exact imported genesis-data file |
| source channel and block | Fabric checkpoint identity |
| source snapshot hash | Bind to the verified peer snapshot |
| target anchor | Last committed Fabric-X block when import ran |
| target-configuration digest | Bind the effective Fabric-X configuration at the anchor |
| namespace-map digest | Bind imported rows to target tables |
| target-policy digest | Bind the installed namespace policies |
| public-state count and digest | Expected and verified state |
| transaction-ID count and digest | Expected and verified anti-replay set |
| status | `VERIFIED` after import and `ACTIVE` after the separate activation command |

Import runs in one SQL transaction. The migration record, public rows, and
transaction IDs all commit or all roll back. There is no journal or partial
import state. If representative snapshots show that one transaction is too
large, the design will need an `IMPORTING` state and a batch checkpoint.

### Migrated transaction IDs

Do not fabricate normal `tx_status` rows. Its current schema requires a status
and non-null Fabric-X height, neither of which exists in a Fabric snapshot.

The separate registry is:

```sql
CREATE TABLE IF NOT EXISTS migrated_tx_ids
(
    tx_id BYTEA NOT NULL PRIMARY KEY
);
```

Update `insert_tx_status` so it reports an ID already present in either
`tx_status` or `migrated_tx_ids` as a duplicate. The migrated table is immutable
after initialization. This preserves anti-replay without inventing source
status or target height.

## Import transaction

The offline initializer must:

1. Verify the entire genesis-data file before connecting for writes.
2. Create or validate the normal system tables.
3. Acquire a database advisory lock dedicated to snapshot initialization.
4. Read the actual `last committed block number` from `metadata` and retain it
   as the target anchor.
5. Require every mapped target namespace table and installed policy to exist.
6. Require every mapped application namespace to contain zero rows.
7. Reject a transaction ID already present in either transaction-ID table.
8. Insert public keys through committer-owned SQL with target version `0`.
9. Insert the migrated transaction IDs.
10. Re-read canonical state and IDs, recompute counts and digests, and insert a
    `VERIFIED` migration record in the same transaction.
11. Commit once and print a human-readable integrity summary.

Running the same verified file again returns success only when the migration
record and the current database digests still match. A different file,
non-empty namespace, changed policy, duplicate target key, or inconsistent
migration record fails without changing the database.

## Implemented surfaces

| Current surface | PoC change |
|---|---|
| `cmd/committer/main.go` | Mutually exclusive root import, verification, and activation operations |
| `cmd/committer/config.go` | Existing VC configuration loading is reused unchanged |
| `service/vc/init_database_tmpl.sql` | `migration_record`, `migrated_tx_ids`, and cross-table duplicate detection |
| `service/vc/bootstrap.go` | Offline initializer, atomic import, independent read-only verification, activation, bindings, digest scans, and idempotency |
| `service/vc/bootstrap_test.go` | Database contract and failure-path coverage |
| `integration/migration/migration_test.go` | Devnet setup, export, CLI import, verification, activation, restart, two organizations, checkpoint rebuild, and both source versions |
| `utils/testdb/container.go` | Use the published host endpoint so Docker-backed Go tests work on macOS |

No Sidecar block is created for imported state. Normal Fabric-X block history
remains unchanged, and the existing last-committed-block metadata is not
advanced by initialization.

## Version 1 operational decisions

Version `1` uses an offline operational barrier. Application ingress is
disabled, namespace transactions reach finality, and all target orderers stop.
Before stopping the write-side services, the test requires the Sidecar
block-store height and Coordinator next-block number to equal `B + 1` and the
Validator-Committer anchor to equal `B`. Together, these values show that the
Sidecar and committer processed block `B` and neither is waiting for an earlier
block.

Each committer organization stores its own singleton migration record and
imports the same bundle independently. Activation is the local offline
`--activate-migration` command, authorized by deployment access rather than a
Fabric-X namespace policy. Ingress opens only after all required organizations
report the same migration ID, source checkpoint, anchor, configuration, map,
policy, state, and transaction-ID results and all local records are `ACTIVE`.

Imported transaction IDs stay in the separate immutable `migrated_tx_ids`
registry. Version `1` accepts only empty Fabric key metadata and does not
translate source policies. The operator installs and approves a Fabric-X MSP
or threshold namespace policy before import; the record binds its digest. The
migration ID already commits to the PDC exclusion manifest, so activation is
the acknowledgement and no second PDC field is stored.

Database backup is the normal recovery path. A full rebuild must replay target
blocks through `B`, stop, reapply the retained bundle to empty mapped
namespaces, verify it, and then replay later target blocks. Blocks cannot
discover this external baseline themselves. Version `1` has no superseding
checkpoint, so the bundle and migration evidence remain available for the
target ledger's lifetime. It binds the effective target configuration at `B`,
not the exact serialized block `0` hash.

## Reproducible acceptance flow

The Go tests own setup, execution, assertions, restart, and cleanup. No manual
database inspection is part of acceptance.

```sh
cd ../fabric-x-migrate-poc
go test ./...
go test -tags=integration ./internal/cmd

cd ../fabric-x-committer
go test ./service/vc \
  -run '^(TestInitFromSnapshot|TestVerifySnapshot|TestActivateSnapshot)$'
DB_TYPE=postgres go test ./service/vc \
  -run '^(TestInitFromSnapshot|TestVerifySnapshot|TestActivateSnapshot)$'
go test -tags=integration ./integration/migration \
  -run '^TestInitFromSnapshot$'
```

The final command builds the committer and creates a disposable Fabric-X
devnet. It creates the mapped namespace, exports the source snapshot, stops
write-side services, imports the bundle, verifies the target database, and
activates the migration. It then restarts the services and updates an imported
key through the Arma orderer. The test expects the new value, a higher target
version, and a `COMMITTED` transaction status.

The same flow runs for Fabric 2.5.16, Fabric 3.1.5, and two channel identities
mapped to separate target networks. Other cases import the same bundle into
Org1 and Org2 before opening ingress. The recovery case destroys a committer
database, replays through `B`, reapplies the bundle, and then replays `B + 1`.

The committer's two-channel case changes the channel identity in the verified
3.1.5 fixture and recomputes the official snapshot hash to test target
isolation. Separately, the exporter integration suite creates a disposable
Fabric 3.1.5 network, captures `payments` and `securities` through `peer
snapshot submitrequest`, and exports both official snapshots. The same harness
deploys a collection-enabled chaincode, writes private data, and captures a
CouchDB-backed snapshot containing PDC hashes and collection history. The
committer suite imports the public baseline, verifies the recorded exclusions,
activates, restarts, and updates it through a normal Fabric-X transaction.

## Test status

Current test coverage includes:

- Fabric 2.5.16 and 3.1.5 fixture export and devnet import;
- one-namespace and multiple-namespace imports, checked against target state and transaction IDs;
- corrupted bundle rejection in the shared reader;
- non-empty or missing target namespace;
- existing normal transaction ID;
- same-file idempotent rerun and different-file rejection;
- migrated transaction-ID duplicate detection;
- state, policy, anchor, migration-record, and transaction-ID integrity checks;
- service restart without changing the imported baseline;
- rollback of public state, transaction IDs, and the migration record when the
  final insert fails;
- idempotent activation and rejection of every import after `ACTIVE`;
- read-only target verification before and after activation, including tamper rejection;
- a post-activation Fabric-X transaction submitted through the Arma orderer;
- two source channel identities imported into two separately initialized target networks;
- `payments` and `securities` snapshots captured separately and exported without metadata rewriting;
- a CouchDB-backed snapshot containing PDC hashes and collection history, followed through export, bootstrap, verification, activation, restart, and a post-activation transaction;
- rejection of non-empty key metadata, empty source namespaces, and unsupported target policy forms, with both MSP and threshold target policies accepted;
- orderer ingress closure, Sidecar height `B + 1`, Coordinator next block `B + 1`, and Validator-Committer anchor `B` before service shutdown;
- matching import evidence, local activation, and one committed transaction across the Org1 and Org2 committers;
- full database rebuild by replaying target blocks through `B`, reapplying the retained bundle, and replaying `B + 1`;
- PostgreSQL and YugabyteDB integration coverage.

The remaining tests before production acceptance are:

- representative production-scale import limits and memory use;
- interruption and restart-journal recovery if batching replaces the atomic transaction;
- production-scale multi-organization and recovery runs.
