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
render_kontxt_story_file           > "${DEMO_WORKDIR}/kontxt-story.txt"

demo_scenario "kontxt — zero-secret zero-trust agent calls" \
  "Same workload identity, two outcomes. Two identical caller Jobs — one with a TxToken that has the required scope (allowed), one without (denied). No API keys, decisions made per-request."

demo_event "🧹" "Clearing any prior kontxt caller Jobs so this run starts clean…"
kubectl delete job/"${ok_job}"     -n "${kontxt_ns}" --ignore-not-found >/dev/null 2>&1 || true
kubectl delete job/"${denied_job}" -n "${kontxt_ns}" --ignore-not-found >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=8

# Chapter 1 ------------------------------------------------------------------
narrate "Zero-secret zero-trust agent calls: caller's SA token becomes a one-shot, scoped TxToken. Same identity, two outcomes — one allowed, one denied."
chapter "What this demo is doing" "🧑"
demo_show_full "${DEMO_WORKDIR}/kontxt-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "The caller workload uses only a normal Kubernetes ServiceAccount — no API keys, no shared secrets. The SA proves WHO the caller is; the TTS will decide WHAT it can do."
chapter "Apply the caller ServiceAccount" "🪪"
demo_event "🆔" "Apply a normal Kubernetes SA. No secrets to manage. Identity = pod's SA, proven by projected tokens."
log_info "TTS URL:  ${DEMO_KONTXT_TTS_URL}"
log_info "Audience: ${DEMO_KONTXT_TTS_AUDIENCE}  (binds the SA token so it can ONLY be exchanged at the TTS)"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/kontxt-sa.yaml"

# Chapter 3 ------------------------------------------------------------------
narrate "The Job mounts a projected SA token at /var/run/orka/token — per-pod, ephemeral, audience-bound. The pod presents it to the TTS at runtime to mint a short-lived TxToken; it never holds a long-lived secret."
chapter "Inspect the Job manifest" "📄"
demo_event "📐" "Look for the projected serviceAccountToken volume — that is the entire 'auth setup'. No Secrets, no env-var credentials."
demo_show "${DEMO_WORKDIR}/kontxt-job.yaml"

# Chapter 4 ------------------------------------------------------------------
narrate "Run the caller. At runtime it reads its projected SA token, exchanges it at the TTS for a TxToken scoped to (action, namespace), then calls the Orka API. Orka enforces the scope on every request."
chapter "Run the allowed caller" "✅"
demo_event "🚀" "Apply the Job. Caller will: (1) read SA token, (2) exchange at TTS for a TxToken, (3) call Orka."
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/kontxt-job.yaml"
wait_for_job_with_progress "${ok_job}" "${kontxt_ns}" 120 complete \
  || die "allowed caller Job did not complete in time"
demo_event "✅" "Job completed — the 3-step dance succeeded. Read the log next to see each step in order."

# Chapter 5 ------------------------------------------------------------------
narrate "What the caller printed: step 1 reads the projected SA token from disk; step 2 trades it at the TTS for a TxToken scoped to THIS exact call; step 3 calls Orka with the TxToken. Orka verifies the scope, allows the request, returns 200. No JWT material is logged."
chapter "Read the caller log" "🪵"
demo_event "🔍" "Step 2 shows the scope-down: a broad parent scope from the SA is narrowed to tctx.namespace for THIS request. Step 3 hits Orka and returns 200."
demo_pe "kubectl logs -n ${kontxt_ns} job/${ok_job} --tail=20 | grep -E '^[0-9]/3'"

# Chapter 6 ------------------------------------------------------------------
narrate "Same caller, same identity, same TTS exchange — but the request targets namespace=not-default, which the TxToken's scope does not authorize."
chapter "Run the denied caller" "🚫"
demo_event "🚀" "Apply the second Job. Identical code, identical SA — only the target namespace differs."
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/kontxt-denied-job.yaml"
wait_for_job_with_progress "${denied_job}" "${kontxt_ns}" 120 fail \
  || die "denied caller Job did not transition to Failed=True within 120s"
demo_event "🛑" "Job failed (expected). Denial happened at the Orka API — TTS still minted the TxToken cleanly."

# Chapter 7 ------------------------------------------------------------------
narrate "Steps 1 and 2 look identical to the allowed run — the TxToken minted cleanly. Step 3 returns 403: the TxToken is valid, but Orka enforces its scope at the API boundary and the requested namespace is outside it. No leaked JWT, just a clean denial."
chapter "Read the denied caller log" "🪵"
demo_event "🔍" "Step 2 shows the same scope-down as the allowed run. 3/3 returns 403 because step 3's target ns now differs from the bound tctx.namespace."
demo_pe "kubectl logs -n ${kontxt_ns} job/${denied_job} --tail=20 | grep -E '^[0-9]/3'"

# Chapter 8 ------------------------------------------------------------------
narrate "Same SA identity, two outcomes — decided per request by the TxToken's scope. The audit trail keeps only safe digests; no JWT material ever lands in Task status or logs. Zero-trust by construction."
chapter "Transaction summary" "🔐"
demo_event "📜" "Payoff card pulls the parsed status + target ns from each caller's log — same SA, same TTS, only scope differed."
payoff_card_kontxt "${ok_job}" "${denied_job}" "${kontxt_ns}"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON (Jobs)%b\n' "${DIM}" "${COLOR_RESET}"
  kubectl get jobs -n "${kontxt_ns}" "${ok_job}" "${denied_job}" \
    -o json | jq '.items | map({name: .metadata.name, succeeded: .status.succeeded, failed: .status.failed, conditions: (.status.conditions // []) | map({type, status, reason})})'
fi
