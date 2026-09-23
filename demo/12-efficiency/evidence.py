#!/usr/bin/env python3
"""Verify demo 12 artifacts without contacting the cluster or executing answers.

Gateway counters establish completeness; operation IDs join their bounded request
and attempt histories to completion logs. Policy tier plus the captured running
configuration identifies the terminal model. Orka usage is a second accounting
view, not extra consumption to add to the gateway totals.
"""

import argparse
import datetime as dt
import hashlib
import json
import math
from pathlib import Path
import re
import sys

import classifier_billing
from check import check_data, check_stock_prose
from fixtures import WORKLOADS

HERE = Path(__file__).resolve().parent
TEAMS = ("payments", "inventory")
MODES = {"baseline": "off", "routed": "enforce"}
TOKEN_FIELDS = ("prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens", "reasoning_tokens")
GENERATION_FIELDS = ("config_generation", "profile_generation", "classifier_generation", "binary_generation")
LEDGER_FIELDS = ("sends", "completed", "errors", "throttled", "reported_usage_sends", "duration_ms")


class EvidenceError(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise EvidenceError(message)


def read(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError) as exc:
        raise EvidenceError("Missing or invalid artifact: " + str(path)) from exc


def timestamp(value):
    try:
        result = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
        require(result.tzinfo is not None, "timestamp has no timezone")
        return result
    except (TypeError, AttributeError, ValueError) as exc:
        raise EvidenceError("Invalid evidence timestamp") from exc


def count(value, label):
    require(type(value) is int and value >= 0, "Missing or invalid counter: " + label)
    return value


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def index(rows, key, label):
    require(isinstance(rows, list), "Missing history: " + label)
    result = {}
    for row in rows:
        value = key(row)
        require(value and value not in result, "Missing or duplicate identity in " + label)
        result[value] = row
    return result


def tokens(value):
    require(isinstance(value, dict), "Missing reported token usage")
    result = {key: count(value.get(key), key) for key in TOKEN_FIELDS}
    require(result["total_tokens"] == result["prompt_tokens"] + result["completion_tokens"],
            "Reported token components do not match their total")
    return result


def add_tokens(rows):
    rows = list(rows)
    return {key: sum(row[key] for row in rows) for key in TOKEN_FIELDS}


def role_totals(operations):
    groups = {"cpuWorker": [], "hostedWorker": [], "coordinator": []}
    for row in operations:
        role = "coordinator" if row["role"] == "coordinator" else (
            "cpuWorker" if row["tier"] == "lightweight" else "hostedWorker")
        groups[role].append(row)
    return {key: {"requests": len(rows), "usage": add_tokens(row["usage"] for row in rows)}
            for key, rows in groups.items()}


def counter_delta(before, after, keys):
    result = {key: count(after.get(key), key) - count(before.get(key), key) for key in keys}
    require(all(value >= 0 for value in result.values()), "Gateway counters decreased or reset")
    return result


def token_delta(before, after):
    return counter_delta(tokens(before), tokens(after), TOKEN_FIELDS)


def ledger(value):
    rows = index(value["by_kind"], lambda row: row["kind"], "physical usage ledger")
    for row in rows.values():
        for key in LEDGER_FIELDS:
            count(row.get(key), key)
        tokens(row.get("usage"))
    require(add_tokens(tokens(row["usage"]) for row in rows.values()) == tokens(value["totals"]["usage"]),
            "Physical usage ledger token totals disagree")
    for key in LEDGER_FIELDS:
        require(sum(row[key] for row in rows.values()) == value["totals"][key],
                "Physical usage ledger counters disagree")
    return rows


def ledger_delta(before, after, *, allow_classifier_errors=False):
    first, last = ledger(before), ledger(after)
    zero = {**dict.fromkeys(LEDGER_FIELDS, 0), "usage": dict.fromkeys(TOKEN_FIELDS, 0)}
    result = {}
    for kind in first.keys() | last.keys():
        a, b = first.get(kind, zero), last.get(kind, zero)
        row = counter_delta(a, b, LEDGER_FIELDS)
        row["usage"] = token_delta(a["usage"], b["usage"])
        require(row["sends"] == row["completed"], "Physical sends are still running")
        require(row["throttled"] == 0 and (row["errors"] == 0 or
                (allow_classifier_errors and kind == "classifier" and row["errors"] <= row["sends"])),
                "Physical send failed or was throttled")
        require(row["reported_usage_sends"] <= row["sends"], "Invalid reported-usage count")
        if row["sends"]:
            result[kind] = row
    require(set(result) <= {"inference", "classifier"}, "Unaccounted auxiliary inference occurred")
    return result


def profile(snapshot):
    rows = snapshot["stats"]["policy_routing"]["profiles"]
    require(len(rows) == 1 and rows[0]["profile"] == "team-assistant", "Unexpected gateway policy profile")
    hashes = rows[0]["generation_hashes"]
    require(all(re.fullmatch(r"[0-9a-f]{16,64}", hashes.get(key, "")) for key in GENERATION_FIELDS),
            "Missing policy generation hashes")
    return rows[0]


def terminal_map(snapshot):
    """Resolve only pinned, single-target routes, never names guessed from a tier."""
    config = snapshot["configuration"]
    topology = config["topology"]
    require(topology["schema_version"] == 2, "Unsupported routing schema")
    providers = index(topology["providers"], lambda row: row["id"], "providers")
    routes = index(topology["model_routes"], lambda row: row["id"], "routes")
    policies = topology["policy_profiles"]
    require(len(policies) == 1 and policies[0]["public_id"] == "team-assistant", "Unexpected deployed policy")
    policy = policies[0]

    def resolve(route_id):
        route = routes[route_id]
        require(route["routing"] == {"mode": "primary_only", "max_target_attempts": 1, "max_upstream_sends": 1},
                "Terminal route permits unaccounted sends or alternate targets")
        require(len(route["targets"]) == 1, "Terminal route is not a single destination")
        target = route["targets"][0]
        provider = providers[target["provider"]]
        return {"route": route_id, "target": target["id"], "provider": provider["id"],
                "providerType": provider["type"], "model": target["upstream_model"],
                "endpoints": route["endpoints"], **({"service": provider["service"]} if "service" in provider else {})}

    result = {tier: resolve(policy[tier]["route"]) for tier in ("lightweight", "powerful")}
    public = [route for route in routes.values() if route.get("public_id") == "coordinator"]
    require(len(public) == 1 and public[0]["exposure"] == "public", "Missing hosted coordinator route")
    result["coordinator"] = resolve(public[0]["id"])
    result["classifier"] = resolve(policy["classifier"]["route"])
    require(result["lightweight"]["providerType"] == "openai-compatible"
            and result["lightweight"]["model"] == "qwen-3.5-2b"
            and result["lightweight"].get("service") == {"namespace": "orka-efficiency", "name": "qwen35-2b", "port": 8080}
            and result["lightweight"]["endpoints"] == ["/chat/completions"]
            and routes[result["lightweight"]["route"]]["context_window"] == 4096,
            "The lightweight route is not the tested Qwen model configuration")
    for tier in ("powerful", "coordinator"):
        require(result[tier]["providerType"] == "copilot" and result[tier]["model"] == "gpt-5.5"
                and result[tier]["endpoints"] == ["/responses"],
                "The hosted route changed")
    require(result["classifier"]["providerType"] == "typesafe-compatible"
            and result["classifier"]["model"] == "typesafe-ai/jev"
            and result["classifier"]["endpoints"] == ["/systemone"], "Classifier model changed")
    require(all(policy[key] == "powerful" for key in
                ("baseline_tier", "classifier_unavailable_tier", "classifier_uncertain_tier")),
            "Baseline or fallback policy changed")
    return policy, result


def completed_logs(logs):
    result = []
    for record in logs:
        row = {**record, **record.get("fields", {})}
        if row.get("msg") == "request completed":
            result.append(row)
    return index(result, lambda row: row.get("operation_id"), "completion logs")


def classifier_response_logs(logs, policy_id):
    """Read prompt-free receipts emitted by the classifier response boundary."""
    result = []
    for record in logs:
        row = {**record, **record.get("fields", {})}
        if row.get("msg") != "policy classifier request completed":
            continue
        require(row.get("policy_id") == policy_id and row.get("model") == "typesafe-ai/jev",
                "Classifier receipt names an unexpected policy or model")
        require(row.get("traffic_bucket") in ("request", "preflight"), "Unexpected classifier traffic bucket")
        require(row["traffic_bucket"] == "preflight" or bool(row.get("operation_id")),
                "Classifier response has no parent operation")
        require(type(row.get("status_code")) is int and 100 <= row["status_code"] <= 599
                and type(row.get("reported_usage")) is bool, "Incomplete classifier response receipt")
        identity = row.get("generation_id")
        require(isinstance(identity, str) and classifier_billing.GENERATION.fullmatch(identity),
                "Classifier response has no safe billing reference")
        result.append({"operationID": row.get("operation_id"), "trafficBucket": row["traffic_bucket"],
                       "policyID": row["policy_id"], "model": row["model"], "statusCode": row["status_code"],
                       "generationID": identity, "reportedUsage": row["reported_usage"]})
    require(len({row["generationID"] for row in result}) == len(result), "Duplicate classifier response receipt")
    return result


def preflight_usage(snapshot, rows, receipts=None):
    """A bucket has no availability flag; use the physical ledger conservatively."""
    if not rows:
        require(snapshot["configuration"]["mode"] == "off", "Missing classifier startup preflight")
        return []
    require(len(rows) == 1, "Duplicate classifier startup preflight")
    row = rows[0]
    physical = ledger(snapshot["stats"]["task_usage"]).get("classifier", {})
    sends = count(row.get("physical_classifier_sends"), "preflight sends")
    require(0 < sends <= physical.get("sends", 0) == physical.get("completed"),
            "Classifier startup preflight has no completed physical sends")
    require(physical["reported_usage_sends"] <= physical["sends"], "Invalid preflight usage availability")
    value = row["classifier_usage"]
    usage = tokens({"prompt_tokens": value["input_tokens"], "completion_tokens": value["output_tokens"],
                    "total_tokens": value["total_tokens"], "cached_tokens": value["cached_input_tokens"],
                    "reasoning_tokens": value["reasoning_tokens"]})
    require(all(usage[key] <= physical["usage"][key] for key in TOKEN_FIELDS),
            "Classifier startup preflight usage exceeds its physical ledger")
    if sends == physical["sends"]:
        require(usage == physical["usage"], "Classifier startup preflight and physical usage disagree")
    if receipts is not None:
        startup = [entry for entry in receipts if entry["trafficBucket"] == "preflight"]
        require(len(startup) == sends, "Classifier startup receipts and physical sends disagree")
        complete = all(entry["reportedUsage"] and 200 <= entry["statusCode"] <= 299 for entry in startup)
    else:
        # Without per-response receipts, the cumulative ledger cannot identify
        # whether earlier missing usage belonged to startup or ordinary work.
        complete = physical["reported_usage_sends"] == physical["sends"]
    return [{**row, "usageComplete": complete}]


def gateway_window(before, after, logs, mode, *, allow_tool_continuations=False,
                   allow_bounded_context=False, allow_classifier_fallback=False):
    """Return a complete, independently reconciled interval of gateway traffic."""
    require(timestamp(before["at"]) <= timestamp(after["at"]), "Reversed gateway interval")
    require(before["podUID"] and before["podUID"] == after["podUID"], "Gateway Pod was replaced")
    for key in ("restartCount", "containerID", "startedAt", "imageID"):
        require(key in before["container"] and before["container"][key] == after["container"].get(key),
                "Gateway container restarted or changed")
    require(all(before["container"].get(key) for key in ("containerID", "startedAt", "imageID")),
            "Gateway process identity is incomplete")
    for key in ("uid", "resourceVersion", "sha256", "digest", "topology", "mode"):
        require(before["configuration"][key] == after["configuration"][key], "Gateway configuration changed")
    require(before["configuration"]["mode"] == mode, "Wrong gateway process mode")
    for key in ("sha256", "digest"):
        require(re.fullmatch(r"[0-9a-f]{64}", before["configuration"][key]), "Missing configuration fingerprint")
    first_profile, last_profile = profile(before), profile(after)
    require(first_profile["generation_hashes"] == last_profile["generation_hashes"], "Policy generation changed")
    for value in (before, after):
        stats = value["stats"]
        require(stats["inflight"] == stats["auxiliary_inflight"] == stats["task_usage"]["inflight"] == 0,
                "Gateway snapshot was taken while requests were running")
        state = profile(value)
        require(state["effective_mode"] == mode, "Policy effective mode differs from the captured mode")
        require(state["preflight_state"] == ("ready" if mode == "enforce" else "not_required"),
                "Classifier preflight is not ready")
    a, b = before["stats"], after["stats"]
    require(b["uptime_seconds"] >= a["uptime_seconds"], "Gateway uptime reset")
    policy, destinations = terminal_map(after)
    classifier_receipts = classifier_response_logs(logs, policy["id"]) if allow_classifier_fallback else None
    previous = index(a["recent"], lambda row: row.get("operation_id"), "previous requests")
    current = index(b["recent"], lambda row: row.get("operation_id"), "recent requests")
    requests = {key: row for key, row in current.items() if key not in previous}
    totals = counter_delta(a["totals"], b["totals"], ("requests", "errors", *TOKEN_FIELDS))
    require(totals["requests"] == len(requests), "Gateway request history is missing or truncated")
    require(totals["errors"] == 0, "Gateway recorded unsuccessful requests")
    attempt_key = lambda row: (row.get("operation_id"), row.get("sequence"))
    old_attempts = index(a["recent_attempts"], attempt_key, "previous attempts")
    new_attempts = index(b["recent_attempts"], attempt_key, "recent attempts")
    attempts = {key: row for key, row in new_attempts.items() if key not in old_attempts}
    sends = counter_delta(a, b, ("upstream_attempts", "retries", "target_switches"))
    require(sends["upstream_attempts"] == len(attempts), "Gateway physical-attempt history is missing or truncated")
    require(sends["retries"] == sends["target_switches"] == 0, "Unexpected gateway retry or target switch")
    require(set(key[0] for key in attempts) == set(requests), "Orphaned or missing physical attempts")
    completion = completed_logs(logs)
    operations = []
    for operation_id, row in requests.items():
        require(operation_id in completion, "Gateway completion log is missing")
        log = completion[operation_id]
        require(row["status"] == log["status"] == 200, "Gateway operation did not succeed")
        require(row["upstream_sends"] == log["upstream_sends"] == 1, "Unexpected number of terminal sends")
        require((operation_id, 1) in attempts, "Physical attempt does not match its logical operation")
        attempt = attempts[(operation_id, 1)]
        require(attempt["outcome"] == "succeeded" and attempt["cleanup_complete"] is True
                and attempt["status_code"] == 200 and attempt["attempt_kind"] == "normal",
                "Incomplete or unexpected physical attempt")
        usage = tokens(attempt.get("reported_usage"))
        require(usage["total_tokens"] == row["total_tokens"], "Logical and physical reported token totals disagree")
        require(all(log.get(key, 0) == usage[key] for key in TOKEN_FIELDS), "Completion log and attempt usage disagree")
        public = row["model"]
        require(log["model"] == public, "Completion log and request model disagree")
        require(row["route_id"] == log["route_id"] == attempt["route_id"], "Operation route identities disagree")
        if public == "team-assistant":
            require(log["policy_id"] == policy["id"] and log["policy_mode"] == mode, "Policy decision does not match the installed policy")
            require(all(log.get(key) == last_profile["generation_hashes"][key] for key in GENERATION_FIELDS),
                    "Decision and snapshot policy generations disagree")
            decision = log["policy_decision"]
            continuation = allow_tool_continuations and decision == "replay_binding"
            fallback = (allow_classifier_fallback and mode == "enforce" and decision == "unavailable_fallback"
                        and log.get("policy_failure_category") in ("upstream_5xx", "breaker_open"))
            if fallback and log["policy_failure_category"] == "breaker_open":
                require(log.get("policy_classifier_latency_ms") == 0,
                        "Open circuit breaker unexpectedly reports classifier latency")
            # Vekil's aggregate flag also covers bounded background instructions
            # and older messages. The coding walkthrough reports this limitation
            # explicitly; the earlier fixed-answer demo remains strict by default.
            require(isinstance(log.get("policy_truncated"), bool), "Missing classifier context flag")
            require(continuation or allow_bounded_context or log["policy_truncated"] is False,
                    "Classifier saw a truncated request")
            tier = log["policy_tier"]
            require(tier in ("lightweight", "powerful"), "Missing terminal policy tier")
            require(continuation or fallback or decision == ("classified" if mode == "enforce" else "baseline"),
                    "Request used fallback or did not receive the expected policy decision")
            if continuation:
                require(tier == "powerful" and not log.get("policy_classifier_latency_ms"),
                        "Tool continuation did not retain the hosted model without classification")
            require(fallback or not log["policy_failure_category"], "Classifier recorded a failure")
            require(not fallback or tier == policy["classifier_unavailable_tier"], "Fallback used the wrong destination")
            require(mode != "off" or tier == policy["baseline_tier"], "Baseline request used the wrong tier")
            destination, role = destinations[tier], "worker"
        else:
            require(public == "coordinator" and not log.get("policy_id"), "Unaccounted public model request")
            destination, role, tier = destinations["coordinator"], "coordinator", None
            require(attempt.get("target_id") == destination["target"]
                    and attempt.get("provider_id") == destination["provider"], "Coordinator destination changed")
        # The public stats view hides policy terminals. When completion logs
        # retain their identities, require that independent evidence to agree.
        require(log["final_target"] in {public, destination["target"]}, "Completion log contradicts the selected terminal")
        for field, expected in (("provider", destination["provider"]), ("provider_kind", destination["providerType"])):
            require(not log.get(field) or log[field] == expected, "Completion log contradicts the selected provider")
        operations.append({"operationID": operation_id, "role": role, "tier": tier,
                           "destination": destination, "usage": usage,
                           "policyMode": log.get("policy_mode"), "policyDecision": log.get("policy_decision"),
                           "classifierFailureCategory": log.get("policy_failure_category", ""),
                           "classifierInputTruncated": log.get("policy_truncated"),
                           "classifierLatencyMs": log.get("policy_classifier_latency_ms", 0),
                           "classifierToolCount": log.get("policy_tool_count", 0),
                           "classifierInputBytes": log.get("policy_input_bytes", 0)})
    require(len(attempts) == len(operations), "Unaccounted terminal attempts")
    if any(row["policyDecision"] == "replay_binding" for row in operations):
        selections = {"classified" if mode == "enforce" else "baseline"}
        if allow_classifier_fallback and mode == "enforce":
            selections.add("unavailable_fallback")
        require(any(row["tier"] == "powerful" and row["policyDecision"] in selections for row in operations),
                "Tool continuation has no initial model selection in this interval")
    usage = add_tokens(row["usage"] for row in operations)
    require(usage == {key: totals[key] for key in TOKEN_FIELDS}, "Request counters and operation usage disagree")
    require(usage == token_delta(a["physical_usage"], b["physical_usage"]), "Physical usage counters disagree")
    require(not any(token_delta(a["wasted_usage"], b["wasted_usage"]).values()), "Gateway recorded wasted usage")
    physical = ledger_delta(a["task_usage"], b["task_usage"], allow_classifier_errors=allow_classifier_fallback)
    inference = physical.get("inference")
    require(bool(inference) == bool(operations), "Missing inference ledger")
    if inference:
        require(inference["sends"] == inference["reported_usage_sends"] == len(operations)
                and inference["usage"] == usage, "Inference ledger and operations disagree")
    worker_count = sum(row["role"] == "worker" for row in operations)
    policy_delta = counter_delta(first_profile["totals"], last_profile["totals"],
                                 ("eligible", "physical_classifier_sends"))
    require(policy_delta["eligible"] == worker_count, "Policy request counts disagree")
    tiers = counter_delta(first_profile["totals"]["actual_tiers"], last_profile["totals"]["actual_tiers"],
                          ("lightweight", "powerful", "unknown"))
    require(tiers == {key: sum(row["tier"] == key for row in operations) for key in tiers}, "Policy tier counters disagree")
    classifier = physical.get("classifier", {**dict.fromkeys(LEDGER_FIELDS, 0), "usage": dict.fromkeys(TOKEN_FIELDS, 0)})
    expected = {row["operationID"]: row for row in operations
                if row["policyDecision"] == "classified" or
                (row["policyDecision"] == "unavailable_fallback"
                 and row["classifierFailureCategory"] == "upstream_5xx")}
    expected_classifications = len(expected) if mode == "enforce" else 0
    require(classifier["sends"] == policy_delta["physical_classifier_sends"] == expected_classifications,
            "Classifier physical sends do not match classified requests")
    if classifier_receipts is not None:
        operation_ids = {row["operationID"] for row in operations}
        own = [row for row in classifier_receipts if row["operationID"] in operation_ids]
        require(len(own) == len(expected) and {row["operationID"] for row in own} == set(expected),
                "Classifier responses do not match classified or fallback operations")
        failures = []
        for row in own:
            require(row["trafficBucket"] == "request", "Classifier request was mislabeled as startup")
            if expected[row["operationID"]]["policyDecision"] == "unavailable_fallback":
                require(500 <= row["statusCode"] <= 599 and row["reportedUsage"] is False,
                        "Fallback is not an unmetered upstream failure")
                failures.append(row)
            else:
                require(200 <= row["statusCode"] <= 299 and row["reportedUsage"] is True,
                        "Successful classification has no reported usage")
        require(classifier["errors"] == len(failures)
                and classifier["reported_usage_sends"] + len(failures) == classifier["sends"],
                "Classifier failure receipts and physical counters disagree")
        classifier["unmeteredFailures"] = failures
    classifier_usage = counter_delta(first_profile["totals"]["classifier_usage"], last_profile["totals"]["classifier_usage"],
                                     ("input_tokens", "output_tokens", "total_tokens", "cached_input_tokens", "reasoning_tokens"))
    require(classifier_usage == {
        "input_tokens": classifier["usage"]["prompt_tokens"], "output_tokens": classifier["usage"]["completion_tokens"],
        "total_tokens": classifier["usage"]["total_tokens"], "cached_input_tokens": classifier["usage"]["cached_tokens"],
        "reasoning_tokens": classifier["usage"]["reasoning_tokens"],
    }, "Classifier policy accounting and physical usage disagree")
    preflights = []
    for state in (first_profile, last_profile):
        rows = [row["metrics"] for row in state["traffic_buckets"] if row["traffic_bucket"] == "preflight"]
        preflights.append(rows)
    require(preflights[0] == preflights[1], "Classifier preflight overlapped the measured interval")
    classifier["usageComplete"] = classifier["reported_usage_sends"] == classifier["sends"]
    return {"operations": operations, "roles": role_totals(operations),
            "workerRequests": worker_count, "classifier": classifier,
            "preflightBeforeInterval": preflight_usage(before, preflights[0], classifier_receipts), "destinations": destinations,
            "gateway": {"podUID": before["podUID"], "container": before["container"],
                        "configurationSHA256": before["configuration"]["sha256"],
                        "configurationDigest": before["configuration"]["digest"],
                        "generations": first_profile["generation_hashes"]}}


def parse_answer(value):
    value = value.strip()
    if value.startswith("```json\n") and value.endswith("```"):
        value = value[8:-3].strip()
    elif value.startswith("```\n") and value.endswith("```"):
        value = value[4:-3].strip()
    return json.loads(value)


def verify_request(root, phase, workload):
    directory = root / phase / workload
    saved = read(directory / "evidence.json")
    item = WORKLOADS[workload]
    team, namespace = item["team"], "team-" + item["team"]
    require(saved["phase"] == phase and saved["workload"] == workload and saved["team"] == team,
            "Wrong request evidence identity")
    request = read(directory / "client-request.json")
    require(request == read(root / (workload + ".request.json")), "Request differs from the retained fixture")
    require(saved["requestSHA256"] == digest(request), "Request fingerprint mismatch")
    task, agent = read(directory / "task.json"), read(directory / "agent.json")
    uid, name = task["metadata"]["uid"], task["metadata"]["name"]
    require(uid == saved["taskUID"] and name == saved["task"] and task["metadata"]["namespace"] == namespace,
            "Task identity mismatch")
    require(task["spec"]["type"] == "ai" and task["status"]["phase"] == "Succeeded", "Native AI Task did not succeed")
    require(task["spec"]["prompt"].strip() == item["prompt"].strip(), "Business prompt changed")
    require(task["spec"]["agentRef"]["name"] == agent["metadata"]["name"] == "efficiency-" + team,
            "Wrong Agent executed the request")
    require(agent["spec"]["providerRef"]["name"] == "platform" and agent["spec"]["model"]["name"] == "team-assistant",
            "Agent bypassed the shared model alias")
    require(not task["spec"].get("ai", {}).get("providerRef"), "Task overrode its Agent provider")
    old = {row["metadata"]["uid"] for row in read(directory / "tasks-before.json")}
    created = [row for row in read(directory / "tasks-after.json") if row["metadata"]["uid"] not in old]
    require(len(created) == 1 and created[0]["metadata"]["uid"] == uid, "Request did not create exactly one new Task")
    history = read(directory / "events.json")
    require(history["streamID"] == name and history["namespace"] == namespace, "Wrong Task event history")
    require([row["seq"] for row in history["events"]] == list(range(1, history["latestSeq"] + 1)),
            "Task event history is missing or truncated")
    require(any(row["type"] == "TaskSucceeded" for row in history["events"]), "Task completion event missing")
    require(not any(row["type"] == "ToolCallStarted" for row in history["events"]), "Worker called a tool")
    before, after = read(directory / "gateway-before.json"), read(directory / "gateway-after.json")
    window = gateway_window(before, after, read(directory / "gateway-operations.json"), MODES.get(phase, "off"))
    workers = [row for row in window["operations"] if row["role"] == "worker"]
    coordinators = [row for row in window["operations"] if row["role"] == "coordinator"]
    require(len(workers) == 1 and coordinators, "Request must contain one worker completion and hosted coordination")
    worker = workers[0]
    model_events = [row["content"] for row in history["events"]
                    if row["type"] == "ModelRequestCompleted" and "inputTokens" in row.get("content", {})]
    require(len(model_events) == 1 and model_events[0]["toolCalls"] == 0, "Native model completion evidence is missing")
    # The raw Anthropic response excludes cached input. Orka's normalized
    # provider event includes it, matching the gateway's total input count.
    normalized = [row["content"]["usage"] for row in history["events"]
                  if row["type"] == "ModelUsageUpdated" and row.get("content", {}).get("usage", {}).get("status") == "completed"]
    require(len(normalized) == 1 and normalized[0]["complete"] is True
            and normalized[0]["scope"] == "call" and normalized[0]["source"] == "provider"
            and normalized[0]["model"] == "team-assistant", "Normalized native usage evidence is missing")
    require(normalized[0]["inputTokens"] == worker["usage"]["prompt_tokens"]
            and normalized[0]["outputTokens"] == model_events[0]["outputTokens"] == worker["usage"]["completion_tokens"],
            "Native Task usage differs from its serially captured gateway operation")
    if normalized[0].get("cachedInputTokens") is not None:
        require(normalized[0]["cachedInputTokens"] == worker["usage"]["cached_tokens"], "Native cached usage and gateway usage disagree")
    result = read(directory / "result.json")["result"]
    reply = read(directory / "client-response.json")["choices"]
    require(len(reply) == 1 and reply[0]["finish_reason"] == "stop", "Incomplete client answer")
    if phase == "orchestration-before":
        require(workload == "inventory-stock", "only the inventory format changes")
        checks = check_stock_prose(result)
        require(reply[0]["message"]["content"].strip() == result.strip(), "Client did not return its Task result")
        require(isinstance(saved.get("answer"), str) and saved["answer"].strip() == result.strip(),
                "Recorded answer differs from its Task result")
        answer = result
    else:
        answer = parse_answer(result)
        require(parse_answer(reply[0]["message"]["content"]) == answer == saved["answer"], "Client and Task answers differ")
        if item["kind"] == "data":
            checks = check_data(workload, answer)
        else:
            validation = read(directory / "validation-request.json")
            finished = read(directory / "validation-task.json")
            spec = validation["spec"]
            require(spec["type"] == "container" and "@sha256:" in spec["image"], "Validation image is not pinned")
            require(spec["command"] == ["python", "-c", (HERE / "check.py").read_text()]
                    and spec["args"] == [workload, json.dumps(answer)], "Different code was checked")
            require(all(finished["spec"][key] == spec[key] for key in ("type", "image", "command", "args"))
                    and finished["metadata"]["name"] == validation["metadata"]["name"]
                    and finished["metadata"]["namespace"] == namespace
                    and finished["status"]["phase"] == "Succeeded", "Code validation Task did not succeed")
            checked = json.loads(read(directory / "validation-result.json")["result"].strip())
            require(checked["workload"] == workload and checked["passed"] is True
                    and checked["checkCount"] == len(checked["checks"]) == (6 if team == "payments" else 8),
                    "Code correctness checks are missing or failed")
            checks = checked["checks"]
    require(saved["checks"]["passed"] is True and saved["checks"]["checks"] == checks
            and saved["checks"]["checkCount"] == len(checks) and saved["workerToolCalls"] == 0,
            "Recorded check summary disagrees with retained evidence")
    for key in ("clientSeconds", "totalSeconds"):
        require(type(saved.get(key)) in (int, float) and math.isfinite(saved[key]) and saved[key] > 0,
                "Missing or invalid actual latency: " + key)
    require(saved["totalSeconds"] >= saved["clientSeconds"], "Verification finished before the client response")
    return {"workload": workload, "team": team, "task": name, "taskUID": uid, "requestSHA256": digest(request),
            "answer": answer, "checkCount": len(checks), "checks": checks, "worker": worker,
            "coordinator": {"requests": len(coordinators), "usage": add_tokens(row["usage"] for row in coordinators)},
            "classifier": window["classifier"], "operationIDs": [row["operationID"] for row in window["operations"]],
            "startedAt": saved["startedAt"], "finishedAt": saved["finishedAt"],
            "clientSeconds": saved["clientSeconds"], "totalSeconds": saved["totalSeconds"],
            "gateway": window["gateway"], "agentSpecSHA256": digest(agent["spec"])}


def orka_usage(directory, team, requests, window, period, *, allow_unreported_tasks=()):
    summary = read(directory / (team + "-usage.json"))
    rows = []
    for category in ("unassociated", "other_requests"):
        report = read(directory / (team + "-usage-" + category + ".json"))
        selection, group = report["selection"], report["otherWork"]
        require(selection["teams"] == ["team-" + team]
                and timestamp(selection["from"]) == timestamp(period["startedAt"])
                and timestamp(selection["until"]) == timestamp(period["finishedAt"]), "Wrong Orka usage selection")
        require(group["category"] == category and group["page"]["offset"] == 0
                and group["page"]["total"] == group["taskCount"] == len(group.get("tasks", [])),
                "Orka usage detail is missing or paginated")
        match = [entry for entry in summary["otherWork"] if entry["category"] == category]
        require(len(match) == 1 and match[0]["usage"] == group["usage"], "Orka summary and detail usage disagree")
        rows.extend(group.get("tasks", []))
    tasks = index(rows, lambda row: row["taskUID"], "Orka usage Tasks")
    worker_ids = {request["taskUID"] for request in requests}
    require(worker_ids <= tasks.keys(), "Native Task is missing from Orka usage")
    require(set(allow_unreported_tasks) <= worker_ids, "Unknown Task allowed to omit Orka usage")
    worker_tokens, coordinator_tokens, unavailable = [], [], []
    gateway_only_tokens = []
    for uid, row in tasks.items():
        usage = row["usage"]
        require(row["namespace"] == "team-" + team, "Cross-team Orka usage")
        require(usage["measurements"] == len(row["measurements"]), "Orka measurement history is missing")
        if uid in allow_unreported_tasks and usage["completeness"] == "unavailable":
            request = next(item for item in requests if item["taskUID"] == uid)
            require(row["taskName"] == request["task"] and row["phase"] == "Succeeded", "Orka Task usage identity mismatch")
            require(usage["measurements"] > 0 and usage["missingMeasurements"] == usage["measurements"]
                    and usage["partialMeasurements"] == usage["estimatedTokens"] == 0
                    and all(usage[key] == 0 for key in ("inputTokens", "outputTokens", "totalTokens", "cachedInputTokens")),
                    "Unavailable Orka usage contains counts or estimates")
            for measurement in row["measurements"]:
                require(measurement["completeness"] == "unavailable" and measurement["status"] == "completed"
                        and measurement["scope"] == "attempt" and measurement["provider"] == "codex"
                        and measurement["source"] == "agent" and measurement["model"] == "team-assistant"
                        and measurement.get("gap") == "No consumed-token counts reported"
                        and measurement.get("inputTokens") is None and measurement.get("outputTokens") is None,
                        "Unexpected gap in Orka coding usage")
            unavailable.append({"taskUID": uid, "task": row["taskName"],
                                "gap": "No consumed-token counts reported", "tokens": None})
            gateway_only_tokens.append(request["worker"]["usage"])
            continue
        require(usage["completeness"] == "complete" and usage["missingMeasurements"] == usage["partialMeasurements"] == 0
                and usage["estimatedTokens"] == 0, "Orka usage is incomplete or estimated")
        value = {"prompt_tokens": count(usage["inputTokens"], "Orka input tokens"),
                 "completion_tokens": count(usage["outputTokens"], "Orka output tokens"),
                 "total_tokens": count(usage["totalTokens"], "Orka total tokens"),
                 "cached_tokens": count(usage["cachedInputTokens"], "Orka cached tokens"), "reasoning_tokens": 0}
        require(value["total_tokens"] == value["prompt_tokens"] + value["completion_tokens"], "Orka token components disagree")
        for measurement in row["measurements"]:
            require(measurement["completeness"] == "complete" and measurement["status"] == "completed"
                    and not measurement.get("gap"), "Orka measurement is incomplete")
        if uid in worker_ids:
            request = next(item for item in requests if item["taskUID"] == uid)
            require(row["taskName"] == request["task"] and row["phase"] == "Succeeded", "Orka Task usage identity mismatch")
            require(all(value[key] == request["worker"]["usage"][key] for key in ("prompt_tokens", "completion_tokens", "total_tokens")),
                    "Orka worker usage and gateway usage disagree")
            worker_tokens.append(value)
        else:
            require(uid.startswith("call-") and all(m["model"] in ("coordinator", "platform/coordinator") for m in row["measurements"]),
                    "Unaccounted Orka model consumption")
            coordinator_tokens.append(value)
    expected = add_tokens(operation["usage"] for operation in window["operations"])
    # Reconcile the available Orka measurements, and separately account for the
    # gateway calls belonging to explicitly unavailable attempts. Never insert
    # gateway numbers into the missing Orka measurement or call that gap zero.
    total = add_tokens(worker_tokens + coordinator_tokens + gateway_only_tokens)
    require(all(total[key] == expected[key] for key in ("prompt_tokens", "completion_tokens", "total_tokens")),
            "Orka total consumption and gateway inference usage disagree")
    return {"workers": add_tokens(worker_tokens), "coordinator": add_tokens(coordinator_tokens),
            "workerTasks": len(worker_ids), "coordinatorMeasurements": len(coordinator_tokens),
            "reportedWorkerTasks": len(worker_tokens), "unavailableTasks": unavailable,
            "category": "Standalone requests are in other usage, outside PR delivery totals."}


def resource_summary(samples, start, end):
    selected = [row for row in samples if timestamp(start) <= timestamp(row["at"]) <= timestamp(end)]
    require(len(selected) >= 2 and not any(row.get("sampleError") for row in selected), "Model resource samples are missing")
    identity = (selected[0]["node"], selected[0]["podUID"], selected[0]["restarts"])
    readings = []
    for sample in selected:
        require((sample["node"], sample["podUID"], sample["restarts"]) == identity, "Model Pod restarted or moved during the interval")
        require(len(sample["stats"]) == 1 and sample["stats"][0]["podRef"]["uid"] == identity[1], "Wrong model Pod resource sample")
        pod = sample["stats"][0]
        readings.append({"at": sample["at"], "cpuAt": pod["cpu"]["time"],
                         "cpu": count(pod["cpu"]["usageCoreNanoSeconds"], "CPU time"),
                         "memory": count(pod["memory"]["workingSetBytes"], "working-set memory")})
    readings.sort(key=lambda row: timestamp(row["at"]))
    require(all(a["cpu"] <= b["cpu"] for a, b in zip(readings, readings[1:])), "Model CPU counter reset")
    times = [timestamp(start), *(timestamp(row["at"]) for row in readings), timestamp(end)]
    require(all((b - a).total_seconds() <= 30 for a, b in zip(times, times[1:])), "Model resource sampling has gaps")
    require(all(0 <= (timestamp(row["at"]) - timestamp(row["cpuAt"])).total_seconds() <= 30 for row in readings),
            "Model resource samples are stale")
    elapsed = (timestamp(readings[-1]["cpuAt"]) - timestamp(readings[0]["cpuAt"])).total_seconds()
    require(elapsed > 0, "Model samples have no elapsed interval")
    cpu_seconds = (readings[-1]["cpu"] - readings[0]["cpu"]) / 1e9
    return {"node": identity[0], "podUID": identity[1], "restarts": identity[2], "sampleCount": len(readings),
            "firstSampleAt": readings[0]["at"], "lastSampleAt": readings[-1]["at"],
            "firstCPUReadingAt": readings[0]["cpuAt"], "lastCPUReadingAt": readings[-1]["cpuAt"],
            "sampledSeconds": round(elapsed, 3), "cpuSeconds": round(cpu_seconds, 3),
            "meanCPUCores": round(cpu_seconds / elapsed, 3),
            "peakWorkingSetMiB": round(max(row["memory"] for row in readings) / 2**20, 1),
            "scope": "Whole model Pod during the sampled interval, including idle time; no per-request allocation."}


def aggregate(root):
    root = Path(root)
    run = read(root / "run.json")
    require(run["context"] == "sertac-aks" and run["demo"] == "12-efficiency", "Wrong run context")
    try:
        resources = [json.loads(line) for line in (root / "resources.jsonl").read_text().splitlines() if line.strip()]
    except (OSError, ValueError) as exc:
        raise EvidenceError("Missing or invalid model resource samples") from exc
    result = {"schemaVersion": 1, "context": run["context"], "phases": {}, "comparisons": []}
    verified = {}
    for phase, mode in MODES.items():
        directory = root / phase
        period = read(directory / "phase.json")
        require(timestamp(period["startedAt"]) < timestamp(period["finishedAt"]), "Phase has not finished")
        verified[phase] = {name: verify_request(root, phase, name) for name in WORKLOADS}
        phase_result = {"teams": {}, "resources": resource_summary(resources, period["startedAt"], period["finishedAt"])}
        for team in TEAMS:
            before, after = (read(directory / f"{team}-gateway-{stage}.json") for stage in ("start", "end"))
            window = gateway_window(before, after, read(directory / f"{team}-operations-end.json"), mode)
            requests = [row for row in verified[phase].values() if row["team"] == team]
            identifiers = [op for row in requests for op in row["operationIDs"]]
            require(len(identifiers) == len(set(identifiers)) and set(identifiers) == {op["operationID"] for op in window["operations"]},
                    "Phase contains overlapping captures or unaccounted model traffic")
            require(all(row["gateway"] == window["gateway"] for row in requests), "Gateway changed between phase requests")
            require(all(timestamp(period["startedAt"]) <= timestamp(row["startedAt"]) < timestamp(row["finishedAt"])
                        <= timestamp(period["finishedAt"]) for row in requests), "Request falls outside the usage interval")
            window["orkaUsage"] = orka_usage(directory, team, requests, window, period)
            window["checkedRequests"] = len(requests)
            window["checksPassed"] = sum(row["checkCount"] for row in requests)
            phase_result["teams"][team] = window
        result["phases"][phase] = phase_result
    for workload in WORKLOADS:
        baseline, routed = (verified[phase][workload] for phase in MODES)
        require(baseline["requestSHA256"] == routed["requestSHA256"]
                and baseline["agentSpecSHA256"] == routed["agentSpecSHA256"], "Platform comparison changed the request or Agent")
        require(baseline["taskUID"] != routed["taskUID"], "Comparison reused a Task")
        require(baseline["gateway"]["configurationDigest"] == routed["gateway"]["configurationDigest"]
                and baseline["gateway"]["container"]["imageID"] == routed["gateway"]["container"]["imageID"],
                "Gateway comparison changed the routes or executable, beyond the routing mode")
        result["comparisons"].append({"workload": workload, "team": baseline["team"],
                                      "requestSHA256": baseline["requestSHA256"], "baseline": baseline, "routed": routed})
    routed_workers = [row["routed"]["worker"] for row in result["comparisons"]]
    require({row["tier"] for row in routed_workers} == {"lightweight", "powerful"}, "Routed run did not demonstrate both model destinations")
    before = verify_request(root, "orchestration-before", "inventory-stock")
    after = verified["baseline"]["inventory-stock"]
    require(before["requestSHA256"] == after["requestSHA256"]
            and before["agentSpecSHA256"] != after["agentSpecSHA256"], "Orchestration comparison is missing the isolated instruction change")
    old_agent = read(root / "orchestration-before/inventory-stock/agent.json")["spec"]
    new_agent = read(root / "baseline/inventory-stock/agent.json")["spec"]
    require({key: value for key, value in old_agent.items() if key != "systemPrompt"}
            == {key: value for key, value in new_agent.items() if key != "systemPrompt"},
            "Orchestration comparison changed more than the Agent instruction")
    require(before["taskUID"] != after["taskUID"]
            and before["gateway"]["configurationDigest"] == after["gateway"]["configurationDigest"],
            "Orchestration comparison reused work or changed gateway routes")
    require(all(result["phases"]["baseline"]["resources"][key] == result["phases"]["routed"]["resources"][key]
                for key in ("node", "podUID", "restarts")), "Model service changed between comparison phases")
    result["orchestration"] = {"requestUnchanged": True, "before": before["answer"], "after": after["answer"],
                               "beforeTaskUID": before["taskUID"], "afterTaskUID": after["taskUID"]}
    result["limitations"] = [
        "These fixed requests passed the retained checks; this is not a general model benchmark.",
        "Gateway inference and Orka usage describe the same calls and must not be added together.",
        "Classifier consumption is separate; startup preflight is outside measured request intervals.",
        "No dollar savings or speed improvement is inferred from token counts or CPU samples.",
    ]
    return result


def render(report):
    lines = ["Verified answers and actual destinations", "", "Request                Baseline         Routed           Checks"]
    for row in report["comparisons"]:
        a, b = row["baseline"], row["routed"]
        lines.append(f"{row['workload']:<22} {a['worker']['destination']['model']:<16} "
                     f"{b['worker']['destination']['model']:<16} {a['checkCount']} / {b['checkCount']} passed")
    lines += ["", "Reported tokens, counted once", "Phase     Team       Worker       Coordinator  Classifier"]
    for phase, value in report["phases"].items():
        for team, window in value["teams"].items():
            usage, classifier = window["orkaUsage"], window["classifier"]
            classified = str(classifier["usage"]["total_tokens"]) if classifier["usageComplete"] else "not reported"
            lines.append(f"{phase:<9} {team:<10} {usage['workers']['total_tokens']:<12} "
                         f"{usage['coordinator']['total_tokens']:<12} {classified} ({classifier['sends']} sends)")
            preflight = window["preflightBeforeInterval"]
            if preflight:
                startup_tokens = (str(preflight[0]["classifier_usage"]["total_tokens"]) + " reported tokens"
                                  if preflight[0]["usageComplete"] else "tokens unavailable")
                lines.append(f"  Startup classifier, outside these requests: {preflight[0]['physical_classifier_sends']} sends; "
                             f"{startup_tokens}.")
        resources = value["resources"]
        lines.append(f"  Model Pod: {resources['cpuSeconds']} CPU seconds over {resources['sampledSeconds']} sampled seconds; "
                     f"{resources['peakWorkingSetMiB']} MiB peak working set.")
    lines += ["", "Same application requests. Separate successful Tasks. Zero worker tool calls.", *report["limitations"]]
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-dir", required=True, type=Path)
    parser.add_argument("--json", action="store_true", help="print the verified machine-readable report")
    args = parser.parse_args()
    try:
        result = aggregate(args.run_dir)
        print(json.dumps(result, indent=2) if args.json else render(result))
    except (EvidenceError, KeyError, TypeError, IndexError, ValueError) as exc:
        print("Evidence check failed: " + str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
