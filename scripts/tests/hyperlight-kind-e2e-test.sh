#!/usr/bin/env bash
set -Eeuo pipefail

# All Docker/kind/kubectl/go/curl calls below are mocked. No live cluster,
# credentials, host device, or global kubeconfig is used by this test.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/hyperlight-kind-e2e.sh
. "${root}/scripts/hyperlight-kind-e2e.sh"
for gid in 1 108 2147483647; do hyperlight_device_gid_valid "${gid}"; done
for gid in 0 -1 2147483648 999999999999999999999999 01 abc ''; do
  if hyperlight_device_gid_valid "${gid}"; then
    echo 'invalid device GID accepted' >&2; exit 1
  fi
done
if (uname() { echo Darwin; }; hyperlight_require_host) >/dev/null 2>&1; then
  echo 'non-Linux host was accepted' >&2; exit 1
fi

scratch="$(mktemp -d)"
trap 'rm -rf -- "${scratch}"' EXIT
mirror="${scratch}/mirror"
mkdir -p "${mirror}/scripts/lib" "${mirror}/.agents/skills/kindctl/bin" "${mirror}/test/e2e" "${scratch}/mock-bin"
cp "${root}/scripts/hyperlight-kind-e2e.sh" "${mirror}/scripts/"
cp "${root}/scripts/lib/e2e-cleanup.sh" "${mirror}/scripts/lib/"
cp "${root}/.agents/skills/kindctl/bin/kindctl" "${mirror}/.agents/skills/kindctl/bin/"
cp "${root}/test/e2e/hyperlight-kind-config.yaml" "${mirror}/test/e2e/"

# Tiny substitute download for testing manifest rendering, not a device allocator.
mkdir -p "${scratch}/upstream/source/device-plugin" "${scratch}/upstream/source/deploy/manifests"
cat > "${scratch}/upstream/source/deploy/manifests/device-plugin.yaml" <<'YAML'
image: ${IMAGE}
imagePullPolicy: Always
count: "${DEVICE_COUNT}"
uid: "${DEVICE_UID}"
gid: "${DEVICE_GID}"
YAML
tar -czf "${scratch}/upstream.tar.gz" -C "${scratch}/upstream" source

cat > "${scratch}/mock-bin/kind" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'kind %s\n' "$*" >> "${TEST_STATE}/calls"
case "$1 $2" in
  'get clusters')
    # Represents the user's existing VM cluster; it must never be deleted.
    echo hl-orka-e64307-hyperlight
    [[ ! -f "${TEST_STATE}/cluster" ]] || cat "${TEST_STATE}/cluster"
    if [[ "${SCENARIO}" == preexisting ]]; then echo "${TEST_CLUSTER}"; fi
    ;;
  'create cluster')
    [[ "$3" == --name && "$5" == --kubeconfig && "$7" == --config ]]
    printf '%s\n' "$4" > "${TEST_STATE}/cluster"
    printf 'scoped\n' > "$6"
    [[ "${SCENARIO}" != create-fail ]] || exit 18
    ;;
  'delete cluster')
    [[ "$3" == --name && "$4" == "${TEST_CLUSTER}" && "$5" == --kubeconfig ]]
    [[ "$6" == "${HOME}/.kube/kind/${TEST_CLUSTER}.kubeconfig" ]]
    [[ "${SCENARIO}" != delete-fail ]] || exit 19
    rm -f "${TEST_STATE}/cluster"
    ;;
  'load docker-image') [[ "$4" == --name && "$5" == "${TEST_CLUSTER}" ]] ;;
  *) echo 'unexpected kind command' >&2; exit 1 ;;
esac
MOCK
cat > "${scratch}/mock-bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'docker %s\n' "$*" >> "${TEST_STATE}/calls"
case "$1" in
  info) ;;
  build) [[ "${SCENARIO}" != build-fail ]] || exit 17 ;;
  *) exit 1 ;;
esac
MOCK
cat > "${scratch}/mock-bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${KUBECONFIG}" == "${HOME}/.kube/kind/${TEST_CLUSTER}.kubeconfig" ]]
printf 'kubectl %s\n' "$*" >> "${TEST_STATE}/calls"
case "$1 $2" in
  'apply -f')
    grep -Fq 'terminationMessagePath: /tmp/hyperlight-termination.log' "$3"
    cp "$3" "${TEST_STATE}/rendered.yaml"
    ;;
  'rollout status') [[ "${SCENARIO}" != rollout-fail ]] || exit 16 ;;
  'get nodes')
    count=16
    [[ "${SCENARIO}" != allocatable-fail ]] || count=0
    printf '{"items":[{"metadata":{"name":"node","annotations":{"token":"DO_NOT_LEAK"}},"status":{"allocatable":{"hyperlight.dev/hypervisor":"%s"},"conditions":[{"type":"Ready","status":"True","message":"DO_NOT_LEAK"}]}}]}\n' "${count}"
    ;;
  'get pods') echo '{"items":[{"metadata":{"name":"pod","namespace":"ns"},"spec":{"env":"DO_NOT_LEAK"},"status":{"phase":"Running"}}]}' ;;
  'wait --for=condition=Ready'|'label nodes') ;;
  *) exit 1 ;;
