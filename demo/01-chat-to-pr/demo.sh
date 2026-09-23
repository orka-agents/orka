#!/usr/bin/env bash
# Orka — from a chat message to a pull request
# Maya asks for a change in Claude Code. Orka runs the agents on her team's cluster. No model key ever reaches her laptop.
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"

here=$repo_root/demo/01-chat-to-pr
work=$demo_root/setup/state/01
mkdir -p "$work"
# The viewer sees `request.md` next to the commands, the way a developer
# keeps a note next to their work, not the recorder's state directory.
cp "$here/request.md" "$work/request.md"
cd "$work"

# Claude Code reads the presenter's own ~/.claude/settings.json, which may
# already pin a base URL or key. Give the demo a config dir of its own so the
# recorded environment variables are the ones that take effect.
export CLAUDE_CONFIG_DIR=$work/claude
mkdir -p "$CLAUDE_CONFIG_DIR"
# Claude Code also uses a small model for housekeeping calls; name one the
# Provider serves, or those calls fail noisily in the controller log.
printf '{"permissions":{"defaultMode":"bypassPermissions"},"env":{"ANTHROPIC_SMALL_FAST_MODEL":"copilot/claude-haiku-4.5"}}\n' >"$CLAUDE_CONFIG_DIR/settings.json"

