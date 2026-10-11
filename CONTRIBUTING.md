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

## Branches and pull requests

`devel` is the integration branch: base your work on it and open pull
requests against it. `main` only moves forward to `devel` commits that
have passed CI, and releases are tagged on `main`.

CI runs on every pull request:

- lint;
- the unit, integration, race, and apiserver tests;
- the Helm chart checks;
- the image builds;
- the TLS, Helm, and K3s end-to-end tests.

A maintainer reviews every pull request. Please keep the conversation in
the pull request so that others can follow it. Add user-visible changes
under "Unreleased" in [CHANGELOG.md](CHANGELOG.md).

## Dependencies

Dependabot proposes weekly updates against `devel` for Go modules, GitHub
Actions, and base images. CI exercises each one before it is merged.

FoundationDB itself is updated by hand. Update every place that names the
version together; `git grep -n 7.3.77` lists them. Keep the Go bindings
(`github.com/apple/foundationdb/bindings/go`) on the matching FoundationDB
release branch. Dependabot is told to leave them alone. A new FoundationDB
minor version also means reviewing the API version (`DefaultAPIVersion`).

## Releases

Maintainers cut releases from `main`; see
[docs/releasing.md](docs/releasing.md).

By contributing you agree that your contributions are licensed under the
Apache License 2.0.
