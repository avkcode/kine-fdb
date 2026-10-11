# Releasing

Releases follow [semantic versioning](https://semver.org/) and are cut from
`main` by pushing a `vX.Y.Z` tag. The release workflow
(`.github/workflows/release.yml`) does the rest.

## What a release publishes

- the image `ghcr.io/avkcode/kine-fdb:X.Y.Z` for linux/amd64 and
  linux/arm64, also tagged `latest`;
- the Helm chart `oci://ghcr.io/avkcode/charts/kine-fdb`, version `X.Y.Z`;
- a GitHub release with the version's CHANGELOG section, `kine-fdb` and
  `kine-fdb-admin` archives for both architectures, the packaged chart, and
  `SHA256SUMS`;
- the Go module version, which proxy.golang.org and pkg.go.dev pick up from
  the tag.

Before it builds anything, the workflow runs `hack/release/check.sh`. That
check rejects a tag in three cases: it is not on `main`, the CHANGELOG has
no section for its version, or the chart is at a different version.

## Steps

1. On `devel`, once everything for the release is merged and listed under
   "Unreleased" in the CHANGELOG, prepare the release:

   ```bash
   git switch devel && git pull --ff-only
   hack/release/prepare.sh X.Y.Z
   git diff
   git commit -am "Prepare the X.Y.Z release"
   git push origin devel
   ```

   `prepare.sh` makes these changes, which `git diff` lets you review:

   - moves the Unreleased entries under a dated `X.Y.Z` heading;
   - sets the chart's `version` and `appVersion`;
   - points the README, the Compose file, and the Helm smoke test example
     at the new image tag.

2. Wait for CI on `devel` to pass. If you run the Jenkins job, its longer
   soak is worth waiting for too (see
   [testing](testing.md#continuous-integration)).

3. Fast-forward `main` to `devel`, then tag and push it:

   ```bash
   git switch main && git pull --ff-only
   git merge --ff-only devel
   git push origin main
   git tag -a vX.Y.Z -m "kine-fdb X.Y.Z"
   git push origin vX.Y.Z
   git switch devel
   ```

4. When the Release workflow has finished, check what it published:

   ```bash
   docker buildx imagetools inspect ghcr.io/avkcode/kine-fdb:X.Y.Z
   helm show chart oci://ghcr.io/avkcode/charts/kine-fdb --version X.Y.Z
   HELM_CHART=oci://ghcr.io/avkcode/charts/kine-fdb HELM_CHART_VERSION=X.Y.Z \
     KINE_FDB_IMAGE=ghcr.io/avkcode/kine-fdb:X.Y.Z IMPORT_IMAGES=0 \
     ./hack/e2e/helm-smoke.sh
   ```

If the check job fails, nothing has been published yet:

- If `main` simply had not been pushed, push it and re-run the workflow.
- Otherwise fix the cause on `devel`, then delete the tag and create it
  again.

Once a version's image or chart is published, do not move its tag. Release
the next patch version instead.

## Pre-releases

You can push a tag with a pre-release suffix, such as `v0.2.0-rc.1`, from
any branch. Use one to try the release pipeline or to share a candidate
build. Such a tag:

- skips the checks;
- publishes the image and chart under that version without moving
  `latest`;
- creates a GitHub pre-release whose notes are the current Unreleased
  section.
