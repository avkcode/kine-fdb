#!/usr/bin/env bash
# Checks client TLS to FoundationDB: starts a TLS-only FoundationDB with a
# throwaway CA, then requires kine-fdb to serve reads and writes with the
# endpoint's tls_* parameters and to stay unready without them. Requires
# Docker and openssl.
#
#   KINE_FDB_IMAGE=kine-fdb:dev ./hack/e2e/tls-smoke.sh
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
id="${E2E_ID:-$(date +%s)}"
image="${KINE_FDB_IMAGE:-kine-fdb:e2e}"
fdb_image="${FDB_IMAGE:-foundationdb/foundationdb:7.3.77}"
etcd_image="${ETCD_IMAGE:-gcr.io/etcd-development/etcd:v3.6.5}"
net="kinefdb-tls-${id}"
fdb="kinefdb-tls-fdb-${id}"
kine="kinefdb-tls-kine-${id}"
plain="kinefdb-tls-plain-${id}"
out="${E2E_OUT:-${repo}/_output/tls-${id}}"
mkdir -p "${out}/tls"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
cleanup() {
  local code=$?
  set +e
  docker logs "${kine}" >"${out}/kine.log" 2>&1
  docker logs "${fdb}" >"${out}/fdb.log" 2>&1
  if [[ "${KEEP:-0}" != "1" ]]; then
    docker rm -f -v "${kine}" "${plain}" "${fdb}" >/dev/null 2>&1
    docker network rm "${net}" >/dev/null 2>&1
  fi
  exit "${code}"
}
trap cleanup EXIT

log "creating a CA and a certificate for server and client"
(
  cd "${out}/tls"
  openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.crt -days 2 -subj "/CN=kine-fdb test CA" 2>/dev/null
  openssl req -newkey rsa:2048 -nodes -keyout tls.key -out tls.csr -subj "/CN=fdb" 2>/dev/null
  printf 'subjectAltName=DNS:fdb\nextendedKeyUsage=serverAuth,clientAuth\n' >ext.cnf
  openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out tls.crt -days 2 -extfile ext.cnf 2>/dev/null
  chmod 0644 ./*
)

docker network create "${net}" >/dev/null
docker create --name "${fdb}" --network "${net}" --network-alias fdb --entrypoint /bin/bash "${fdb_image}" -c '
  ip=$(hostname -i | awk "{print \$1}")
  echo "tls:test@${ip}:4500:tls" > /var/fdb/fdb.cluster
  exec fdbserver --listen-address "0.0.0.0:4500:tls" --public-address "${ip}:4500:tls" \
    --datadir /var/fdb/data --logdir /var/fdb/logs -C /var/fdb/fdb.cluster \
    --tls-certificate-file /tls/tls.crt --tls-key-file /tls/tls.key --tls-ca-file /tls/ca.crt' >/dev/null
tar -C "${out}" -cf - tls | docker cp - "${fdb}:/"
docker start "${fdb}" >/dev/null

fdbcli() {
  docker exec "${fdb}" fdbcli -C /var/fdb/fdb.cluster --timeout 5 \
    --tls-certificate-file /tls/tls.crt --tls-key-file /tls/tls.key --tls-ca-file /tls/ca.crt --exec "$1"
}
for _ in $(seq 1 30); do
  fdbcli "status minimal" 2>/dev/null | grep -q "available" && break
  fdbcli "configure new single ssd" >/dev/null 2>&1 || true
  sleep 2
done
fdbcli "status minimal" | grep -q "available" || { log "FAIL: TLS FoundationDB did not start"; exit 1; }
docker exec "${fdb}" cat /var/fdb/fdb.cluster >"${out}/fdb.cluster"
log "TLS cluster: $(cat "${out}/fdb.cluster")"

mkdir -p "${out}/etc/foundationdb"
cp "${out}/fdb.cluster" "${out}/etc/foundationdb/fdb.cluster"
start_kine() {
  local name=$1 endpoint=$2
  docker create --name "${name}" --network "${net}" "${image}" --endpoint "${endpoint}" >/dev/null
  tar -C "${out}/etc" -cf - foundationdb | docker cp - "${name}:/etc/"
  tar -C "${out}" -cf - tls | docker cp - "${name}:/"
  docker start "${name}" >/dev/null
}
ready() {
  docker exec "$1" bash -c 'exec 3<>/dev/tcp/127.0.0.1/8081 && printf "GET /readyz HTTP/1.0\r\n\r\n" >&3 && grep -q "200 OK" <&3' 2>/dev/null
}

log "starting kine-fdb with TLS parameters"
start_kine "${kine}" "fdb:///etc/foundationdb/fdb.cluster?namespace=/tls/&tls_cert_file=/tls/tls.crt&tls_key_file=/tls/tls.key&tls_ca_file=/tls/ca.crt"
for _ in $(seq 1 30); do ready "${kine}" && break; sleep 2; done
ready "${kine}" || { log "FAIL: kine-fdb with TLS did not become ready"; exit 1; }

etcdctl() { docker run --rm --network "${net}" "${etcd_image}" etcdctl --endpoints "http://${kine}:2379" "$@"; }
etcdctl put /tls/key over-tls >/dev/null
[[ "$(etcdctl get /tls/key --print-value-only)" == "over-tls" ]] || { log "FAIL: put/get through TLS did not round-trip"; exit 1; }
log "reads and writes over TLS work"

log "starting kine-fdb without TLS parameters (must not become ready)"
start_kine "${plain}" "fdb:///etc/foundationdb/fdb.cluster?namespace=/tls/&startup_timeout=15s&timeout=3s"
sleep 10
if ready "${plain}"; then
  log "FAIL: kine-fdb without TLS reached a TLS-only cluster"
  exit 1
fi
log "TLS smoke test PASSED"
