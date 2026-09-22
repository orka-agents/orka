#!/usr/bin/env bash
# Help the customer. Fix the cause.
# shellcheck disable=SC2016
# shellcheck source=demo/lib/demo.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"
here=$demo_root/12-efficiency
umask 077
run_id=$(date -u +%Y%m%dT%H%M%SZ)-$(python3 -c 'import secrets; print(secrets.token_hex(3))')
run_dir=${EFFICIENCY_RUN_DIR:-$repo_root/bin/efficiency-production/customer-story-$run_id}
run_dir=$(python3 -c 'from pathlib import Path; import sys; print(Path(sys.argv[1]).resolve())' "$run_dir")
story() { python3 "$here/story.py" --run-dir "$run_dir" "$@"; }

# Prepare authentication offscreen. These defaults go to the actual Orka CLI,
# without changing HOME, the presenter's CLI configuration, or kubeconfig.
demo_token=$(kubectl --context sertac-aks -n team-payments create token demo-client --duration=4h)
orka() {
  command "$repo_root/bin/orka" --server http://127.0.0.1:18101 \
    --namespace team-payments --token "$demo_token" "$@"
}
connections_pid=
monitor_pid=
cleanup() {
  local pid
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

banner 'Help the customer. Fix the cause.' 'One checkout problem. Two teams with different jobs.'
say 'Checkout froze. The customer clicked Pay again and saw two charges.'
say 'Support needs a factual reply. Payments engineering needs to fix the cause.'

chapter '1. Give each assistant its instructions'
pe 'cat customer.txt'
say 'An Agent saves the instructions for an assistant.'
pe 'orka agent get customer-support -o json | jq -r .spec.systemPrompt.inline'
pe 'orka agent get payments-engineer -o json | jq -r .spec.systemPrompt.inline'
say 'The sample repository includes six checks for payment retries.'

chapter '2. Handle the customer problem'
say "A Task is Orka's record of one piece of work. Start with hosted GPT-5.5."
story phase baseline start
story begin baseline support
pe 'orka task create -f support-baseline.yaml'
pe 'orka task wait "$SUPPORT_BASELINE" --timeout 10m'
story collect baseline support
pe 'orka task result "$SUPPORT_BASELINE"'
story begin baseline engineering
pe 'orka task create -f engineering-baseline.yaml'
pe 'orka task wait "$ENGINEERING_BASELINE" --timeout 10m'
story collect baseline engineering
pe 'orka task result "$ENGINEERING_BASELINE"'
story checkout baseline
say 'Check the published change with the same tests, in an isolated container.'
pe 'git -C baseline/repository diff "$SOURCE_REVISION" --stat'
pe 'podman run --rm --pull=never --network=none --read-only \
  --cap-drop=all --security-opt=no-new-privileges --user=65532 \
  --memory=256m --pids-limit=64 --timeout=60 \
  -v "$PWD/baseline/repository/payments:/checks:ro" "$NODE_TEST_IMAGE" \
  node --test /checks/payment.test.mjs | tee baseline/engineering/tests.txt'
story finish-tests baseline
story phase baseline end

chapter '3. Use hardware we already operate'
say "AIKit serves Qwen3.5 2B on the cluster's CPUs."
say 'A lightweight model on hardware you already pay for and operate.'
pe 'kubectl --context sertac-aks -n orka-efficiency get deployment qwen35-2b'
pe 'kubectl --context sertac-aks -n orka-efficiency top pod -l app=qwen35-2b'

chapter '4. Connect Orka to the semantic router'
say "A Provider is Orka's connection to a model service."
pe 'orka provider get semantic-router -o json | jq ".spec | {type, baseURL, defaultModel}"'
say 'Support uses this Provider. The coding runtime also sends model requests to Vekil.'
say 'Vekil is the gateway between the agents and the available models.'

chapter '5. Let the classifier choose'
pe 'cat routing.yaml'
say 'Jev assesses a new request and recommends lightweight or powerful.'
say 'Vekil applies that choice to the destinations configured here.'
say 'Related tool calls stay with the selected model while the agent finishes the job.'
pe 'kubectl --context sertac-aks -n team-payments set env deployment/vekil POLICY_ROUTING_MODE=enforce'
pe 'kubectl --context sertac-aks -n team-payments rollout status deployment/vekil --timeout=180s'
wait_for 'the replacement gateway connection' 'curl -fsS --max-time 2 http://127.0.0.1:18111/readyz' 60

chapter '6. Repeat the same work'
say 'Same requests and Agent instructions. Same starting repository revision.'
story phase routed start
story begin routed support
pe 'orka task create -f support-routed.yaml'
pe 'orka task wait "$SUPPORT_ROUTED" --timeout 10m'
story collect routed support
pe 'orka task result "$SUPPORT_ROUTED"'
pe 'cat support-routed-routes.txt'
story begin routed engineering
pe 'orka task create -f engineering-routed.yaml'
pe 'orka task wait "$ENGINEERING_ROUTED" --timeout 10m'
story collect routed engineering
pe 'orka task result "$ENGINEERING_ROUTED"'
story checkout routed
pe 'podman run --rm --pull=never --network=none --read-only \
  --cap-drop=all --security-opt=no-new-privileges --user=65532 \
  --memory=256m --pids-limit=64 --timeout=60 \
  -v "$PWD/routed/repository/payments:/checks:ro" "$NODE_TEST_IMAGE" \
  node --test /checks/payment.test.mjs | tee routed/engineering/tests.txt'
story finish-tests routed
pe 'cat engineering-routed-routes.txt'
story phase routed end

chapter '7. Check the results and usage'
story verify >verification.txt
USAGE_FROM=$(jq -r .startedAt baseline/phase.json)
USAGE_UNTIL=$(jq -r .finishedAt routed/phase.json)
pe 'orka usage summary --from "$USAGE_FROM" --until "$USAGE_UNTIL"'
say 'These standalone Tasks appear under other team usage.'
pe 'cat usage-notes.txt'
pe 'cat comparison.txt'
nap 5
say 'Tokens are the small pieces of text a model processes. Jev adds work too.'
pe 'cat resources.txt'
nap 4
say 'Orka sets the jobs and keeps their records. Vekil chooses model destinations.'
say 'AIKit supplies local capacity. The results show what those choices produced.'
note 'What would you build for your team?'
say 'https://orka-agents.github.io/orka/'