# --- on-camera helpers ---------------------------------------------------
# readme — the service's API table, straight from the repository.
readme() {
  curl -fsS https://raw.githubusercontent.com/sozercan/orka-demo-inventory/main/README.md | sed -n '13,19p'
}
# tasks — one row per child Task, described by role rather than raw spec fields.
task_records() {
  local records
  records=$(kubectl -n "$ORKA_NAMESPACE" get tasks -l orka.ai/source=anthropic-proxy \
    --sort-by=.metadata.creationTimestamp -o json |
    jq -c --arg started "$started" '.items |= map(select(.metadata.creationTimestamp >= $started))') || return
  # The live list can lose a failed Task before the coordinator returns.
  # Retain each observation so the closing count includes what viewers saw.
  printf '%s\n' "$records" >>"$work/task-history.jsonl" || return
  printf '%s\n' "$records"
}
tasks() {
  task_records | jq -r '
    ["TASK","ROLE","PHASE"],
    (.items[] | [
      .metadata.name,
      (if .spec.type == "container" then
         "validate in " + (.spec.image // "the worker image")
       elif .spec.agentRef.name == "codex-coder" then
         (if .spec.workspace.intent == "write" then "implement (codex-coder)" else "inspect (codex-coder)" end)
       elif .spec.agentRef.name == "claude-reviewer" then "review (claude-reviewer)"
       elif .spec.agentRef.name == "analyst" then "analyse (analyst)"
       else .spec.type + " (" + (.spec.agentRef.name // "-") + ")" end),
      (.status.phase // "Pending")
    ]) | @tsv' | column -t -s $'\t'
}
watch_tasks_by_role() {
  local until=$1 interval=${2:-10} last="" now
  while true; do
    now=$(tasks 2>/dev/null || true)
    if [[ $now != "$last" ]]; then
      printf '%s── %s ──%s\n' "$C_DIM" "$(date -u +%H:%M:%S)" "$C_RESET"
      printf '%s\n' "$now"
      last=$now
    fi
    if eval "$until" >/dev/null 2>&1; then return 0; fi
    sleep "$interval"
  done
}
# last_message TASK — the agent's final progress message, one line.
last_message() {
  orka task events "$1" | grep ModelMessage | tail -n 1 | cut -c1-240
}
# pr_checks URL — title, state, branch, and CI result from GitHub itself.
pr_checks() {
  gh pr view "$1" --json title,state,headRefName,statusCheckRollup \
    --jq '"title:  " + .title, "state:  " + .state, "branch: " + .headRefName,
          ("checks: " + ([.statusCheckRollup[]? | (.name // .context) + "=" + (.conclusion // .state)] | join(", ")))'
}

# Only close a PR whose publication, exact-commit review, and CI were checked
# by a completed run. A chat answer alone is not an ownership receipt.
accepted_run=$work/accepted-run.json
if [[ -f $accepted_run ]]; then
  jq -e '.accepted == true and (.headSHA | test("^[a-f0-9]{40}$")) and
    (.branch | startswith("orka/"))' "$accepted_run" >/dev/null || {
    bad "the previous demo run has no valid cleanup receipt"; exit 1;
  }
  previous_pr=$(jq -r .pullRequest "$accepted_run")
  [[ $previous_pr =~ ^https://github.com/sozercan/orka-demo-inventory/pull/[0-9]+$ ]] || {
    bad "the previous demo receipt names an unexpected repository"; exit 1;
  }
  previous_record=$(gh pr view "$previous_pr" --json state,headRefName,headRefOid)
  if jq -e --slurpfile accepted "$accepted_run" '.state == "OPEN" and
      .headRefName == $accepted[0].branch and .headRefOid == $accepted[0].headSHA' \
      <<<"$previous_record" >/dev/null; then
    gh pr close "$previous_pr" >/dev/null
  fi
  # Keep published branches so earlier recordings retain their source code.
fi
ensure_port_forward
# The connect chapter shows these steps on camera; run them quietly first so
# the earlier CLI calls have a live token too.
orka_connect

banner "Orka — from a chat message to a pull request" \
  "Maya asks for a change in Claude Code. Orka runs the agents on her team's cluster. No model key ever reaches her laptop."

say "Maya is a developer on the inventory team. Her service has no health check."
say "She will ask for one in chat and end with a reviewed pull request."
helpers_note readme, tasks, last_message, pr_checks

chapter "Maya has no model key"

say "A Provider is a saved connection to a model service. It refers to a"
say "Secret that keeps the model key in the cluster."
pe "orka provider list"
say "The platform team also defined three Agents: a coder, a reviewer, and an analyst."
pe "orka agent list"
say "Maya gets a token for the cluster, not a key for a model vendor."
pe "orka config set-server $ORKA_API"
pe "orka config set-namespace $ORKA_NAMESPACE"
pe "orka config set-token --file <(kubectl -n $ORKA_NAMESPACE create token $ORKA_CLIENT_SA)"
pe "orka status | sed -n '1,4p'"
ok "Maya is connected with a cluster token. The model key stayed where it was."

chapter "Maya asks in chat"

say "The service today: three endpoints, none of them a health check."
pe "readme"
say "Claude Code talks to Orka instead of the vendor. Two variables do that."
pe "export ANTHROPIC_BASE_URL=$ORKA_API/anthropic"
pe "export ANTHROPIC_API_KEY=\$(kubectl -n $ORKA_NAMESPACE create token $ORKA_CLIENT_SA)"
pe "orka models list --compat anthropic | sed -n '1,5p'"
say "The request names the change, a test, the docs, and a pull request. It"
say "does not choose which Agents do the work. Its Git credential is named,"
say "but the credential's value stays in the cluster."
pe "cat request.md"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
: >"$work/task-history.jsonl"
p "claude -p --model copilot/claude-opus-4.7 \"\$(cat request.md)\" > answer.md &"
claude -p --model copilot/claude-opus-4.7 --no-session-persistence "$(cat request.md)" \
  >answer.md 2>"$work/claude.err" &
claude_pid=$!
nap 0.6
ok "Sent. Claude Code is waiting for an answer; Orka is starting the work."

chapter "Orka turns the chat into Tasks"

say "Each row is a Task: one piece of work Orka runs and keeps a record of."
say "Orka decides how many to run and in what order. A Failed row needs"
say "inspection; we will check the completed work before calling it done."
say "Quiet stretches are cut from the recording."
wait_for "the coordinator's first Task" \
  "task_records | jq -e '.items | length > 0'" 600
watch_tasks_by_role "! kill -0 $claude_pid" 12
wait "$claude_pid" || {
  bad "claude exited with an error"
  cat "$work/claude.err" >&2
  exit 1
}
ok "The chat turn returned."

chapter "Open the Task that made the pull request"

# Inspect the Task whose publication became the pull request, so the receipt
# on screen is the receipt for the branch GitHub shows in the next chapter.
pr=$(pr_url_from "$(cat answer.md)")
assert_pr "$pr"
gh pr view "$pr" --json headRefName,headRefOid >"$work/pr-head.json"
pr_branch=$(jq -r .headRefName "$work/pr-head.json")
pr_commit=$(jq -r .headRefOid "$work/pr-head.json")
task_records >"$work/tasks.json"
jq -s '{items: (reduce (.[].items[]) as $task ({}; .[$task.metadata.uid] = $task)
  | [.[]] | sort_by(.metadata.creationTimestamp))}' \
  "$work/task-history.jsonl" >"$work/observed-tasks.json"
coder=$(jq -r --arg branch "$pr_branch" '
    [.items[] | select(.spec.type == "agent" and .status.delivery.branch == $branch and .status.delivery.state == "VerifiedExact")]
    | last | .metadata.name // empty' "$work/tasks.json")
[[ -n $coder ]] || { bad "no implement Task found for $pr_branch"; exit 1; }
jq -e --arg name "$coder" --arg commit "$pr_commit" '
  any(.items[]; .metadata.name == $name and .status.phase == "Succeeded" and
    .status.delivery.state == "VerifiedExact" and .status.delivery.verifiedRemoteSHA == $commit)
  ' "$work/tasks.json" >/dev/null || { bad "the pull request has no successful VerifiedExact Task"; exit 1; }
review_count=$(jq --arg repo "$DEMO_REPO" --arg branch "$pr_branch" --arg commit "$pr_commit" '
  [.items[] | select(.spec.agentRef.name == "claude-reviewer" and .status.phase == "Succeeded" and
    .spec.workspace.gitRepo == $repo and .spec.workspace.branch == $branch and
    .spec.workspace.intent == "read" and .status.delivery.state == "ReadValidated" and
    .status.delivery.startingSHA == $commit)] | length' "$work/tasks.json")
((review_count > 0)) || { bad "no successful reviewer Task for this pull request's exact commit"; exit 1; }
say "The coder worked in a checkout of the repository. Orka kept what it said."
pe "last_message $coder"
say "The coder never pushed. Orka's Publisher, which alone holds the Git"
say "token, verified the files and published the branch. The Task has the receipt."
pe "task_summary $coder"
ok "Delivery VerifiedExact: what reached GitHub is exactly what Orka checked."

chapter "GitHub shows the pull request"

say "Maya's answer arrived in the chat window."
pe "cat answer.md"
say "Now check it somewhere Orka does not control."
wait_for "GitHub to register CI checks" \
  "gh pr view $pr --json statusCheckRollup | jq -e '.statusCheckRollup | length > 0'" 120
pe "gh pr checks $pr --watch --fail-fast --interval 5"
gh pr view "$pr" --json title,state,headRefName,headRefOid,statusCheckRollup >"$work/pull-request.json"
jq -e --arg commit "$pr_commit" '.state == "OPEN" and .headRefOid == $commit and
  any(.statusCheckRollup[]?; .conclusion == "SUCCESS" or .state == "SUCCESS")' \
  "$work/pull-request.json" >/dev/null || { bad "the open pull request has no passing check"; exit 1; }
pe "pr_checks $pr"
pe "gh pr diff $pr --name-only"
ok "A reviewed pull request with a passing check, from one chat message."

task_count=$(jq '.items | length' "$work/observed-tasks.json")
failed_count=$(jq -s '[.[].items[] | select(.status.phase == "Failed") | .metadata.uid]
  | unique | length' "$work/task-history.jsonl")
checks=$(jq -r '[.statusCheckRollup[]? | (.conclusion // .state)] | join(",")' "$work/pull-request.json")
evidence \
  "Chat messages sent" "1" \
  "Tasks observed in this run" "$task_count ($failed_count failed Tasks observed)" \
  "Reviews of this exact PR commit" "$review_count" \
  "Pull request" "$pr" \
  "CI on the pull request" "${checks:-none reported}" \
  "Publication" "$(kubectl -n "$ORKA_NAMESPACE" get task "$coder" -o jsonpath='{.status.delivery.state}')" \
  "Maya's authentication" "cluster token for $ORKA_CLIENT_SA"
cta

# Write this only after every evidence check and the closing summary passed.
receipt_tmp=$(mktemp "$work/accepted-run.XXXXXX")
jq -n --arg pr "$pr" --arg branch "$pr_branch" --arg sha "$pr_commit" --arg task "$coder" \
  '{accepted:true, pullRequest:$pr, branch:$branch, headSHA:$sha, coderTask:$task}' >"$receipt_tmp"
mv "$receipt_tmp" "$accepted_run"
