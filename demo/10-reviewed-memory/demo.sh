#!/usr/bin/env bash
# A warehouse procedure becomes shared knowledge after review and explicit apply.
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

banner "A procedure the next agent can use" \
  "One assistant proposes a note. A person decides when it becomes shared knowledge."

chapter "1. Today's procedure"
say "An assistant has read a warehouse procedure for returned filters."
say "The next person handling a return will need the same instructions."
pe "cat procedure.txt"
say "Here, memory means saved project information Orka supplies to future work."
say "We start with no shared notes available to these agents."
pe "jq '{sharedNotes: (.items // [] | length)}' raw/memories-initial.json"

chapter "2. The agent proposes a note"
say "A Task is Orka's record of one piece of work. This task gets the note."
say "We ask an assistant to propose the reusable procedure for review."
pe "orka task create -f author-task.json | tee raw/author-create.txt"
capture_task "$author" author
orka task result "$author" -o json >raw/author-result.json
orka memory proposal list --task-name "$author" --limit 2 -o json >raw/proposals.json
proposal=$(jq -er '.items | select(length == 1) | .[0].id' raw/proposals.json)
[[ $proposal =~ ^mprop-[a-z0-9-]+$ ]] || { bad "unexpected proposal identity"; exit 1; }
orka memory proposal get "$proposal" -o json >raw/proposal-pending.json
orka memory list --limit 200 -o json >raw/memories-before.json
check_evidence author
pe "jq -r '.events[] | select(.toolName == \"remember\") | [.type, .toolName] | @tsv' raw/author-events.json"
say "remember is a tool the agent can call. It creates a proposal for review."

chapter "3. Inspect the proposal"
pe "orka memory proposal list --task-name $author -o json | tee raw/proposals.json | jq '.items[] | {id,status}'"
pe "orka memory proposal get $proposal -o json | tee raw/proposal-pending.json | jq '{content,taskName,status}'"
check_evidence author
say "The record keeps the proposed text and the task it came from."
say "Its pending status means it is waiting for a decision."

chapter "4. Ask a fresh agent"
say "A different agent gets only this question in a fresh task."
pe "cat question.txt"
say "We ask it to use reviewed notes already supplied with its task."
pe "sed -n '1,4p' reader-rules.txt"
pe "orka task create -f reader-before-task.json | tee raw/before-create.txt"
capture_task "$before" before
pe "orka task result $before -o json | tee raw/before-result.json | jq -r '.result | fromjson | .answer'"
check_evidence before
pe "jq '{sharedSession,toolCalls}' before-evidence.json"
say "A Session carries a conversation between tasks. This task shares none."
say "The saved events also confirm that this reader called no tools."

chapter "5. Review it"
say "The presenter checks the proposal against the operations note and accepts it."
pe "orka memory proposal review $proposal --status accepted --note 'Checked against the operations note.' | tee raw/review-response.txt"
pe "orka memory proposal get $proposal -o json | tee raw/proposal-accepted.json | jq '{status,reviewer,appliedMemoryId}'"
pe "orka memory list --limit 200 -o json | tee raw/memories-accepted.json | jq '{sharedNotes: (.items // [] | length)}'"
check_evidence accepted
say "Accepting records the review decision. Publishing the note is a separate action."

chapter "6. Apply it"
pe "orka memory proposal apply $proposal | tee raw/apply-response.txt"
orka memory proposal get "$proposal" -o json >raw/proposal-applied.json
orka memory list --limit 200 -o json >raw/memories-applied.json
check_evidence applied
pe "orka memory list --tags warehouse-returns -o json | tee raw/memories-tagged.json | jq '.items[] | {content,sourceProposalId}'"
say "The reviewed procedure is now shared memory. It still names its source proposal."

chapter "7. Ask the fresh question again"
say "We start another new task with the same reader and the same question."
say "We do not pass it the operations note or the earlier conversation."
pe "cat question.txt"
pe "orka task create -f reader-after-task.json | tee raw/after-create.txt"
capture_task "$after" after
pe "orka task result $after -o json | tee raw/after-result.json | jq -r '.result | fromjson | .answer'"
check_evidence after
orka memory list --limit 200 -o json >raw/memories-after.json
kubectl -n "$ORKA_NAMESPACE" get agent demo-memory-reader -o json >raw/reader-agent-after.json
check_evidence summary
pe "jq '{sharedSession,toolCalls,dock,label}' after-evidence.json"
say "This reader called no tools. It answered from the reviewed note Orka supplied."

chapter "8. Trace the answer back"
say "These links come from the saved records for this run."
pe "cat evidence.txt"
say "One agent proposed a procedure. A person reviewed and applied it."
say "A later agent used that knowledge, with a record linking it to its source."
printf '\nFull responses and evidence: %s\n' "$run_dir"
