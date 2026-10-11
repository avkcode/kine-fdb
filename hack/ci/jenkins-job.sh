#!/usr/bin/env bash
# Creates or updates the kine-fdb CI job on a Jenkins controller with the jc
# CLI. The job is a core freestyle job that clones the repository and runs
# hack/ci/run-ci.sh, so it needs no Pipeline or Git plugins; controllers with
# Pipeline can use the Jenkinsfile instead.
#
#   KINE_FDB_REPO=https://github.com/avkcode/kine-fdb.git hack/ci/jenkins-job.sh [job-name]
#
# JENKINS_LABEL pins the job to agents with that label (for example
# built-in); the agent needs git, bash, and access to a Docker daemon.
set -euo pipefail

job="${1:-kine-fdb}"
repo="${KINE_FDB_REPO:-https://github.com/avkcode/kine-fdb.git}"
branch="${KINE_FDB_BRANCH:-devel}"
soak="${SOAK_SECONDS:-600}"
label="${JENKINS_LABEL:-}"

xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'; }

command=$(xml_escape <<'SCRIPT'
set -eu
rm -rf src
git clone --depth 1 --branch "$KINE_FDB_BRANCH" "$KINE_FDB_REPO" src
git -C src log -1 --oneline
cd src
./hack/ci/run-ci.sh all
SCRIPT
)

param() {
  printf '<hudson.model.StringParameterDefinition><name>%s</name><description>%s</description><defaultValue>%s</defaultValue><trim>true</trim></hudson.model.StringParameterDefinition>' \
    "$1" "$(printf '%s' "$2" | xml_escape)" "$(printf '%s' "$3" | xml_escape)"
}

config="$(mktemp)"
trap 'rm -f "${config}"' EXIT
cat >"${config}" <<XML
<?xml version='1.1' encoding='UTF-8'?>
<project>
  <description>kine-fdb CI: lint, unit/integration/race tests, Kubernetes apiserver storage tests, image build, and K3s soak test, all in Docker.</description>
  <keepDependencies>false</keepDependencies>
  <properties>
    <jenkins.model.BuildDiscarderProperty>
      <strategy class="hudson.tasks.LogRotator">
        <daysToKeep>-1</daysToKeep>
        <numToKeep>30</numToKeep>
        <artifactDaysToKeep>-1</artifactDaysToKeep>
        <artifactNumToKeep>-1</artifactNumToKeep>
      </strategy>
    </jenkins.model.BuildDiscarderProperty>
    <hudson.model.ParametersDefinitionProperty>
      <parameterDefinitions>
        $(param KINE_FDB_REPO "Repository to clone" "${repo}")
        $(param KINE_FDB_BRANCH "Branch or tag to build" "${branch}")
        $(param SOAK_SECONDS "K3s soak test duration in seconds (0 skips it)" "${soak}")
      </parameterDefinitions>
    </hudson.model.ParametersDefinitionProperty>
  </properties>
  <scm class="hudson.scm.NullSCM"/>
$(if [[ -n "${label}" ]]; then printf '  <assignedNode>%s</assignedNode>\n  <canRoam>false</canRoam>' "$(printf '%s' "${label}" | xml_escape)"; else printf '  <canRoam>true</canRoam>'; fi)
  <disabled>false</disabled>
  <blockBuildWhenDownstreamBuilding>false</blockBuildWhenDownstreamBuilding>
  <blockBuildWhenUpstreamBuilding>false</blockBuildWhenUpstreamBuilding>
  <triggers/>
  <concurrentBuild>false</concurrentBuild>
  <builders>
    <hudson.tasks.Shell>
      <command>${command}</command>
      <configuredLocalRules/>
    </hudson.tasks.Shell>
  </builders>
  <publishers>
    <hudson.tasks.ArtifactArchiver>
      <artifacts>src/_output/e2e/**</artifacts>
      <allowEmptyArchive>true</allowEmptyArchive>
      <onlyIfSuccessful>false</onlyIfSuccessful>
      <fingerprint>false</fingerprint>
      <defaultExcludes>true</defaultExcludes>
      <caseSensitive>true</caseSensitive>
      <followSymlinks>false</followSymlinks>
    </hudson.tasks.ArtifactArchiver>
  </publishers>
  <buildWrappers/>
</project>
XML

jc job apply "${job}" -f "${config}"
