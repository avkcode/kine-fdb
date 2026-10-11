#!/usr/bin/env bash
# End-to-end soak test: FoundationDB, kine-fdb, and a K3s server in Docker.
# It compacts aggressively (kine and the apiserver every minute), churns
# objects, and checks continuously that long-lived and newly created watches
# keep delivering events, that leases expire, and that storage stays bounded.
# Requires Docker on a Linux host.
#
#   KINE_FDB_IMAGE=kine-fdb:dev SOAK_SECONDS=900 ./hack/e2e/k3s-soak.sh
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
id="${E2E_ID:-$(date +%s)}"
image="${KINE_FDB_IMAGE:-kine-fdb:e2e}"
k3s_image="${K3S_IMAGE:-rancher/k3s:v1.37.1-k3s1}"
fdb_image="${FDB_IMAGE:-foundationdb/foundationdb:7.3.77}"
etcd_image="${ETCD_IMAGE:-gcr.io/etcd-development/etcd:v3.6.5}"
soak="${SOAK_SECONDS:-900}"
compact_interval="${KINE_COMPACT_INTERVAL:-1m}"
apiserver_compaction="${APISERVER_COMPACTION_INTERVAL:-1m}"
namespace="/e2e/${id}/"
# The endpoint can be overridden, and E2E_ADMIN=0 skips the kine-fdb-admin
# checks, to run other kine builds through the same scenario.
endpoint="${KINE_ENDPOINT:-fdb:///etc/foundationdb/fdb.cluster?namespace=${namespace}}"
out="${E2E_OUT:-${repo}/_output/e2e-${id}}"
net="kinefdb-e2e-${id}"
fdb="kinefdb-e2e-fdb-${id}"
kine="kinefdb-e2e-kine-${id}"
k3s="kinefdb-e2e-k3s-${id}"
agent="kinefdb-e2e-agent-${id}"
token="soak-${id}"
mkdir -p "${out}"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "${out}/soak.log" >&2; }
fail() { log "FAIL: $*"; failed=1; }
failed=0

cleanup() {
  local code=$?
  set +e
  docker logs "${kine}" >"${out}/kine.log" 2>&1
  docker logs "${k3s}" >"${out}/k3s.log" 2>&1
  docker logs "${agent}" >"${out}/agent.log" 2>&1
  if [[ "${KEEP:-0}" != "1" ]]; then
    docker rm -f -v "${agent}" "${k3s}" "${kine}" "${fdb}" >/dev/null 2>&1
    docker network rm "${net}" >/dev/null 2>&1
  fi
  log "logs and results are in ${out}"
  exit "${code}"
}
trap cleanup EXIT

kubectl() { docker exec "${k3s}" kubectl "$@"; }
admin() { docker exec "${kine}" kine-fdb-admin --endpoint "${endpoint}" "$@"; }

log "network ${net}"
docker network create "${net}" >/dev/null

log "starting FoundationDB (${fdb_image})"
docker run -d --name "${fdb}" --network "${net}" -e FDB_NETWORKING_MODE=container "${fdb_image}" >/dev/null
for _ in $(seq 1 60); do
  if docker exec "${fdb}" fdbcli --timeout 5 --exec "status minimal" 2>/dev/null | grep -q "available"; then
    break
  fi
  docker exec "${fdb}" fdbcli --timeout 5 --exec "configure new single ssd" >/dev/null 2>&1 || true
  sleep 2
done
docker exec "${fdb}" cat /var/fdb/fdb.cluster >"${out}/fdb.cluster"
chmod 0644 "${out}/fdb.cluster"

log "starting kine-fdb (${image}), compaction every ${compact_interval}"
# The cluster file is copied in rather than bind-mounted so the script also
# works from CI agents that talk to a remote Docker daemon.
docker create --name "${kine}" --network "${net}" \
  "${image}" \
  --endpoint "${endpoint}" \
  --listen-address 0.0.0.0:2379 \
  --metrics-bind-address 0.0.0.0:8080 \
  --compact-interval "${compact_interval}" \
  --compact-min-retain 100 >/dev/null
mkdir -p "${out}/etc/foundationdb"
cp "${out}/fdb.cluster" "${out}/etc/foundationdb/fdb.cluster"
tar -C "${out}/etc" -cf - foundationdb | docker cp - "${kine}:/etc/"
docker start "${kine}" >/dev/null

# The control plane and the node run in separate containers, so the control
# plane can be restarted on its own (a single-container K3s cannot restart its
# kubelet on cgroup v2 hosts).
log "starting K3s (${k3s_image}), apiserver compaction every ${apiserver_compaction}"
docker run -d --name "${k3s}" --network "${net}" --privileged \
  "${k3s_image}" server \
  --disable-agent \
  --token "${token}" \
  --datastore-endpoint "http://${kine}:2379" \
  --disable=traefik,servicelb,metrics-server,local-storage \
  --kube-apiserver-arg "etcd-compaction-interval=${apiserver_compaction}" \
  --kube-apiserver-arg "event-ttl=${EVENT_TTL_SECONDS:-120}s" >/dev/null
