#!/usr/bin/env bash
# Orka — a procedure the next agent can use
# One agent proposes a warehouse note. Jordan reviews and publishes it. A fresh agent then answers from it, with the trail on record.
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
here=$demo_root/10-reviewed-memory
scenario_init 10-reviewed-memory
scenario_connect

author=memory-author-$run_id
before=memory-before-$run_id
after=memory-after-$run_id
cp "$here/procedure.txt" procedure.txt
cp "$here/question.txt" question.txt
jq -n --arg namespace "$ORKA_NAMESPACE" --arg author "$author" --arg before "$before" --arg after "$after" \
  '{namespace:$namespace,authorTask:$author,beforeTask:$before,afterTask:$after}' >run.json

check_evidence() { python3 "$here/check.py" "$1"; }
capture_task() {
  local name=$1 label=$2
  wait_task "$name" 300
  orka task get "$name" -o json >"raw/$label-task.json"
  orka task result "$name" -o json >"raw/$label-result.json"
  scenario_collect_events "$name" "raw/$label-events.json"
}

# Refuse polluted comparisons. The worker loads active namespace memory at
# startup, so filtering only our example's tag would not isolate the reader.
orka memory list --limit 200 -o json >raw/memories-initial.json
check_evidence initial
for role in author reader; do
  kubectl -n "$ORKA_NAMESPACE" get agent "demo-memory-$role" -o json >"raw/$role-agent.json"
  jq -e '.metadata.labels["demo.orka.ai/name"] == "10-reviewed-memory" and .spec.providerRef.name != null and .spec.runtime == null' \
    "raw/$role-agent.json" >/dev/null
done
jq -r '.spec.systemPrompt.inline' raw/reader-agent.json >reader-rules.txt


jq -n --arg namespace "$ORKA_NAMESPACE" --arg name "$author" --rawfile note procedure.txt '
  {apiVersion:"core.orka.ai/v1alpha1",kind:"Task",
   metadata:{name:$name,namespace:$namespace,labels:{"demo.orka.ai/name":"10-reviewed-memory"}},
   spec:{type:"ai",agentRef:{name:"demo-memory-author"},timeout:"5m",retryPolicy:{maxRetries:0},
     prompt:("Propose the reusable warehouse return procedure from this operations note. Use remember exactly once, with the tag warehouse-returns.\n\n" + $note)}}
' >author-task.json
for stage in before after; do
  name=memory-$stage-$run_id
  jq -n --arg namespace "$ORKA_NAMESPACE" --arg name "$name" --rawfile question question.txt '
    {apiVersion:"core.orka.ai/v1alpha1",kind:"Task",
     metadata:{name:$name,namespace:$namespace,labels:{"demo.orka.ai/name":"10-reviewed-memory"}},
     spec:{type:"ai",agentRef:{name:"demo-memory-reader"},timeout:"5m",retryPolicy:{maxRetries:0},
       prompt:($question | rtrimstr("\n"))}}
  ' >"reader-$stage-task.json"
done

banner "Orka — a procedure the next agent can use" \
  "One agent proposes a warehouse note. Jordan reviews and publishes it. A fresh agent then answers from it, with the trail on record."
say "Jordan runs the warehouse. An assistant just read the return procedure, and"
say "the next assistant will need it too. Nothing becomes shared until Jordan says so."

chapter "1. Today's procedure"
pe "cat procedure.txt"
say "Memory here means saved team knowledge Orka hands to future work."
pe "orka memory list"
ok "No shared notes yet."

chapter "2. The agent proposes a note"
say "A Task is Orka's record of one piece of work. This one asks an assistant"
say "to propose the reusable part of the procedure."
pe "orka task create -f author-task.json | tee raw/author-create.txt"
capture_task "$author" author
orka memory proposal list --task-name "$author" --limit 2 -o json >raw/proposals.json
target=$(jq -er '.items | select(length == 1) | .[0].id' raw/proposals.json)
[[ $target =~ ^mprop-[a-z0-9-]+$ ]] || { bad "unexpected proposal identity"; exit 1; }
orka memory proposal get "$target" -o json >raw/proposal-pending.json
orka memory list --limit 200 -o json >raw/memories-before.json
check_evidence author
say "The Task's events show which tools the agent called."
pe "orka task events $author --type ToolCallCompleted"
say "The remember tool files a proposal. A person decides whether to publish it."
pe "orka memory proposal get $target"
check_evidence author
ok "A pending proposal with its text and the Task it came from. Nothing is shared."

chapter "3. A fresh agent does not know it yet"
say "A different assistant gets only this question, in a new Task, with no"
say "conversation history."
pe "cat question.txt"
pe "orka task create -f reader-before-task.json | tee raw/before-create.txt"
capture_task "$before" before
say "The reader answers as a small record: whether a reviewed procedure was"
say "available, the dock, the label, and a sentence for the customer."
pe "orka task result $before"
check_evidence before
say "Its events show no tool calls: it did not search for the answer."
pe "orka task events $before --type ToolCallStarted --type ToolCallCompleted --type ToolCallFailed"
ok "It does not know, and it did not guess. No tools, no shared conversation."

chapter "4. Jordan reviews and publishes it"
say "Jordan checks the proposal against the operations note and accepts it."
pe "orka memory proposal review $target --status accepted --note 'Matches the operations note.'"
orka memory proposal get "$target" -o json >raw/proposal-accepted.json
orka memory list --limit 200 -o json >raw/memories-accepted.json
check_evidence accepted
pe "orka memory list"
say "Accepting records the decision. Publishing is a second, explicit step."
pe "orka memory proposal apply $target | tee raw/apply-response.txt"
orka memory proposal get "$target" -o json >raw/proposal-applied.json
orka memory list --limit 200 -o json >raw/memories-applied.json
orka memory list --tags warehouse-returns -o json >raw/memories-tagged.json
check_evidence applied
pe "orka memory list"
ok "One shared note, and it still names the proposal it came from."

chapter "5. A fresh agent knows it now"
say "Same reader, same question, another new Task. No note and no history passed in."
pe "orka task create -f reader-after-task.json | tee raw/after-create.txt"
capture_task "$after" after
pe "orka task result $after"
check_evidence after
orka memory list --limit 200 -o json >raw/memories-after.json
kubectl -n "$ORKA_NAMESPACE" get agent demo-memory-reader -o json >raw/reader-agent-after.json
check_evidence summary
pe "orka task events $after --type ToolCallStarted --type ToolCallCompleted --type ToolCallFailed"
ok "Dock 3 and the right label, from the reviewed note alone."

say "Every step above is a record, and the records link to each other."
pe "cat evidence.txt"
note "Full responses and evidence: $run_dir"
cta
