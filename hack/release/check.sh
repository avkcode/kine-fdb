#!/usr/bin/env bash
# Checks that a release tag is ready to publish: the CHANGELOG has a section
# for it, the Helm chart carries its version, and the tagged commit is on
# main. Pre-release tags (v0.2.0-rc.1) skip the checks. The release workflow
# runs this before it builds anything.
#
#   hack/release/check.sh v0.2.0 [commit]
set -euo pipefail

tag="${1:?usage: $0 TAG [COMMIT]}"
commit="${2:-HEAD}"
version="${tag#v}"

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

if [[ "${version}" == *-* ]]; then
  echo "${tag} is a pre-release; skipping the release checks"
  exit 0
fi

failed=0
fail() { echo "::error::$*" >&2; failed=1; }

grep -q "^## \[${version}\] - " CHANGELOG.md ||
  fail "CHANGELOG.md has no section for ${version}"
grep -qx "version: ${version}" charts/kine-fdb/Chart.yaml ||
  fail "charts/kine-fdb/Chart.yaml is not at version ${version}"
grep -qx "appVersion: \"${version}\"" charts/kine-fdb/Chart.yaml ||
  fail "charts/kine-fdb/Chart.yaml has another appVersion than ${version}"
git merge-base --is-ancestor "${commit}" origin/main ||
  fail "${tag} is not on main"

exit "${failed}"
