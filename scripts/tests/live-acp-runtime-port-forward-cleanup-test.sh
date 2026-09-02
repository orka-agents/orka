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
script="${root}/scripts/live-acp-runtime-e2e.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/orka-acp-port-forward-test.XXXXXX")"
fake_bin="${test_root}/bin"
fake_pid_file="${test_root}/kubectl.pid"
fake_args_file="${test_root}/kubectl.args"
tracked_pid=""
fake_kubectl_pid=""

terminate_pid() {
  local pid="${1:-}"
  local attempt
  [[ "${pid}" =~ ^[1-9][0-9]*$ ]] || return 0
  if ! kill -0 "${pid}" >/dev/null 2>&1; then
    return 0
  fi
  kill "${pid}" >/dev/null 2>&1 || true
  for attempt in {1..100}; do
    if ! kill -0 "${pid}" >/dev/null 2>&1; then
      wait "${pid}" >/dev/null 2>&1 || true
      return 0
    fi
    sleep 0.01
  done
  kill -KILL "${pid}" >/dev/null 2>&1 || true
  wait "${pid}" >/dev/null 2>&1 || true
}

cleanup() {
  set +e
  terminate_pid "${tracked_pid}"
  if [[ "${fake_kubectl_pid}" != "${tracked_pid}" ]]; then
    terminate_pid "${fake_kubectl_pid}"
  fi
  if [[ -d "${test_root}" ]]; then
    rm -R -- "${test_root}"
  fi
}
trap cleanup EXIT

fail() {
  printf 'not ok - %s\n' "$*" >&2
  exit 1
}

extract_function() {
  local name="$1"
  awk -v signature="${name}() {" '
    $0 == signature { copying = 1 }
    copying { print }
    copying && $0 == "}" { exit }
  ' "${script}"
}

mkdir -p "${fake_bin}"
cat >"${fake_bin}/kubectl" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$$" >"${FAKE_KUBECTL_PID_FILE:?}"
printf '%s\n' "$*" >"${FAKE_KUBECTL_ARGS_FILE:?}"
exec tail -f /dev/null
FAKE_KUBECTL
chmod +x "${fake_bin}/kubectl"

export PATH="${fake_bin}:${PATH}"
export FAKE_KUBECTL_PID_FILE="${fake_pid_file}"
export FAKE_KUBECTL_ARGS_FILE="${fake_args_file}"

stop_body="$(extract_function stop_api_forward)"
start_process_body="$(extract_function start_api_forward_process)"
start_body="$(extract_function start_api_forward)"
[[ -n "${stop_body}" && -n "${start_body}" ]] || fail 'could not extract port-forward lifecycle functions'
eval "${stop_body}"
[[ -z "${start_process_body}" ]] || eval "${start_process_body}"
eval "${start_body}"

context="fixture-context"
orka_namespace="fixture-system"
controller_deployment="fixture-controller"
api_local_port=18080
controller_api_port=8080
api_forward_log="${test_root}/port-forward.log"
api_forward_pid=""

k() {
  kubectl --context "${context}" "$@"
}

api_forward_ready() {
  local pid
  [[ -s "${fake_pid_file}" ]] || return 1
  read -r pid <"${fake_pid_file}"
  [[ -n "${api_forward_pid}" ]] &&
    kill -0 "${api_forward_pid}" >/dev/null 2>&1 &&
    kill -0 "${pid}" >/dev/null 2>&1
}

die() {
  fail "$*"
}

redact() {
  cat
}

start_api_forward
tracked_pid="${api_forward_pid}"
for _ in {1..100}; do
  [[ -s "${fake_pid_file}" ]] && break
  sleep 0.01
done
[[ -s "${fake_pid_file}" ]] || fail 'fake kubectl did not record its PID'
read -r fake_kubectl_pid <"${fake_pid_file}"

[[ "${tracked_pid}" == "${fake_kubectl_pid}" ]] ||
  fail "tracked PID ${tracked_pid} is not the kubectl PID ${fake_kubectl_pid}"

expected_args="--context ${context} -n ${orka_namespace} port-forward"
expected_args+=" --address=127.0.0.1 deployment/${controller_deployment}"
expected_args+=" ${api_local_port}:${controller_api_port}"
[[ "$(cat "${fake_args_file}")" == "${expected_args}" ]] ||
  fail 'kubectl port-forward did not retain the explicit context and expected arguments'

stop_api_forward || fail 'stop_api_forward reported a cleanup failure'
[[ -z "${api_forward_pid}" ]] || fail 'stop_api_forward did not clear the tracked PID'
if kill -0 "${fake_kubectl_pid}" >/dev/null 2>&1; then
  fail "kubectl PID ${fake_kubectl_pid} survived stop_api_forward"
fi

printf '%s\n' 'ok - controller API port-forward tracks and terminates the actual kubectl process'
