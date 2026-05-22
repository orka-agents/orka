#!/usr/bin/env bash
# Demo 50 — kontxt (Transaction Tokens)
#
# A workload-issued ServiceAccount token is exchanged at an in-cluster TTS
# for a TxToken, which Orka uses for fine-grained, request-scoped authz.
# The same identity gets two outcomes: one call is allowed (target ns
# matches policy), one is denied (target ns is wrong).
#
# Pacing is controlled by DEMO_RECORD_PROFILE (presenter|docs|social|hero).
#
# SECURITY: helpers MUST NOT log Txn-Token values, Authorization values,
# subject-token contents, or anything matching eyJ[A-Za-z0-9_=-]{20,}.
# kontxt-caller's caller.sh enforces redaction on its own output;
# payoff_card_kontxt reads only the safe orka.ai/transaction-id annotation.

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

# ---------------------------------------------------------------------------
# Setup (silent — uses legacy log()).
# ---------------------------------------------------------------------------
require_demo_base
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir

kontxt_ns="${DEMO_KONTXT_NAMESPACE:-default}"
ok_job="${DEMO_KONTXT_JOB_NAME:-orka-kontxt-caller}"
denied_job="${DEMO_KONTXT_DENIED_JOB_NAME:-orka-kontxt-caller-denied}"

render_kontxt_caller_sa            > "${DEMO_WORKDIR}/kontxt-sa.yaml"
render_kontxt_caller_job           > "${DEMO_WORKDIR}/kontxt-job.yaml"
render_kontxt_denied_caller_job    > "${DEMO_WORKDIR}/kontxt-denied-job.yaml"

log "Resetting any prior kontxt caller jobs"
kubectl delete job/"${ok_job}"     -n "${kontxt_ns}" --ignore-not-found >/dev/null 2>&1 || true
kubectl delete job/"${denied_job}" -n "${kontxt_ns}" --ignore-not-found >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=7
clear
banner "kontxt — TxTokens in action"

# Chapter 1 ------------------------------------------------------------------
narrate "Workload identity (SA token) becomes a request-scoped TxToken via TTS."
chapter "Apply the caller ServiceAccount" "🪪"
log_info "TTS URL: ${DEMO_KONTXT_TTS_URL}"
log_info "Audience: ${DEMO_KONTXT_TTS_AUDIENCE}"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/kontxt-sa.yaml"
demo_pe "kubectl get sa ${DEMO_KONTXT_SA_NAME} -n ${kontxt_ns}"

# Chapter 2 ------------------------------------------------------------------
narrate "The Job mounts a projected SA token with audience=${DEMO_KONTXT_TTS_AUDIENCE}."
chapter "Inspect the Job manifest" "📄"
demo_show "${DEMO_WORKDIR}/kontxt-job.yaml"

# Chapter 3 ------------------------------------------------------------------
narrate "Allowed call: target namespace matches what the TTS will authorize."
chapter "Run the allowed caller" "✅"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/kontxt-job.yaml"
log_info "Waiting for the caller Job to complete..."
kubectl wait --for=condition=complete --timeout=120s \
  -n "${kontxt_ns}" "job/${ok_job}" >/dev/null \
  || die "allowed caller Job did not complete in time"

# Chapter 4 ------------------------------------------------------------------
narrate "The caller prints 1/3 → 2/3 → 3/3; JWTs are redacted by the image."
chapter "Read the caller log" "🪵"
demo_pe "kubectl logs -n ${kontxt_ns} job/${ok_job} --tail=20 | grep -E '^[0-9]/3'"

# Chapter 5 ------------------------------------------------------------------
narrate "Denied call: same identity, wrong scope — the TxToken can't list Tasks."
chapter "Run the denied caller" "🚫"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/kontxt-denied-job.yaml"
log_info "Waiting for the denied caller Job to fail (this is expected)..."
# The denied job sets backoffLimit: 0; either Failed or Complete=false within 120s.
deadline=$(( SECONDS + 120 ))
denied_status=""
denied_complete=""
while (( SECONDS < deadline )); do
  denied_status="$(kubectl get job "${denied_job}" -n "${kontxt_ns}" \
    -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)"
  if [[ "${denied_status}" == "True" ]]; then
    break
  fi
  denied_complete="$(kubectl get job "${denied_job}" -n "${kontxt_ns}" \
    -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null || true)"
  if [[ "${denied_complete}" == "True" ]]; then
    die "denied caller Job completed; expected authorization failure"
  fi
  sleep 3
done
if [[ "${denied_status}" != "True" ]]; then
  die "denied caller Job did not transition to Failed=True within 120s"
fi

# Chapter 6 ------------------------------------------------------------------
narrate "Failure surface: 3/3 reports status=403, no JWT material in logs."
chapter "Read the denied caller log" "🪵"
demo_pe "kubectl logs -n ${kontxt_ns} job/${denied_job} --tail=20 | grep -E '^[0-9]/3'"

# Chapter 7 ------------------------------------------------------------------
narrate "One identity, two outcomes. Audit trail keeps only safe digests."
chapter "Transaction summary" "🔐"
# Pull the first allowed Job's controller-tracked Task (if Orka recorded one)
# from the safe orka.ai/transaction-id annotation. The card never reads the
# raw TxToken — only the annotation digest the controller writes.
ok_task=""
ok_task="$(kubectl get tasks -n "${DEMO_NAMESPACE}" \
  -l "orka.ai/source=kontxt-caller" \
  -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)"
payoff_card_kontxt "${ok_task}" "${denied_job}"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON (Jobs)%b\n' "${DIM}" "${COLOR_RESET}"
  kubectl get jobs -n "${kontxt_ns}" "${ok_job}" "${denied_job}" \
    -o json | jq '.items | map({name: .metadata.name, succeeded: .status.succeeded, failed: .status.failed, conditions: (.status.conditions // []) | map({type, status, reason})})'
fi
