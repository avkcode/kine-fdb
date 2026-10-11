# Contributing to kine-fdb

Thanks for your interest in improving kine-fdb. Bug reports, fixes, tests,
and documentation are all welcome.

## Reporting issues

Open a GitHub issue with the kine-fdb version (`kine-fdb --version`), the
FoundationDB version, the Kubernetes or K3s version, what you expected, and
what happened, including logs. Report security problems privately as
described in [SECURITY.md](SECURITY.md).

## Development environment

Building needs the FoundationDB client library and headers (cgo). The
easiest way to get them, and a cluster to test against, is Docker:

```bash
make dev-up      # FoundationDB 7.3 + a Go container with the FDB client
make dev-shell   # a shell in that container, with the repository at /src
make dev-test    # every Go test, including integration and race tests
make dev-down    # remove it all
```

On a machine with the FoundationDB client installed you can also run
`make build` and `make test` directly; integration tests run when
`KINE_FDB_TEST_CLUSTER_FILE` points at a cluster.

## Making changes

- Keep changes focused, and add or update tests with every behavior change.
  Storage and watch changes should be covered by the integration tests and
  should keep the apiserver storage tests passing (`test/apiserver/run.sh`).
- Run `make lint` (gofmt, `go vet`, staticcheck) and the tests before sending
  a pull request.
- Every source file starts with the license header and a short description
  of what the file is for; follow the existing files.
- The on-disk format is versioned (`StorageFormat`, `record.FormatVersion`).
  Changing it needs a design discussion in an issue first and an explicit
  migration path.
- Write commit messages in the imperative mood ("Fix watch catch-up after
  compaction") with a body explaining why when it is not obvious.

## Pull requests

CI runs the lint, unit, integration, race, apiserver, Helm, and image
builds. A maintainer reviews every pull request; please keep the
conversation in the pull request so others can follow it.

By contributing you agree that your contributions are licensed under the
Apache License 2.0.
