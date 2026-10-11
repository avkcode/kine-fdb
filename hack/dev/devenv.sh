#!/usr/bin/env bash
# Manages a disposable Docker-based development environment for kine-fdb:
# a single-process FoundationDB server plus a Go build/test container that has
# the FoundationDB client library installed. Nothing is published on host
# ports. Set DEVENV_COPY_SOURCE=1 where bind mounts do not work (for example a
# CI agent talking to the host's Docker daemon); the tree is then copied in.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

FDB_VERSION="${FDB_VERSION:-7.3.77}"
FDB_IMAGE="${FDB_IMAGE:-foundationdb/foundationdb:${FDB_VERSION}}"
DEV_IMAGE="${DEV_IMAGE:-kine-fdb-dev:${FDB_VERSION}}"
PREFIX="${DEVENV_PREFIX:-kinefdb}"
NETWORK="${DEVENV_NETWORK:-${PREFIX}-dev}"
FDB_CONTAINER="${DEVENV_FDB_CONTAINER:-${PREFIX}-fdb}"
DEV_CONTAINER="${DEVENV_DEV_CONTAINER:-${PREFIX}-dev}"
STORAGE_ENGINE="${DEVENV_STORAGE_ENGINE:-ssd}"
COPY_SOURCE="${DEVENV_COPY_SOURCE:-0}"
# Go module and build caches can be shared between environments.
CACHE_PREFIX="${DEVENV_CACHE_PREFIX:-${PREFIX}}"

log() { printf '[devenv] %s\n' "$*" >&2; }

ensure_network() {
  if ! docker network inspect "${NETWORK}" >/dev/null 2>&1; then
    log "creating network ${NETWORK}"
    docker network create "${NETWORK}" >/dev/null
  fi
}

fdb_cli() {
  docker exec "${FDB_CONTAINER}" fdbcli -C /var/fdb/fdb.cluster --timeout 10 --exec "$1"
}

start_fdb() {
  if ! docker container inspect "${FDB_CONTAINER}" >/dev/null 2>&1; then
    log "starting FoundationDB ${FDB_VERSION}"
    docker run -d --name "${FDB_CONTAINER}" --network "${NETWORK}" \
      -e FDB_NETWORKING_MODE=container \
      -v "${FDB_CONTAINER}-data:/var/fdb/data" \
      "${FDB_IMAGE}" >/dev/null
  else
    docker start "${FDB_CONTAINER}" >/dev/null
  fi

  for _ in $(seq 1 60); do
    if docker exec "${FDB_CONTAINER}" test -s /var/fdb/fdb.cluster 2>/dev/null; then
      break
    fi
    sleep 1
  done

  # A fresh cluster has no database yet: "status minimal" either says so or
  # times out, so create one whenever it is not available.
  local status
  for _ in $(seq 1 30); do
    status="$(fdb_cli 'status minimal' 2>&1 || true)"
    if [[ "${status}" == *"The database is available"* ]]; then
      log "FoundationDB is available"
      return 0
    fi
    fdb_cli "configure new single ${STORAGE_ENGINE}" >/dev/null 2>&1 || true
    sleep 2
  done
  log "FoundationDB did not become available: ${status}"
  return 1
}

cluster_string() {
  docker exec "${FDB_CONTAINER}" cat /var/fdb/fdb.cluster
}

build_dev_image() {
  if [[ "${REBUILD:-0}" == "1" ]] || ! docker image inspect "${DEV_IMAGE}" >/dev/null 2>&1; then
    log "building ${DEV_IMAGE}"
    docker build --build-arg FDB_VERSION="${FDB_VERSION}" -t "${DEV_IMAGE}" - <"${repo_root}/hack/dev/Dockerfile"
  fi
}

copy_source() {
  log "copying the source tree into ${DEV_CONTAINER}"
  docker exec "${DEV_CONTAINER}" mkdir -p /src
  tar -C "${repo_root}" --exclude ./_output --exclude ./bin -cf - . | docker exec -i "${DEV_CONTAINER}" tar -C /src -xf -
}

start_dev() {
  if ! docker container inspect "${DEV_CONTAINER}" >/dev/null 2>&1; then
    log "starting ${DEV_CONTAINER}"
    local source=(-v "${repo_root}:/src")
    if [[ "${COPY_SOURCE}" == "1" ]]; then
      source=()
    fi
    docker run -d --name "${DEV_CONTAINER}" --network "${NETWORK}" \
      "${source[@]}" \
      -v "${CACHE_PREFIX}-gocache:/root/.cache" \
      -v "${CACHE_PREFIX}-gomod:/go/pkg/mod" \
      -e FDB_CLUSTER_FILE=/etc/foundationdb/fdb.cluster \
      -e KINE_FDB_TEST_CLUSTER_FILE=/etc/foundationdb/fdb.cluster \
      -w /src \
      "${DEV_IMAGE}" sleep infinity >/dev/null
  else
    docker start "${DEV_CONTAINER}" >/dev/null
  fi
  if [[ "${COPY_SOURCE}" == "1" ]]; then
    copy_source
  fi
  cluster_string | docker exec -i "${DEV_CONTAINER}" sh -c 'mkdir -p /etc/foundationdb && cat > /etc/foundationdb/fdb.cluster'
}

cmd="${1:-help}"
shift || true
case "${cmd}" in
  up)
    ensure_network
    start_fdb
    build_dev_image
    start_dev
    log "cluster file: $(cluster_string)"
    ;;
  sync)
    copy_source
    ;;
  exec)
    docker exec -i "${DEV_CONTAINER}" "$@"
    ;;
  sh)
    docker exec -i "${DEV_CONTAINER}" bash -c "$*"
    ;;
  fdbcli)
    fdb_cli "$*"
    ;;
  cluster-string)
    cluster_string
    ;;
  fetch)
    # Copy a path from the dev container's /src back to the host tree.
    docker cp "${DEV_CONTAINER}:/src/$1" "${repo_root}/$(dirname "$1")/"
    ;;
  restart-fdb)
    docker restart "${FDB_CONTAINER}" >/dev/null
    start_fdb
    ;;
  stop-fdb)
    docker stop "${FDB_CONTAINER}" >/dev/null
    ;;
  start-fdb)
    start_fdb
    ;;
  down)
    docker rm -f -v "${DEV_CONTAINER}" "${FDB_CONTAINER}" >/dev/null 2>&1 || true
    docker volume rm -f "${FDB_CONTAINER}-data" >/dev/null 2>&1 || true
    docker network rm "${NETWORK}" >/dev/null 2>&1 || true
    if [[ "${PURGE:-0}" == "1" ]]; then
      docker volume rm -f "${CACHE_PREFIX}-gocache" "${CACHE_PREFIX}-gomod" >/dev/null 2>&1 || true
    fi
    ;;
  *)
    cat >&2 <<EOF
usage: $(basename "$0") <command>

commands:
  up              start FoundationDB and the dev container (builds the image once)
  sync            copy the source tree into the dev container (DEVENV_COPY_SOURCE=1)
  exec CMD...     run a command inside the dev container (cwd /src)
  sh 'SCRIPT'     run a bash script inside the dev container
  fetch PATH      copy PATH (relative to the repo) out of the dev container
  fdbcli 'CMD'    run an fdbcli command against the dev cluster
  cluster-string  print the cluster file of the dev cluster
  restart-fdb     restart the FoundationDB container
  stop-fdb        stop the FoundationDB container (outage testing)
  start-fdb       start it again
  down            remove containers, data, and network (PURGE=1 also drops Go caches)
EOF
    exit 2
    ;;
esac
