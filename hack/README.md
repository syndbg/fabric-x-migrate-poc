# Fabric snapshot test network

`compose.yaml` is a local development source network. The same topology runs
either supported source version:

```bash
make run-hack FABRIC_VERSION=2.5.16 STATE_DATABASE=goleveldb
make stop-hack FABRIC_VERSION=2.5.16 STATE_DATABASE=goleveldb

make run-hack FABRIC_VERSION=3.1.5 STATE_DATABASE=CouchDB
make stop-hack FABRIC_VERSION=3.1.5 STATE_DATABASE=CouchDB
```

The Makefile downloads the matching Fabric binaries and checks out
`fabric-samples` under `hack/fabric-samples` at the verified commit
listed in [`fabric-samples.commit`](../fabric-samples.commit). Both are ignored
by Git.

The integration tests use their own configuration under
`internal/integrationtest/testdata`; they do not read this directory:

```bash
go test -tags=integration ./internal/cmd -run TestExport -v
```

Generated crypto material and ledgers remain under the ignored local setup or
Docker volumes. Run `make stop-hack` to remove the volumes.
