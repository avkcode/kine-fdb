# kine-fdb Helm chart

Deploys [kine-fdb](https://github.com/avkcode/kine-fdb) — the etcd v3 API for
Kubernetes control planes, stored in FoundationDB — in front of an existing
FoundationDB cluster.

```bash
helm install prod-a oci://ghcr.io/avkcode/charts/kine-fdb \
  --set fdb.clusterFile.configMap=my-cluster-config \
  --set fdb.namespace=/clusters/prod-a
```

Then point K3s or a kube-apiserver at
`http://<release>-kine-fdb.<namespace>.svc:2379`.

## FoundationDB connection

kine-fdb needs the cluster file. Provide exactly one of:

- `fdb.clusterFile.configMap`: an existing ConfigMap, for example the
  `<cluster>-config` ConfigMap that
  [fdb-kubernetes-operator](https://github.com/FoundationDB/fdb-kubernetes-operator)
  creates (its key is `cluster-file`, the default of `fdb.clusterFile.key`);
- `fdb.clusterFile.secret`: an existing Secret;
- `fdb.clusterFile.value`: the cluster file contents, stored in a new ConfigMap.

An init container copies the cluster file into a writable volume, because
FoundationDB clients update it when coordinators change.

`fdb.dev.enabled=true` instead starts a single FoundationDB process inside
the release. It keeps one copy of the data and is meant for evaluation and CI
only.

Each control plane needs its own `fdb.namespace`; many releases can share one
FoundationDB cluster.

## Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `2` | kine-fdb replicas; all serve the same namespace. |
| `image.repository` | `ghcr.io/avkcode/kine-fdb` | Image repository. |
| `image.tag` | chart appVersion | Image tag. |
| `fdb.namespace` | `/kine/` | etcd keyspace inside FoundationDB. |
| `fdb.clusterFile.configMap` / `.secret` / `.key` / `.value` | | Cluster file source, see above. |
| `fdb.params` | `{}` | Extra endpoint parameters, e.g. `{timeout: 10s}`. |
| `fdb.tls.enabled` | `false` | Use TLS to FoundationDB. |
| `fdb.tls.secretName` | | Secret with `tls.crt`, `tls.key`, `ca.crt`. |
| `fdb.tls.verifyPeers` | | FoundationDB `tls_verify_peers` string. |
| `fdb.dev.enabled` | `false` | Run an evaluation-only FoundationDB in the release. |
| `fdb.dev.persistence.*` | 8Gi PVC | Storage of the evaluation FoundationDB. |
| `kine.port` | `2379` | etcd API port. |
| `kine.metricsPort` | `8080` | Prometheus metrics port. |
| `kine.healthPort` | `8081` | `/healthz` and `/readyz` port. |
| `kine.compactInterval` | `5m` | Automatic compaction interval; `0` disables it. |
| `kine.compactMinRetain` | `1000` | Newest revisions never compacted. |
| `kine.watchProgressNotifyInterval` | `5s` | Watch progress notification interval. |
| `kine.logFormat` | `json` | `plain` or `json`. |
| `kine.debug` | `false` | Trace logging. |
| `kine.serverTLS.enabled` | `false` | Serve the etcd API over TLS. |
| `kine.serverTLS.secretName` | | Secret with `tls.crt`, `tls.key`, and `ca.crt` (client certificates are then required). |
| `kine.extraArgs` / `kine.extraEnv` | `[]` | Additional flags and environment. |
| `service.type` | `ClusterIP` | Service type. |
| `serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor. |
| `podDisruptionBudget.enabled` | `true` | PDB with `maxUnavailable: 1` when `replicaCount > 1`. |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName`, `podAnnotations`, `podLabels` | | Pod scheduling and resources; replicas spread across nodes by default. |
| `podSecurityContext`, `securityContext` | non-root, read-only root filesystem | Security settings. |
