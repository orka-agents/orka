#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/e2e-cleanup.sh
. "${root}/scripts/lib/e2e-cleanup.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/orka-e2e-cleanup-test.XXXXXX")"
trap 'rm -rf "${work}"' EXIT

mkdir -p "${work}/evidence"
evidence="${work}/evidence"
if e2e_cleanup_evidence_passed "${evidence}"; then
  echo 'missing cleanup evidence was accepted' >&2
  exit 1
fi
cat >"${evidence}/suite-cleanup.json" <<'JSON'
{"schemaVersion":1,"passed":true,"stage":"complete"}
JSON
cat >"${evidence}/cleanup-task.json" <<'JSON'
{"schemaVersion":1,"passed":true,"stage":"complete","tasks":[{"uid":"task-uid","deleteRequested":true,"productFinalizersReleased":true,"observerReleased":true,"absent":true,"receiptRequired":true,"receiptVerified":true,"runtimeSessionCleanupDigest":"sha256:original"}],"sessions":[{"absent":true}]}
JSON
e2e_cleanup_evidence_passed "${evidence}"
cp "${evidence}/cleanup-task.json" "${work}/passed.json"
for mutation in '.passed=false' '.tasks[0].absent=false' '.tasks[0].receiptVerified=false' '.tasks[0].productFinalizersReleased=false' '.sessions[0].absent=false'; do
  jq "${mutation}" "${work}/passed.json" >"${evidence}/cleanup-task.json"
  if e2e_cleanup_evidence_passed "${evidence}"; then
    echo "unproved cleanup boundary was accepted: ${mutation}" >&2
    exit 1
  fi
done
printf '{invalid\n' >"${evidence}/cleanup-task.json"
if e2e_cleanup_evidence_passed "${evidence}"; then
  echo 'malformed cleanup evidence was accepted' >&2
  exit 1
fi
cp "${work}/passed.json" "${evidence}/cleanup-task.json"
printf '%s\n' 'ok - cleanup evidence must prove every recorded Task and Session boundary'

kind() {
  if [[ "$*" == 'delete cluster --name owned-e2e' ]]; then
    return "${fake_delete_status}"
  fi
  if [[ "$*" == 'get clusters' ]]; then
    printf '%s\n' "${fake_clusters}"
    return "${fake_list_status}"
  fi
  echo 'unexpected Kind target' >&2
  return 99
}
KIND=kind
fake_delete_status=0
fake_list_status=0
fake_clusters=another-cluster
e2e_cleanup_kind owned-e2e "${evidence}"
jq -e '.passed and .absent and .kindCluster == "owned-e2e"' "${evidence}/kind-cleanup.json" >/dev/null
for failure in delete list retained; do
  fake_delete_status=0
  fake_list_status=0
  fake_clusters=another-cluster
  case "${failure}" in
    delete) fake_delete_status=7 ;;
    list) fake_list_status=8 ;;
    retained) fake_clusters=$'another-cluster\nowned-e2e' ;;
  esac
  if e2e_cleanup_kind owned-e2e "${evidence}"; then
    echo "unproved Kind teardown was accepted: ${failure}" >&2
    exit 1
  fi
  jq -e '.passed == false' "${evidence}/kind-cleanup.json" >/dev/null
done
printf '%s\n' 'ok - Kind deletion failure, failed readback and retained exact cluster fail cleanup'

# Exercise the real EXIT handler without creating a cluster or starting a
# provider. Exit 90 is the workflow contract: cleanup failures cannot be
# hidden by its external-upstream retry.
body="$(awk '/^on_exit\(\) \{/,/^\}$/' "${root}/scripts/live-copilot-proxy-e2e.sh")"
[[ -n "${body}" ]]
eval "${body}"
cleanup_port_forward() { :; }
dump_diagnostics() { :; }
log() { :; }
proxy_pf_pid=""
kind_cluster=owned-e2e
cleanup_report_dir="${evidence}"
e2e_started=true
for scenario in success upstream_error missing_receipt failed_kind; do
  fake_delete_status=0
  fake_list_status=0
  fake_clusters=another-cluster
  cp "${work}/passed.json" "${evidence}/cleanup-task.json"
  work_dir="${work}/private-${scenario}"
  mkdir -p "${work_dir}"
  initial_status=0
  expected_status=0
  case "${scenario}" in
    upstream_error) initial_status=13; expected_status=13 ;;
    missing_receipt)
      jq '.tasks[0].receiptVerified=false' "${work}/passed.json" >"${evidence}/cleanup-task.json"
      expected_status=90
      ;;
    failed_kind) fake_delete_status=7; expected_status=90 ;;
  esac
  actual_status=0
  if (on_exit "${initial_status}"); then
    :
  else
    actual_status=$?
  fi
  [[ "${actual_status}" -eq "${expected_status}" ]] || {
    echo "EXIT handler returned ${actual_status}, expected ${expected_status} for ${scenario}" >&2
    exit 1
  }
done
printf '%s\n' 'ok - upstream failure remains retryable only after normal cleanup and Kind absence'
