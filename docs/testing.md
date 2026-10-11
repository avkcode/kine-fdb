# Testing

kine-fdb is tested at four levels. Everything except the record codec needs a
FoundationDB cluster; `make dev-up` starts one in Docker together with a Go
container that has the FoundationDB client installed.

```bash
make dev-up
make dev-test          # unit, integration, and race tests in the container
./hack/dev/devenv.sh sh './test/apiserver/run.sh'
make test-e2e          # on a Linux host with Docker
```

## Unit tests

`internal/record` holds the binary value formats and has round-trip,
corruption, and fuzz tests (`make fuzz`). It needs no FoundationDB and runs
anywhere. `pkg/fdbdriver` has unit tests for endpoint parsing and the key
layout; they compile against the FoundationDB client but need no cluster.

## Integration tests

The tests in `pkg/fdbdriver` run against the cluster named by
`KINE_FDB_TEST_CLUSTER_FILE` and skip themselves when it is unset. Each test
uses a fresh namespace and deletes it afterwards. They cover:

- create, update, and delete semantics, versions, leases, binary keys, and
  values large enough to be chunked;
- list and count ranges as kine passes them, limits that must skip deleted
  keys, reads pinned to old revisions, and multi-page reads;
- watches from the present and the past, after compactions, from other
  servers on the same namespace, and with a subscriber that stops reading;
- compaction removing exactly the unreachable history, minimum retention,
  automatic compaction, and compaction concurrent with writes;
- nested namespaces staying isolated, a namespace deleted under a running
  server, and an unknown storage format being refused;
- concurrent writers producing dense, unique revisions with no lost updates;
- lease expiry, including after a compaction and a restart;
- cancelled and expired request contexts.

Most scenarios end with the consistency checker (`kine-fdb-admin verify`).
Run them with the race detector too: `go test -race ./pkg/...`.

## Kubernetes apiserver storage tests

`test/apiserver/run.sh` runs the `k8s.io/apiserver/pkg/storage/etcd3` test
suite — the tests Kubernetes runs against etcd — against kine-fdb, the same
way kine's CI does for its own backends. It checks out the `k8s.io/apiserver`
release that `test/apiserver/go.mod` requires (Dependabot proposes newer
ones, and CI runs the suite on them before they are merged), replaces the
embedded etcd test server with `test/apiserver/testserver` (which starts
kine-fdb on a fresh namespace per test), and makes the two adjustments kine's
CI makes for every kine backend. kine-fdb 0.1.0 passes all 509 tests of
`k8s.io/apiserver` v0.37.0.

```bash
KINE_FDB_TEST_CLUSTER_FILE=/etc/foundationdb/fdb.cluster ./test/apiserver/run.sh
# APISERVER_TESTS_RUN=TestWatch narrows the run; APISERVER_TESTS_VERBOSE=1 lists tests.
```

## End-to-end soak test

`hack/e2e/k3s-soak.sh` builds nothing itself; it runs FoundationDB, a
kine-fdb image (`KINE_FDB_IMAGE`), and a K3s server in Docker and then, for
`SOAK_SECONDS`:

- creates, patches, and deletes objects and creates leased Events;
- compacts aggressively: kine-fdb and the apiserver each every minute;
- checks that every new object reaches a long-lived watch, a fresh
  `kubectl --watch`, and a fresh `etcdctl watch` straight against kine-fdb;
- restarts kine-fdb halfway and K3s at three quarters, and requires the
  control plane to become ready again;
- requires leased Events to expire, the change log to stay bounded, a
  deployment to stay available, and `kine-fdb-admin verify` to pass at the
  end.

```bash
make image IMAGE=kine-fdb VERSION=dev
KINE_FDB_IMAGE=kine-fdb:dev SOAK_SECONDS=900 ./hack/e2e/k3s-soak.sh
```

Logs, statistics, and the verification report are written to
`_output/e2e-<id>/`. `KINE_ENDPOINT` and `E2E_ADMIN=0` let other kine builds
run through the same scenario for comparison.

## TLS and Helm smoke tests

`hack/e2e/tls-smoke.sh` starts a TLS-only FoundationDB with a throwaway CA and
requires kine-fdb to serve reads and writes with the endpoint's `tls_*`
parameters, and to stay unready without them. `hack/e2e/helm-smoke.sh`
installs the Helm chart with its evaluation FoundationDB into a throwaway K3s
and checks that the replicas become ready and serve their namespace.

```bash
KINE_FDB_IMAGE=kine-fdb:dev ./hack/e2e/tls-smoke.sh
KINE_FDB_IMAGE=kine-fdb:dev ./hack/e2e/helm-smoke.sh
```

## Continuous integration

GitHub Actions (`.github/workflows/ci.yml`) runs on every push and pull
request. It installs FoundationDB on the runner and runs:

- lint;
- the unit, integration, race, and apiserver storage tests;
- the fuzzers;
- `helm lint`;
- native amd64 and arm64 image builds.

On amd64 it also runs the TLS and Helm smoke tests and a seven-minute K3s
soak test, whose logs are kept as the `e2e-soak` artifact.

`hack/ci/run-ci.sh` runs the same stages, with a ten-minute soak, on any
host with Docker. It needs no bind mounts, so it also works on CI agents
that use the host's Docker daemon. For Jenkins:

- Controllers with Pipeline can use the `Jenkinsfile`.
- `hack/ci/jenkins-job.sh` creates an equivalent freestyle job, which needs
  only core Jenkins. It applies the job with the `jc` Jenkins CLI; with
  `--print` it writes the job's XML for other tools instead.

```bash
JENKINS_LABEL=built-in ./hack/ci/jenkins-job.sh       # create or update the job
jc job build kine-fdb -p KINE_FDB_BRANCH=devel --logs # build devel and follow it
./hack/ci/jenkins-job.sh --print | java -jar jenkins-cli.jar -s "$JENKINS_URL" create-job kine-fdb
```

## Benchmarks

```bash
go test -run XXX -bench . -benchtime 5s ./pkg/fdbdriver
```

See [performance](performance.md) for results and how to read them.
