# Fabric to Fabric-X migration PoC

This repository contains the proof of concept for
[LFDT mentorship issue #65](https://github.com/LF-Decentralized-Trust-Mentorships/mentorship-program/issues/65).
It implements the CLI contract from that issue:

```sh
fabric-x-migrate export \
  --snapshot <fabric-peer-snapshot-directory> \
  --output <snapshot-datafile>

committer --init-from-snapshot <snapshot-datafile>
```

The exporter produces one portable, verifiable genesis-data file. It retains
public application state and Fabric transaction IDs, but excludes PDC
artifacts.

## Review links

* [Migration RFC source](https://github.com/syndbg/fabric-rfcs/blob/feat-add-fabric-to-fabric-x-migration/text/0000-fabric-x-snapshot-migration.md)
* [Migration exporter and genesis-data PoC](https://github.com/syndbg/fabric-x-migrate-poc)
* [Fabric-X committer implementation branch](https://github.com/syndbg/fabric-x-committer/tree/feat-add-fabric-to-fabric-x-migration)

## Export one snapshot

From this repository, export the captured Fabric 3.1.5 snapshot as one file:

```sh
go run ./cmd/fabric-x-migrate export \
  --snapshot ../fabric-x-migration-lab/artifacts/snapshots/fabric-3.1.5/block-3 \
  --fabric-version 3.1.5 \
  --namespace basic=migration_basic \
  --output artifacts/genesis-data/fabric-3.1.5-block-3.fxgenesis

go run ./cmd/fabric-x-migrate verify \
  --input artifacts/genesis-data/fabric-3.1.5-block-3.fxgenesis

go run ./cmd/fabric-x-migrate verify-source \
  --snapshot ../fabric-x-migration-lab/artifacts/snapshots/fabric-3.1.5/block-3 \
  --input artifacts/genesis-data/fabric-3.1.5-block-3.fxgenesis
```

The exporter verifies the Fabric snapshot metadata and every declared source
file before decoding it. It accepts `SimpleKeyValueDB` and `CouchDB`, one or
more explicit namespace mappings, no Fabric key metadata, and retains all
snapshot transaction IDs. Repeat `--namespace SOURCE=TARGET` for every included
application namespace. Missing or empty sources, duplicate source mappings,
target namespace collisions, and duplicate mapped keys are rejected.
Unselected public namespaces, PDC hash namespaces, and collection configuration
history are recorded as exclusions in the output manifest. Target namespace
IDs must satisfy the Fabric-X contract
(`[a-z0-9_]+`, at most 60 characters, excluding `_meta` and `_config`).

`verify` checks the bundle on its own. `verify-source` rebuilds it from the
original peer snapshot and requires an exact match.

## Genesis-data file

The file is a deterministic, uncompressed tar archive. The committer
opens it directly; operators do not unpack it.

```text
snapshot-datafile
├── manifest.json
├── public_state.data
└── transaction_ids.data
```

The two `.data` members begin with format byte `1`, followed by repeated
`uvarint(message length) || deterministic protobuf message` records. The
manifest identifies the source checkpoint, namespace mapping, member hashes,
record counts, and every excluded source artifact.

The record schema is
[`proto/fabricx/migration/v1/`](proto/fabricx/migration/v1/).
Generated Go is checked in under `pkg/genesisdata/`.
That package is the PoC's public bundle contract; the exporter, Fabric snapshot
reader, and integration harness stay under `internal/`.

> TODO(schema-ownership): this repository owns the exporter, reader/writer,
> `.proto`, and generated Go package for the PoC. Before upstream merge, move
> them to the proposed production owner,
> `github.com/hyperledger/fabric-x-common`, and update
> the [Fabric-X committer implementation](https://github.com/syndbg/fabric-x-committer/tree/feat-add-fabric-to-fabric-x-migration)
> to consume that released package. Do not copy and
> independently edit the schema in the committer.

Bundle format version `1` is the accepted initial PoC contract. Prefer adding
fields with well-defined defaults; never remove, renumber, or repurpose existing
fields. Changes to required archive members, framing, canonical identity, or
imported-state semantics require a new bundle-format version. Older readers may
reject additive fields they do not understand.

## Regenerate protobuf Go

You need Go, plus network access on the first run. Generator versions are
pinned in `generate.go` and `buf.gen.yaml`.

```sh
make regen-proto
make test
make test-integration
make lint
```

The first command runs the pinned Buf and `protoc-gen-go` versions. Regeneration
must leave the generated `pkg/genesisdata/*.pb.go` files unchanged.

The integration test exports the Fabric 2.5.16 and 3.1.5 fixtures twice and
compares the bytes, records, transaction IDs, mappings, and exclusions. It also
starts a disposable Fabric 3.1.5 network, captures the `payments` and
`securities` channels with `peer snapshot submitrequest`, and repeats the test
with CouchDB. Fabric binaries are cached under `.cache/fabric`; set
`FABRIC_SOURCE_HOME` to reuse an existing release.
The checked-in Compose and Fabric configuration are under [`hack/`](hack/).
Run `make help` for build, lint-fix, local-network, and cleanup targets. For
example, `make run-hack FABRIC_VERSION=2.5.16 STATE_DATABASE=goleveldb` starts
and joins a local source network using the pinned `fabric-samples` checkout.

## Fabric-X committer work

The committer implementation is on the
[migration branch](https://github.com/syndbg/fabric-x-committer/tree/feat-add-fabric-to-fabric-x-migration).
Its Go integration test creates a disposable
Fabric-X devnet and runs the migration for both source versions. The
test covers export, atomic offline bootstrap, idempotency, target verification,
activation, service restart, and rejection of imports after activation. It also
bootstraps two channel-specific bundles into separate Fabric-X networks:

```sh
cd ../fabric-x-committer
go test ./service/vc \
  -run '^(TestInitFromSnapshot|TestVerifySnapshot|TestActivateSnapshot)$'
DB_TYPE=postgres go test ./service/vc \
  -run '^(TestInitFromSnapshot|TestVerifySnapshot|TestActivateSnapshot)$'
go test -tags=integration ./integration/migration \
  -run '^TestInitFromSnapshot$'
```

The database contract runs against YugabyteDB and PostgreSQL. The devnet test
uses the repository's PostgreSQL configuration and owns setup and teardown. It
does not rely on manual inspection. Its derived channel fixtures test target
network isolation. The exporter integration test also captures snapshots from
two Fabric channels with the peer CLI. The committer integration test captures
a CouchDB-backed snapshot and runs it through export, bootstrap,
verification, activation, restart, and a post-activation transaction.
The fixed version `1` decisions and remaining production acceptance evidence are tracked in
[docs/committer-bootstrap.md](docs/committer-bootstrap.md).
