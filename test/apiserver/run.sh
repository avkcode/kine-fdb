#!/usr/bin/env bash
# Runs the Kubernetes apiserver etcd3 storage test suite against kine-fdb.
# It checks out the k8s.io/apiserver release that test/apiserver/go.mod
# requires, swaps the embedded etcd test server for test/apiserver/testserver,
# and runs the suite against the cluster in KINE_FDB_TEST_CLUSTER_FILE.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../.." && pwd)"
work="${APISERVER_TESTS_WORKDIR:-${repo}/_output/apiserver-tests}"
: "${KINE_FDB_TEST_CLUSTER_FILE:?set KINE_FDB_TEST_CLUSTER_FILE to a FoundationDB cluster file}"

cd "${here}"
go mod tidy >/dev/null
tag="$(go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' k8s.io/apiserver)"

src="${work}/apiserver"
if [[ -d "${src}/.git" ]] && [[ "$(git -C "${src}" describe --tags --exact-match 2>/dev/null || true)" == "${tag}" ]]; then
  git -C "${src}" checkout -q -- .
  git -C "${src}" clean -qfdx
else
  rm -rf "${src}"
  mkdir -p "${work}"
  git clone -q --depth 1 --branch "${tag}" https://github.com/kubernetes/apiserver "${src}"
fi

cd "${src}"
# Use the kine-fdb test server instead of embedded etcd.
grep -rlF 'k8s.io/apiserver/pkg/storage/etcd3/testserver' pkg/ \
  | xargs sed -i 's|k8s.io/apiserver/pkg/storage/etcd3/testserver|github.com/avkcode/kine-fdb/test/apiserver/testserver|'
# kine stores two revisions at startup (the health key is created and then
# updated), where embedded etcd has one; kine's own CI makes the same change.
sed -i 's|NewTooLargeResourceVersionError(uint64(102), 1, 0)|NewTooLargeResourceVersionError(uint64(102), 2, 0)|' pkg/storage/etcd3/watcher_test.go
# Give the compaction helper the same extra time kine's CI does.
sed -i 's|waitTimeout = 100 \* waitDelay|waitTimeout = 500 * waitDelay|' pkg/storage/etcd3/compact_test.go

go mod edit \
  -require "github.com/avkcode/kine-fdb/test/apiserver@v0.0.0" \
  -replace "github.com/avkcode/kine-fdb/test/apiserver=${here}" \
  -replace "github.com/avkcode/kine-fdb=${repo}"
go mod tidy >/dev/null

mkdir -p "${work}/bin"
go test -c -o "${work}/bin/etcd3.test" ./pkg/storage/etcd3/

cd "${work}"
args=(-test.count 1 -test.parallel 1 -test.timeout "${APISERVER_TESTS_TIMEOUT:-45m}")
if [[ -n "${APISERVER_TESTS_RUN:-}" ]]; then
  args+=(-test.run "${APISERVER_TESTS_RUN}")
fi
if [[ "${APISERVER_TESTS_VERBOSE:-0}" == "1" ]]; then
  args+=(-test.v)
fi
exec "${work}/bin/etcd3.test" "${args[@]}"