docker run -d --name "${agent}" --network "${net}" --privileged \
  "${k3s_image}" agent --server "https://${k3s}:6443" --token "${token}" >/dev/null

for _ in $(seq 1 90); do
  if kubectl get nodes 2>/dev/null | grep -q " Ready"; then
    break
  fi
  sleep 2
done
kubectl get nodes >"${out}/nodes-start.txt" 2>&1 || true
grep -q " Ready" "${out}/nodes-start.txt" || { fail "node never became Ready"; exit 1; }
log "K3s is Ready"

# etcdctl talks to kine-fdb directly, so the scenario also covers etcd
# watches opened after compactions (the apiserver's own watches are opened
# once and then served from its cache).
docker create --name "${k3s}-etcdctl" "${etcd_image}" >/dev/null
docker cp "${k3s}-etcdctl:/usr/local/bin/etcdctl" "${out}/etcdctl"
docker rm "${k3s}-etcdctl" >/dev/null
docker cp "${out}/etcdctl" "${k3s}:/bin/etcdctl"

wait_ready() {
  local what=$1 deadline=$(( $(date +%s) + 180 ))
  until kubectl get --raw /readyz >/dev/null 2>&1 && kubectl get nodes 2>/dev/null | grep -q " Ready"; do
    if [[ "$(docker inspect -f '{{.State.Running}}' "${k3s}" 2>/dev/null)" != "true" ]]; then
      fail "the K3s server exited after ${what}"
      return 1
    fi
    if (( $(date +%s) > deadline )); then
      fail "control plane not ready within 180s after ${what}"
      return 1
    fi
    sleep 2
  done
  log "control plane ready after ${what}"
}

kubectl create namespace soak >/dev/null
kubectl -n soak create deployment pause --image=registry.k8s.io/pause:3.10 --replicas=3 >/dev/null

# A long-lived watch that must see every probe for the whole run.
docker exec "${k3s}" sh -c 'kubectl -n soak get configmaps --watch --output-watch-events -o name > /tmp/long-watch.log 2>&1 &'

stats() {
  [[ "${E2E_ADMIN:-1}" == "1" ]] || return 0
  admin stats 2>/dev/null | tr -d ' \n' | sed 's/,/, /g'
}

