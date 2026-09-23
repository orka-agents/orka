#!/usr/bin/env bash
# Orka — two teams, one URL
# Alice and Bob use the same AI address. Their tokens pick their teams, and each team's own Orka does the rest.
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"
cd "$repo_root"

here=demo/05-two-teams
router_ns=orka-router-system
router_url=http://127.0.0.1:8090
work=$demo_root/setup/state/05
mkdir -p "$work"
cd "$work"

# Claude Code reads the presenter's own settings; give each developer a
# config dir of their own so only the recorded environment applies.
claude_for() {
  local who=$1 small=$2
  mkdir -p "$work/$who"
  printf '{"permissions":{"defaultMode":"bypassPermissions"},"env":{"ANTHROPIC_SMALL_FAST_MODEL":"%s"}}\n' "$small" >"$work/$who/settings.json"
}
claude_for alice approved-models/claude-haiku-4.5
claude_for bob openai/gpt-5.5

# Keep previous work and reuse a healthy forward. Stop only forwards started
# by this recording when it exits.
_router_pf=
if ! curl -fsS -m 2 "$router_url/healthz" >/dev/null 2>&1; then
  kubectl -n "$router_ns" port-forward service/orka-compat-router 8090:8080 >/dev/null 2>&1 &
  _router_pf=$!
fi
# Each team's own API, for the Task lists and for the direct-door refusal.
# The router forwards only the model endpoints, so these go straight in.
payments_api=http://127.0.0.1:8091
inventory_api=http://127.0.0.1:8092
_pay_pf=
_inv_pf=
trap 'kill ${_router_pf:+$_router_pf} ${_pay_pf:+$_pay_pf} ${_inv_pf:+$_inv_pf} 2>/dev/null || true; stop_port_forward' EXIT
wait_for "the router port-forward" "curl -fsS -m 2 $router_url/healthz" 60
kubectl -n team-payments port-forward svc/orka-api 8091:8080 >/dev/null 2>&1 &
_pay_pf=$!
kubectl -n team-inventory port-forward svc/orka-api 8092:8080 >/dev/null 2>&1 &
_inv_pf=$!
wait_for "the payments API port-forward" "curl -fsS -m 2 $payments_api/healthz" 60
wait_for "the inventory API port-forward" "curl -fsS -m 2 $inventory_api/healthz" 60

# Claude Code resets terminal modes on exit when it owns a tty; a pipe keeps
# those escape codes out of the recording without changing its output.
# Every recorded call is one-shot; the flag is plumbing, so it stays off camera.
claude() { command claude --no-session-persistence "$@" 2>&1 | cat; }

# --- on-camera helpers ---------------------------------------------------
# routes — the router's whole configuration: which token namespace goes where.
routes() {
  kubectl -n "$router_ns" get configmap orka-compat-router -o jsonpath='{.data.routes\.yaml}'
}
# models_as NAME — the models the shared URL offers to that person's token.
models_as() {
  local who=$1 ns token
  case $who in alice) ns=team-payments; token=$ALICE ;; bob) ns=team-inventory; token=$BOB ;; esac
  orka models list --compat anthropic --server "$router_url" --namespace "$ns" --token "$token"
}
# team_task_records NAMESPACE — proxy Tasks created during these requests,
# kept off camera for the closing table.
team_task_records() {
  kubectl get tasks -n "$1" -l orka.ai/source=anthropic-proxy -o json |
    jq --arg started "$started" '.items |= map(select(.metadata.creationTimestamp >= $started))'
}

request="Run a container task that prints today's date and tell me what it printed."

banner "Orka — two teams, one URL" \
  "Alice and Bob use the same AI address. Their tokens pick their teams, and each team's own Orka does the rest."

say "Alice is on the payments team, Bob on inventory. Different models, different"
say "budgets, and they must never see each other's work. Both get one AI URL."
helpers_note routes, models_as

chapter "Two teams, one door"

say "Each team has its own namespace, a room of its own in the cluster, with"
say "a complete Orka inside: its own model key, Agents, and Tasks."
pe "kubectl get pods -n team-payments -l app=orka-controller"
pe "kubectl get pods -n team-inventory -l app=orka-controller"
pe "kubectl -n team-payments get providers"
pe "kubectl -n team-inventory get providers"
say "In front of both sits one small router. It reads the caller's token and"
say "forwards to that team. It holds no keys and runs no models."
pe "routes"
router_secret_refs=$(kubectl -n "$router_ns" get deployment orka-compat-router -o json |
  jq '[.spec.template.spec | .. | objects | select(has("secretKeyRef") or has("secretRef") or has("secret"))] | length')
