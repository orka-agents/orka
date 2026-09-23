#!/usr/bin/env python3
"""Check actual A2A, Orka, and Kubernetes records before narrating a result."""

import argparse
import base64
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import sys


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    return json.loads(Path(path).read_text())


def task_reference(task):
    parts = task["id"].split(".")
    require(len(parts) == 3 and parts[0] == "a2a1", "unrecognized public Task reference")
    decoded = []
    for part in parts[1:]:
        require(re.fullmatch(r"[A-Za-z0-9_-]+", part), "invalid public Task reference")
        decoded.append(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)).decode())
    require(re.fullmatch(r"[A-Za-z0-9._-]+", decoded[0]), "unsafe event identity")
    return decoded


def canonical_time(value):
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(parsed.tzinfo is not None, "admission time is missing its timezone")
    fraction = re.search(r"\.(\d+)(?:Z|[+-]\d\d:\d\d)$", value)
    digits = fraction[1].rstrip("0") if fraction else ""
    return parsed.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + ("." + digits if digits else "") + "Z"


def correlate(a2a, event, task):
    event_id, admitted = task_reference(a2a)
    require(event["id"] == event_id and canonical_time(event["createdAt"]) == admitted,
            "public reference does not identify this admission")
    require(a2a["contextId"] == event["threadId"], "conversation identity changed")
    require(event["gatewayName"] == "demo-a2a" and event["agentName"] == "demo-a2a-inventory",
            "event belongs to another route")
    require(event.get("taskUid") and event["taskUid"] == task["metadata"]["uid"], "Task UID does not match the saved event")
    require(event["taskName"] == task["metadata"]["name"], "Task name does not match the saved event")
    require(event["namespace"] == task["metadata"]["namespace"], "Task namespace changed")
    require(event.get("sessionName") and event["sessionName"] == task["spec"]["sessionRef"]["name"], "Session identity changed")
    require(task["metadata"]["annotations"]["gateway.orka.ai/event-id"] == event_id, "Task belongs to another event")


def reply(a2a, result):
    require(a2a["status"]["state"] == "TASK_STATE_COMPLETED", "A2A did not return a completed result")
    artifacts = a2a.get("artifacts", [])
    require(len(artifacts) == 1 and artifacts[0]["artifactId"] == "final", "expected one final answer")
    parts = artifacts[0].get("parts", [])
    require(len(parts) == 1 and isinstance(parts[0].get("text"), str), "expected one text answer")
    answer = parts[0]["text"]
    require(answer.strip() and answer.strip() == result["result"].strip(), "A2A and Orka returned different answers")
    require(all(re.search(pattern, answer, re.IGNORECASE) for pattern in (r"\b18\b", r"\b6\b", r"\btomorrow\b")),
            "the answer did not retain the supplied quantities and delivery timing")
    return answer


def task_set(snapshot, session=None):
    items = snapshot["items"]
    if session is not None:
        items = [task for task in items if task.get("spec", {}).get("sessionRef", {}).get("name") == session]
    values = {(task["metadata"]["name"], task["metadata"]["uid"]) for task in items}
    require(len(values) == len(items), "snapshot contains duplicate Task identities")
    return values


def retry_check(raw):
    first = read(raw / "first-admission.json")
    retry = read(raw / "retry.json")
    require(first["id"] == retry["id"] and first["contextId"] == retry["contextId"], "retry created a new public Task reference")
    event = read(raw / "first-completed-event.json")
    correlate(retry, read(raw / "retry-event.json"), read(raw / "retry-task.json"))
    original = {(event["taskName"], event["taskUid"])}
    before = task_set(read(raw / "tasks-after-first.json"), event["sessionName"])
    after = task_set(read(raw / "tasks-after-retry.json"), event["sessionName"])
    require(before == after == original, "retry changed the actual Orka Task identities")
    return len(after)


def ready_pods(snapshot):
    return {pod["metadata"]["uid"] for pod in snapshot["items"]
            if not pod["metadata"].get("deletionTimestamp")
            and any(c["type"] == "Ready" and c["status"] == "True" for c in pod.get("status", {}).get("conditions", []))}


def conversation(snapshot):
    tasks = snapshot["items"]
    require(len(task_set(snapshot)) == 2, "expected two distinct Tasks")
    require(len({task["spec"]["sessionRef"]["name"] for task in tasks}) == 1, "Tasks do not share one Session")
    require(all(task["status"]["phase"] == "Succeeded" for task in tasks), "both Tasks must finish first")
    print(f"{'TASK (short name)':22} {'SESSION (short name)':34} STATUS")
    for task in tasks:
        print(f"{task['metadata']['name'][:15] + '...':22} {task['spec']['sessionRef']['name'][:27] + '...':34} {task['status']['phase']}")


