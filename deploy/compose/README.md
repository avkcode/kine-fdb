# Docker Compose quick start

Starts a single-process FoundationDB, kine-fdb, and a K3s server whose
datastore is kine-fdb. It is meant for trying kine-fdb out; the FoundationDB
here keeps one copy of the data.

```bash
docker compose up -d
export KUBECONFIG=$PWD/output/kubeconfig.yaml
kubectl get nodes
```

This uses the released `ghcr.io/avkcode/kine-fdb` image; `docker compose up
-d --build` builds kine-fdb from this checkout instead, and `KINE_FDB_IMAGE`
selects another image.

Inspect the namespace K3s writes to:

```bash
docker compose exec kine-fdb kine-fdb-admin --endpoint 'fdb:///etc/foundationdb/fdb.cluster?namespace=/quickstart/' stats
```

The etcd API is published on `127.0.0.1:2379`, so etcd tools work too:

```bash
etcdctl --endpoints http://127.0.0.1:2379 get /registry/namespaces --prefix --keys-only
```

If ports 2379, 6443, or 8080 are taken, set `KINE_PORT`, `K3S_PORT`, or
`METRICS_PORT` (and adjust the server port in `output/kubeconfig.yaml`).

Remove everything, including the data:

```bash
docker compose down -v
```