((router_secret_refs == 0)) || { bad "the router has unexpected Secret references"; exit 1; }
ok "Two installations behind one URL. Each keeps its own work records."

chapter "Alice and Bob use the same URL"

say "The token is the badge: it says who you are and which room you belong to."
say "It goes where an API key normally goes."
pe "export ANTHROPIC_BASE_URL=$router_url/anthropic"
pe "ALICE=\$(kubectl -n team-payments create token alice)"
pe "BOB=\$(kubectl -n team-inventory create token bob)"
say "Ask the same URL which models it offers, once as Alice and once as Bob."
pe "models_as alice"
pe "models_as bob"
ok "One URL, two answers. The token chose the team; nothing in the request did."

chapter "The same request lands in different homes"

say "Both ask Claude Code for the same small thing."
pe "echo \"$request\""
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
export CLAUDE_CONFIG_DIR=$work/alice
p "ANTHROPIC_API_KEY=\$ALICE claude -p --model approved-models/claude-opus-4.7 \"\$request\""
ANTHROPIC_API_KEY=$ALICE claude -p --model approved-models/claude-opus-4.7 "$request" 2>"$work/alice.err" | sed -n '1,6p'
nap 0.8
export CLAUDE_CONFIG_DIR=$work/bob
p "ANTHROPIC_API_KEY=\$BOB claude -p --model openai/gpt-5.5 \"\$request\""
ANTHROPIC_API_KEY=$BOB claude -p --model openai/gpt-5.5 "$request" 2>"$work/bob.err" | sed -n '1,6p'
nap 0.8
team_task_records team-payments >"$work/payments-tasks.json"
team_task_records team-inventory >"$work/inventory-tasks.json"
for team in payments inventory; do
  jq -e 'any(.items[]; .spec.type == "container" and .status.phase == "Succeeded")' \
    "$work/$team-tasks.json" >/dev/null || { bad "no successful container Task for $team"; exit 1; }
done
say "Each request became a Task in its own team's installation, and nowhere else."
say "Each team's Orka answers with its own token."
pe "orka task list -s $payments_api -n team-payments -t \$ALICE --since $started"
pe "orka task list -s $inventory_api -n team-inventory -t \$BOB --since $started"
ok "Same words, two homes. Each team's Orka ran its own work on its own budget."

chapter "Bob knocks on the wrong door"

say "Bob asks for the payments team's model by name, through the same URL."
export CLAUDE_CONFIG_DIR=$work/bob
pex "ANTHROPIC_API_KEY=\$BOB claude -p --model approved-models/claude-opus-4.7 'Reply with OK' | tee model-refusal.txt"
grep -Fq 'provider "approved-models" not found in namespace "team-inventory"' model-refusal.txt ||
  { bad "the model request failed for a different reason"; exit 1; }
ok "Refused, with no fallback. The inventory installation has no such model."
say "And Bob going straight to the payments installation with his token:"
pex "orka task list -s $payments_api -n team-payments -t \$BOB 2>&1 | tee team-refusal.txt"
grep -Fq 'HTTP 403' team-refusal.txt && grep -Fq 'not allowed' team-refusal.txt &&
  grep -Fq 'team-payments' team-refusal.txt && grep -Fq 'team-inventory' team-refusal.txt ||
  { bad "the direct request did not produce the expected team-access refusal"; exit 1; }
ok "Refused again. Every installation checks identity, not just the router."


for team in payments inventory; do
  team_task_records "team-$team" >"$work/$team-tasks-after-refusals.json"
  [[ $(jq -c '[.items[].metadata.uid] | sort' "$work/$team-tasks.json") == \
     "$(jq -c '[.items[].metadata.uid] | sort' "$work/$team-tasks-after-refusals.json")" ]] ||
    { bad "the refused requests changed the $team Task list"; exit 1; }
done
pay_tasks=$(jq '.items | length' "$work/payments-tasks.json")
inv_tasks=$(jq '.items | length' "$work/inventory-tasks.json")
evidence \
  "URLs published" "1" \
  "Installations" "2 (team-payments, team-inventory)" \
  "Alice's Tasks" "$pay_tasks in team-payments" \
  "Bob's Tasks" "$inv_tasks in team-inventory" \
  "Cross-team requests" "2 attempted, 2 refused" \
  "Router Secret references" "$router_secret_refs"
cta
