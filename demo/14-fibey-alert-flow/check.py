#!/usr/bin/env python3
"""Validate the records behind demo 14 and print its closing record.

The approval checks are adapted from demo 13's Foundry lane. Usage comes from
Orka's own token records for the coordinator, checked against its deployment's
Azure Monitor meter, and from the Azure meter of Fibey's separate deployment,
because external runtimes do not report tokens to Orka. Prices come from the
Azure Retail Prices API.
"""

import argparse
import base64
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request

TOOLS = ["create-work-order", "read-inventory"]
SUMMARY = "Inspect the pressure transmitter."
POLICY = {"allowedTools": TOOLS, "disallowedTools": [], "allowBash": False,
          "approvalRequiredTools": ["create-work-order"]}
GATEWAY = "fibey-alerts"
COORDINATOR = "demo-maintenance-coordinator"
SPECIALIST = "demo-fibey-foundry"
RUNTIME = "fibey-on-foundry-runtime"
FINDING = "Finding: only the replaced transmitter changed."
BINDING_FIELDS = {
    "taskAttempt": "attempt", "promptID": "promptID",
    "runtimeSessionUID": "runtimeSessionUID", "runtimeSessionGeneration": "runtimeSessionGeneration",
    "controllerEpoch": "controllerEpoch",
}
DIGEST_FIELDS = {
    "runtimeInstanceIDDigest": "runtimeInstanceID", "supervisorBootIDDigest": "runtimeSessionSupervisorBootID",
}
PRICES_URL = "https://prices.azure.com/api/retail/prices"
PRICE_REGION = os.environ.get("FIBEY_HOSTED_LOCATION", "eastus2")
# Global Standard list-price meters for the models this demo can use.
PRICE_METERS = {
    "gpt-6-luna": {"input": "6-luna ShortCo Inp Std Gl 1M Tokens", "cached": "6-luna ShortCo Cd Inp Std Gl 1M Tokens",
                   "output": "6-luna ShortCo Opt Std Gl 1M Tokens"},
    "gpt-4.1-mini": {"input": "gpt 4.1 mini Inp glbl Tokens", "cached": "gpt 4.1 mini cached Inp glbl Tokens",
                     "output": "gpt 4.1 mini Outp glbl Tokens"},
}
UNITS = {"1M": 1, "1K": 1000}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    return json.loads(Path(path).read_text())


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def bytes_digest(value):
    return "sha256:" + hashlib.sha256(str(value).encode()).hexdigest()


