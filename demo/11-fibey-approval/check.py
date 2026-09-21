#!/usr/bin/env python3
"""Validate the records behind the Fibey approval walkthrough."""

import argparse
from datetime import datetime
import hashlib
import json
from pathlib import Path
import sys

TOOLS = ["create-work-order", "read-inventory"]
SUMMARY = "Inspect the pressure transmitter."
POLICY = {"allowedTools": TOOLS, "disallowedTools": [], "allowBash": False,
          "approvalRequiredTools": ["create-work-order"]}
BINDING_FIELDS = {
    "taskAttempt": "attempt", "promptID": "promptID",
    "runtimeSessionUID": "runtimeSessionUID", "runtimeSessionGeneration": "runtimeSessionGeneration",
    "runtimeInstanceID": "runtimeInstanceID", "supervisorBootID": "runtimeSessionSupervisorBootID",
    "controllerEpoch": "controllerEpoch",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    return json.loads(Path(path).read_text())


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def objects(snapshot):
    values = {(obj["kind"], obj["metadata"]["name"]): obj for obj in snapshot["items"]}
    require(len(values) == len(snapshot["items"]), "duplicate installation records")
    return values


def identities(snapshot):
    return {kind + "/" + name: {"uid": obj["metadata"]["uid"],
            "generation": obj["metadata"].get("generation"),
            "configuration": digest({"spec": obj.get("spec"), "data": obj.get("data")})}
            for (kind, name), obj in objects(snapshot).items()}


def runtime(value):
    spec, status = value["spec"], value.get("status", {})
    require(spec["contractVersion"] == "orka.harness.v2" and
            spec["deployment"]["mode"] == "external-endpoint", "Fibey needs an external v2 runtime")
    require(not value["metadata"].get("deletionTimestamp"), "Fibey runtime is deleting")
    require(status.get("ready") is True and status.get("observedGeneration") == value["metadata"]["generation"],
            "Fibey runtime has not passed conformance for this configuration")
    capabilities = spec["capabilities"]
    require(capabilities["mcpPolicy"] == POLICY,
            "Fibey needs both tools, with human approval required only for create-work-order")
    profile = capabilities["profile"]
    require(profile["providerKind"] in ("agentkit", "foundry") and
            profile["adapterName"] == profile["providerKind"] + "-serve-acp" and
            profile["workspaceIntent"] == "read",
            "unexpected runtime provider or workspace intent")
    observed = status["observedCapabilities"]
    require(observed["runtimeProfileDigest"] == profile["digest"] and
            observed["runtimeInstanceID"] == capabilities["runtimeInstanceID"], "runtime observation is stale")
    governance = capabilities["workspaceGovernance"]
    require(governance["mode"] == "strict-governed" and not governance.get("trusted", False),
            "runtime must use strict governance")


def installation(snapshot, config):
    values = objects(snapshot)
    require(all(not value["metadata"].get("deletionTimestamp") for value in values.values()),
            "a prepared resource is deleting")
    selected = values["AgentRuntime", config["runtimeName"]]
    runtime(selected)
    namespace = values["Namespace", config["namespace"]]
    require(namespace["metadata"]["labels"].get("orka.ai/controller-mode") == "harness-v2",
            "namespace must belong to the v2 controller")
    agent = values["Agent", "demo-fibey"]
    # Typed Kubernetes responses can serialize empty resource requirements.
    spec = dict(agent["spec"])
    resources = spec.pop("resources", {})
    require(spec == {"runtime": {"runtimeRef": {"name": config["runtimeName"]}}} and resources == {},
            "the Fibey Agent must select only its prepared runtime")
    require(values["PersistentVolumeClaim", "demo-fibey-tools"]["status"]["phase"] == "Bound",
            "the simulator needs its persistent receipt database")
    return values


def counts(value, task_name, reads, orders):
    require(value.get("simulation") is True and value.get("runID") == task_name,
            "receipt is not from this simulated run")
    require(type(value.get("inventoryReads")) is int and value["inventoryReads"] == reads and
            type(value.get("workOrderExecutions")) is int and value["workOrderExecutions"] == orders,
            "unexpected lookup or work-order execution count")
    require(value["workOrderIDs"] == [f"simulated-{task_name}-{n}" for n in range(1, orders + 1)],
            "receipt identities do not match the execution count")


def task(value, config, installed):
    require(value["metadata"]["name"] == config["task"] and
            value["metadata"]["namespace"] == config["namespace"] and value["metadata"].get("uid"),
            "wrong Task identity")
    spec = value["spec"]
    require(spec["type"] == "agent" and spec["agentRef"]["name"] == "demo-fibey" and
            spec["agentRuntime"]["allowedTools"] == TOOLS and spec["workspace"]["intent"] == "read",
            "Task does not use the prepared Fibey agent and tools")
    require(not spec.get("sessionRef") and not spec.get("priorTaskRef"), "unexpected earlier conversation")
    binding = value["status"]["agentExecutionBinding"]
    agent = installed["Agent", "demo-fibey"]["metadata"]
    selected = installed["AgentRuntime", config["runtimeName"]]
    require(binding["contractVersion"] == "orka.harness.v2" and binding["backend"] == "external-endpoint",
            "Task did not bind to an external v2 runtime")
    for key in ("name", "uid", "generation"):
        require(binding["agent"][key] == agent[key] and binding["runtimeRef"][key] == selected["metadata"][key],
                "Task is bound to another Agent or runtime version")
    require(binding["runtimeProfileDigest"] == selected["spec"]["capabilities"]["profile"]["digest"] and
            binding["task"]["uid"] == value["metadata"]["uid"] and
            binding["task"]["boundSpecGeneration"] == value["metadata"]["generation"] and
            binding["task"]["namespaceUID"] == installed["Namespace", config["namespace"]]["metadata"]["uid"],
            "Task binding differs from this run's prepared configuration")


def approval(document, config):
    require(document["namespace"] == config["namespace"] and document["taskName"] == config["task"],
            "approval list belongs to another Task")
    values = document.get("approvals") or []
    require(len(values) == 1, "expected one work-order approval")
    return values[0]


def pending(value, task_record, task_name):
    arguments = {"runID": task_name, "asset": "pump-1", "summary": SUMMARY}
    require(value["status"] == "pending" and value["executionOutcome"] == "not_started",
            "work order is no longer waiting unexecuted for review")
    require(value["taskUID"] == task_record["metadata"]["uid"] and value["targetTool"] == "create-work-order" and
            value["targetArgsPreview"] == arguments and value["targetArgsDigest"] == digest(arguments) and
            value.get("targetSpecDigest"), "review is not for this exact inspection request")
    binding = value["binding"]
    for key, field in BINDING_FIELDS.items():
        require(binding.get(key) and binding[key] == task_record["status"]["execution"][field],
                "review belongs to another Task attempt or waiting call")
    require(binding.get("operationID") and binding.get("requestDigest") and binding.get("callIDDigest"),
            "review lost its original call identity")
    created = datetime.fromisoformat(value["createdAt"].replace("Z", "+00:00"))
    expires = datetime.fromisoformat(value["expiresAt"].replace("Z", "+00:00"))
    require(0 < (expires - created).total_seconds() <= 600, "review deadline is not bounded to ten minutes")


def same_approval(original, current):
    for field in ("id", "taskUID", "targetTool", "targetArgsPreview", "targetArgsDigest", "targetSpecDigest",
                  "binding", "createdAt", "expiresAt"):
        require(original[field] == current[field], f"review changed: {field}")


def check(stage):
    config = read("run.json")
    installed = installation(read("raw/installation.json"), config)
    require(identities(read("raw/installation.json")) == read("ready.json")["identities"],
            "prepared resources changed; run setup again")
    counts(read("raw/counts-initial.json"), config["task"], 0, 0)
    if stage == "initial":
        return
    first = read("raw/task-pending.json")
    task(first, config, installed)
    require(first["spec"]["prompt"] == read("task.json")["spec"]["prompt"], "Task prompt changed")
    original = approval(read("raw/approval-pending.json"), config)
    pending(original, first, config["task"])
    counts(read("raw/counts-pending.json"), config["task"], 1, 0)
    if stage == "pending":
        return
    before = read("raw/task-before-decision.json")
    task(before, config, installed)
    require(before["metadata"]["uid"] == first["metadata"]["uid"], "Task was replaced before review")
    current = approval(read("raw/approval-before-decision.json"), config)
    same_approval(original, current)
    pending(current, before, config["task"])
    before_counts = read("raw/counts-before-decision.json")
    counts(before_counts, config["task"], 1, 0)
    if stage == "before-decision":
        return
    decision = read("raw/decision.json")
    same_approval(original, decision)
    require(decision["status"] == "approved" and
            decision.get("decisionActor") == f"system:serviceaccount:{config['namespace']}:orka-client" and
            decision.get("decisionTime"),
            "the presenter's approval decision is missing")
    final = read("raw/task-final.json")
    task(final, config, installed)
    require(final["metadata"]["uid"] == first["metadata"]["uid"] and final["spec"] == first["spec"],
            "completion is from another or modified Task")
    same_task_and_call = (final["metadata"]["uid"] == first["metadata"]["uid"] and
                          all(final["status"]["execution"][field] == original["binding"][key]
                              for key, field in BINDING_FIELDS.items()))
    require(same_task_and_call, "Fibey restarted instead of continuing the original call")
    require(final["status"]["phase"] == "Succeeded" and final["status"]["execution"]["outcome"] == "Succeeded" and
            final["status"].get("completionTime"), "Fibey did not finish successfully")
    final_approval = approval(read("raw/approval-final.json"), config)
    same_approval(original, final_approval)
    require(final_approval["status"] == "approved" and final_approval["executionOutcome"] == "succeeded",
            "approved action did not complete")
    require(all(final_approval[key] == decision[key] for key in ("decisionActor", "decisionTime")),
            "final review does not retain the presenter's decision")
    receipt = read("raw/counts-final.json")
    counts(receipt, config["task"], 1, 1)
    require(receipt["workOrderIDs"][0] in read("raw/result.json")["result"],
            "Fibey's answer does not contain the actual work-order receipt")
    history = read("raw/events.json")
    require(history["namespace"] == config["namespace"] and history["streamID"] == config["task"] and
            history["streamType"] == "task" and
            all(event["streamID"] == config["task"] for event in history["events"]) and
            [event["seq"] for event in history["events"]] == list(range(1, history["latestSeq"] + 1)),
            "Task event history is incomplete")
    types = [event["type"] for event in history["events"]]
    require(types.count("ApprovalRequested") == types.count("ApprovalApproved") == 1 and "TaskSucceeded" in types,
            "the saved event history does not show one approved request and a completed Task")
    require(types.index("ApprovalRequested") < types.index("ApprovalApproved") < types.index("TaskSucceeded") and
            all(event["toolCallID"] == original["id"] for event in history["events"]
                if event["type"] in ("ApprovalRequested", "ApprovalApproved")),
            "event history does not link this review to its later decision and completion")
    require(identities(read("raw/installation-final.json")) == identities(read("raw/installation.json")),
            "runtime, tools, or receipt storage changed during the demonstration")
    evidence = {"task": config["task"], "approval": original["id"], "reviewer": decision["decisionActor"],
                "inventoryReads": receipt["inventoryReads"],
                "ordersBeforeApproval": before_counts["workOrderExecutions"],
                "ordersAfterApproval": receipt["workOrderExecutions"], "workOrderID": receipt["workOrderIDs"][0],
                "sameTaskAndCall": same_task_and_call}
    Path("evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print(f"Inventory lookup          {evidence['inventoryReads']} completed")
    print(f"Before approval           {evidence['ordersBeforeApproval']} work orders")
    print(f"After approval            {evidence['ordersAfterApproval']} work order")
    print(f"Original Task and call    {'retained' if evidence['sameTaskAndCall'] else 'changed'}")
    print(f"Work-order receipt        {evidence['workOrderID']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", choices=("installation", "initial", "pending", "before-decision", "final"))
    args = parser.parse_args()
    if args.stage == "installation":
        config = read("setup.json")
        snapshot = read("installed.json")
        installation(snapshot, config)
        config["identities"] = identities(snapshot)
        Path("ready.json").write_text(json.dumps(config, indent=2) + "\n")
    else:
        check(args.stage)


if __name__ == "__main__":
    try:
        main()
    except (KeyError, IndexError, TypeError, ValueError, OSError) as error:
        sys.exit(f"Fibey evidence check failed: {error}")
