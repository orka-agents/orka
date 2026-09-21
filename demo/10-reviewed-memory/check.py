#!/usr/bin/env python3
"""Check captured Orka records before the walkthrough makes its next claim."""

import json
import re
import sys
from pathlib import Path

DOCK = "Dock 3"
LABEL = "RET-4827"
TAG = "warehouse-returns"


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(name):
    return json.loads(Path(name).read_text())


def items(document):
    require("items" in document, "list response has no items field")
    value = document["items"]
    require(value is None or isinstance(value, list), "invalid list response")
    return value or []


def empty_memories(document):
    require(not items(document), "active shared memory already exists; use a clean demo namespace or disable only this demo's earlier note")


def procedure(content):
    require(isinstance(content, str) and DOCK in content and LABEL in content,
            "the exact dock and return label are missing")


def complete_events(document, task_name, namespace):
    require(document["streamID"] == task_name and document["streamType"] == "task"
            and document["namespace"] == namespace,
            "event history belongs to another task")
    events = document["events"]
    require([event["seq"] for event in events] == list(range(1, document["latestSeq"] + 1)),
            "event history is incomplete or out of order")
    require(all(event.get("taskName") == task_name for event in events),
            "event history contains another task")
    types = [event["type"] for event in events]
    for required in ("WorkerStarted", "WorkerCompleted", "ResultSubmitted", "TaskSucceeded"):
        require(required in types, f"missing {required} evidence")
    require(types.count("WorkerStarted") == 1, "more than one worker attempt")
    require(not set(types) & {"WorkerFailed", "TaskFailed", "TaskCancelled", "ModelRequestFailed"},
            "the task did not complete cleanly")
    groups = {}
    for event_type in ("ModelRequestStarted", "ModelRequestCompleted", "ModelMessage"):
        group = [event for event in events if event["type"] == event_type]
        require(group, f"missing {event_type} evidence")
        iterations = [event["content"]["iteration"] for event in group]
        require(iterations == list(range(1, len(iterations) + 1)),
                f"incomplete {event_type} history")
        groups[event_type] = group
    require(len({len(group) for group in groups.values()}) == 1,
            "model requests, responses, and messages do not match")
    require(groups["ModelRequestStarted"][0]["content"]["messageCount"] == 1,
            "the task started with conversation history")
    for event in groups["ModelRequestCompleted"] + groups["ModelMessage"]:
        count = event["content"].get("toolCalls")
        require(type(count) is int and count >= 0, "missing model tool-call count")
    return events


def task(document, run, key, agent_name, prompt):
    require(document["metadata"]["name"] == run[key], "wrong task record")
    require(document["metadata"]["namespace"] == run["namespace"], "wrong task namespace")
    require(document["status"]["phase"] == "Succeeded", "task did not succeed")
    spec = document["spec"]
    require(spec["type"] == "ai" and spec["agentRef"]["name"] == agent_name,
            "expected the configured native AI agent")
    require(spec["prompt"] == prompt, "task prompt differs from the saved request")
    for key in ("sessionRef", "priorTaskRef", "workspace", "ai", "env", "execution"):
        require(not spec.get(key), f"unexpected task input: {key}")


