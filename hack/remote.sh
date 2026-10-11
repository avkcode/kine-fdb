#!/usr/bin/env bash
# Mirrors the working tree to a remote Linux host and runs commands there.
# The host and directory come from the untracked .env file (see .env.example).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -f "${repo_root}/.env" ]]; then
  # shellcheck disable=SC1091
  set -a && source "${repo_root}/.env" && set +a
fi

: "${REMOTE_HOST:?set REMOTE_HOST in .env, for example REMOTE_HOST=user@build-host}"
REMOTE_DIR="${REMOTE_DIR:-~/kine-fdb}"

sync_tree() {
  ssh -o BatchMode=yes "${REMOTE_HOST}" "mkdir -p ${REMOTE_DIR}"
  rsync -az --no-owner --no-group --delete \
    --exclude '/bin/' --exclude '/dist/' --exclude '/_output/' \
    --exclude '.env' --exclude '*.test' --exclude 'coverage*' \
    "${repo_root}/" "${REMOTE_HOST}:${REMOTE_DIR}/"
}

cmd="${1:-help}"
shift || true
case "${cmd}" in
  sync)
    sync_tree
    ;;
  run)
    sync_tree
    ssh -o BatchMode=yes "${REMOTE_HOST}" "cd ${REMOTE_DIR} && $*"
    ;;
  dev)
    # Run a script inside the remote dev container after syncing.
    sync_tree
    ssh -o BatchMode=yes "${REMOTE_HOST}" "cd ${REMOTE_DIR} && ./hack/dev/devenv.sh sh $(printf '%q' "$*")"
    ;;
  *)
    cat >&2 <<EOF
usage: $(basename "$0") <sync|run CMD|dev 'SCRIPT'>

  sync         rsync the working tree to \$REMOTE_HOST:\$REMOTE_DIR
  run CMD      sync, then run CMD in \$REMOTE_DIR on the remote host
  dev SCRIPT   sync, then run SCRIPT inside the remote dev container
EOF
    exit 2
    ;;
esac
