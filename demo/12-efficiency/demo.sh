#!/usr/bin/env bash
# Orka — twenty customers, one checkout problem
# Dana's platform team keeps the same replies and the same fix while routing most of the work to a small local model.
# shellcheck disable=SC2016
# shellcheck source=demo/lib/demo.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"
here=$demo_root/12-efficiency
umask 077
run_id=$(date -u +%Y%m%dT%H%M%SZ)-$(python3 -c 'import secrets; print(secrets.token_hex(3))')
run_dir=${EFFICIENCY_RUN_DIR:-$repo_root/bin/efficiency-production/twenty-customers-$run_id}
run_dir=$(python3 -c 'from pathlib import Path; import sys; print(Path(sys.argv[1]).resolve())' "$run_dir")
story() { python3 "$here/story.py" --run-dir "$run_dir" "$@"; }

# Prepare authentication offscreen. These defaults go to the actual Orka CLI,
# without changing HOME, the presenter's CLI configuration, or kubeconfig.
demo_context=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["context"])' \
  "$repo_root/bin/efficiency-production/setup.json")
kubectl() { command kubectl --context "$demo_context" "$@"; }
demo_token=$(kubectl -n team-payments create token demo-client --duration=4h)
# Called through pe's eval below.
# shellcheck disable=SC2329
orka() {
  # Retain before/after evidence around the real support CLI commands. These
  # observers do not change the command, its output, or its exit status.
  local identity job phase
  if [[ ${1:-} == task && ${2:-} == create && ${3:-} == -f ]]; then
    identity=${4##*/}
    if [[ $identity =~ ^(support-[0-9][0-9])-(baseline|routed)- ]]; then
      job=${BASH_REMATCH[1]}; phase=${BASH_REMATCH[2]}
      story begin "$phase" "$job" >>"$run_dir/verification.log" 2>&1 || return
    fi
  fi
  command "$repo_root/bin/orka" --server http://127.0.0.1:18101 \
    --namespace team-payments --token "$demo_token" "$@" || return
  if [[ ${1:-} == task && ${2:-} == wait ]]; then
    identity=${3:-}
    if [[ $identity =~ ^(support-[0-9][0-9])-(baseline|routed)- ]]; then
      job=${BASH_REMATCH[1]}; phase=${BASH_REMATCH[2]}
      story collect "$phase" "$job" >>"$run_dir/verification.log" 2>&1 || return
    fi
  fi
}
connections_pid=
monitor_pid=
cleanup() {
  local status=$? pid
  if ((status != 0)) && [[ -d $run_dir ]]; then
    story retain-failure >>"$run_dir/verification.log" 2>&1 || true
  fi
  for pid in "$monitor_pid" "$connections_pid"; do
    [[ -z $pid ]] && continue
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  unset demo_token
}
trap cleanup EXIT
python3 "$here/story.py" setup >"$run_dir-preparation.log" 2>&1
python3 "$here/run.py" connect >"$run_dir-connections.log" 2>&1 &
connections_pid=$!
for port in 18101 18111; do
  wait_for 'the dedicated cluster connection' \
    "kill -0 $connections_pid && curl -fsS --max-time 2 http://127.0.0.1:$port/healthz" 60
done
story init >/dev/null
# shellcheck disable=SC1091
source "$run_dir/names.sh"
python3 "$here/run.py" --run-dir "$run_dir" monitor >"$run_dir/monitor.log" 2>&1 &
monitor_pid=$!
cd "$run_dir"

# --- on-camera helpers ---------------------------------------------------
# instructions AGENT — the Agent's saved instructions, folded to the terminal.
instructions() {
  orka agent get "$1" -o json | jq -r .spec.systemPrompt.inline | fold -s -w 96
}
# run_batch DIR — create and wait for each customer Task, one at a time.
# story.py's begin/collect hooks fire on the wrapped create and wait calls.
run_batch() {
  local request
  for request in "$1"/*.yaml; do
    orka task create -f "$request" >/dev/null
    orka task wait "$(basename "$request" .yaml)" --timeout 10m
  done
}
# check_tests RUN — rerun the six payment tests on the published branch in a
# sealed container: no network, read-only, unprivileged, resource-limited.
# The agent reported its own test results; this is the independent check.
check_tests() {
  podman run --rm --pull=never --network=none --read-only \
    --cap-drop=all --security-opt=no-new-privileges --user=65532 \
    --memory=256m --pids-limit=64 --timeout=60 \
    -v "$PWD/$1/repository/payments:/checks:ro" "$NODE_TEST_IMAGE" \
    node --test /checks/payment.test.mjs | tee "$1/engineering/tests.txt" | grep -E '^(✔|✖|ℹ (tests|pass|fail))'
}

banner 'Orka — twenty customers, one checkout problem' "Same checks, two ways to run the work. Compare the results and estimated cost."
say "Twenty customers report double charges after a frozen checkout. Support needs"
say "twenty replies; engineering needs one fix. Dana's platform team runs it twice:"
say "first on a hosted model, then with a router that prefers a small local model."
say "The replies and the fix are checked the same way both times. Then the bill."
helpers_note instructions, run_batch, check_tests

chapter '1. Give each assistant its instructions'
pe 'cat customers.txt'
say 'An Agent holds the instructions for an assistant. Two Agents, two jobs.'
pe 'instructions customer-support'
pe 'instructions payments-engineer'
ok 'Support replies in two factual sentences. Engineering fixes the bug and runs six tests.'

chapter '2. Run everything on the hosted model'
say "A Task is Orka's record of one piece of work. Twenty support Tasks, then"
say 'one engineering Task, all on hosted GPT-5.5. Quiet stretches are cut.'
story phase baseline start
pe 'run_batch support-baseline'
story batch-summary baseline
pe 'result "$SUPPORT_01_BASELINE"'
pe 'result "$SUPPORT_02_BASELINE"'
pe 'cat support-baseline-routes.txt'
story begin baseline engineering
pe 'orka task create -f engineering-baseline.yaml'
pe 'orka task wait "$ENGINEERING_BASELINE" --timeout 10m'
story collect baseline engineering
pe 'result "$ENGINEERING_BASELINE"'
story checkout baseline
say 'The agent says the tests pass. Check that ourselves, in a sealed container.'
pe 'git -C baseline/repository diff "$SOURCE_REVISION" --stat'
pe 'check_tests baseline'
story finish-tests baseline
story phase baseline end
ok 'Twenty replies checked, six tests passed, on the hosted model. That is the baseline.'

chapter '3. Add a small model on our own hardware'
say "AIKit serves Qwen3.5 2B on the cluster's CPUs: hardware the team already pays for."
pe 'kubectl -n orka-efficiency get deployment qwen35-2b'
pe 'kubectl -n orka-efficiency top pod -l app=qwen35-2b'
ok 'A local model is up and idle, waiting for work.'

chapter '4. Let a router choose the model'
say "A Provider is Orka's connection to a model service. Support's Provider"
say 'points at Vekil, a gateway that decides where each request goes.'
pe 'orka provider get semantic-router -o json | jq ".spec | {type, baseURL}"'
pe 'cat routing.yaml'
say 'Jev, a classifier, reads each request and recommends lightweight or powerful.'
say 'If Jev cannot be reached, the request goes to the powerful model.'
pe 'kubectl -n team-payments set env deployment/vekil POLICY_ROUTING_MODE=enforce'
pe 'kubectl -n team-payments rollout status deployment/vekil --timeout=180s'
wait_for 'the replacement gateway connection' 'curl -fsS --max-time 2 http://127.0.0.1:18111/readyz' 60
classifier_ready() {
  python3 - "$here" <<'PY'
import sys
sys.path.insert(0, sys.argv[1])
import evidence, run
profile = evidence.profile(run.gateway_snapshot("payments"))
sys.exit(0 if profile["effective_mode"] == "enforce" and profile["preflight_state"] == "ready" else 1)
PY
}
wait_for 'the classifier preflight' classifier_ready 120 || {
  bad "Jev is not reachable through Vekil; the routed batch would fall back to the hosted model"
  exit 1
}
ok 'Routing is on and the classifier answers. The Agents and requests are unchanged.'

chapter '5. Run the same work again'
say 'Same twenty reports, same instructions, same starting code and tests.'
story phase routed start
pe 'run_batch support-routed'
story batch-summary routed
pe 'result "$SUPPORT_01_ROUTED"'
pe 'result "$SUPPORT_02_ROUTED"'
pe 'cat support-routed-routes.txt'
story begin routed engineering
pe 'orka task create -f engineering-routed.yaml'
pe 'orka task wait "$ENGINEERING_ROUTED" --timeout 10m'
story collect routed engineering
pe 'result "$ENGINEERING_ROUTED"'
story checkout routed
pe 'check_tests routed'
story finish-tests routed
pe 'cat engineering-routed-routes.txt'
story phase routed end
ok 'Twenty replies checked, six tests passed again. The records show which model answered.'

chapter '6. Compare the bill'
story verify >verification.txt
say 'Both runs, side by side: outcomes, elapsed time, and tokens.'
pe 'cat comparison.txt'
nap 5
say 'The local model answers on CPU, and both batches ran one Task at a time.'
say 'Compare the elapsed time as well as the tokens. Now apply published rates.'
pe 'cat cost-summary.txt'
nap 8
say 'These estimates cover this batch and its elapsed time. They are not an invoice.'
say "Orka's own usage report counts the support Tasks; the coding runtime's"
say 'tokens are unavailable there, so the comparison uses the gateway counts.'
pe 'cat usage-notes.txt'
ok 'Both runs passed the same checks. The measured usage gives us the cost comparison.'

local_replies=$(jq -er '[.jobs | to_entries[] | select(.key | startswith("support-")) |
  .value.routed.gateway.operations | any(.tier == "lightweight")] | map(select(.)) | length' verified-report.json)
api_change=$(jq -er '.costs.comparison.api.percentChange' verified-report.json)
support_time=$(jq -er '(.phases.routed.supportElapsedSeconds - .phases.baseline.supportElapsedSeconds)
  / .phases.baseline.supportElapsedSeconds * 100' verified-report.json)
printf -v api_change '%+.1f%%' "$api_change"
printf -v support_time '%+.1f%%' "$support_time"
evidence \
  "Customer replies checked" "20 / 20 in both runs" \
  "Payment tests passed" "6 / 6 in both runs" \
  "Replies answered by the local model" "$local_replies of 20" \
  "Estimated model cost change" "$api_change" \
  "Support batch wall-clock" "$support_time" \
  "Agents, prompts, and code changed between runs" "none"
cta
