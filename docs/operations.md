# Operating kine-fdb

## Topology

```
   K3s servers / kube-apiservers
              │  etcd v3 (gRPC, optionally TLS)
              ▼
   ┌──────────┴──────────┐
   kine-fdb   kine-fdb   kine-fdb      stateless, any replica serves any request
   └──────────┬──────────┘
              │  FoundationDB client (optionally TLS)
              ▼
        FoundationDB cluster           replicated, one per region
```

- **kine-fdb is stateless.** Run at least two replicas per control plane
  behind one Service or load balancer. Replicas share nothing but
  FoundationDB; each runs its own watch poller, compactor, and lease expiry,
  and they coordinate through FoundationDB transactions.
- **One namespace per control plane.** A kine-fdb process serves the single
  namespace named in its endpoint. Many control planes can share one
  FoundationDB cluster, each with its own kine-fdb deployment and namespace.
- **K3s with several servers** works like any external datastore: point every
  server at the kine-fdb Service.

## FoundationDB

Run FoundationDB as you would for any production workload: with the
[fdb-kubernetes-operator](https://github.com/FoundationDB/fdb-kubernetes-operator)
or on dedicated hosts, with `double` or `triple` redundancy and the
`ssd-redwood-1` or `ssd-2` storage engine. kine-fdb needs no special
configuration and uses FoundationDB's directory layer under the `kine` root.

The client library in the kine-fdb image must match the cluster's
major.minor version (7.3 for the default image).

### Capacity

A namespace holds the live objects plus the history that compaction has not
yet removed — about one compaction interval of changes, and at least
`--compact-min-retain` revisions. Each change writes a version (with the
value), an index row, and a change-log entry; values above 90,000 bytes are
split into chunks. `kine-fdb-admin stats` reports exact counts and sizes.

## Configuration checklist

- Endpoint: `fdb:///etc/foundationdb/fdb.cluster?namespace=/clusters/<name>`.
- Give the cluster file a writable location: FoundationDB clients rewrite it
  when coordinators change. The Helm chart copies it from a ConfigMap into an
  `emptyDir` at startup.
- `--compact-interval` (default `5m`) and `--compact-min-retain` (default
  `1000`) control how much history is kept. Clients whose watches fall
  further behind than that receive `ErrCompacted` and re-list.
- Probes: `/readyz` and `/healthz` on `--health-bind-address` (default
  `:8081`).

## Monitoring

kine-fdb exposes Prometheus metrics on `--metrics-bind-address` (default
`:8080`), in addition to kine's gRPC metrics:

| Metric | Labels | Meaning |
|---|---|---|
| `kine_fdb_transactions_total` | `namespace`, `operation`, `result` | Operations by result: `success`, `rejected` (expected outcomes such as a key that already exists), `canceled` (client gave up), `error`. |
| `kine_fdb_transaction_duration_seconds` | `namespace`, `operation` | Latency including retries. |
| `kine_fdb_transaction_retries_total` | `namespace`, `operation`, `code` | Retries by FoundationDB error code: `1020` is a conflict, `1007` a read that took too long, `1021` an unknown commit result. |
| `kine_fdb_revision` | `namespace`, `kind` | `current` and `compact` revisions. |
| `kine_fdb_watchers` | `namespace` | Active watch subscriptions. |
| `kine_fdb_compacted_versions_total` | `namespace` | Versions removed by compaction. |
| `kine_compact_total` | `result` | Compaction runs (shared with kine's SQL backends). |

Suggested alerts:

- `rate(kine_fdb_transactions_total{result="error"}[5m]) > 0` — FoundationDB
  errors or timeouts.
- `kine_fdb_revision{kind="current"} - kine_fdb_revision{kind="compact"}`
  growing without bound — compaction is not keeping up or is disabled.
- `/readyz` failing — the cluster is unreachable or the namespace is missing.
- A high rate of `code="1020"` retries on write operations means many
  concurrent writers in one namespace; see [performance](performance.md).

FoundationDB's own health (`fdbcli --exec status`) matters as much as
kine-fdb's.

## Compaction

Automatic compaction runs every `--compact-interval` and logs a line per run:

```
fdb: compacted /clusters/prod-a/ to revision 1187: 279 revisions, 342 versions removed in 1 transactions over 21ms
```

To compact by hand, for example after disabling automatic compaction:

```bash
kine-fdb-admin --endpoint "$ENDPOINT" compact --retain 1000
```

Compaction removes history in batches and resumes after interruptions, so it
is safe to stop at any time.

## Backups

Use FoundationDB's backup tooling: `fdbbackup` to a blob store or a
directory, and `fdbrestore`. A backup covers every namespace in the cluster
at one consistent version. After restoring, restart the control planes:
their watch caches hold revisions newer than the restored data.

`kine-fdb-admin` does not take backups; etcd snapshot RPCs are not supported.

## Upgrades

- **kine-fdb:** roll the Deployment. The storage format is versioned; a
  release that changes it will say so and migrate explicitly. A binary never
  opens a namespace whose format it does not know.
- **FoundationDB:** follow FoundationDB's upgrade procedure. Before upgrading
  across minor versions, make the new client library available to kine-fdb
  through the multi-version client (mount it in a directory and set
  `FDB_NETWORK_OPTION_EXTERNAL_CLIENT_DIRECTORY`), or roll out an image built
  with the matching `FDB_VERSION` right after the cluster upgrade.

## TLS

- **To FoundationDB:** set `tls_cert_file`, `tls_key_file`, and `tls_ca_file`
  in the endpoint (or kine's `--cert-file`, `--key-file`, `--ca-file`, or the
  `FDB_TLS_*` environment variables). The cluster file's addresses end in
  `:tls` for TLS clusters.
- **For etcd clients:** kine's `--server-cert-file`, `--server-key-file`, and
  `--trusted-ca-file` (to require client certificates). In the Helm chart set
  `kine.serverTLS`.

## Namespaces

```bash
kine-fdb-admin --endpoint "$ENDPOINT" namespaces
kine-fdb-admin --endpoint "$ENDPOINT" stats --namespace /clusters/prod-a
kine-fdb-admin --endpoint "$ENDPOINT" delete --namespace /clusters/old --yes
```

Stop every kine-fdb serving a namespace before deleting it. A server whose
namespace disappears fails its requests with `namespace metadata is missing`
rather than writing into an orphaned key range; restart it to create a fresh,
empty namespace.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `/readyz` returns 503, requests fail with `Unavailable` | FoundationDB is unreachable or unhealthy: check `fdbcli --exec status` from the pod (the image includes `fdbcli`) and the cluster file. |
| Startup logs `waiting for the FoundationDB cluster` | Same; kine-fdb waits up to `startup_timeout` before exiting. |
| `the directory was created with an incompatible layer` | The `kine` directory is used by something else in this cluster. |
| `uses storage format N; this build supports format 1` | The namespace was written by a newer kine-fdb. |
| Watches cancelled with `required revision has been compacted` | A client fell further behind than the retained history; raise `--compact-min-retain` or `--compact-interval`. |
| Write latency high, many `1020` retries | Many concurrent writers in one namespace; see [performance](performance.md). |

For a deeper look, enable FoundationDB client traces with
`trace_dir=/tmp/fdb-trace` in the endpoint, and kine trace logging with
`--debug`. `kine-fdb-admin verify` checks a namespace for internal
inconsistencies; run it while the namespace is quiet.
