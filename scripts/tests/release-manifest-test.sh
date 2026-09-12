#!/usr/bin/env bash

set -Eeuo pipefail

# scripts/tests suites rely on 'set -e' stopping on failed (( )) arithmetic,
# which macOS's stock bash 3.2 does not honor; failures would be silently
# masked there. Require a modern bash (for example: brew install bash).
if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  echo "error: this test suite requires bash >= 4; found ${BASH_VERSION}" >&2
  exit 1
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
updater="${root}/scripts/update-release-version.py"

for command in awk grep python3; do
  command -v "${command}" >/dev/null 2>&1 || {
    printf 'required command not found: %s\n' "${command}" >&2
    exit 1
  }
done

test_root="$(mktemp -d "${TMPDIR:-/tmp}/orka-release-manifest-test.XXXXXX")"
cleanup() {
  rm -rf -- "${test_root}"
}
trap cleanup EXIT

workflow_job() {
  local job_name="$1"
  awk -v header="  ${job_name}:" '
    $0 == header {
      in_job = 1
    }
    in_job && $0 != header && $0 ~ /^  [^[:space:]][^:]*:$/ {
      exit
    }
    in_job {
      print
    }
  ' "${root}/.github/workflows/release.yml"
}

mkdir -p \
  "${test_root}/scripts" \
  "${test_root}/cmd/build/helmify/static" \
  "${test_root}/config/manager"
cp "${updater}" "${test_root}/scripts/update-release-version.py"

cat >"${test_root}/Makefile" <<'EOF_MAKEFILE'
VERSION := v0.0.1
EOF_MAKEFILE

cat >"${test_root}/cmd/build/helmify/static/Chart.yaml" <<'EOF_CHART'
apiVersion: v2
name: orka
version: 0.0.1
appVersion: "v0.0.1"
EOF_CHART

cat >"${test_root}/cmd/build/helmify/static/values.yaml" <<'EOF_VALUES'
controller:
  image:
    repository: ghcr.io/orka-agents/orka
    tag: "0.0.1"
publisher:
  image:
    repository: ghcr.io/orka-agents/orka/workspace-publisher
    tag: "0.0.1"
workers:
  ai:
    image:
      repository: ghcr.io/orka-agents/orka/ai-worker
      tag: "0.0.1"
  general:
    image:
      repository: ghcr.io/orka-agents/orka/general-worker
      tag: "0.0.1"
EOF_VALUES

cat >"${test_root}/config/manager/manager.yaml" <<'EOF_MANAGER'
args:
  - --ai-worker-image=ghcr.io/orka-agents/orka/ai-worker:0.0.1
  - --general-worker-image=ghcr.io/orka-agents/orka/general-worker:0.0.1
EOF_MANAGER

cat >"${test_root}/config/manager/kustomization.yaml" <<'EOF_KUSTOMIZATION'
images:
  - name: ghcr.io/orka-agents/orka
    newTag: 0.0.1
  - name: controller
    newTag: 0.0.1
EOF_KUSTOMIZATION

python3 "${test_root}/scripts/update-release-version.py" v9.8.7-rc.3 >/dev/null

grep -Fx 'VERSION := v9.8.7-rc.3' "${test_root}/Makefile" >/dev/null
grep -Fx 'version: 9.8.7-rc.3' "${test_root}/cmd/build/helmify/static/Chart.yaml" >/dev/null
grep -Fx 'appVersion: "v9.8.7-rc.3"' "${test_root}/cmd/build/helmify/static/Chart.yaml" >/dev/null
test "$(grep -Fc 'tag: "9.8.7-rc.3"' "${test_root}/cmd/build/helmify/static/values.yaml")" -eq 4
grep -Fq 'ghcr.io/orka-agents/orka/ai-worker:9.8.7-rc.3' "${test_root}/config/manager/manager.yaml"
grep -Fq 'ghcr.io/orka-agents/orka/general-worker:9.8.7-rc.3' "${test_root}/config/manager/manager.yaml"
test "$(grep -Fc 'newTag: 9.8.7-rc.3' "${test_root}/config/manager/kustomization.yaml")" -eq 2

if python3 "${test_root}/scripts/update-release-version.py" 9.8.7 >/dev/null 2>&1; then
  echo 'release updater accepted a tag without the required v prefix' >&2
  exit 1
fi

grep -Fq 'run: scripts/validate-release-manifest.sh "${GITHUB_REF_NAME}"' \
  "${root}/.github/workflows/release.yml"
grep -Fq 'make verify-release-manifest NEWVERSION="${NEWVERSION}"' \
  "${root}/.github/workflows/release-pr.yml"
for job in build-and-push scan sign-and-attest promote-release-tags; do
  rendered_job="$(workflow_job "${job}")"
  if grep -Eq 'agent-harness-wrapper|workers/harness/Dockerfile' <<<"${rendered_job}"; then
    echo "release job ${job} still builds or publishes the removed wrapper" >&2
    exit 1
  fi
done
for target in docker-build-all docker-push-all; do
  rendered_target="$(make --no-print-directory -s -n -C "${root}" "${target}")"
  if grep -Eq 'agent-harness-wrapper|workers/harness/Dockerfile' <<<"${rendered_target}"; then
    echo "${target} still includes the removed wrapper" >&2
    exit 1
  fi
done

printf '%s\n' 'ok - release versioning and current image policy are coherent'
