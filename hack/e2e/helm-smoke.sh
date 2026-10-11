#!/usr/bin/env bash
# Installs the Helm chart into a throwaway K3s cluster running in Docker,
# with the evaluation FoundationDB and a locally built image, and checks that
# kine-fdb becomes ready and serves its namespace. Requires Docker and helm.
#
#   KINE_FDB_IMAGE=kine-fdb:dev ./hack/e2e/helm-smoke.sh
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
id="${E2E_ID:-$(date +%s)}"
image="${KINE_FDB_IMAGE:-kine-fdb:e2e}"
k3s_image="${K3S_IMAGE:-rancher/k3s:v1.37.1-k3s1}"
fdb_image="${FDB_IMAGE:-foundationdb/foundationdb:7.3.77}"
net="kinefdb-helm-${id}"
k3s="kinefdb-helm-k3s-${id}"
out="${E2E_OUT:-${repo}/_output/helm-${id}}"
mkdir -p "${out}"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

cleanup() {
  local code=$?
  set +e
  if [[ ${code} -ne 0 ]]; then
    kubectl get pods -A -o wide >"${out}/pods.txt" 2>&1
    kubectl -n kine-fdb describe pods >"${out}/describe.txt" 2>&1
    kubectl -n kine-fdb logs -l app.kubernetes.io/name=kine-fdb --all-containers --tail 200 >"${out}/kine-fdb.log" 2>&1
  fi
  if [[ "${KEEP:-0}" != "1" ]]; then
    docker rm -f -v "${k3s}" >/dev/null 2>&1
    docker network rm "${net}" >/dev/null 2>&1
  fi
  exit "${code}"
}
trap cleanup EXIT

docker network create "${net}" >/dev/null
log "starting K3s (${k3s_image})"
docker run -d --name "${k3s}" --network "${net}" --privileged "${k3s_image}" server \
  --disable=traefik,servicelb,metrics-server >/dev/null

for _ in $(seq 1 90); do
  docker exec "${k3s}" kubectl get nodes 2>/dev/null | grep -q " Ready" && break
  sleep 2
done
ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${k3s}")"
docker exec "${k3s}" cat /etc/rancher/k3s/k3s.yaml | sed "s/127.0.0.1/${ip}/" >"${out}/kubeconfig.yaml"
export KUBECONFIG="${out}/kubeconfig.yaml"

log "importing ${image} and ${fdb_image} into K3s"
docker image inspect "${fdb_image}" >/dev/null 2>&1 || docker pull -q "${fdb_image}" >/dev/null
docker save "${image}" "${fdb_image}" | docker exec -i "${k3s}" ctr images import - >/dev/null

repository="${image%:*}"
tag="${image##*:}"
log "installing the chart"
helm install smoke "${repo}/charts/kine-fdb" --namespace kine-fdb --create-namespace \
  --set image.repository="${repository}" --set image.tag="${tag}" --set image.pullPolicy=Never \
  --set fdb.dev.enabled=true --set fdb.dev.image="${fdb_image}" --set fdb.dev.persistence.enabled=false \
  --set fdb.namespace=/helm-smoke/ \
  --wait --timeout 6m

kubectl -n kine-fdb get pods -o wide
ready="$(kubectl -n kine-fdb get deploy smoke-kine-fdb -o jsonpath='{.status.readyReplicas}')"
[[ "${ready}" == "2" ]] || { log "FAIL: ${ready:-0} of 2 kine-fdb replicas ready"; exit 1; }

endpoint="$(kubectl -n kine-fdb get deploy smoke-kine-fdb -o jsonpath='{.spec.template.spec.containers[0].args[0]}')"
endpoint="${endpoint#--endpoint=}"
kubectl -n kine-fdb exec deploy/smoke-kine-fdb -- kine-fdb-admin --endpoint "${endpoint}" stats >"${out}/stats.json"
# A fresh namespace holds only the health key that kine-fdb seeds on start.
grep -q '"keys": 1,' "${out}/stats.json" || { log "FAIL: unexpected stats: $(cat "${out}/stats.json")"; exit 1; }

log "Helm smoke test PASSED"
