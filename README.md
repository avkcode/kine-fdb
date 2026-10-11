# kine-fdb

[![CI](https://github.com/avkcode/kine-fdb/actions/workflows/ci.yml/badge.svg)](https://github.com/avkcode/kine-fdb/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/avkcode/kine-fdb)](https://github.com/avkcode/kine-fdb/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/avkcode/kine-fdb.svg)](https://pkg.go.dev/github.com/avkcode/kine-fdb/pkg/fdbdriver)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

kine-fdb stores Kubernetes control-plane state in
[FoundationDB](https://www.foundationdb.org/). It is a storage driver for
[kine](https://github.com/k3s-io/kine), the etcd v3 API shim that K3s uses,
and ships as a drop-in `kine-fdb` server: point K3s (or any kube-apiserver)
at it with `--datastore-endpoint` and the cluster's objects, watches, and
leases live in FoundationDB.

```
 kube-apiserver / K3s ──etcd v3 gRPC──▶ kine-fdb ──▶ FoundationDB
                                        (stateless,      (strictly serializable,
                                         run several)     replicated, fault tolerant)
```

Why FoundationDB:

- **Durable, replicated, strictly serializable.** Every etcd write is one
  FoundationDB transaction; there is no local state to back up or fail over.
- **Stateless servers.** Run as many `kine-fdb` replicas as you like behind
  one Service. Any replica can serve any request and sees every write.
- **Many control planes, one cluster.** Each control plane gets its own
  *namespace*: an independent etcd keyspace with its own revisions, isolated
  in its own FoundationDB directory.

## Features

- The full kine backend contract: get, list, count, streaming range reads,
  create / compare-and-swap update / delete, watches with previous values,
  leases with expiry, and compaction.
- **Passes the Kubernetes apiserver storage test suite**
  (`k8s.io/apiserver/pkg/storage/etcd3`, 509 tests) — see
  [Testing](docs/testing.md).
- **Bounded storage.** Compaction deletes the history it makes unreachable,
  in small resumable batches, without conflicting with writes.
- **Low-latency watches.** One poller per process follows a revision change
  log and is woken by a FoundationDB watch, so events arrive within
  milliseconds; a slow watcher falls back to reading the log and is never
  dropped.
- **Large namespaces.** Lists and counts are read in pages pinned to one
  revision, so they never hit FoundationDB's five-second transaction limit;
  values up to the etcd request limit are chunked transparently.
- **Group commit.** Concurrent writes share FoundationDB transactions, so
  write throughput grows with the number of clients instead of collapsing
  into conflicts.
- **Robust under failure.** Every request honors its deadline, transactions
  have bounded timeouts, and a write whose commit outcome was unknown is
  retried without being applied twice.
- `kine-fdb-admin` to list namespaces and inspect, compact, verify, or delete
  them; Prometheus metrics; `/healthz` and `/readyz`; a Helm chart; and a
  multi-architecture container image.

## Quick start

With Docker Compose this starts FoundationDB, kine-fdb, and a K3s server that
stores its state through kine-fdb:

```bash
git clone https://github.com/avkcode/kine-fdb && cd kine-fdb/deploy/compose
docker compose up -d                # add --build to build kine-fdb from this tree
export KUBECONFIG=$PWD/output/kubeconfig.yaml
kubectl get nodes
```

Inspect what K3s wrote:

```bash
docker compose exec kine-fdb kine-fdb-admin stats
```

## Running kine-fdb

`kine-fdb` is kine with the FoundationDB driver built in and selected by
default; it accepts all of kine's flags.

```bash
kine-fdb \
  --endpoint 'fdb:///etc/foundationdb/fdb.cluster?namespace=/clusters/prod-a' \
  --listen-address 0.0.0.0:2379
```

Then start the control plane against it:

```bash
k3s server --datastore-endpoint http://kine-fdb.example:2379
# or: kube-apiserver --etcd-servers=http://kine-fdb.example:2379 ...
```

The container image is `ghcr.io/avkcode/kine-fdb`. It reads the cluster file
from `/etc/foundationdb/fdb.cluster` by default:

```bash
docker run -d -p 2379:2379 -v /etc/foundationdb:/etc/foundationdb ghcr.io/avkcode/kine-fdb:0.1.0
```

### Kubernetes

The Helm chart deploys kine-fdb replicas in front of an existing FoundationDB
cluster, for example one managed by the
[fdb-kubernetes-operator](https://github.com/FoundationDB/fdb-kubernetes-operator):

```bash
helm install prod-a oci://ghcr.io/avkcode/charts/kine-fdb \
  --set fdb.clusterFile.configMap=my-fdb-cluster-config \
  --set fdb.namespace=/clusters/prod-a
```

See [charts/kine-fdb](charts/kine-fdb/README.md) for all values, including
TLS and an evaluation-only single-pod FoundationDB.

## Configuration

The endpoint names the cluster file and the namespace:

```
fdb://[cluster-file-path][?parameter=value&...]
```

`fdb:///etc/foundationdb/fdb.cluster` is an absolute path and
`fdb://fdb.cluster` a relative one; `fdb://` alone uses the FoundationDB
default (`$FDB_CLUSTER_FILE`, `./fdb.cluster`, then
`/etc/foundationdb/fdb.cluster`). Unknown parameters are rejected.

| Parameter | Default | Meaning |
|---|---|---|
| `namespace` | `/kine/` | Independent etcd keyspace; `a`, `/a`, and `/a/` are the same namespace. |
| `cluster_file` | path of the URL | Cluster file; overrides the path. |
| `api_version` | `730` | FoundationDB API version (630–730). |
| `timeout` | `15s` | Limit for one operation, retries included. |
| `startup_timeout` | `2m` | How long to wait for the cluster at startup. |
| `poll_interval` | `1s` | Fallback poll period for watches; changes normally arrive through a FoundationDB watch first. |
| `tls_cert_file`, `tls_key_file`, `tls_ca_file`, `tls_password`, `tls_verify_peers` | | Client TLS to FoundationDB. kine's `--cert-file`, `--key-file`, `--ca-file` fill unset values; `FDB_TLS_*` environment variables also work. |
| `trace_dir`, `trace_format` | | Write FoundationDB client trace files (`xml` or `json`). |

kine's own flags apply as usual, notably:

| Flag | Default | Meaning |
|---|---|---|
| `--compact-interval` | `5m` | Automatic compaction period; `0` disables it and honors compaction requests from clients instead. |
| `--compact-min-retain` | `1000` | Newest revisions never compacted. |
| `--compact-batch-size` | `1000` | Revisions cleaned per transaction. |
| `--poll-batch-size` | `500` | Revisions read per watch poll. |
| `--watch-progress-notify-interval` | `5s` | Watch progress notifications. |

`kine-fdb` adds `--health-bind-address` (default `:8081`, `0` disables) for
`/healthz` (process is up) and `/readyz` (the namespace is readable). Metrics
are served on kine's `--metrics-bind-address` (default `:8080`).

## Operating kine-fdb

[docs/operations.md](docs/operations.md) covers sizing, multiple replicas and
tenants, monitoring, compaction, backups with `fdbbackup`, upgrades, and
troubleshooting. In short:

```bash
kine-fdb-admin --endpoint "$ENDPOINT" namespaces        # list namespaces
kine-fdb-admin --endpoint "$ENDPOINT" stats             # revisions, keys, sizes
kine-fdb-admin --endpoint "$ENDPOINT" compact --retain 1000
kine-fdb-admin --endpoint "$ENDPOINT" verify            # consistency check
kine-fdb-admin --endpoint "$ENDPOINT" delete --namespace /old --yes
```

[docs/architecture.md](docs/architecture.md) describes the key layout,
revisions, watches, and compaction.

## Compatibility

| kine-fdb | kine | FoundationDB | Kubernetes tested |
|---|---|---|---|
| 0.1.x | v0.17.2 | 7.3 (client 7.3.77, API 730) | K3s v1.37.1; apiserver storage tests v0.37.0 |

The FoundationDB client library in the image must match the major.minor
version of your cluster. For other versions build the image with
`--build-arg FDB_VERSION=...`, or load additional client libraries with
FoundationDB's multi-version client
(`FDB_NETWORK_OPTION_EXTERNAL_CLIENT_DIRECTORY`) while you upgrade the
cluster.

## Limitations

- Writes to one namespace are serialized: each takes the next revision, and
  etcd semantics need revisions to be dense and ordered. Group commit lets
  concurrent writes share transactions (thousands of writes per second even
  on a single-process FoundationDB), but one namespace cannot use more than
  one commit stream; separate control planes use separate namespaces. See
  [docs/performance.md](docs/performance.md).
- The database size reported to etcd clients (`etcdctl endpoint status`, the
  apiserver's storage size metric) is FoundationDB's sampled estimate; small
  namespaces may report imprecise sizes.
- kine does not implement every etcd feature (for example lease keep-alive
  RPCs or arbitrary transactions); those limits apply here too.

## Development

Building requires the FoundationDB client library and headers. The
development environment runs them in Docker together with a test cluster:

```bash
make dev-up          # FoundationDB + a Go container with the FDB client
make dev-test        # unit, integration, and race tests
make test-apiserver  # Kubernetes apiserver storage tests (in the dev container)
make test-e2e        # K3s soak test against a freshly built image
```

See [CONTRIBUTING.md](CONTRIBUTING.md) and [docs/testing.md](docs/testing.md).

## License

Apache License 2.0; see [LICENSE](LICENSE). kine and FoundationDB are also
Apache-2.0 licensed.