start=$(date +%s)
probe=0
event_times=()
restarted_kine=0
restarted_k3s=0
next_stats=0
while (( $(date +%s) - start < soak )); do
  elapsed=$(( $(date +%s) - start ))
  probe=$((probe + 1))
  name="probe-${probe}"

  # Restart kine-fdb halfway and K3s at three quarters: both must recover on
  # a namespace that has been compacted many times.
  if (( ! restarted_kine && elapsed >= soak / 2 )); then
    log "restarting kine-fdb"
    docker restart "${kine}" >/dev/null
    restarted_kine=1
    wait_ready "restarting kine-fdb" || exit 1
  fi
  if (( ! restarted_k3s && elapsed >= soak * 3 / 4 )); then
    log "restarting the K3s server"
    docker restart "${k3s}" >/dev/null
    restarted_k3s=1
    wait_ready "restarting the K3s server" || exit 1
  fi

  # Fresh watches per probe, through the apiserver and directly against
  # kine-fdb: kine-fdb's lab predecessor cancelled every new etcd watch once
  # the namespace had been compacted.
  docker exec "${k3s}" sh -c "timeout 20 kubectl -n soak get configmaps --watch --output-watch-events -o name > /tmp/fresh-${probe}.log 2>&1 &"
  docker exec "${k3s}" sh -c "timeout 20 etcdctl --endpoints http://${kine}:2379 watch --prefix /registry/configmaps/soak/${name} > /tmp/etcd-fresh-${probe}.log 2>&1 &"
  sleep 1
  kubectl -n soak create configmap "${name}" --from-literal=n="${probe}" >/dev/null || fail "creating ${name} failed at t=${elapsed}s"
  # Churn: update and delete earlier objects.
  kubectl -n soak patch configmap "${name}" -p "{\"data\":{\"patched\":\"${elapsed}\"}}" >/dev/null || fail "patching ${name} failed at t=${elapsed}s"
  if (( probe > 5 )); then
    kubectl -n soak delete configmap "probe-$((probe - 5))" --wait=false >/dev/null || fail "deleting probe-$((probe - 5)) failed at t=${elapsed}s"
  fi
  # Events are stored with a lease (--event-ttl); kine's TTL watcher must
  # delete them once it expires.
  printf 'apiVersion: v1\nkind: Event\nmetadata:\n  generateName: soak-\ninvolvedObject:\n  kind: ConfigMap\n  name: %s\n  namespace: soak\nreason: Soak\nmessage: probe %s\ntype: Normal\n' "${name}" "${probe}" \
    | docker exec -i "${k3s}" kubectl -n soak create -f - >/dev/null || fail "creating an event failed at t=${elapsed}s"
  event_times+=("$(date +%s)")

  deadline=$(( $(date +%s) + 15 ))
  until docker exec "${k3s}" grep -qx "configmap/${name}" "/tmp/fresh-${probe}.log" 2>/dev/null; do
    if (( $(date +%s) > deadline )); then
      fail "fresh watch for ${name} saw nothing at t=${elapsed}s: $(docker exec "${k3s}" cat "/tmp/fresh-${probe}.log" 2>/dev/null | tail -2 | tr '\n' ' ')"
      break
    fi
    sleep 1
  done
  if ! docker exec "${k3s}" grep -qF "/registry/configmaps/soak/${name}" "/tmp/etcd-fresh-${probe}.log" 2>/dev/null; then
    sleep 2
    docker exec "${k3s}" grep -qF "/registry/configmaps/soak/${name}" "/tmp/etcd-fresh-${probe}.log" 2>/dev/null \
      || fail "fresh etcd watch for ${name} saw nothing at t=${elapsed}s: $(docker exec "${k3s}" cat "/tmp/etcd-fresh-${probe}.log" 2>/dev/null | tr -cd '[:print:]\n' | tail -2 | tr '\n' ' ')"
  fi
  if (( ! restarted_k3s )) && ! docker exec "${k3s}" grep -qx "configmap/${name}" /tmp/long-watch.log; then
    sleep 3
    docker exec "${k3s}" grep -qx "configmap/${name}" /tmp/long-watch.log \
      || fail "long-lived watch missed ${name} at t=${elapsed}s"
  fi

  if (( elapsed >= next_stats )); then
    log "t=${elapsed}s probe=${probe} $(stats)"
    next_stats=$((elapsed + 60))
  fi
  sleep 2
done

log "soak finished after ${probe} probes"
kubectl get --raw /readyz >/dev/null || fail "apiserver /readyz failed"
kubectl -n soak get deployment pause -o jsonpath='{.status.readyReplicas}' >"${out}/ready-replicas.txt" || true
[[ "$(cat "${out}/ready-replicas.txt")" == "3" ]] || fail "pause deployment has $(cat "${out}/ready-replicas.txt") ready replicas, want 3"

if [[ "${E2E_ADMIN:-1}" == "1" ]]; then
  final="$(admin stats)"
  printf '%s\n' "${final}" >"${out}/stats-final.json"
  compact=$(sed -n 's/.*"compactRevision": \([0-9]*\).*/\1/p' <<<"${final}")
  current=$(sed -n 's/.*"currentRevision": \([0-9]*\).*/\1/p' <<<"${final}")
  logs=$(sed -n 's/.*"logEntries": \([0-9]*\).*/\1/p' <<<"${final}")
  log "final: current=${current} compact=${compact} logEntries=${logs}"
  (( compact > 0 )) || fail "the namespace was never compacted"
  # Compaction keeps roughly one interval of history; the log must not hold
  # anything close to the full history.
  (( logs < current / 2 )) || fail "change log holds ${logs} of ${current} revisions: history is not being removed"
fi

# Every probe created one leased Event; only those younger than the event
# TTL (plus slack for kine's expiry queue) may remain if lease expiry kept
# working through compactions.
ttl_seconds=$(( ${EVENT_TTL_SECONDS:-120} + 60 ))
now=$(date +%s)
young=0
for t in "${event_times[@]}"; do
  (( now - t < ttl_seconds )) && young=$((young + 1))
done
events=$(kubectl -n soak get events --field-selector reason=Soak -o name | wc -l)
log "leased events still stored: ${events} of ${#event_times[@]} (${young} are younger than ${ttl_seconds}s)"
(( events <= young )) || fail "${events} leased events are stored but only ${young} are young enough: lease expiry stopped"

log "stopping K3s"
docker stop "${agent}" "${k3s}" >/dev/null
sleep 2
if [[ "${E2E_ADMIN:-1}" == "1" ]]; then
  if admin verify >"${out}/verify.json"; then
    log "verify: OK"
  else
    fail "verify reported problems: $(head -c 2000 "${out}/verify.json")"
  fi
fi

if (( failed )); then
  log "SOAK FAILED"
  exit 1
fi
log "SOAK PASSED"
