#!/usr/bin/env bash
# Two teams, one address, more useful work
# Commands in pe are evaluated after they are visibly typed.
# shellcheck disable=SC2016
# shellcheck source=demo/lib/demo.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"
here=$demo_root/12-efficiency
cd "$repo_root"
umask 077
run_id=$(date -u +%Y%m%dT%H%M%SZ)-$(python3 -c 'import secrets; print(secrets.token_hex(3))')
run_dir=${EFFICIENCY_RUN_DIR:-$repo_root/bin/efficiency-production/recording-$run_id}
run_dir=$(python3 -c 'from pathlib import Path; import sys; print(Path(sys.argv[1]).resolve())' "$run_dir")
run() { python3 "$here/run.py" --run-dir "$run_dir" "$@"; }
view() { python3 "$here/show.py" --run-dir "$run_dir" "$@"; }
gateway_mode() { python3 "$here/prepare.py" --context sertac-aks mode "$1"; }

run init >/dev/null
gateway_mode off >"$run_dir/preparation.log"
run agents plain >>"$run_dir/preparation.log"
cp "$repo_root/bin/efficiency-production/setup.json" "$run_dir/setup-before.json"
connections_pid=
monitor_pid=
cleanup() {
  local pid
  for pid in "$monitor_pid" "$connections_pid"; do
    [[ -z $pid ]] && continue
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT
python3 "$here/run.py" connect >"$run_dir/connections.log" 2>&1 &
connections_pid=$!
for port in 18090 18101 18102 18111 18112; do
  wait_for "the dedicated cluster connection on $port" \
    "kill -0 $connections_pid && curl -fsS --max-time 2 http://127.0.0.1:$port/healthz" 60
done
python3 "$here/run.py" --run-dir "$run_dir" monitor >"$run_dir/monitor.log" 2>&1 &
monitor_pid=$!
cd "$run_dir"

banner 'Two teams, one address, more useful work' \
  'Change how AI work runs, then measure the results.'
say 'Payments must count duplicate events once and fix a double-charge bug.'
say 'Inventory has 24 filters requested, 18 available, and an overselling bug.'
say 'Both teams use one company address for AI help. The platform team wants'
say 'useful answers and working fixes from the capacity it already has.'

chapter '1. Two teams, one address'
say "A developer's identity selects their team's Orka installation."
pe 'view connections'
say 'An Agent holds saved worker instructions. A Task records one piece of work.'
say 'A hosted model coordinates each incoming request and its worker Task.'
pe 'cat payments-summary.txt'
pe 'cat inventory-stock.txt'

chapter '2. Let the platform shape the answer'
pe 'run ask orchestration-before inventory-stock'
say 'The platform team updates the saved instructions for stock questions.'
pe 'run agents structured'
pe 'view agent'
pe 'run ask orchestration-after inventory-stock'
say 'Same request and connection. The Agent now includes a customer next action.'

chapter '3. Measure the hosted baseline'
say 'Keep those instructions fixed. First, run all four requests on hosted models.'
run phase baseline start
pe 'run ask baseline payments-summary'
pe 'run ask baseline payments-fix'
pe 'run ask baseline inventory-stock'
pe 'run ask baseline inventory-fix'
run phase baseline end
say 'The proposed fixes passed real checks, including overlapping requests and retries.'

chapter '4. Use the gateway to choose a model'
say 'Vekil is the gateway between each team Agent and the available models.'
say 'Jev assesses what the request asks for and helps choose its destination.'
pe 'gateway_mode enforce'
run phase routed start
pe 'view routes'
say 'AIKit serves Qwen3.5 2B on CPU capacity in this cluster.'

chapter '5. Repeat the same work'
say 'Repeat the saved requests. Keep the application connection and Agents fixed.'
pe 'run ask routed payments-summary'
pe 'run ask routed payments-fix'
pe 'run ask routed inventory-stock'
pe 'run ask routed inventory-fix'
run phase routed end
say 'Each model destination above comes from the gateway records for that request.'

chapter '6. Compare outcomes and usage'
pe 'view outcomes'
nap 5
pe 'view usage'
nap 7
say 'Tokens are the small pieces of text a model processes.'
say 'Count coordination and routing assessment too. Local compute also uses capacity.'
pe 'view resources'
nap 4

chapter '7. What the platform controls'
pe 'view ending'
say 'The developers kept their connection while the platform changed the work.'
say 'Use the recorded outcomes and resource use to decide which choices help.'
cp "$repo_root/bin/efficiency-production/setup.json" "$run_dir/setup-after.json"
note "Full responses and verified evidence: $run_dir"
