#!/usr/bin/env bash
# Exercise cleanup and preflight without a cluster or provider credentials.
set -Eeuo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/orka-native-substrate-test.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT
source "${root}/scripts/agent-substrate-e2e.sh"

# Direct egress changes exactly the supported deployment argument, even when
# containers or arguments move. Ambiguous state and failed updates stop setup.
(
  deployment='{"metadata":{"uid":"api-uid","resourceVersion":"42"},"spec":{"template":{"spec":{"containers":[{"name":"sidecar","args":["--unrelated"]},{"name":"ate-api-server","args":["--unrelated","--egress-gateway-address=atenet-egress.ate-system.svc:443","--other"]}]}}}}'
  egress_failure=""
  kubectl() {
    case "$*" in
      '-n ate-system get deployment ate-api-server -o json')
        [[ "${egress_failure}" != read ]] || return 7
        printf '%s\n' "${deployment}"
        ;;
      '-n ate-system patch deployment ate-api-server --type=json -p '*)
        [[ "${egress_failure}" != patch ]] || return 7
        printf '%s\n' "${@: -1}" >"${test_root}/egress-patch.json"
        ;;
      '-n ate-system rollout status deployment/ate-api-server --timeout=5m')
        [[ "${egress_failure}" != rollout ]] || return 7
        ;;
      *) return 9 ;;
    esac
  }
  substrate_configure_direct_egress
  jq -e '. == [
    {op:"test",path:"/metadata/uid",value:"api-uid"},
    {op:"test",path:"/metadata/resourceVersion",value:"42"},
    {op:"replace",path:"/spec/template/spec/containers/1/args/1",value:"--egress-gateway-address="}
  ]' "${test_root}/egress-patch.json" >/dev/null
  for egress_failure in read patch rollout; do
    if substrate_configure_direct_egress >"${test_root}/egress-error" 2>&1; then
      echo 'Substrate setup ignored a failed direct egress configuration' >&2
      exit 1
    fi
  done
  egress_failure=""
  deployment="$(jq '.spec.template.spec.containers[1].args += ["--egress-gateway-address=another:443"]' <<<"${deployment}")"
  if substrate_configure_direct_egress >"${test_root}/egress-error" 2>&1; then
    echo 'Substrate setup accepted ambiguous egress configuration' >&2
    exit 1
  fi
)

# A failed API read is not proof of deletion.
kubectl() { return 7; }
if wait_absent task gone; then
  echo 'cleanup treated an API error as absence' >&2
  exit 1
fi
kubectl() { return 0; }
wait_absent task gone
unset -f kubectl

# Terminal failure must not wait out the success deadline. Unknown or unreadable
# status must not pass. These checks complete without sleeping or a cluster.
job_status='{"status":{"conditions":[{"type":"Complete","status":"True"}]}}'
kubectl() { printf '%s\n' "${job_status}"; }
wait_job complete 0
for condition in Failed FailureTarget; do
  job_status="{\"status\":{\"conditions\":[{\"type\":\"${condition}\",\"status\":\"True\"}]}}"
  if wait_job failed 600 2>"${test_root}/error"; then
    echo 'failed conformance job passed' >&2
    exit 1
  fi
  grep -Fq 'job/failed failed' "${test_root}/error"
done
job_status='{"status":{}}'
if wait_job incomplete 0 2>"${test_root}/error"; then
  echo 'incomplete conformance job passed' >&2
  exit 1
fi
grep -Fq 'Timed out' "${test_root}/error"
kubectl() { return 7; }
if wait_job unreadable 0; then
  echo 'unreadable conformance job passed' >&2
  exit 1
fi

# Job logs pass through redaction before cleanup prints them. Never dump a Pod
# spec, even when a container fails before it can produce logs.
saved_run_dir="${TMP_ROOT}"
TMP_ROOT="${test_root}/diagnostics"
mkdir -p "${TMP_ROOT}"
printf 'fixture-bootstrap-value\n' >"${TMP_ROOT}/bootstrap-token"
kubectl() {
  case "$*" in
    *'get pods'*) printf '{"items":[{"metadata":{"name":"failed-pod"},"spec":{"env":"spec-must-not-be-printed"},"status":{"phase":"Failed"}}]}\n' ;;
    *'get tools,substrateactorpools'*) printf '{"items":[{"kind":"SubstrateActorPool","metadata":{"name":"blocked-pool"},"spec":{"env":"spec-must-not-be-printed"},"status":{"lifecycle":"Degraded","message":"native MCP provisioning failed fixture-bootstrap-value","result":"result-must-not-be-printed"}}]}\n' ;;
    *'logs '*) printf 'native boot failed\nAuthorization: Bearer fixture-header-value\nfixture-bootstrap-value\n' ;;
    *) return 9 ;;
  esac
}
job_diagnostics failed >"${test_root}/diagnostics.log" 2>&1
runtime_diagnostics >>"${test_root}/diagnostics.log" 2>&1
grep -Fq 'native boot failed' "${test_root}/diagnostics.log"
grep -Fq 'failed-pod' "${test_root}/diagnostics.log"
grep -Fq 'blocked-pool' "${test_root}/diagnostics.log"
grep -Fq 'native MCP provisioning failed' "${test_root}/diagnostics.log"
if grep -Eq 'fixture-header-value|fixture-bootstrap-value|spec-must-not-be-printed|result-must-not-be-printed' "${test_root}/diagnostics.log"; then
  echo 'conformance diagnostics exposed credentials or Pod specs' >&2
  exit 1