def summary(raw):
    """The closing table without the adapter-replacement chapter."""
    retry_count = retry_check(raw)
    first = read(raw / "first-completed-event.json")
    followup = read(raw / "followup-completed-event.json")
    require(first["state"] == followup["state"] == "Completed", "the saved requests did not both complete")
    correlate(read(raw / "first-answer.json"), first, read(raw / "first-task.json"))
    correlate(read(raw / "followup-answer.json"), followup, read(raw / "followup-task.json"))
    require(first["sessionName"] == followup["sessionName"], "follow-up did not share the Session")
    require(first["taskUid"] != followup["taskUid"], "follow-up did not produce a distinct Task")
    require(first["threadId"] == followup["threadId"], "follow-up changed the conversation ID")
    reply(read(raw / "first-answer.json"), read(raw / "first-result.json"))
    final = reply(read(raw / "followup-answer.json"), read(raw / "followup-result.json"))
    expected = {(e["taskName"], e["taskUid"]) for e in (first, followup)}
    tasks = read(raw / "tasks-after-followup.json")
    require(task_set(tasks, first["sessionName"]) == expected, "expected exactly two Tasks in this Session")
    require(all(task.get("status", {}).get("phase") == "Succeeded" for task in tasks["items"]
                if task["metadata"]["uid"] in {uid for _, uid in expected}), "a demonstrated Task was not successful")
    sessions = len({event["sessionName"] for event in (first, followup)})
    counts = {"tasksAfterRetry": retry_count, "distinctRequests": len(expected), "sessions": sessions}
    (raw.parent / "evidence.json").write_text(json.dumps(counts, indent=2) + "\n")
    print("Evidence from this run")
    print(f"Requests sent               3 (one was a retry)")
    print(f"Tasks after the retry       {retry_count}, same identity")
    print(f"Distinct requests           {len(expected)} Tasks in {sessions} Session")
    print("Answer via A2A and via Orka  identical")
    print("\nCustomer reply\n" + final)


def report(raw):
    retry_count = retry_check(raw)
    first = read(raw / "first-completed-event.json")
    followup = read(raw / "followup-completed-event.json")
    require(first["state"] == followup["state"] == "Completed", "the saved requests did not both complete")
    correlate(read(raw / "first-answer.json"), first, read(raw / "first-task.json"))
    correlate(read(raw / "followup-answer.json"), followup, read(raw / "followup-task.json"))
    require(first["sessionName"] == followup["sessionName"], "follow-up did not share the Session")
    require(first["taskUid"] != followup["taskUid"], "follow-up did not produce a distinct Task")
    require(first["threadId"] == followup["threadId"], "follow-up changed the conversation ID")
    reply(read(raw / "first-answer.json"), read(raw / "first-result.json"))
    final = reply(read(raw / "followup-answer.json"), read(raw / "followup-result.json"))
    expected = {(e["taskName"], e["taskUid"]) for e in (first, followup)}
    before = read(raw / "tasks-before-restart.json")
    after = read(raw / "tasks-after-restart.json")
    require(task_set(before, first["sessionName"]) == task_set(after, first["sessionName"]) == expected,
            "expected exactly two retained Tasks in this Session")
    require(task_set(before) == task_set(after), "restarting and reading changed this Gateway's Task identities")
    for snapshot in (before, after):
        require(all(task.get("status", {}).get("phase") == "Succeeded" for task in snapshot["items"]
                    if task["metadata"]["uid"] in {uid for _, uid in expected}), "a demonstrated Task was not successful")
    old_pods = ready_pods(read(raw / "pods-before-restart.json"))
    new_pods = ready_pods(read(raw / "pods-after-restart.json"))
    require(len(old_pods) == len(new_pods) == 1 and old_pods.isdisjoint(new_pods), "a different adapter Pod was not observed")
    retained = read(raw / "after-restart-answer.json")
    require(retained["id"] == read(raw / "followup-answer.json")["id"], "retrieval changed the public Task identity")
    correlate(retained, read(raw / "after-restart-event.json"), read(raw / "followup-task.json"))
    require(reply(retained, read(raw / "followup-result.json")) == final, "retained answer changed")
    require(retained["artifacts"] == read(raw / "followup-answer.json")["artifacts"], "saved answer data changed")
    sessions = len({event["sessionName"] for event in (first, followup)})
    counts = {
        "tasksAfterRetry": retry_count, "distinctRequests": len(expected), "sessions": sessions,
        "tasksBeforeRestart": len(task_set(before, first["sessionName"])),
        "tasksAfterRestart": len(task_set(after, first["sessionName"])),
        "newTasksAfterRestart": len(task_set(after) - task_set(before)),
        "sameRetainedAnswer": retained["artifacts"] == read(raw / "followup-answer.json")["artifacts"],
    }
    (raw.parent / "evidence.json").write_text(json.dumps(counts, indent=2) + "\n")
    print("Evidence from this run")
    print(f"Retry                         {retry_count} original Task, same UID")
    print(f"Two distinct requests         {len(expected)} Tasks in {sessions} Session")
    print(f"Read after adapter replacement  {counts['newTasksAfterRestart']} new Tasks, same saved answer")
    print("\nCustomer reply\n" + final)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("event-id", "correlate", "reply", "retry", "conversation", "summary", "report"))
    parser.add_argument("files", nargs="+")
    args = parser.parse_args()
    if args.command == "event-id":
        print(task_reference(read(args.files[0]))[0])
    elif args.command == "correlate":
        correlate(*(read(path) for path in args.files))
    elif args.command == "reply":
        reply(*(read(path) for path in args.files))
    elif args.command == "retry":
        count = retry_check(Path(args.files[0]))
        print(f"Retry returned the same reference. Matching Orka Tasks: {count}, same UID.")
    elif args.command == "conversation":
        conversation(read(args.files[0]))
    elif args.command == "summary":
        summary(Path(args.files[0]))
    else:
        report(Path(args.files[0]))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, IndexError, TypeError) as exc:
        sys.exit(f"A2A evidence check failed: {exc}")