def reader(document, events, result, run, stage, agent):
    prompt = Path("question.txt").read_text().strip()
    task(document, run, f"{stage}Task", "demo-memory-reader", prompt)
    spec = agent["spec"]
    for key in ("skills", "runtime", "coordination", "tools", "execution", "session", "secretRef"):
        require(not spec.get(key), f"unexpected reader configuration: {key}")
    require(not spec.get("systemPrompt", {}).get("configMapRef"), "reader loads an extra prompt")
    supplied = json.dumps({"agent": spec, "prompt": prompt}).casefold()
    require(DOCK.casefold() not in supplied and LABEL.casefold() not in supplied,
            "reader was given the procedure directly")
    history = complete_events(events, run[f"{stage}Task"], run["namespace"])
    tool_events = [event for event in history if event["type"].startswith("ToolCall")]
    require(not tool_events, "reader used a tool; the before-and-after comparison is invalid")
    model_counts = [event["content"]["toolCalls"] for event in history
                    if event["type"] in ("ModelRequestCompleted", "ModelMessage")]
    require(not any(model_counts), "model requested a tool, even though a tool event may be missing")
    answer = json.loads(result["result"])
    require(isinstance(answer["answer"], str) and answer["answer"].strip(), "reader has no answer")
    if stage == "before":
        require(answer["reviewedProcedureAvailable"] is False,
                "reader claimed a reviewed procedure before publication")
        require(answer["dock"] is None and answer["label"] is None, "reader invented warehouse instructions")
        require(not re.search(r"\bDock\s+\d|\bRET-\d", answer["answer"], re.IGNORECASE),
                "reader's answer contains unreviewed instructions")
        require(any(word in answer["answer"].lower() for word in ("unavailable", "not available", "no reviewed")),
                "reader did not explain that the reviewed procedure is unavailable")
    else:
        require(answer["reviewedProcedureAvailable"] is True, "reader did not receive the reviewed procedure")
        require(answer["dock"] == DOCK and answer["label"] == LABEL, "reader did not recall the exact procedure")
        procedure(answer["answer"])
    return {"task": run[f"{stage}Task"], "sharedSession": bool(document["spec"].get("sessionRef")),
            "toolCalls": len(tool_events), **answer}


def proposal(document, run, status):
    require(document["namespace"] == run["namespace"], "wrong proposal namespace")
    require(document.get("taskName") == run["authorTask"], "proposal came from another task")
    require(document.get("agentName") == "demo-memory-author", "proposal came from another agent")
    require(document["type"] == "memory" and document["status"] == status, "unexpected proposal type or status")
    procedure(document["content"])
    require(TAG in document.get("description", ""), "proposal omitted the warehouse-returns tag")
    if status != "applied":
        require(not document.get("appliedMemoryId") and not document.get("appliedAt"), "proposal was applied too early")
    if status in ("accepted", "applied"):
        require(document.get("reviewer") and document.get("reviewedAt"), "proposal has no review record")


def same_proposal(previous, current):
    for field in ("id", "namespace", "taskName", "agentName", "type", "content", "description"):
        require(previous.get(field) == current.get(field), f"proposal changed during review: {field}")


def applied_memory(memory, applied, run):
    require(memory["id"] == applied["appliedMemoryId"], "applied memory ID does not match")
    require(memory["sourceProposalId"] == applied["id"] and memory["source"] == "memory_proposal",
            "memory is not linked to the reviewed proposal")
    require(memory["namespace"] == run["namespace"] and memory["taskName"] == run["authorTask"],
            "memory lost its originating task")
    require(memory["agentName"] == "demo-memory-author", "memory lost its originating agent")
    require(not memory["disabled"] and not memory["deleted"], "memory is unavailable to future tasks")
    require(memory["content"] == applied["content"] and TAG in memory["tags"], "saved note differs from the proposal")
    procedure(memory["content"])