fi
TMP_ROOT="${saved_run_dir}"
unset -f kubectl

mkdir -p "${test_root}/tools"
cat >"${test_root}/tools/docker" <<'SH'
#!/usr/bin/env bash
exit 1
SH
cat >"${test_root}/tools/git" <<'SH'
#!/usr/bin/env bash
printf 'unexpected provider access\n' >>"${ORKA_SUBSTRATE_TEST_CALLS}"
exit 99
SH
chmod +x "${test_root}/tools/docker" "${test_root}/tools/git"
export ORKA_SUBSTRATE_TEST_CALLS="${test_root}/unexpected-calls"
original_path="${PATH}"
export PATH="${test_root}/tools:${PATH}"
# Calling in a conditional disables errexit inside a Bash function. The
# preflight must still return failure before creating or installing anything.
if substrate_prepare_upstream "${root}" "${test_root}/run" guarded-cluster 2>"${test_root}/error"; then
  echo 'preflight ignored an unavailable Docker engine' >&2
  exit 1
fi
[[ ! -e "${ORKA_SUBSTRATE_TEST_CALLS}" ]]
[[ ! -e "${test_root}/run" ]]
grep -Fq 'Docker engine is unavailable' "${test_root}/error"
# A working Docker stub cannot authorize a provider fork or a movable ref.
printf '#!/usr/bin/env bash\nexit 0\n' >"${test_root}/tools/docker"
for selection in fork branch; do
  if [[ "${selection}" == fork ]]; then
    export SUBSTRATE_REPO=https://example.invalid/provider-fork.git
    unset SUBSTRATE_REF
  else
    unset SUBSTRATE_REPO
    export SUBSTRATE_REF=main
  fi
  if substrate_prepare_upstream "${root}" "${test_root}/run" guarded-cluster 2>"${test_root}/error"; then
    echo 'preflight accepted an unsupported provider source' >&2
    exit 1
  fi
  [[ ! -e "${ORKA_SUBSTRATE_TEST_CALLS}" ]]
  grep -Fq 'provider forks and patches are unsupported' "${test_root}/error"
done
unset SUBSTRATE_REPO SUBSTRATE_REF
export PATH="${original_path}"

# A reusable provider checkout must reject source files Git does not track,
# as well as staged and unstaged changes to tracked files.
provider_dir="${test_root}/provider"
git init -q "${provider_dir}"
printf 'package fixture\n' >"${provider_dir}/fixture.go"
git -C "${provider_dir}" add fixture.go
git -C "${provider_dir}" -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false commit -qs -m 'fixture'
substrate_require_clean_upstream "${provider_dir}"
printf 'package fixture\n' >"${provider_dir}/unexpected.go"
if substrate_require_clean_upstream "${provider_dir}" 2>"${test_root}/error"; then
  echo 'preflight accepted an untracked provider source file' >&2
  exit 1
fi
rm "${provider_dir}/unexpected.go"
printf '// changed\n' >>"${provider_dir}/fixture.go"
for state in unstaged staged; do
  if [[ "${state}" == staged ]]; then git -C "${provider_dir}" add fixture.go; fi
  if substrate_require_clean_upstream "${provider_dir}" 2>"${test_root}/error"; then
    echo "preflight accepted ${state} provider changes" >&2
    exit 1
  fi
done

source "${root}/hack/agent-substrate/upstream.env"
[[ "$(shasum -a 256 "${root}/internal/substratepb/ateapi.proto" | awk '{print $1}')" == "${SUBSTRATE_UPSTREAM_PROTO_SHA256}" ]]
printf 'Native Substrate source, preflight, and absence checks passed\n'
