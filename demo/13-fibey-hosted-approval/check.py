#!/usr/bin/env python3
"""Validate the records behind the two-host Fibey approval walkthrough.

Adapted from demo 11's check.py. Each lane is one Fibey: `aks` is hosted by
Orka on AKS, `foundry` is hosted in Microsoft Foundry. Both lanes must satisfy
the same policy, approval, and receipt checks.
"""

import argparse
from datetime import datetime
import hashlib
import json
from pathlib import Path
import re
import sys

LANES = ("aks", "foundry")
PROVIDERS = {"aks": "agentkit", "foundry": "foundry"}
TOOLS = ["create-work-order", "read-inventory"]
SUMMARY = "Inspect the pressure transmitter."
POLICY = {"allowedTools": TOOLS, "disallowedTools": [], "allowBash": False,
          "approvalRequiredTools": ["create-work-order"]}
BINDING_FIELDS = {
    "taskAttempt": "attempt", "promptID": "promptID",
    "runtimeSessionUID": "runtimeSessionUID", "runtimeSessionGeneration": "runtimeSessionGeneration",
    "controllerEpoch": "controllerEpoch",
}
# Reviews record these identifiers only as sha256 digests of the raw value.
DIGEST_FIELDS = {
    "runtimeInstanceIDDigest": "runtimeInstanceID", "supervisorBootIDDigest": "runtimeSessionSupervisorBootID",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    return json.loads(Path(path).read_text())


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def bytes_digest(value):
    return "sha256:" + hashlib.sha256(str(value).encode()).hexdigest()


def same_call(binding, execution):
    return (all(binding.get(key) and binding[key] == execution[field] for key, field in BINDING_FIELDS.items()) and
            all(execution.get(field) and binding.get(key) == bytes_digest(execution[field])
                for key, field in DIGEST_FIELDS.items()))


def objects(snapshot):
    values = {(obj["kind"], obj["metadata"]["name"]): obj for obj in snapshot["items"]}
    require(len(values) == len(snapshot["items"]), "duplicate installation records")
    return values


def identities(snapshot):
    return {kind + "/" + name: {"uid": obj["metadata"]["uid"],
            "generation": obj["metadata"].get("generation"),
            "configuration": digest({"spec": obj.get("spec"), "data": obj.get("data")})}
            for (kind, name), obj in objects(snapshot).items()}


def runtime(value, lane):
    spec, status = value["spec"], value.get("status", {})
    require(spec["contractVersion"] == "orka.harness.v2" and
            spec["deployment"]["mode"] == "external-endpoint", f"{lane}: Fibey needs an external v2 runtime")
    require(not value["metadata"].get("deletionTimestamp"), f"{lane}: Fibey runtime is deleting")
    require(status.get("ready") is True and status.get("observedGeneration") == value["metadata"]["generation"],
            f"{lane}: Fibey runtime has not passed conformance for this configuration")
    capabilities = spec["capabilities"]
    require(capabilities["mcpPolicy"] == POLICY,
            f"{lane}: Fibey needs both tools, with human approval required only for create-work-order")
    profile = capabilities["profile"]
    require(profile["providerKind"] == PROVIDERS[lane] and
            profile["adapterName"] == profile["providerKind"] + "-serve-acp" and
            profile["workspaceIntent"] == "read",
            f"{lane}: unexpected runtime provider or workspace intent")
    observed = status["observedCapabilities"]
    require(observed["runtimeProfileDigest"] == profile["digest"] and
            observed["runtimeInstanceID"] == capabilities["runtimeInstanceID"], f"{lane}: runtime observation is stale")
    governance = capabilities["workspaceGovernance"]
    require(governance["mode"] == "strict-governed" and not governance.get("trusted", False),
            f"{lane}: runtime must use strict governance")


def installation(snapshot, config):
    values = objects(snapshot)
    require(all(not value["metadata"].get("deletionTimestamp") for value in values.values()),
            "a prepared resource is deleting")
    namespace = values["Namespace", config["namespace"]]
    require(namespace["metadata"]["labels"].get("orka.ai/controller-mode") == "harness-v2",
            "namespace must belong to the v2 controller")
    for lane in LANES:
        settings = config["lanes"][lane]
        runtime(values["AgentRuntime", settings["runtimeName"]], lane)
        agent = values["Agent", settings["agent"]]
        spec = dict(agent["spec"])
        resources = spec.pop("resources", {})
        require(spec == {"runtime": {"runtimeRef": {"name": settings["runtimeName"]}}} and resources == {},
                f"{lane}: the Fibey Agent must select only its prepared runtime")
    require(values["PersistentVolumeClaim", "demo-fibey-tools"]["status"]["phase"] == "Bound",
            "the simulator needs its persistent receipt database")
    return values


def counts(value, task_name, reads, orders):
    require(value.get("simulation") is True and value.get("runID") == task_name,
            "receipt is not from this simulated run")
    require(type(value.get("inventoryReads")) is int and value["inventoryReads"] == reads and
            type(value.get("workOrderExecutions")) is int and value["workOrderExecutions"] == orders,
            f"{task_name}: unexpected lookup or work-order execution count")
    require(value["workOrderIDs"] == [f"simulated-{task_name}-{n}" for n in range(1, orders + 1)],
            f"{task_name}: receipt identities do not match the execution count")


def task(value, settings, config, installed, lane):
    require(value["metadata"]["name"] == settings["task"] and
            value["metadata"]["namespace"] == config["namespace"] and value["metadata"].get("uid"),
            f"{lane}: wrong Task identity")
    spec = value["spec"]
    require(spec["type"] == "agent" and spec["agentRef"]["name"] == settings["agent"] and
            spec["agentRuntime"]["allowedTools"] == TOOLS and spec["workspace"]["intent"] == "read",
            f"{lane}: Task does not use the prepared Fibey agent and tools")
    require(not spec.get("sessionRef") and not spec.get("priorTaskRef"), f"{lane}: unexpected earlier conversation")
    binding = value["status"]["agentExecutionBinding"]
    agent = installed["Agent", settings["agent"]]["metadata"]
    selected = installed["AgentRuntime", settings["runtimeName"]]
    require(binding["contractVersion"] == "orka.harness.v2" and binding["backend"] == "external-endpoint",
            f"{lane}: Task did not bind to an external v2 runtime")
    for key in ("name", "uid", "generation"):
        require(binding["agent"][key] == agent[key] and binding["runtimeRef"][key] == selected["metadata"][key],
                f"{lane}: Task is bound to another Agent or runtime version")
    require(binding["runtimeProfileDigest"] == selected["spec"]["capabilities"]["profile"]["digest"] and
            binding["task"]["uid"] == value["metadata"]["uid"] and
            binding["task"]["boundSpecGeneration"] == value["metadata"]["generation"] and
            binding["task"]["namespaceUID"] == installed["Namespace", config["namespace"]]["metadata"]["uid"],
            f"{lane}: Task binding differs from this run's prepared configuration")


def approval(document, settings, config, lane):
    require(document["namespace"] == config["namespace"] and document["taskName"] == settings["task"],
            f"{lane}: approval list belongs to another Task")
    values = document.get("approvals") or []
    require(len(values) == 1, f"{lane}: expected one work-order approval")
    return values[0]


def pending(value, task_record, task_name, lane):
    arguments = {"runID": task_name, "asset": "pump-1", "summary": SUMMARY}
    require(value["status"] == "pending" and value["executionOutcome"] == "not_started",
            f"{lane}: work order is no longer waiting unexecuted for review")
    require(value["taskUID"] == task_record["metadata"]["uid"] and value["targetTool"] == "create-work-order" and
            value["targetArgsPreview"] == arguments and value["targetArgsDigest"] == digest(arguments) and
            value.get("targetSpecDigest"), f"{lane}: review is not for this exact inspection request")
    binding = value["binding"]
    require(same_call(binding, task_record["status"]["execution"]),
            f"{lane}: review belongs to another Task attempt or waiting call")
    require(binding.get("operationIDDigest") and binding.get("requestDigest") and binding.get("callIDDigest"),
            f"{lane}: review lost its original call identity")
    created = datetime.fromisoformat(value["createdAt"].replace("Z", "+00:00"))
    expires = datetime.fromisoformat(value["expiresAt"].replace("Z", "+00:00"))
    require(0 < (expires - created).total_seconds() <= 600, f"{lane}: review deadline is not bounded to ten minutes")


def same_approval(original, current, lane):
    for field in ("id", "taskUID", "targetTool", "targetArgsPreview", "targetArgsDigest", "targetSpecDigest",
                  "binding", "createdAt", "expiresAt"):
        require(original[field] == current[field], f"{lane}: review changed: {field}")


def lane_check(stage, lane, config, installed):
    settings = config["lanes"][lane]
    name = settings["task"]
    raw = Path("raw") / lane
    counts(read(raw / "counts-initial.json"), name, 0, 0)
    if stage == "initial":
        return None
    first = read(raw / "task-pending.json")
    task(first, settings, config, installed, lane)
    require(first["spec"]["prompt"] == read(f"task-{lane}.json")["spec"]["prompt"], f"{lane}: Task prompt changed")
    original = approval(read(raw / "approval-pending.json"), settings, config, lane)
    pending(original, first, name, lane)
    counts(read(raw / "counts-pending.json"), name, 1, 0)
    if stage == "pending":
        return None
    before = read(raw / "task-before-decision.json")
    task(before, settings, config, installed, lane)
    require(before["metadata"]["uid"] == first["metadata"]["uid"], f"{lane}: Task was replaced before review")
    current = approval(read(raw / "approval-before-decision.json"), settings, config, lane)
    same_approval(original, current, lane)
    pending(current, before, name, lane)
    before_counts = read(raw / "counts-before-decision.json")
    counts(before_counts, name, 1, 0)
    if stage == "before-decision":
        return None
    decision = read(raw / "decision.json")
    same_approval(original, decision, lane)
    require(decision["status"] == "approved" and decision.get("decisionActor") == config["reviewerActor"] and
            decision.get("decisionTime"), f"{lane}: the presenter's approval decision is missing")
    final = read(raw / "task-final.json")
    task(final, settings, config, installed, lane)
    require(final["metadata"]["uid"] == first["metadata"]["uid"] and final["spec"] == first["spec"],
            f"{lane}: completion is from another or modified Task")
    same_task_and_call = same_call(original["binding"], final["status"]["execution"])
    require(same_task_and_call, f"{lane}: Fibey restarted instead of continuing the original call")
    require(final["status"]["phase"] == "Succeeded" and final["status"]["execution"]["outcome"] == "Succeeded" and
            final["status"].get("completionTime"), f"{lane}: Fibey did not finish successfully")
    final_approval = approval(read(raw / "approval-final.json"), settings, config, lane)
    same_approval(original, final_approval, lane)
    require(final_approval["status"] == "approved" and final_approval["executionOutcome"] == "succeeded",
            f"{lane}: approved action did not complete")
    require(all(final_approval[key] == decision[key] for key in ("decisionActor", "decisionTime")),
            f"{lane}: final review does not retain the presenter's decision")
    receipt = read(raw / "counts-final.json")
    counts(receipt, name, 1, 1)
    answer = read(raw / "result.json")["result"]
    require(receipt["workOrderIDs"][0] in answer, f"{lane}: Fibey's answer does not contain the actual receipt")
    require(re.search(r"\b(created|opened|recorded)\b", answer, re.IGNORECASE) and
            not re.search(r"\b(pending|awaiting|waiting for)\b[^.!?\n]{0,80}\b(approval|review)\b",
                          answer, re.IGNORECASE),
            f"{lane}: Fibey's answer must confirm creation, not describe approval as pending")
    history = read(raw / "events.json")
    require(history["namespace"] == config["namespace"] and history["streamID"] == name and
            history["streamType"] == "task" and all(event["streamID"] == name for event in history["events"]) and
            [event["seq"] for event in history["events"]] == list(range(1, history["latestSeq"] + 1)),
            f"{lane}: Task event history is incomplete")
    types = [event["type"] for event in history["events"]]
    require(types.count("ApprovalRequested") == types.count("ApprovalApproved") == 1 and "TaskSucceeded" in types,
            f"{lane}: the event history does not show one approved request and a completed Task")
    require(types.index("ApprovalRequested") < types.index("ApprovalApproved") < types.index("TaskSucceeded") and
            all(event["toolCallID"] == original["id"] for event in history["events"]
                if event["type"] in ("ApprovalRequested", "ApprovalApproved")),
            f"{lane}: event history does not link this review to its decision and completion")
    return {"task": name, "approval": original["id"], "reviewer": decision["decisionActor"],
            "inventoryReads": receipt["inventoryReads"],
            "ordersBeforeApproval": before_counts["workOrderExecutions"],
            "ordersAfterApproval": receipt["workOrderExecutions"], "workOrderID": receipt["workOrderIDs"][0],
            "sameTaskAndCall": same_task_and_call}


def check(stage):
    config = read("run.json")
    installed = installation(read("raw/installation.json"), config)
    require(identities(read("raw/installation.json")) == read("ready.json")["identities"],
            "prepared resources changed; run setup again")
    results = {lane: lane_check(stage, lane, config, installed) for lane in LANES}
    if stage != "final":
        return
    require(identities(read("raw/installation-final.json")) == identities(read("raw/installation.json")),
            "runtimes, tools, or receipt storage changed during the demonstration")
    Path("evidence.json").write_text(json.dumps(results, indent=2) + "\n")
    aks, foundry = results["aks"], results["foundry"]
    rows = [
        ("", "Orka on AKS", "Microsoft Foundry"),
        ("Inventory lookups", f"{aks['inventoryReads']}", f"{foundry['inventoryReads']}"),
        ("Orders before approval", f"{aks['ordersBeforeApproval']}", f"{foundry['ordersBeforeApproval']}"),
        ("Orders after approval", f"{aks['ordersAfterApproval']}", f"{foundry['ordersAfterApproval']}"),
        ("Same Task and call", "retained" if aks["sameTaskAndCall"] else "changed",
         "retained" if foundry["sameTaskAndCall"] else "changed"),
    ]
    for label, left, right in rows:
        print(f"{label:<24}{left:<22}{right}")
    print(f"{'Receipts':<24}{aks['workOrderID']}")
    print(f"{'':<24}{foundry['workOrderID']}")


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
