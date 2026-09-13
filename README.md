# Fabric to Fabric-X migration PoC

A standalone Go CLI for the
[Fabric Classic snapshot migration RFC](https://github.com/syndbg/fabric-x-rfcs/blob/feat-fabric-snapshot-migration/0003-fabric-x-snapshot-migration.md).
It streams official peer snapshots directly into each organization's committer
database. It imports public keys and private-data hashes at version `0`, reuses
source MSPs and endorsement policies, and maps source channels and namespaces
into a target network.

The CLI uses upstream Fabric-X database and policy helpers. It does not require
committer changes. Block history, transaction IDs, private values, and source key
versions are not imported. There is no migration bundle or migration state table.

## Run an import

Build with the Go version specified in `go.mod`:

```sh
make build
```

1. Stop writes on the source channels and wait for pending transactions to finish.
   Capture the agreed peer snapshots and the channel configuration effective at
   each checkpoint. Confirm the source configuration using trusted channel records.
2. Agree on target membership, ordering settings, administrative policies, and
   namespace mappings. Prepare the target configuration offline.
3. Stop committer services and all other target database writers. Keep the
   databases running. The target must not have accepted application transactions,
   and every mapped application namespace must be empty.
4. Run the same import independently for each organization, changing only the
   database connection.
5. Compare the source identities, mappings, resolved configuration, policies, and
   database record counts in the reports. Complete Fabric-X database digest
   verification when that feature becomes available, before starting services or
   enabling application traffic.

`mappings.json`, for one channel:

```json
{
  "target_network": "network1",
  "mappings": [
    {
      "source_channel": "assets",
      "source_namespace": "basic",
      "target_namespace": "assets_basic"
    }
  ]
}
```

```sh
PGPASSFILE=./postgres.pass \
./artifacts/bin/fabric-x-migrate import \
  --snapshot assets=./snapshots/assets \
  --source-config assets=./config/assets.block \
  --mapping mappings.json \
  --target-config ./config/network1.config \
  --database-url 'postgres://migration@db.org1.example:5432/committer?sslmode=verify-full' \
  > org1-import.json
```

pgx supports the standard PostgreSQL password file through `PGPASSFILE`.

`--source-config` accepts a CONFIG block from `peer channel fetch config`. Its
channel must match the snapshot, and its block number must not exceed the
snapshot checkpoint. This check cannot establish that a supplied configuration
was the latest one at that checkpoint; operators must confirm that separately.

`--target-config` accepts a standard initial Fabric-X CONFIG envelope or config
block, with the target channel ID, ordering settings, and administrative and
system namespace policies. The tool adds missing source application MSPs and
rejects conflicting MSP definitions. It validates the resolved configuration and
application namespace policies with the upstream committer code before writing.
Signing keys are not inputs.

The JSON report includes the resolved standard CONFIG envelope as Base64 in
`target_config_envelope`. Save it for the target's offline startup configuration:

```sh
jq -r .target_config_envelope org1-import.json | base64 --decode > resolved.config
```

This envelope contains public configuration. Keep target service configuration
consistent with it. Writing this envelope into the committer database does not
start any service or submit namespace-creation transactions.

## Consolidate channels

Add one `--snapshot CHANNEL=DIR` and `--source-config CHANNEL=BLOCK` pair for each
source channel. Every mapping identifies its source channel, so equally named
source namespaces remain distinct.

```json
{
  "target_network": "network1",
  "mappings": [
    {
      "source_channel": "assets",
      "source_namespace": "basic",
      "target_namespace": "assets_basic",
      "target_hash_namespace": "assets_hashes"
    },
    {
      "source_channel": "securities",
      "source_namespace": "basic",
      "target_namespace": "securities_basic"
    }
  ]
}
```

```sh
PGPASSFILE=./postgres.pass \
./artifacts/bin/fabric-x-migrate import \
  --snapshot assets=./snapshots/assets \
  --source-config assets=./config/assets.block \
  --snapshot securities=./snapshots/securities \
  --source-config securities=./config/securities.block \
  --mapping mappings.json \
  --target-config ./config/network1.config \
  --database-url 'postgres://migration@db.org1.example:5432/committer?sslmode=verify-full' \
  > org1-import.json
```

To share a target namespace, set both `target_namespace` fields to the same ID.
Their effective endorsement policies must match and their keys must not overlap.
A duplicate target key fails the import even when the values are identical.

Without `target_hash_namespace`, public records and collection hash records share
`target_namespace`. With it, all hashes from that source namespace go to the
specified application namespace. Hash bytes are preserved: the source key hash
becomes the target key, and the source value hash becomes the target value.
There is no new key encoding or collection mapping.

A separate hash namespace does not hide records from network members. Channel
consolidation changes who can see public records and hashes. It also cannot
resolve duplicate hash keys between collections mapped to the same destination.

## Validation and database writes

The reader checks snapshot metadata, SHA-256 file hashes, file pairs, record
framing, and source record order. It supports the official SimpleKeyValueDB and
CouchDB snapshot formats. It reads records with bounded memory and discards
source key versions after checking them.

The tool reads selected committed `_lifecycle` definitions, resolves endorsement
policy references in their original source channel, and reuses collection
endorsement policies where present. It rejects unsupported validation plugins,
conflicting namespace policies, and key-level metadata it cannot preserve.
Unmapped application state and lifecycle state are not copied into application
tables.

After preflight validation, the tool creates missing namespace tables and their
normal committer functions, writes MSP configuration and namespace policies, and
inserts bounded batches in one serializable transaction. It uses the normal
configuration row to serialize imports. Database constraints reject duplicate
keys. Errors roll back all changes made by that transaction.

Before committing, it checks database row counts and version `0`, then rechecks
all source files, configuration, and mappings for changes. A successful report
contains source identities, resolved configuration, policies, mappings, and
counts read from the database. Repeated imports into nonempty namespaces fail.
Save source inputs and take a database backup before accepting target writes;
Fabric-X block history cannot reconstruct the imported rows.

## Verification status

The Fabric-X database state digest feature is being implemented separately and
is **not available yet**. This PoC does not compute a custom digest or provide a
fallback. Record counts alone do not establish state equality across committers.
Full migration verification and cutover depend on that feature.

Unit tests cover source parsing, mappings, MSP conflicts, and policy conversion.
The database test uses two independent databases and covers public state, both
hash destinations, consolidation, collision rollback, repeated imports, and
concurrent imports. It compares actual rows within the test; this does not add a
verification algorithm to the CLI.

```sh
make test

FABRIC_X_MIGRATION_TEST_DATABASE_URL='postgres://postgres@localhost:25432/postgres?sslmode=disable' \
go test -tags=integration ./internal/migrate -run '^TestImport$' -count=1 -v
```

Use a disposable PostgreSQL or YugabyteDB server with permission to create and
drop test databases. Full normal-startup and application-transaction validation,
YugabyteDB validation, and end-to-end runs against freshly captured snapshots
remain part of the PoC work.

The source-network harness and Fabric configuration are under `hack/` and
`internal/integrationtest/`. `make run-hack` starts a local Fabric source network;
`make stop-hack` stops it and deletes its volumes. Run `make help` for the available
targets.
