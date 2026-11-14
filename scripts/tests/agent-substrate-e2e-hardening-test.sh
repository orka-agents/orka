#!/usr/bin/env bash
# Exercise cleanup and preflight without a cluster or provider credentials.
set -Eeuo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/orka-native-substrate-test.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT
source "${root}/scripts/agent-substrate-e2e.sh"

# The v1 control plane is installed by the pinned ate-setup. Only the shared
# test identity is a bootstrap owner, only Orka's controller may reach Actors
# through the router, and the egress gateway injects credentials only from the
# default-deny Kubernetes Secret provider. A failed install or rollout stops
# setup.
(
  SUBSTRATE_DIR="${test_root}/substrate-checkout"
  mkdir -p "${SUBSTRATE_DIR}"
  KIND_CLUSTER_NAME=fixture-cluster KUBECONFIG="${test_root}/kubeconfig" KUBECTL_CONTEXT=kind-fixture-cluster
  install_failure=""
  go() {
    [[ "${install_failure}" != install ]] || return 7
    printf '%s\n' "$@" >"${test_root}/ate-setup-args"
  }
  kubectl() {
    [[ "$*" == '-n ate-system rollout status deployment/ate-api-server --timeout=5m' ]] || return 9
    [[ "${install_failure}" != rollout ]] || return 7
  }
  substrate_install_ate_system
  grep -Fxq 'deploy' "${test_root}/ate-setup-args"
  grep -Fxq -- '--kind' "${test_root}/ate-setup-args"
  grep -Fxq -- '--credential-provider={"name":"k8s.io"}' "${test_root}/ate-setup-args"
  grep -Fxq -- '--ateapi-authz-bootstrap-owners=system:serviceaccount:ate-system:ate-client' "${test_root}/ate-setup-args"
  grep -Fxq -- '--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/orka-system/sa/orka-controller-manager' "${test_root}/ate-setup-args"
  if grep -q -- 'egress-gateway' "${test_root}/ate-setup-args"; then
    echo 'Substrate setup changed the egress gateway' >&2
    exit 1
  fi
  for install_failure in install rollout; do
    if substrate_install_ate_system >"${test_root}/install-error" 2>&1; then
      echo "Substrate setup ignored a failed ${install_failure}" >&2
      exit 1
    fi
  done
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
# The generated upstream credential is redacted like the bootstrap secret.
printf 'fixture-upstream-value' >"${TMP_ROOT}/upstream-token"
kubectl() { printf 'upstream said fixture-upstream-value\n'; }
workload_logs ate-demo -l app=credential-echo >"${test_root}/upstream.log" 2>&1
grep -Fq 'upstream said' "${test_root}/upstream.log"
if grep -Fq 'fixture-upstream-value' "${test_root}/upstream.log"; then
  echo 'conformance diagnostics exposed the upstream credential' >&2
  exit 1
fi
# The injected credential is compared only as a digest of "Bearer <token>".
[[ "$(expected_injected_digest)" == "$(printf 'Bearer fixture-upstream-value' | sha256sum | cut -d' ' -f1)" ]]
TMP_ROOT="${saved_run_dir}"
unset -f kubectl

# Tool calls go only through the orka-api proxy and carry a traceparent only
# when a trace is requested.
TOOL_CLIENT_IMAGE=registry.example.test/orka/tool-e2e-client@sha256:fixture
kubectl() {
  case "$*" in
    '-n orka-system apply -f -') cat >"${test_root}/client-job.json" ;;
    *) return 0 ;;
  esac
}
wait_job() { return 0; }
run_tool_client fixture-job native-mcp egress-probe 'mcp-e2e-egress:blocked'
jq -e '.spec.template.spec.containers[0].env | from_entries |
  .ORKA_CONTROLLER_URL == "http://orka-api.orka-system.svc:8080" and
  .ORKA_TOOL_NAME == "native-mcp" and .ORKA_TOOL_EXPECT_RESULT == "mcp-e2e-egress:blocked" and
  (.ORKA_TOOL_ARGS | fromjson) == {message: "egress-probe"} and has("ORKA_TOOL_TRACEPARENT") == false' \
  <(jq '.spec.template.spec.containers[0].env |= map({key: .name, value: .value}) | .' "${test_root}/client-job.json") >/dev/null
run_tool_client fixture-trace native-mcp trace-context 'mcp-e2e-trace:abc' '00-abc-def-01'
jq -e '[.spec.template.spec.containers[0].env[] | select(.name == "ORKA_TOOL_TRACEPARENT")][0].value == "00-abc-def-01"' \
  "${test_root}/client-job.json" >/dev/null
unset -f wait_job kubectl
source "${root}/scripts/agent-substrate-e2e.sh"

# Trace checks read the collector debug exporter. The router span must be the
# orka-api span's child in the caller's trace; any other parent fails.
trace_logs() {
  cat <<LOG
ResourceSpans #0
ScopeSpans #0
Span #0
    Trace ID       : 0123456789abcdef0123456789abcdef
    Parent ID      : 1111111111111111
    ID             : 2222222222222222
    Name           : POST /api/v1/tools/:name/mcp
Span #1
    Trace ID       : 0123456789abcdef0123456789abcdef
    Parent ID      : $1
    ID             : 3333333333333333
    Name           : substrate.router POST
Span #2
    Trace ID       : ffffffffffffffffffffffffffffffff
    Parent ID      :
    ID             : 4444444444444444
    Name           : substrate.router POST
LOG
}
trace_parent=2222222222222222
kubectl() { trace_logs "${trace_parent}"; }
collector_spans >"${test_root}/spans"
[[ "$(wc -l <"${test_root}/spans")" == 3 ]]
grep -Fxq "$(printf 'substrate.router POST\t0123456789abcdef0123456789abcdef\t3333333333333333\t2222222222222222')" "${test_root}/spans"
log() { :; }
verify_mcp_trace 0123456789abcdef0123456789abcdef
trace_parent=9999999999999999
if verify_mcp_trace 0123456789abcdef0123456789abcdef 2>"${test_root}/error"; then
  echo 'trace check accepted a router span outside the orka-api span' >&2
  exit 1
fi
grep -Fq 'is not the orka-api span' "${test_root}/error"
unset -f kubectl log trace_logs

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