def when(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def duration(start, end):
    seconds = round((when(end) - when(start)).total_seconds())
    return f"{seconds // 60}m{seconds % 60:02d}s" if seconds >= 60 else f"{seconds}s"


def same_call(binding, execution):
    return (all(binding.get(key) and binding[key] == execution[field] for key, field in BINDING_FIELDS.items()) and
            all(execution.get(field) and binding.get(key) == bytes_digest(execution[field])
                for key, field in DIGEST_FIELDS.items()))


def objects(snapshot):
    values = {(obj["kind"], obj["metadata"]["name"]): obj for obj in snapshot["items"]}
    require(len(values) == len(snapshot["items"]), "duplicate installation records")
    return values


def identities(snapshot):
    return sorted([{"kind": kind, "name": name, "uid": obj["metadata"]["uid"]}
                   for (kind, name), obj in objects(snapshot).items()], key=lambda x: (x["kind"], x["name"]))


def ready_condition(obj):
    return any(c["type"] == "Ready" and c["status"] == "True" for c in obj.get("status", {}).get("conditions", []))


def installation(snapshot, ready):
    values = objects(snapshot)
    require(all(not v["metadata"].get("deletionTimestamp") for v in values.values()), "a prepared resource is deleting")
    require(identities(snapshot) == sorted(ready["identities"], key=lambda x: (x["kind"], x["name"])),
            "prepared resources changed; run fibey-flow.sh ready again")
    namespace = values["Namespace", ready["namespace"]]
    require(namespace["metadata"]["labels"].get("orka.ai/controller-mode") == "harness-v2",
            "namespace must belong to the v2 controller")
    require(ready_condition(values["Gateway", GATEWAY]) and ready_condition(values["GatewayBinding", COORDINATOR]),
            "the alert gateway is not ready")
    coordinator = values["Agent", COORDINATOR]["spec"]
    require(coordinator["providerRef"]["name"] == "demo-foundry-models" and
            coordinator["model"]["name"] == ready["coordinatorModel"], "the coordinator does not use the prepared model")
    require(coordinator["coordination"]["enabled"] and coordinator["coordination"]["maxDepth"] == 1 and
            coordinator["coordination"]["allowedAgents"] == [{"name": SPECIALIST}],
            "the coordinator may delegate beyond the Foundry specialist")
    require(ready["images"]["analysis"] in coordinator["systemPrompt"]["inline"],
            "the coordinator does not name the pinned analysis image")
    specialist = dict(values["Agent", SPECIALIST]["spec"])
    specialist.pop("resources", None)
    require(specialist == {"runtime": {"runtimeRef": {"name": RUNTIME}}}, "Fibey must select only its Foundry runtime")
    runtime = values["AgentRuntime", RUNTIME]
    status, capabilities = runtime.get("status", {}), runtime["spec"]["capabilities"]
    require(status.get("ready") is True and status.get("observedGeneration") == runtime["metadata"]["generation"],
            "the Foundry runtime has not passed conformance")
    require(capabilities["mcpPolicy"] == POLICY and capabilities["profile"]["providerKind"] == "foundry",
            "the Foundry runtime must require approval for create-work-order")
    return values


def counts(value, run_id, reads, orders):
    require(value.get("simulation") is True and value.get("runID") == run_id, "receipt is not from this simulated run")
    require(value.get("inventoryReads") == reads and value.get("workOrderExecutions") == orders,
            f"unexpected lookup or work-order count: {value.get('inventoryReads')}, {value.get('workOrderExecutions')}")
    require(value["workOrderIDs"] == [f"simulated-{run_id}-{n}" for n in range(1, orders + 1)],
            "receipt identities do not match the execution count")


def task_reference(admission):
    parts = admission["id"].split(".")
    require(len(parts) == 3 and parts[0] == "a2a1", "unrecognized public Task reference")
    decoded = [base64.urlsafe_b64decode(p + "=" * (-len(p) % 4)).decode() for p in parts[1:]]
    require(re.fullmatch(r"[A-Za-z0-9._-]+", decoded[0]), "unsafe event identity")
    return decoded[0]


def coordinator_task(value, event, run):
    require(value["metadata"]["uid"] == event["taskUid"] and value["metadata"]["name"] == event["taskName"],
            "the coordinator Task is not the one the gateway created")
    require(event["gatewayName"] == GATEWAY and event["agentName"] == COORDINATOR, "the alert took another route")
    require(value["spec"]["type"] == "ai" and value["spec"]["agentRef"]["name"] == COORDINATOR,
            "the gateway Task does not run the coordinator")
    require(value["metadata"]["namespace"] == run["namespace"], "the coordinator ran in another namespace")


def children(snapshot, coordinator, ready, run):
    owned = [c for c in snapshot["items"]
             if any(o.get("uid") == coordinator["metadata"]["uid"] for o in c["metadata"].get("ownerReferences", []))]
    require(len(owned) == len(snapshot["items"]) == 2, "expected exactly two child Tasks of this coordinator")
    job = [c for c in owned if c["spec"]["type"] == "container"]
    fibey = [c for c in owned if c["spec"]["type"] == "agent"]
    require(len(job) == len(fibey) == 1, "expected one analysis job and one specialist Task")
    job, fibey = job[0], fibey[0]
    require(job["spec"]["image"] == ready["images"]["analysis"] and not job["spec"].get("command") and
            not job["spec"].get("args"), "the analysis job is not the pinned image with its own entrypoint")
    require(job["status"]["phase"] == "Succeeded", "the analysis job did not succeed")
    require(fibey["spec"]["agentRef"]["name"] == SPECIALIST and fibey["spec"]["agentRuntime"]["allowedTools"] == TOOLS,
            "the specialist Task does not use Fibey's prepared tools")
    require(run["alertID"] in fibey["spec"]["prompt"], "the specialist Task lost the alert ID")
    return job, fibey


def specialist_binding(fibey, installed):
    binding = fibey["status"]["agentExecutionBinding"]
    runtime = installed["AgentRuntime", RUNTIME]
    require(binding["contractVersion"] == "orka.harness.v2" and binding["backend"] == "external-endpoint" and
            binding["runtimeRef"]["uid"] == runtime["metadata"]["uid"] and
            binding["runtimeProfileDigest"] == runtime["spec"]["capabilities"]["profile"]["digest"],
            "Fibey's Task did not bind to the prepared Foundry runtime")


def one_approval(document, fibey):
    require(document["taskName"] == fibey["metadata"]["name"], "approval list belongs to another Task")
    values = document.get("approvals") or []
    require(len(values) == 1, "expected one work-order approval")
    return values[0]


def pending(value, fibey, run):
    arguments = {"runID": run["alertID"], "asset": "pump-1", "summary": SUMMARY}
    require(value["status"] == "pending" and value["executionOutcome"] == "not_started",
            "the work order is no longer waiting unexecuted for review")
    require(value["taskUID"] == fibey["metadata"]["uid"] and value["targetTool"] == "create-work-order" and
            value["targetArgsPreview"] == arguments and value["targetArgsDigest"] == digest(arguments),
            "the review is not for this exact inspection request")
    require(same_call(value["binding"], fibey["status"]["execution"]), "the review belongs to another call")


def same_approval(original, current):
    for field in ("id", "taskUID", "targetTool", "targetArgsPreview", "targetArgsDigest", "targetSpecDigest",
                  "binding", "createdAt", "expiresAt"):
        require(original[field] == current[field], f"review changed: {field}")


def staged(stage):
    run, ready = read("run.json"), read("ready.json")
    installed = installation(read("raw/installation.json"), ready)
    counts(read("raw/counts-initial.json"), run["alertID"], 0, 0)
    if stage == "initial":
        return None
    event = read("raw/event-dispatched.json")
    require(event["id"] == task_reference(read("raw/admission.json")), "the gateway event is for another alert")
    coordinator = read("raw/coordinator-pending.json")
    coordinator_task(coordinator, event, run)
    job, fibey = children(read("raw/children-pending.json"), coordinator, ready, run)
    analysis = read("raw/analysis-result.json")["result"].strip()
    require(FINDING in analysis and re.search(r"PT-101 .*CHANGED", analysis), "unexpected analysis result")
    require(analysis in fibey["spec"]["prompt"], "Fibey did not receive the analysis verbatim")
    specialist_binding(fibey, installed)
    original = one_approval(read("raw/approval-pending.json"), fibey)
    pending(original, fibey, run)
    counts(read("raw/counts-pending.json"), run["alertID"], 1, 0)
    if stage == "pending":
        return None
    before = read("raw/children-before-decision.json")
    _, fibey_before = children(before, coordinator, ready, run)
    require(fibey_before["metadata"]["uid"] == fibey["metadata"]["uid"], "Fibey's Task was replaced before review")
    current = one_approval(read("raw/approval-before-decision.json"), fibey_before)
    same_approval(original, current)
    pending(current, fibey_before, run)
    counts(read("raw/counts-before-decision.json"), run["alertID"], 1, 0)
    if stage == "before-decision":
        return None
    decision = read("raw/decision.json")
    same_approval(original, decision)
    require(decision["status"] == "approved" and decision.get("decisionActor") == run["reviewerActor"] and
            decision.get("decisionTime"), "Mark's approval decision is missing")
    final_coordinator = read("raw/coordinator-final.json")
    coordinator_task(final_coordinator, event, run)
    require(final_coordinator["metadata"]["uid"] == coordinator["metadata"]["uid"] and
            final_coordinator["status"]["phase"] == "Succeeded", "the coordinator did not finish")
    final_job, final_fibey = children(read("raw/children-final.json"), final_coordinator, ready, run)
    require(final_fibey["metadata"]["uid"] == fibey["metadata"]["uid"] and final_fibey["spec"] == fibey["spec"],
            "completion is from another or modified Fibey Task")
    require(same_call(original["binding"], final_fibey["status"]["execution"]),
            "Fibey restarted instead of continuing the original call")
    require(final_fibey["status"]["phase"] == "Succeeded", "Fibey did not finish successfully")
    final_approval = one_approval(read("raw/approval-final.json"), final_fibey)
    same_approval(original, final_approval)
    require(final_approval["status"] == "approved" and final_approval["executionOutcome"] == "succeeded" and
            final_approval["decisionActor"] == decision["decisionActor"], "the approved action did not complete")
    receipt = read("raw/counts-final.json")
    counts(receipt, run["alertID"], 1, 1)
    work_order = receipt["workOrderIDs"][0]
    require(work_order in read("raw/fibey-result.json")["result"], "Fibey's answer lacks the actual receipt")
    result = read("raw/coordinator-result.json")["result"].strip()
    require(work_order in result, "the coordinator's answer lacks the actual receipt")
    answer = read("raw/answer.json")
    texts = [p["text"].strip() for a in answer.get("artifacts", []) for p in a.get("parts", [])]
    require(answer["status"]["state"] == "TASK_STATE_COMPLETED" and texts == [result],
            "the gateway returned a different answer than the coordinator recorded")
    completed = read("raw/event-completed.json")
    require(completed["id"] == event["id"] and completed["taskUid"] == event["taskUid"] and completed["deliveryId"],
            "the gateway did not record delivery of this alert's result")
    history = read("raw/fibey-events.json")
    types = [e["type"] for e in history["events"]]
    require(types.count("ApprovalRequested") == types.count("ApprovalApproved") == 1 and "TaskSucceeded" in types and
            types.index("ApprovalRequested") < types.index("ApprovalApproved") < types.index("TaskSucceeded"),
            "Fibey's history does not show one approved request before completion")
    require(identities(read("raw/installation-final.json")) == identities(read("raw/installation.json")),
            "prepared resources changed during the demonstration")
    return {"event": completed, "coordinator": final_coordinator, "job": final_job, "fibey": final_fibey,
            "approval": final_approval, "receipt": work_order}


def orka_usage(history):
    """Sum the coordinator's completed model-call usage records."""
    totals = {"input": 0, "cached": 0, "output": 0, "calls": 0}
    for event in history["events"]:
        usage = (event.get("content") or {}).get("usage") if event["type"] == "ModelUsageUpdated" else None
        if usage and usage.get("complete"):
            totals["input"] += usage.get("inputTokens") or 0
            totals["cached"] += usage.get("cachedInputTokens") or 0
            totals["output"] += usage.get("outputTokens") or 0
            totals["calls"] += 1
    return totals


def azure_metrics(start, end, model):
    resource = ("/subscriptions/{FIBEY_HOSTED_SUBSCRIPTION}/resourceGroups/{FIBEY_HOSTED_RESOURCE_GROUP}"
                "/providers/Microsoft.CognitiveServices/accounts/{FIBEY_HOSTED_ACCOUNT}").format(**os.environ)
    output = subprocess.run(
        ["az", "monitor", "metrics", "list", "--resource", resource, "--metric",
         "InputTokens", "OutputTokens", "cacheReadInputTokens", "--interval", "PT1M", "--aggregation", "Total",
         "--start-time", start.strftime("%Y-%m-%dT%H:%M:%SZ"), "--end-time", end.strftime("%Y-%m-%dT%H:%M:%SZ"),
         "--filter", f"ModelDeploymentName eq '{model}'", "-o", "json"],
        check=True, capture_output=True, text=True).stdout
    value = json.loads(output)
    totals, latest = {}, None
    for metric in value["value"]:
        points = [p for series in metric["timeseries"] for p in series["data"] if p.get("total")]
        totals[metric["name"]["value"]] = int(sum(p["total"] for p in points))
        if points:
            stamp = max(when(p["timeStamp"]) for p in points)
            latest = stamp if latest is None or stamp > latest else latest
    return value, totals, latest


def retail_prices(model):
    """List prices for one model's Global Standard deployment, per 1M tokens."""
    meters = PRICE_METERS[model]
    names = " or ".join(f"meterName eq '{name}'" for name in meters.values())
    query = urllib.parse.urlencode({"$filter": (
        f"serviceName eq 'Foundry Models' and armRegionName eq '{PRICE_REGION}' and ({names})")})
    with urllib.request.urlopen(f"{PRICES_URL}?{query}", timeout=30) as response:
        items = json.load(response)["Items"]
    prices = {}
    for kind, name in meters.items():
        found = {(i["retailPrice"], i["unitOfMeasure"], i["effectiveStartDate"]) for i in items if i["meterName"] == name}
        require(len(found) == 1, f"expected one current list price for {name}")
        price, unit, effective = found.pop()
        require(unit in UNITS, f"unexpected price unit {unit} for {name}")
        prices[kind] = {"usdPerMillion": price * UNITS[unit], "meter": name, "effective": effective}
    return prices


def meter():
    """Wait until Azure's per-minute token meters cover the whole run, then save them.

    The coordinator and Fibey use separate deployments, so each meter belongs
    to one of them. Nothing else should call these deployments during a run.
    """
    run = read("run.json")
    event = read("raw/event-completed.json")
    orka = orka_usage(read("raw/coordinator-events.json"))
    start = when(event["receivedAt"]).replace(second=0, microsecond=0)
    last_minute = when(event["completedAt"]).replace(second=0, microsecond=0)
    models = {"coordinator": run["coordinatorModel"], "fibey": run["specialistModel"]}
    require(models["coordinator"] != models["fibey"], "the coordinator and Fibey must use separate deployments")
    previous, settled, raw = None, False, {}
    deadline = time.monotonic() + 600
    while time.monotonic() < deadline:
        end = datetime.now(timezone.utc).replace(microsecond=0) + timedelta(minutes=1)
        totals, covered = {}, True
        for role, model in models.items():
            raw[role], totals[role], latest = azure_metrics(start, end, model)
            covered = covered and latest is not None and latest >= last_minute - timedelta(minutes=2)
        # Azure publishes minutes a few minutes late; never settle before that.
        old_enough = datetime.now(timezone.utc) >= when(event["completedAt"]) + timedelta(minutes=3)
        if old_enough and covered and totals["coordinator"].get("InputTokens", 0) >= orka["input"] and \
                totals == previous:
            settled = True
            break
        previous = totals
        time.sleep(30)
    Path("raw/azure-meter.json").write_text(json.dumps(raw, indent=2) + "\n")
    Path("raw/prices.json").write_text(json.dumps({role: retail_prices(model) for role, model in models.items()},
                                                  indent=2) + "\n")
    Path("meter.json").write_text(json.dumps({"settled": settled, "totals": totals, "orka": orka, "models": models,
                                              "window": [start.isoformat(), end.isoformat()]}, indent=2) + "\n")
    require(settled, "Azure's token meters did not settle within ten minutes")


def cost(tokens_in, cached, tokens_out, prices):
    return ((tokens_in - cached) * prices["input"]["usdPerMillion"] + cached * prices["cached"]["usdPerMillion"] +
            tokens_out * prices["output"]["usdPerMillion"]) / 1e6


def record():
    result = staged("final")
    run, usage, prices = read("run.json"), read("meter.json"), read("raw/prices.json")
    require(usage["settled"], "Azure's token meters did not settle")
    event, coordinator, job, fibey, approval = (result[k] for k in ("event", "coordinator", "job", "fibey", "approval"))
    orka, fibey_meter, coordinator_meter = usage["orka"], usage["totals"]["fibey"], usage["totals"]["coordinator"]
    # Orka's record is the coordinator's source; its own deployment's meter must agree.
    require(coordinator_meter.get("InputTokens") == orka["input"] and coordinator_meter.get("OutputTokens") == orka["output"],
            f"Orka's coordinator record {orka} differs from Azure's meter {coordinator_meter}")
    f_in, f_cached, f_out = (fibey_meter.get(k, 0) for k in ("InputTokens", "cacheReadInputTokens", "OutputTokens"))
    total = cost(orka["input"], orka["cached"], orka["output"], prices["coordinator"]) + \
        cost(f_in, f_cached, f_out, prices["fibey"])
    evidence = {"alertID": run["alertID"], "workOrderID": result["receipt"], "approvedBy": approval["decisionActor"],
                "elapsed": duration(event["receivedAt"], event["completedAt"]), "coordinator": orka,
                "fibeyMeter": fibey_meter, "estimatedCostUSD": round(total, 6), "models": usage["models"]}
    Path("evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")

    def row(label, text):
        print(f"{label:<14}{text}")

    def span(task):
        return duration(task["status"]["startTime"], task["status"]["completionTime"])

    row("Tasks", f"coordinator       Orka on AKS        {coordinator['status']['phase']:<10} {span(coordinator)}")
    row("", f"sensor analysis   AKS job            {job['status']['phase']:<10} {span(job)}")
    row("", f"Fibey             Microsoft Foundry  {fibey['status']['phase']:<10} {span(fibey)}")
    actor = approval["decisionActor"].rsplit(":", 1)[-1]
    row("Approval", f"create-work-order pump-1 \"{SUMMARY}\"")
    row("", f"approved by {actor} after {duration(approval['createdAt'], approval['decisionTime'])}: "
            f"\"{approval['decisionReason']}\"")
    row("Result", f"{result['receipt']}, returned through the gateway")
    row("Elapsed", f"{evidence['elapsed']} from alert to answer")
    row("Tokens", f"coordinator  {run['coordinatorModel']:<13} {orka['input']:>7,} in, {orka['output']:>6,} out  "
                  "Orka record")
    row("", f"Fibey        {run['specialistModel']:<13} {f_in:>7,} in, {f_out:>6,} out  Azure meter")
    row("Est. cost", f"${total:.4f} at Azure list prices, not an invoice")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", choices=("installation", "initial", "event-id", "pending", "before-decision",
                                          "final", "meter"))
    parser.add_argument("path", nargs="?")
    args = parser.parse_args()
    if args.stage == "installation":
        installation(read("raw/installation.json"), read("ready.json"))
    elif args.stage == "event-id":
        print(task_reference(read(args.path)))
    elif args.stage == "meter":
        meter()
    elif args.stage == "final":
        record()
    else:
        staged(args.stage)


if __name__ == "__main__":
    try:
        main()
    except (KeyError, IndexError, TypeError, ValueError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f"demo 14 evidence check failed: {error}")
