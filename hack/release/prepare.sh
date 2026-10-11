#!/usr/bin/env bash
# Prepares a release on devel: moves the CHANGELOG's Unreleased entries under
# the new version and points the Helm chart, the Docker Compose quick start,
# and the README at the new version. Review the result with git diff, then
# follow docs/releasing.md.
#
#   hack/release/prepare.sh 0.2.0
set -euo pipefail

die() { echo "prepare: $*" >&2; exit 1; }

version="${1:-}"
version="${version#v}"
[[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "usage: $0 X.Y.Z"

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

previous="$(awk '$1 == "version:" {print $2; exit}' charts/kine-fdb/Chart.yaml)"
[[ -n "${previous}" ]] || die "no version in charts/kine-fdb/Chart.yaml"
[[ "${previous}" != "${version}" ]] || die "the chart is already at ${version}"
grep -q '^## \[Unreleased\]' CHANGELOG.md || die "CHANGELOG.md has no Unreleased section"
! grep -q "^## \[${version}\]" CHANGELOG.md || die "CHANGELOG.md already has a ${version} section"

# The Unreleased section must say what changed.
entries="$(awk '/^## \[Unreleased\]/ {on=1; next} /^## \[/ {on=0} on && /^- /' CHANGELOG.md)"
[[ -n "${entries}" ]] || die "the Unreleased section of CHANGELOG.md is empty"

# rewrite FILE AWK-PROGRAM [awk args...] edits FILE in place, portably.
rewrite() {
  local file="$1" tmp
  shift
  tmp="$(mktemp)"
  awk "$@" "${file}" >"${tmp}"
  cat "${tmp}" >"${file}"
  rm -f "${tmp}"
}

url="https://github.com/avkcode/kine-fdb"
rewrite CHANGELOG.md -v v="${version}" -v p="${previous}" -v d="$(date -u +%Y-%m-%d)" -v url="${url}" '
  /^## \[Unreleased\]/ { print; print ""; print "## [" v "] - " d; next }
  /^\[Unreleased\]: / {
    print "[Unreleased]: " url "/compare/v" v "...HEAD"
    print "[" v "]: " url "/compare/v" p "...v" v
    next
  }
  { print }'

rewrite charts/kine-fdb/Chart.yaml -v v="${version}" '
  $1 == "version:" { print "version: " v; next }
  $1 == "appVersion:" { print "appVersion: \"" v "\""; next }
  { print }'

for f in README.md deploy/compose/docker-compose.yml hack/e2e/helm-smoke.sh; do
  rewrite "${f}" -v v="${version}" -v p="${previous}" '
    BEGIN { gsub(/\./, "\\.", p) }
    {
      gsub("kine-fdb:" p, "kine-fdb:" v)
      gsub("HELM_CHART_VERSION=" p, "HELM_CHART_VERSION=" v)
      print
    }'
done

echo "Prepared ${version} (previous release ${previous}). Review the changes:"
git --no-pager diff --stat