def check(stage):
    run = read("run.json")
    if stage == "initial":
        empty_memories(read("raw/memories-initial.json"))
    elif stage == "author":
        original = read("author-task.json")
        task(read("raw/author-task.json"), run, "authorTask", "demo-memory-author", original["spec"]["prompt"])
        history = complete_events(read("raw/author-events.json"), run["authorTask"], run["namespace"])
        calls = [event for event in history if event["type"] == "ToolCallStarted"]
        done = [event for event in history if event["type"] == "ToolCallCompleted"]
        require(len(calls) == len(done) == 1 and calls[0]["toolName"] == done[0]["toolName"] == "remember",
                "expected exactly one successful remember call")
        require(calls[0].get("toolCallID") and calls[0]["toolCallID"] == done[0]["toolCallID"], "remember completion does not match")
        require(not any(event["type"] == "ToolCallFailed" for event in history), "remember failed")
        proposals = items(read("raw/proposals.json"))
        require(len(proposals) == 1, "expected one proposal from the author task")
        pending = read("raw/proposal-pending.json")
        require(proposals[0]["id"] == pending["id"], "proposal list and detail do not match")
        proposal(pending, run, "pending")
        empty_memories(read("raw/memories-before.json"))
    elif stage in ("before", "after"):
        result = reader(read(f"raw/{stage}-task.json"), read(f"raw/{stage}-events.json"),
                        read(f"raw/{stage}-result.json"), run, stage, read("raw/reader-agent.json"))
        Path(f"{stage}-evidence.json").write_text(json.dumps(result, indent=2) + "\n")
    elif stage == "accepted":
        accepted = read("raw/proposal-accepted.json")
        same_proposal(read("raw/proposal-pending.json"), accepted)
        proposal(accepted, run, "accepted")
        empty_memories(read("raw/memories-accepted.json"))
    elif stage == "applied":
        applied = read("raw/proposal-applied.json")
        accepted = read("raw/proposal-accepted.json")
        same_proposal(accepted, applied)
        proposal(applied, run, "applied")
        require(applied.get("appliedAt") and applied.get("appliedBy"), "proposal has no application record")
        require(applied["reviewedAt"] == accepted["reviewedAt"] and applied["reviewer"] == accepted["reviewer"],
                "application changed the review decision")
        memories = items(read("raw/memories-applied.json"))
        require(len(memories) == 1, "expected exactly one shared note after applying")
        applied_memory(memories[0], applied, run)
    elif stage == "summary":
        for earlier in ("initial", "author", "before", "accepted", "applied", "after"):
            check(earlier)
        require(read("raw/reader-agent.json")["spec"] == read("raw/reader-agent-after.json")["spec"],
                "reader configuration changed during the comparison")
        require(items(read("raw/memories-applied.json")) == items(read("raw/memories-after.json")),
                "shared memory changed during the final reader task")
        require(items(read("raw/memories-tagged.json")) == items(read("raw/memories-applied.json")),
                "the tag-filtered memory list does not match the applied note")
        before, after = read("before-evidence.json"), read("after-evidence.json")
        pending, accepted, applied = (read(f"raw/proposal-{state}.json") for state in ("pending", "accepted", "applied"))
        memory = items(read("raw/memories-applied.json"))[0]
        evidence = {"sourceTask": run["authorTask"], "proposal": pending["id"],
                    "reviewer": accepted["reviewer"], "reviewedAt": accepted["reviewedAt"],
                    "acceptedMemoryCount": len(items(read("raw/memories-accepted.json"))),
                    "memory": memory["id"], "sourceProposalId": memory["sourceProposalId"],
                    "appliedAt": applied["appliedAt"], "before": before, "after": after}
        Path("evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
        lines = [f"Original Task   {evidence['sourceTask']}", f"Proposal        {evidence['proposal']}",
                 f"Review          {accepted['status']} by {evidence['reviewer']}",
                 f"After review    {evidence['acceptedMemoryCount']} shared notes",
                 f"Applied memory  {evidence['memory']}", f"Later Task      {after['task']}",
                 f"Reader tools    before {before['toolCalls']}, after {after['toolCalls']}",
                 f"Recalled        {after['dock']}, {after['label']}"]
        Path("evidence.txt").write_text("\n".join(lines) + "\n")
    else:
        raise ValueError(f"unknown evidence check: {stage}")


if __name__ == "__main__":
    try:
        require(len(sys.argv) == 2, "usage: check.py initial|author|before|accepted|applied|after|summary")
        check(sys.argv[1])
    except (KeyError, IndexError, TypeError, ValueError, OSError) as error:
        print(f"Evidence check failed: {error}", file=sys.stderr)
        sys.exit(1)
