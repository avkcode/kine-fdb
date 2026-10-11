# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org/).

## [Unreleased]

### Changed

- The Docker Compose quick start runs the released image instead of
  building one; set `KINE_FDB_IMAGE` to run another build.
- `kine-fdb --version` shows the commit as a 12-character hash in every
  build.

## [0.1.0] - 2026-10-11

First public release.

### Added

- FoundationDB driver for kine v0.17 (`pkg/fdbdriver`), registered as the
  `fdb` endpoint scheme, and the `kine-fdb` server with it built in.
- Namespaces isolated in FoundationDB directories, with a versioned binary
  storage format (format 1).
- Dense, totally ordered revisions with group commit: concurrent writes
  share transactions; idempotent retries after unknown commit results;
  context-aware transactions with bounded timeouts.
- Paged list, count, and streaming range reads pinned to a revision.
- Watches driven by one change-log poller per process and FoundationDB
  watches; lagging subscribers catch up from the log instead of being
  dropped.
- Compaction that deletes unreachable history in resumable batches,
  automatic or on request.
- `kine-fdb-admin`: `namespaces`, `stats`, `compact`, `verify`, `delete`.
- Prometheus metrics, `/healthz` and `/readyz` endpoints.
- Container image (amd64, arm64), Helm chart, Docker Compose quick start.
- Tests: unit and fuzz tests, FoundationDB integration tests, the Kubernetes
  apiserver storage test suite, and a K3s end-to-end soak test.

[Unreleased]: https://github.com/avkcode/kine-fdb/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/avkcode/kine-fdb/releases/tag/v0.1.0