esac
MOCK
cat > "${scratch}/mock-bin/curl" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'curl %s\n' "$*" >> "${TEST_STATE}/calls"
[[ "$*" == *https://codeload.github.com/hyperlight-dev/hyperlight-on-kubernetes/tar.gz/fc71b4501d23977fcc54f7be144d884fc8210667* ]]
while [[ "$1" != -o ]]; do shift; done
cp "${TEST_ARCHIVE}" "$2"
MOCK
cat > "${scratch}/mock-bin/go" <<'MOCK'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'go %s\n' "$*" >> "${TEST_STATE}/calls"
[[ "$*" == 'test -tags=e2e ./test/e2e/ -timeout 40m -v -ginkgo.v -ginkgo.label-filter=hyperlight' ]]
[[ "${KIND_CLUSTER}" == "${TEST_CLUSTER}" && "${KIND}" == /*/kind ]]
[[ "${KUBECONFIG}" == "${HOME}/.kube/kind/${TEST_CLUSTER}.kubeconfig" ]]
[[ "${E2E_HYPERLIGHT}" == 1 && "${E2E_HYPERLIGHT_DEVICE_GID}" == 108 ]]
[[ "${E2E_DISABLE_MOCK_LLM}" == 0 && "${E2E_ACP_IDLE_POOL_TTL}" == 5m ]]
[[ "${E2E_HYPERLIGHT_BUNDLE_IMAGE}" == test-bundle:e2e ]]
[[ "${E2E_CLEANUP_REPORT_DIR}" == /* ]]
# go test runs its binary in the package directory, not necessarily repo_root.
cd "${HOME}"
if [[ "${SCENARIO}" != missing-evidence ]]; then
  echo '{"schemaVersion":1,"stage":"complete","passed":true}' > "${E2E_CLEANUP_REPORT_DIR}/suite-cleanup.json"
  absent=true
  [[ "${SCENARIO}" != incomplete-evidence ]] || absent=false
  printf '{"schemaVersion":1,"stage":"complete","passed":true,"tasks":[{"uid":"task-uid","deleteRequested":true,"productFinalizersReleased":true,"observerReleased":true,"absent":%s,"receiptRequired":true,"receiptVerified":true,"runtimeSessionCleanupDigest":"sha256:proof"}],"sessions":[{"absent":true}]}\n' "${absent}" > "${E2E_CLEANUP_REPORT_DIR}/cleanup-task.json"
fi
[[ "${SCENARIO}" != go-fail ]] || exit 23
MOCK
cat > "${scratch}/mock-bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod +x "${scratch}/mock-bin/"*

run_case() {
  local scenario="$1" expected="$2" status=0 case_dir
  case_dir="${scratch}/${scenario}"
  mkdir -p "${case_dir}/home/.kube"
  printf 'GLOBAL_CONFIG_DO_NOT_TOUCH\n' > "${case_dir}/home/.kube/config"
  local cluster artifact_parent="${case_dir}/artifacts"
  if [[ "${scenario}" == relative-artifacts ]]; then artifact_parent=../relative-artifacts/artifacts; fi
  cluster="$(HOME="${case_dir}/home" bash -c '
    . "$1/.agents/skills/kindctl/bin/kindctl"
    cd "$1"; kindctl_derive hyperlight-aimock; echo "$KINDCTL_NAME"
  ' _ "${mirror}")"
  if [[ "${scenario}" == env-file ]]; then printf 'OPENAI_API_KEY=DO_NOT_LEAK\n' > "${mirror}/test/e2e/.env"; fi
  # Another run of the same tag holds its lock, as if it had just passed the
  # preflight and not yet created its cluster.
  local held=''
  if [[ "${scenario}" == locked ]]; then
    mkdir -p "${case_dir}/home/.kube/kind"
    exec {held}>>"${case_dir}/home/.kube/kind/${cluster}.e2e.lock"
    flock -n "${held}"
  fi
  # shellcheck disable=SC2016 # The child shell expands its own variables.
  HOME="${case_dir}/home" KINDCTL_STORE="${case_dir}/home/.kube/kind" \
    PATH="${scratch}/mock-bin:${PATH}" \
    KIND="${scratch}/mock-bin/kind" TEST_CLUSTER="${cluster}" \
    TEST_STATE="${case_dir}" TEST_ARCHIVE="${scratch}/upstream.tar.gz" SCENARIO="${scenario}" \
    E2E_HYPERLIGHT_ARTIFACT_DIR="${artifact_parent}" E2E_HYPERLIGHT_BUNDLE_IMAGE=test-bundle:e2e \
    env -u E2E_OPENAI_API_KEY -u E2E_ANTHROPIC_API_KEY -u E2E_GITHUB_TOKEN \
      -u OPENAI_API_KEY -u ANTHROPIC_API_KEY -u COPILOT_GITHUB_TOKEN -u GH_TOKEN -u GITHUB_TOKEN \
      -u E2E_LIVE_COPILOT_PROXY_BASE_URL -u E2E_COPILOT_PROXY_BASE_URL \
      bash -c '
        . "$1/scripts/hyperlight-kind-e2e.sh"
        # The production host guard is not bypassable by an environment flag.
        hyperlight_require_host() { export E2E_HYPERLIGHT_DEVICE_GID=108; }
        if [[ "$SCENARIO" == credential ]]; then export E2E_OPENAI_API_KEY=DO_NOT_LEAK; fi
        hyperlight_main
      ' _ "${mirror}" >"${case_dir}/output" 2>&1 || status=$?
  rm -f "${mirror}/test/e2e/.env"
  if [[ -n "${held}" ]]; then exec {held}>&-; fi
  if [[ "${status}" != "${expected}" ]]; then
    echo "${scenario}: expected ${expected}, got ${status}" >&2
    cat "${case_dir}/output" >&2; exit 1
  fi
  [[ "$(cat "${case_dir}/home/.kube/config")" == GLOBAL_CONFIG_DO_NOT_TOUCH ]]
  if grep -R -F DO_NOT_LEAK "${case_dir}/output" "${case_dir}/artifacts" 2>/dev/null; then
    echo 'diagnostics exposed unallowlisted data' >&2; exit 1
  fi
  case "${scenario}" in
    credential|env-file|preexisting|locked)
      [[ ! -e "${case_dir}/artifacts" ]]
      if [[ "${scenario}" == locked ]] && ! grep -Fq 'another run holds this tag' "${case_dir}/output"; then
        echo 'a held tag did not stop the run' >&2; exit 1
      fi
      if grep -Eq 'kind (create|delete)' "${case_dir}/calls" 2>/dev/null; then
        echo 'preflight failure mutated a cluster' >&2; exit 1
      fi
      ;;
    *)
      local evidence
      evidence="$(find "${case_dir}/artifacts" -name kind-cleanup.json)"
      [[ -n "${evidence}" ]]
      if [[ "${scenario}" == delete-fail ]]; then
        jq -e '.passed == false and .absent == false' "${evidence}" >/dev/null
      else
        jq -e '.passed == true and .absent == true' "${evidence}" >/dev/null
        [[ ! -f "${case_dir}/cluster" && ! -f "${case_dir}/home/.kube/kind/${cluster}.kubeconfig" ]]
        jq -e '.clusters | length == 0' "${case_dir}/home/.kube/kind/registry.json" >/dev/null
      fi
      # Nothing targeted the existing user's other VM cluster.
      if grep -F 'delete cluster --name hl-orka-e64307-hyperlight' "${case_dir}/calls"; then
        echo 'cleanup targeted the existing VM cluster' >&2; exit 1
      fi
      ;;
  esac
  case "${scenario}" in
    success|relative-artifacts|go-fail|missing-evidence|incomplete-evidence|delete-fail)
      grep -F 'go test -tags=e2e' "${case_dir}/calls" >/dev/null
      grep -F 'image: orka-hyperlight-device-plugin:fc71b4501d23977fcc54f7be144d884fc8210667' "${case_dir}/rendered.yaml" >/dev/null
      for field in 'imagePullPolicy: Never' 'count: "16"' 'uid: "0"' 'gid: "108"'; do
        grep -Fx "${field}" "${case_dir}/rendered.yaml" >/dev/null
      done
      ;;
    *) if grep -F 'go test' "${case_dir}/calls" 2>/dev/null; then
         echo 'suite ran despite a failed bootstrap' >&2; exit 1
       fi ;;
  esac
  echo "PASS: ${scenario}"
}

run_case success 0
run_case relative-artifacts 0
run_case go-fail 23
run_case missing-evidence 90
run_case incomplete-evidence 90
run_case delete-fail 90
run_case create-fail 18
run_case build-fail 17
run_case rollout-fail 16
run_case allocatable-fail 1
run_case preexisting 1
run_case locked 1
run_case credential 1
run_case env-file 1

grep -F 'hostPath: /dev/kvm' "${mirror}/test/e2e/hyperlight-kind-config.yaml" >/dev/null
grep -F 'enable_cdi = true' "${mirror}/test/e2e/hyperlight-kind-config.yaml" >/dev/null
if grep -F extraPortMappings "${mirror}/test/e2e/hyperlight-kind-config.yaml"; then
  echo 'Hyperlight kind config must not bind host ports' >&2; exit 1
fi
echo 'Hyperlight kind bootstrap shell tests passed'
