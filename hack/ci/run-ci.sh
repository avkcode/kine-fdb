#!/usr/bin/env bash
# Runs kine-fdb CI stages in Docker: a throwaway FoundationDB and Go build
# container for lint, tests, and the apiserver storage suite, then the image
# build and the K3s soak test. Bind mounts are avoided, so it also works on
# CI agents that talk to the host's Docker daemon (for example Jenkins in a
# container). Used by the Jenkinsfile and the Jenkins freestyle job.
#
#   hack/ci/run-ci.sh [all|env|lint|test|apiserver|image|soak|cleanup]...
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${repo_root}"

id="${CI_ID:-ci-${BUILD_NUMBER:-local}}"
image="kine-fdb:${id}"
soak="${SOAK_SECONDS:-600}"
export DEVENV_PREFIX="kinefdb-${id}"
export DEVENV_CACHE_PREFIX="${DEVENV_CACHE_PREFIX:-kinefdb-ci}"
export DEVENV_COPY_SOURCE=1
devenv=./hack/dev/devenv.sh

step() { printf '\n==== %s ====\n' "$*"; }

run_stage() {
  case "$1" in
    env)
      step "environment"
      "${devenv}" up
      ;;
    lint)
      step "lint"
      "${devenv}" sh 'make lint'
      ;;
    test)
      step "unit, integration, and race tests"
      "${devenv}" sh 'go test -race -count=1 -timeout 30m ./...'
      ;;
    apiserver)
      step "Kubernetes apiserver storage tests"
      "${devenv}" sh './test/apiserver/run.sh'
      ;;
    image)
      step "image"
      docker build --build-arg VERSION="${id}" \
        --build-arg COMMIT="$(git rev-parse --short=12 HEAD 2>/dev/null || true)" -t "${image}" .
      docker run --rm "${image}" --version
      ;;
    soak)
      if [[ "${soak}" == "0" ]]; then
        echo "soak test skipped"
        return 0
      fi
      step "K3s soak test (${soak}s)"
      KINE_FDB_IMAGE="${image}" SOAK_SECONDS="${soak}" E2E_ID="${id}" \
        E2E_OUT="${repo_root}/_output/e2e" ./hack/e2e/k3s-soak.sh
      ;;
    cleanup)
      step "cleanup"
      "${devenv}" down || true
      docker image rm -f "${image}" >/dev/null 2>&1 || true
      ;;
    all)
      trap 'run_stage cleanup' EXIT
      for s in env lint test apiserver image soak; do
        run_stage "${s}"
      done
      ;;
    *)
      echo "unknown stage $1" >&2
      return 2
      ;;
  esac
}

if [[ $# -eq 0 ]]; then
  set -- all
fi
for stage in "$@"; do
  run_stage "${stage}"
done
