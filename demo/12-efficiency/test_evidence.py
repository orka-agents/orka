"""Failure cases for the report's evidence boundaries, using synthetic records."""

import copy
import json
from pathlib import Path
import tempfile
import unittest

import yaml

import evidence as e
from fixtures import WORKLOADS, agent_spec, client_request


def usage(prompt=0, completion=0, cached=0):
    return {"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion,
            "cached_tokens": cached, "reasoning_tokens": 0}


def policy_usage(value):
    return {"input_tokens": value["prompt_tokens"], "output_tokens": value["completion_tokens"],
            "total_tokens": value["total_tokens"], "cached_input_tokens": value["cached_tokens"], "reasoning_tokens": 0}


def physical_row(sends=0, value=None, reported=None):
    return {"sends": sends, "completed": sends, "errors": 0, "throttled": 0,
            "reported_usage_sends": sends if reported is None else reported, "duration_ms": sends * 25,
            "usage": value or usage()}


def metrics(sends=0):
    return {"eligible": 0, "sampled": 0, "admitted": 0, "drop_reasons": [],
            "classifier": {"completion": 0, "unavailable": 0, "uncertain": 0, "abstain": 0},
            "actual_tiers": {"lightweight": 0, "powerful": 0, "unknown": 0},
            "classifier_usage": policy_usage(usage(sends * 5, sends * 2)), "physical_classifier_sends": sends}


def snapshot(mode="off"):
    config = yaml.safe_load((Path(__file__).parent / "manifests/providers.yaml").read_text())
    providers = [{key: row[key] for key in ("id", "type", "trust_domain")} for row in config["providers"]]
    next(row for row in providers if row["id"] == "local-aikit")["service"] = {
        "namespace": "orka-efficiency", "name": "qwen35-2b", "port": 8080}
    topology = {"schema_version": 2, "providers": providers,
                "model_routes": config["model_routes"], "policy_profiles": config["policy_profiles"]}
    preflight = 1 if mode == "enforce" else 0
    profile = {"profile": "team-assistant", "effective_mode": mode,
               "preflight_state": "ready" if preflight else "not_required", "breaker_state": "closed",
               "generation_hashes": {key: str(i + 1) * 64 for i, key in enumerate(e.GENERATION_FIELDS)},
               "totals": metrics(preflight), "traffic_buckets": []}
    if preflight:
        profile["traffic_buckets"] = [{"traffic_bucket": "preflight", "metrics": metrics(1)}]
    classifier = {"kind": "classifier", **physical_row(preflight, usage(preflight * 5, preflight * 2))}
    return {"at": "2026-09-22T00:00:00Z", "podUID": "gateway-pod", "pod": "gateway",
            "container": {"containerID": "containerd://abc", "startedAt": "2026-09-21T23:50:00Z",
                          "restartCount": 0, "imageID": "test@sha256:" + "a" * 64},
            "configuration": {"uid": "config-map", "resourceVersion": "3", "sha256": "a" * 64,
                              "digest": "b" * 64, "mode": mode, "topology": topology},
            "stats": {"uptime_seconds": 100, "inflight": 0, "auxiliary_inflight": 0,
                      "totals": {"requests": 0, "errors": 0, **usage()},
                      "recent": [], "recent_attempts": [], "physical_usage": usage(), "wasted_usage": usage(),
                      "upstream_attempts": 0, "retries": 0, "target_switches": 0,
                      "task_usage": {"inflight": 0, "totals": physical_row(preflight, classifier["usage"]),
                                     "by_kind": [classifier] if preflight else []},
                      "policy_routing": {"profiles": [profile]}}}


def window(mode="enforce", tier="lightweight"):
    before = snapshot(mode)
    after = copy.deepcopy(before)
    after["at"] = "2026-09-22T00:01:00Z"
    stats = after["stats"]
    stats["uptime_seconds"] += 60
    logs = []
    for operation_id, model, value in (("coord-1", "coordinator", usage(80, 20)),
                                       ("worker-1", "team-assistant", usage(10, 5))):
        route = "coordinator-route" if model == "coordinator" else "team-assistant"
        target = "coordinator-copilot" if model == "coordinator" else "team-assistant"
        row = {"operation_id": operation_id, "model": model, "route_id": route, "final_target": target,
               "upstream_sends": 1, "status": 200, "total_tokens": value["total_tokens"]}
        stats["recent"].append(row)
        stats["recent_attempts"].append({"operation_id": operation_id, "route_id": route, "target_id": target,
                                          "provider_id": "copilot" if model == "coordinator" else "",
                                          "sequence": 1, "outcome": "succeeded", "cleanup_complete": True,
                                          "status_code": 200, "attempt_kind": "normal", "reported_usage": value})
        log = {"msg": "request completed", **row, **value}
        if model == "team-assistant":
            log.update({"policy_id": "team-efficiency", "policy_mode": mode, "policy_tier": tier,
                        "policy_decision": "classified" if mode == "enforce" else "baseline",
                        "policy_failure_category": "", "policy_truncated": False,
                        "policy_classifier_latency_ms": 25, "policy_tool_count": 4, "policy_input_bytes": 1200,
                        **e.profile(after)["generation_hashes"]})
        logs.append(log)
    stats["totals"] = {"requests": 2, "errors": 0, **usage(90, 25)}
    stats["physical_usage"] = usage(90, 25)
    stats["upstream_attempts"] = 2
    classifier_sends = 2 if mode == "enforce" else 0
    stats["task_usage"]["by_kind"] = [{"kind": "inference", **physical_row(2, usage(90, 25))}]
    if classifier_sends:
        stats["task_usage"]["by_kind"].append({"kind": "classifier", **physical_row(2, usage(10, 4))})
    stats["task_usage"]["totals"] = physical_row(2 + classifier_sends,
                                                 usage(90 + 5 * classifier_sends, 25 + 2 * classifier_sends))
    profile = e.profile(after)
    profile["totals"]["eligible"] += 1
    profile["totals"]["actual_tiers"][tier] += 1
    if mode == "enforce":
        profile["totals"]["physical_classifier_sends"] += 1
        profile["totals"]["classifier_usage"] = policy_usage(usage(10, 4))
    return before, after, logs


class GatewayEvidenceTests(unittest.TestCase):
    def fallback_window(self):
        before, after, logs = window(tier="powerful")
        logs[1].update(policy_decision="unavailable_fallback", policy_failure_category="upstream_5xx")
        stats = after["stats"]
        classifier = next(row for row in stats["task_usage"]["by_kind"] if row["kind"] == "classifier")
        classifier.update(errors=1, reported_usage_sends=1, usage=usage(5, 2))
        stats["task_usage"]["totals"].update(errors=1, reported_usage_sends=3, usage=usage(95, 27))
        e.profile(after)["totals"]["classifier_usage"] = policy_usage(usage(5, 2))
        for identity, bucket, operation, status, reported in (
                ("A", "preflight", None, 200, True), ("B", "request", "worker-1", 503, False)):
            logs.append({"msg": "policy classifier request completed", "policy_id": "team-efficiency",
                         "model": "typesafe-ai/jev", "traffic_bucket": bucket, "operation_id": operation,
                         "status_code": status, "reported_usage": reported, "generation_id": "gen_" + identity * 26})
        return before, after, logs

    def test_recorded_fallback_retains_unknown_tokens_and_exact_failure_identity(self):
        args = self.fallback_window()
        result = e.gateway_window(*args, "enforce", allow_classifier_fallback=True)
        classifier = result["classifier"]
        self.assertIs(classifier["usageComplete"], False)
        self.assertEqual(classifier["reported_usage_sends"], 0)
        self.assertEqual(classifier["errors"], 1)
        self.assertEqual(classifier["unmeteredFailures"][0]["generationID"], "gen_" + "B" * 26)
        self.assertTrue(result["preflightBeforeInterval"][0]["usageComplete"])
        self.assertEqual(result["operations"][1]["destination"]["model"], "gpt-5.5")
        with self.assertRaises(e.EvidenceError):
            e.gateway_window(*args, "enforce")

    def test_fallback_requires_exact_response_receipt_and_failure_counters(self):
        changes = (
            lambda a, b, logs: logs.pop(),
            lambda a, b, logs: logs[-1].update(operation_id="other"),
            lambda a, b, logs: logs[-1].update(generation_id="missing"),
            lambda a, b, logs: logs[-1].update(status_code=200),
            lambda a, b, logs: logs[-1].update(reported_usage=True),
            lambda a, b, logs: logs[-1].update(traffic_bucket="preflight"),
            lambda a, b, logs: logs.append(copy.deepcopy(logs[-1])),
            lambda a, b, logs: logs[1].update(policy_tier="lightweight"),
            lambda a, b, logs: logs[1].update(policy_failure_category="timeout"),
        )
        for change in changes:
            args = self.fallback_window()
            change(*args)
            with self.subTest(change=change), self.assertRaises(e.EvidenceError):
                e.gateway_window(*args, "enforce", allow_classifier_fallback=True)

    def breaker_window(self):
        before, after, logs = self.fallback_window()
        logs[1].update(policy_failure_category="breaker_open", policy_classifier_latency_ms=0)
        logs.pop()  # No HTTP response exists when the circuit breaker skips Jev.
        stats = after["stats"]
        classifier = next(row for row in stats["task_usage"]["by_kind"] if row["kind"] == "classifier")
        classifier.update(physical_row(1, usage(5, 2)))
        stats["task_usage"]["totals"] = physical_row(3, usage(95, 27))
        e.profile(after)["totals"]["physical_classifier_sends"] = 1
        return before, after, logs

    def test_open_circuit_breaker_proves_no_classifier_request_was_sent(self):
        args = self.breaker_window()
        result = e.gateway_window(*args, "enforce", allow_classifier_fallback=True)
        self.assertEqual(result["classifier"]["sends"], 0)
        self.assertTrue(result["classifier"]["usageComplete"])
        self.assertEqual(result["classifier"]["unmeteredFailures"], [])
        self.assertEqual(result["operations"][1]["classifierFailureCategory"], "breaker_open")
        self.assertEqual(result["operations"][1]["destination"]["model"], "gpt-5.5")
        with self.assertRaises(e.EvidenceError):
            e.gateway_window(*args, "enforce")

    def test_open_circuit_breaker_cannot_hide_an_actual_classifier_call(self):
        before, after, logs = self.breaker_window()
        logs.append(self.fallback_window()[2][-1])
        with self.assertRaisesRegex(e.EvidenceError, "responses do not match"):
            e.gateway_window(before, after, logs, "enforce", allow_classifier_fallback=True)
        before, after, logs = self.breaker_window()
        stats = after["stats"]["task_usage"]
        classifier = next(row for row in stats["by_kind"] if row["kind"] == "classifier")
        classifier.update(physical_row(2, usage(5, 2), reported=1))
        stats["totals"] = physical_row(4, usage(95, 27), reported=3)
        e.profile(after)["totals"]["physical_classifier_sends"] = 2
        with self.assertRaisesRegex(e.EvidenceError, "physical sends"):
            e.gateway_window(before, after, logs, "enforce", allow_classifier_fallback=True)
        before, after, logs = self.breaker_window()
        logs[1]["policy_classifier_latency_ms"] = 100
        with self.assertRaisesRegex(e.EvidenceError, "unexpectedly reports classifier latency"):
            e.gateway_window(before, after, logs, "enforce", allow_classifier_fallback=True)

    def continuation_window(self, mode="enforce"):
        before, after, logs = window(mode, tier="powerful")
        fields = {key: value for key, value in logs[1].items()
                  if key.startswith("policy_") or key in e.GENERATION_FIELDS}
        logs[0].update(fields)
        for row in (logs[0], after["stats"]["recent"][0]):
            row.update(model="team-assistant", route_id="team-assistant", final_target="team-assistant")
        after["stats"]["recent_attempts"][0].update(route_id="team-assistant", target_id="team-assistant", provider_id="")
        logs[1].update(policy_decision="replay_binding", policy_classifier_latency_ms=0,
                       policy_truncated=True)
        e.profile(after)["totals"]["eligible"] += 1
        e.profile(after)["totals"]["actual_tiers"]["powerful"] += 1
        return before, after, logs

    def test_tool_continuations_keep_the_selection_without_another_classifier_call(self):
        for mode in ("off", "enforce"):
            with self.subTest(mode=mode):
                before, after, logs = self.continuation_window(mode)
                result = e.gateway_window(before, after, logs, mode, allow_tool_continuations=True)
                self.assertEqual(result["workerRequests"], 2)
                self.assertEqual(result["classifier"]["sends"], 1 if mode == "enforce" else 0)
                self.assertEqual(result["operations"][1]["policyDecision"], "replay_binding")
                with self.assertRaises(e.EvidenceError):
                    e.gateway_window(before, after, logs, mode)

    def test_tool_continuation_does_not_justify_a_truncated_initial_classification(self):
        before, after, logs = self.continuation_window()
        logs[0]["policy_truncated"] = True
        with self.assertRaisesRegex(e.EvidenceError, "truncated request"):
            e.gateway_window(before, after, logs, "enforce", allow_tool_continuations=True)

    def test_bounded_context_is_explicit_and_retains_the_reported_flag(self):
        before, after, logs = self.continuation_window()
        logs[0]["policy_truncated"] = True
        result = e.gateway_window(before, after, logs, "enforce", allow_tool_continuations=True,
                                  allow_bounded_context=True)
        self.assertTrue(result["operations"][0]["classifierInputTruncated"])
        self.assertEqual(result["classifier"]["sends"], 1)
        logs[0]["policy_decision"] = "uncertain_fallback"
        with self.assertRaisesRegex(e.EvidenceError, "fallback"):
            e.gateway_window(before, after, logs, "enforce", allow_tool_continuations=True,
                             allow_bounded_context=True)

    def test_bounded_context_does_not_accept_a_missing_context_flag(self):
        before, after, logs = self.continuation_window()
        del logs[0]["policy_truncated"]
        with self.assertRaisesRegex(e.EvidenceError, "Missing classifier context flag"):
            e.gateway_window(before, after, logs, "enforce", allow_tool_continuations=True,
                             allow_bounded_context=True)

    def test_tool_continuation_needs_an_initial_selection_and_the_same_hosted_tier(self):
        before, after, logs = self.continuation_window()
        logs[0].update(policy_decision="replay_binding", policy_classifier_latency_ms=0)
        with self.assertRaisesRegex(e.EvidenceError, "no initial model selection"):
            e.gateway_window(before, after, logs, "enforce", allow_tool_continuations=True)
        before, after, logs = self.continuation_window()
        logs[1]["policy_tier"] = "lightweight"
        with self.assertRaisesRegex(e.EvidenceError, "did not retain the hosted model"):
            e.gateway_window(before, after, logs, "enforce", allow_tool_continuations=True)

    def test_classified_destination_comes_from_tier_and_installed_route(self):
        before, after, logs = window()
        result = e.gateway_window(before, after, logs, "enforce")
        worker = next(row for row in result["operations"] if row["role"] == "worker")
        self.assertEqual(worker["destination"]["model"], "qwen-3.5-2b")
        self.assertEqual(worker["destination"]["service"]["name"], "qwen35-2b")
        self.assertEqual(result["classifier"]["sends"], 1)
        self.assertEqual(result["classifier"]["usage"]["total_tokens"], 7)
        self.assertEqual(result["preflightBeforeInterval"][0]["physical_classifier_sends"], 1)
        self.assertTrue(result["preflightBeforeInterval"][0]["usageComplete"])
        # Hidden gateway model/target labels remain aliases, so they alone
        # cannot establish that a request went to Qwen.
        self.assertEqual(after["stats"]["recent"][1]["final_target"], "team-assistant")

    def test_baseline_runs_hosted_without_classifier_sends(self):
        args = window("off", "powerful")
        result = e.gateway_window(*args, "off")
        self.assertEqual(result["classifier"]["sends"], 0)
        self.assertEqual(result["operations"][1]["destination"]["model"], "gpt-5.5")

    def test_rejects_missing_request_attempt_log_and_mismatched_usage(self):
        changes = (
            ("request history", lambda a, b, l: b["stats"]["recent"].pop()),
            ("physical-attempt history", lambda a, b, l: b["stats"]["recent_attempts"].pop()),
            ("completion log", lambda a, b, l: l.pop()),
            ("token totals", lambda a, b, l: b["stats"]["recent"][1].update(total_tokens=14)),
            ("log and attempt", lambda a, b, l: l[1].update(prompt_tokens=999)),
        )
        for message, change in changes:
            with self.subTest(message=message):
                args = window()
                change(*args)
                with self.assertRaisesRegex(e.EvidenceError, message):
                    e.gateway_window(*args, "enforce")

    def test_ring_eviction_cannot_masquerade_as_a_short_interval(self):
        before, after, logs = window()
        after["stats"]["totals"]["requests"] = 82
        with self.assertRaisesRegex(e.EvidenceError, "truncated"):
            e.gateway_window(before, after, logs, "enforce")

    def test_same_pod_container_restart_is_rejected(self):
        for field, value in (("restartCount", 1), ("containerID", "different"),
                             ("startedAt", "2026-09-22T00:00:30Z")):
            with self.subTest(field=field):
                before, after, logs = window()
                after["container"][field] = value
                with self.assertRaisesRegex(e.EvidenceError, "container restarted"):
                    e.gateway_window(before, after, logs, "enforce")

    def test_config_and_policy_generation_are_bound_to_decision(self):
        before, after, logs = window()
        after["configuration"]["resourceVersion"] = "4"
        with self.assertRaisesRegex(e.EvidenceError, "configuration changed"):
            e.gateway_window(before, after, logs, "enforce")
        before, after, logs = window()
        logs[1]["profile_generation"] = "e" * 64
        with self.assertRaisesRegex(e.EvidenceError, "generations disagree"):
            e.gateway_window(before, after, logs, "enforce")

    def test_fallback_and_truncation_are_not_presented_as_semantic_routing(self):
        for field, value, message in (("policy_decision", "uncertain_fallback", "fallback"),
                                      ("policy_truncated", True, "truncated")):
            with self.subTest(field=field):
                before, after, logs = window()
                logs[1][field] = value
                with self.assertRaisesRegex(e.EvidenceError, message):
                    e.gateway_window(before, after, logs, "enforce")

    def test_unknown_classifier_tokens_are_explicitly_incomplete(self):
        before, after, logs = window()
        for snapshot_value in (before, after):
            row = next(row for row in snapshot_value["stats"]["task_usage"]["by_kind"] if row["kind"] == "classifier")
            total = snapshot_value["stats"]["task_usage"]["totals"]
            total["reported_usage_sends"] -= row["reported_usage_sends"]
            total["usage"] = {key: total["usage"][key] - row["usage"][key] for key in e.TOKEN_FIELDS}
            row["reported_usage_sends"], row["usage"] = 0, usage()
            e.profile(snapshot_value)["totals"]["classifier_usage"] = policy_usage(usage())
            e.profile(snapshot_value)["traffic_buckets"][0]["metrics"]["classifier_usage"] = policy_usage(usage())
        result = e.gateway_window(before, after, logs, "enforce")
        self.assertFalse(result["classifier"]["usageComplete"])
        self.assertEqual(result["classifier"]["sends"], 1)
        self.assertFalse(result["preflightBeforeInterval"][0]["usageComplete"])

    def test_preflight_availability_is_separate_from_measured_usage(self):
        before, after, logs = window()
        startup = usage(5, 2)
        for snapshot_value in (before, after):
            ledger = snapshot_value["stats"]["task_usage"]
            classifier = next(row for row in ledger["by_kind"] if row["kind"] == "classifier")
            for row in (classifier, ledger["totals"]):
                row["reported_usage_sends"] -= 1
                row["usage"] = {key: row["usage"][key] - startup[key] for key in e.TOKEN_FIELDS}
            state = e.profile(snapshot_value)
            state["totals"]["classifier_usage"] = {
                key: value - policy_usage(startup)[key] for key, value in state["totals"]["classifier_usage"].items()}
            state["traffic_buckets"][0]["metrics"]["classifier_usage"] = policy_usage(usage())
        result = e.gateway_window(before, after, logs, "enforce")
        self.assertTrue(result["classifier"]["usageComplete"])
        self.assertFalse(result["preflightBeforeInterval"][0]["usageComplete"])

    def test_partial_prior_history_cannot_establish_preflight_availability(self):
        before, after, logs = window()
        for snapshot_value in (before, after):
            ledger = snapshot_value["stats"]["task_usage"]
            classifier = next(row for row in ledger["by_kind"] if row["kind"] == "classifier")
            for row in (classifier, ledger["totals"]):
                row["sends"] += 1
                row["completed"] += 1
            e.profile(snapshot_value)["totals"]["physical_classifier_sends"] += 1
        result = e.gateway_window(before, after, logs, "enforce")
        self.assertEqual(result["preflightBeforeInterval"][0]["classifier_usage"]["total_tokens"], 7)
        self.assertFalse(result["preflightBeforeInterval"][0]["usageComplete"])

    def test_preflight_cannot_be_mixed_into_workload_interval(self):
        before, after, logs = window()
        e.profile(after)["traffic_buckets"][0]["metrics"]["physical_classifier_sends"] += 1
        with self.assertRaisesRegex(e.EvidenceError, "preflight overlapped"):
            e.gateway_window(before, after, logs, "enforce")

    def test_alternate_routes_and_wrong_local_service_are_rejected(self):
        for mutate in (
            lambda topology: topology["model_routes"][0]["routing"].update(max_upstream_sends=2),
            lambda topology: topology["providers"][1]["service"].update(name="elsewhere"),
        ):
            with self.subTest(mutate=mutate):
                before, after, logs = window()
                for value in (before, after):
                    mutate(value["configuration"]["topology"])
                with self.assertRaises(e.EvidenceError):
                    e.gateway_window(before, after, logs, "enforce")

    def test_nested_json_logger_fields_are_supported(self):
        before, after, logs = window()
        nested = [{"msg": row["msg"], "fields": {key: value for key, value in row.items() if key != "msg"}} for row in logs]
        self.assertEqual(e.gateway_window(before, after, nested, "enforce")["workerRequests"], 1)

    def test_terminal_log_cannot_contradict_policy_tier(self):
        before, after, logs = window()
        logs[1]["final_target"] = "copilot-gpt"
        with self.assertRaisesRegex(e.EvidenceError, "contradicts the selected terminal"):
            e.gateway_window(before, after, logs, "enforce")


def orka_totals(prompt, completion):
    return {"inputTokens": prompt, "outputTokens": completion, "totalTokens": prompt + completion,
            "cachedInputTokens": 0, "measurements": 1, "missingMeasurements": 0, "partialMeasurements": 0,
            "estimatedTokens": 0, "completeness": "complete"}


class OrkaEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.period = {"startedAt": "2026-09-22T00:00:00Z", "finishedAt": "2026-09-22T00:01:00Z"}
        self.selection = {"teams": ["team-payments"], "from": self.period["startedAt"], "until": self.period["finishedAt"]}
        self.requests = [{"taskUID": "native-uid", "task": "native-task", "worker": {"usage": usage(10, 5)}}]
        self.window = {"operations": [{"usage": usage(10, 5)}, {"usage": usage(80, 20)}]}
        measurement = {"completeness": "complete", "status": "completed", "model": "team-assistant"}
        native = {"taskUID": "native-uid", "taskName": "native-task", "namespace": "team-payments", "phase": "Succeeded",
                  "usage": orka_totals(10, 5), "measurements": [measurement]}
        coordinator = {"taskUID": "call-coordinator", "taskName": "", "namespace": "team-payments", "phase": "Unknown",
                       "usage": orka_totals(80, 20), "measurements": [{**measurement, "model": "coordinator"}]}
        self.group = {"category": "unassociated", "usage": orka_totals(90, 25), "taskCount": 2,
                      "page": {"offset": 0, "limit": 100, "total": 2}, "tasks": [native, coordinator]}
        self.empty = {"category": "other_requests", "usage": orka_totals(0, 0), "taskCount": 0,
                      "page": {"offset": 0, "limit": 100, "total": 0}, "tasks": []}
        self.save()

    def save(self):
        values = {"payments-usage.json": {"otherWork": [self.group, self.empty]},
                  "payments-usage-unassociated.json": {"selection": self.selection, "otherWork": self.group},
                  "payments-usage-other_requests.json": {"selection": self.selection, "otherWork": self.empty}}
        for name, value in values.items():
            (self.path / name).write_text(json.dumps(value))

    def verify(self):
        return e.orka_usage(self.path, "payments", self.requests, self.window, self.period)

    def test_workers_and_coordinator_are_separate_views_of_same_gateway_calls(self):
        result = self.verify()
        self.assertEqual(result["workers"]["total_tokens"], 15)
        self.assertEqual(result["coordinator"]["total_tokens"], 100)

    def test_hidden_usage_page_and_wrong_namespace_are_rejected(self):
        self.group["page"]["total"] = 3
        self.save()
        with self.assertRaisesRegex(e.EvidenceError, "paginated"):
            self.verify()
        self.group["page"]["total"] = 2
        self.selection["teams"] = ["team-inventory"]
        self.save()
        with self.assertRaisesRegex(e.EvidenceError, "selection"):
            self.verify()

    def test_missing_task_and_unrelated_model_are_rejected(self):
        self.requests[0]["taskUID"] = "missing"
        with self.assertRaisesRegex(e.EvidenceError, "missing from Orka"):
            self.verify()
        self.requests[0]["taskUID"] = "native-uid"
        self.group["tasks"][1]["measurements"][0]["model"] = "unrelated"
        self.save()
        with self.assertRaisesRegex(e.EvidenceError, "Unaccounted"):
            self.verify()

    def test_estimated_usage_is_not_reported_as_measured(self):
        self.group["tasks"][0]["usage"]["estimatedTokens"] = 9
        self.save()
        with self.assertRaisesRegex(e.EvidenceError, "estimated"):
            self.verify()

    def make_usage_unavailable(self):
        row = self.group["tasks"][0]
        row["usage"] = {**orka_totals(0, 0), "completeness": "unavailable", "missingMeasurements": 1}
        row["measurements"] = [{"completeness": "unavailable", "status": "completed", "scope": "attempt",
                                "provider": "codex", "source": "agent", "model": "team-assistant",
                                "inputTokens": None, "outputTokens": None,
                                "gap": "No consumed-token counts reported"}]
        self.group["usage"] = {**orka_totals(80, 20), "completeness": "partial", "missingMeasurements": 1}
        self.save()

    def test_missing_coding_usage_remains_unavailable_with_gateway_accounted_separately(self):
        self.make_usage_unavailable()
        with self.assertRaisesRegex(e.EvidenceError, "incomplete"):
            self.verify()
        result = e.orka_usage(self.path, "payments", self.requests, self.window, self.period,
                             allow_unreported_tasks=("native-uid",))
        self.assertEqual(result["reportedWorkerTasks"], 0)
        self.assertIsNone(result["unavailableTasks"][0]["tokens"])
        self.assertEqual(result["workers"]["total_tokens"], 0)
        self.assertEqual(result["coordinator"]["total_tokens"], 100)

    def test_unavailable_coding_usage_cannot_hide_counts_or_a_different_gap(self):
        for field, value in (("inputTokens", 8), ("estimatedTokens", 4)):
            with self.subTest(field=field):
                self.make_usage_unavailable()
                self.group["tasks"][0]["usage"][field] = value
                self.save()
                with self.assertRaisesRegex(e.EvidenceError, "counts or estimates"):
                    e.orka_usage(self.path, "payments", self.requests, self.window, self.period,
                                 allow_unreported_tasks=("native-uid",))
        self.make_usage_unavailable()
        self.group["tasks"][0]["measurements"][0]["gap"] = "Stream lost"
        self.save()
        with self.assertRaisesRegex(e.EvidenceError, "Unexpected gap"):
            e.orka_usage(self.path, "payments", self.requests, self.window, self.period,
                         allow_unreported_tasks=("native-uid",))


class RequestEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.directory = self.root / "baseline/payments-summary"
        self.directory.mkdir(parents=True)
        request = client_request("payments-summary")
        self.answer = {"unique_payments": 2, "total_amount": 30}
        self.task = {"metadata": {"name": "native-task", "uid": "native-uid", "namespace": "team-payments"},
                     "spec": {"type": "ai", "agentRef": {"name": "efficiency-payments"},
                              "prompt": WORKLOADS["payments-summary"]["prompt"]}, "status": {"phase": "Succeeded"}}
        before, after, logs = window("off", "powerful")
        checks = ["unique_payments", "total_amount"]
        artifacts = {
            "client-request.json": request, "task.json": self.task,
            "agent.json": {"metadata": {"name": "efficiency-payments"}, "spec": agent_spec("payments")},
            "tasks-before.json": [], "tasks-after.json": [self.task],
            "events.json": {"streamID": "native-task", "namespace": "team-payments", "latestSeq": 4,
                            "events": [{"seq": 1, "type": "TaskStarted"},
                                       {"seq": 2, "type": "ModelRequestCompleted", "content": {
                                           "inputTokens": 10, "outputTokens": 5, "toolCalls": 0}},
                                       {"seq": 3, "type": "ModelUsageUpdated", "content": {"usage": {
                                           "inputTokens": 10, "outputTokens": 5, "status": "completed", "complete": True,
                                           "scope": "call", "source": "provider", "model": "team-assistant"}}},
                                       {"seq": 4, "type": "TaskSucceeded"}]},
            "gateway-before.json": before, "gateway-after.json": after, "gateway-operations.json": logs,
            "result.json": {"result": json.dumps(self.answer)},
            "client-response.json": {"choices": [{"message": {"content": json.dumps(self.answer)}, "finish_reason": "stop"}]},
            "evidence.json": {"phase": "baseline", "workload": "payments-summary", "team": "payments",
                              "task": "native-task", "taskUID": "native-uid", "answer": self.answer,
                              "requestSHA256": e.digest(request), "workerToolCalls": 0,
                              "clientSeconds": 50.5, "totalSeconds": 60,
                              "checks": {"passed": True, "checks": checks, "checkCount": 2},
                              "startedAt": before["at"], "finishedAt": after["at"]},
        }
        for name, value in artifacts.items():
            self.write(name, value)
        (self.root / "payments-summary.request.json").write_text(json.dumps(request))

    def write(self, name, value):
        (self.directory / name).write_text(json.dumps(value))

    def verify(self):
        return e.verify_request(self.root, "baseline", "payments-summary")

    def stock_prose_fixture(self, answer):
        previous = self.directory
        self.directory = self.root / "orchestration-before/inventory-stock"
        self.directory.parent.mkdir()
        previous.rename(self.directory)
        request = client_request("inventory-stock")
        self.write("client-request.json", request)
        (self.root / "inventory-stock.request.json").write_text(json.dumps(request))
        self.task["metadata"]["namespace"] = "team-inventory"
        self.task["spec"]["agentRef"]["name"] = "efficiency-inventory"
        self.task["spec"]["prompt"] = WORKLOADS["inventory-stock"]["prompt"]
        self.write("task.json", self.task)
        self.write("tasks-after.json", [self.task])
        self.write("agent.json", {"metadata": {"name": "efficiency-inventory"},
                                  "spec": agent_spec("inventory", structured=False)})
        history = e.read(self.directory / "events.json")
        history["namespace"] = "team-inventory"
        self.write("events.json", history)
        saved = e.read(self.directory / "evidence.json")
        saved.update(phase="orchestration-before", workload="inventory-stock", team="inventory",
                     requestSHA256=e.digest(request),
                     checks={"passed": True, "checks": ["stock and shortage in prose"], "checkCount": 1})
        self.write("evidence.json", saved)
        self.write_prose_answers(answer)

    def write_prose_answers(self, answer):
        self.write("result.json", {"result": answer})
        self.write("client-response.json", {"choices": [{"message": {"content": answer}, "finish_reason": "stop"}]})
        saved = e.read(self.directory / "evidence.json")
        saved["answer"] = answer
        self.write("evidence.json", saved)

    def verify_prose(self):
        return e.verify_request(self.root, "orchestration-before", "inventory-stock")

    def test_plain_stock_answer_rejects_false_success_despite_matching_records(self):
        self.stock_prose_fixture(" \nWe can ship 18 today, with 6 remaining until tomorrow.\t")
        result = self.verify_prose()
        self.assertEqual(result["checks"], ["stock and shortage in prose"])
        self.assertEqual(result["checkCount"], 1)
        for answer in ("We can ship 6 today, with 18 remaining until tomorrow.",
                       "We can ship 18 today, with 6 remaining until tomorrow. Actually, nothing ships today."):
            with self.subTest(answer=answer):
                self.write_prose_answers(answer)
                with self.assertRaisesRegex(ValueError, "stock prose"):
                    self.verify_prose()

    def test_plain_stock_answer_keeps_client_record_and_event_consistency_checks(self):
        answer = "We can ship 18 today, with 6 remaining until tomorrow."
        self.stock_prose_fixture(answer)
        self.write("client-response.json", {"choices": [{"message": {"content": answer + " Extra instructions."},
                                                          "finish_reason": "stop"}]})
        with self.assertRaisesRegex(e.EvidenceError, "Client did not return its Task result"):
            self.verify_prose()
        self.write_prose_answers(answer)
        saved = e.read(self.directory / "evidence.json")
        saved["answer"] = "Ship 6 today; 18 are short."
        self.write("evidence.json", saved)
        with self.assertRaisesRegex(e.EvidenceError, "Recorded answer differs"):
            self.verify_prose()
        self.write_prose_answers(answer)
        history = e.read(self.directory / "events.json")
        history["events"].pop(0)
        self.write("events.json", history)
        with self.assertRaisesRegex(e.EvidenceError, "event history"):
            self.verify_prose()

    def test_native_task_and_gateway_tokens_join_in_serial_interval(self):
        result = self.verify()
        self.assertEqual(result["taskUID"], "native-uid")
        self.assertEqual(result["worker"]["operationID"], "worker-1")
        self.assertEqual(result["checkCount"], 2)
        self.assertEqual(result["coordinator"]["usage"]["total_tokens"], 100)
        self.assertEqual(result["clientSeconds"], 50.5)
        self.assertEqual(result["totalSeconds"], 60)

    def test_gapped_or_tool_using_task_is_rejected(self):
        value = e.read(self.directory / "events.json")
        value["events"].pop(0)
        self.write("events.json", value)
        with self.assertRaisesRegex(e.EvidenceError, "event history"):
            self.verify()
        value["events"].insert(0, {"seq": 1, "type": "ToolCallStarted"})
        self.write("events.json", value)
        with self.assertRaisesRegex(e.EvidenceError, "called a tool"):
            self.verify()

    def test_independent_answer_check_rejects_recorded_success_claim(self):
        wrong = {"unique_payments": 3, "total_amount": 42}
        self.write("result.json", {"result": json.dumps(wrong)})
        self.write("client-response.json", {"choices": [{"message": {"content": json.dumps(wrong)}, "finish_reason": "stop"}]})
        saved = e.read(self.directory / "evidence.json")
        saved["answer"] = wrong
        self.write("evidence.json", saved)
        with self.assertRaisesRegex(ValueError, "incorrect"):
            self.verify()

    def test_changed_prompt_or_second_task_is_rejected(self):
        self.task["spec"]["prompt"] = "A different question."
        self.write("task.json", self.task)
        with self.assertRaisesRegex(e.EvidenceError, "prompt changed"):
            self.verify()
        self.task["spec"]["prompt"] = WORKLOADS["payments-summary"]["prompt"]
        self.write("task.json", self.task)
        extra = copy.deepcopy(self.task)
        extra["metadata"]["uid"] = "extra-task"
        self.write("tasks-after.json", [self.task, extra])
        with self.assertRaisesRegex(e.EvidenceError, "exactly one"):
            self.verify()

    def test_different_worker_token_counts_break_correlation(self):
        value = e.read(self.directory / "events.json")
        value["events"][2]["content"]["usage"]["inputTokens"] = 11
        self.write("events.json", value)
        with self.assertRaisesRegex(e.EvidenceError, "Native Task usage differs"):
            self.verify()

    def test_normalized_cache_input_is_used_instead_of_raw_anthropic_input(self):
        value = e.read(self.directory / "events.json")
        value["events"][1]["content"]["inputTokens"] = 9
        value["events"][2]["content"]["usage"]["cachedInputTokens"] = 1
        self.write("events.json", value)
        after = e.read(self.directory / "gateway-after.json")
        for row in (after["stats"]["totals"], after["stats"]["physical_usage"],
                    after["stats"]["recent_attempts"][1]["reported_usage"],
                    after["stats"]["task_usage"]["by_kind"][0]["usage"], after["stats"]["task_usage"]["totals"]["usage"]):
            row["cached_tokens"] = 1
        self.write("gateway-after.json", after)
        logs = e.read(self.directory / "gateway-operations.json")
        logs[1]["cached_tokens"] = 1
        self.write("gateway-operations.json", logs)
        self.assertEqual(self.verify()["worker"]["usage"]["prompt_tokens"], 10)

    def test_missing_nonfinite_or_reversed_latency_is_rejected(self):
        value = e.read(self.directory / "evidence.json")
        for client, total in ((None, 60), (0, 60), (True, 60), (float("nan"), 60), (50, 40), (50, float("inf"))):
            with self.subTest(client=client, total=total):
                value["clientSeconds"], value["totalSeconds"] = client, total
                self.write("evidence.json", value)
                with self.assertRaises(e.EvidenceError):
                    self.verify()


def sample(second, cpu, pod="model-pod", restarts=0):
    return {"at": f"2026-09-22T00:00:{second:02}Z", "node": "existing-node", "podUID": pod, "restarts": restarts,
            "stats": [{"podRef": {"uid": pod}, "cpu": {"time": f"2026-09-22T00:00:{second:02}Z",
                                                          "usageCoreNanoSeconds": cpu * 10**9},
                       "memory": {"workingSetBytes": 2 * 2**30}}]}


class ResourceEvidenceTests(unittest.TestCase):
    def test_whole_pod_cpu_and_sampled_peak(self):
        result = e.resource_summary([sample(1, 5), sample(6, 8), sample(11, 10)],
                                    "2026-09-22T00:00:00Z", "2026-09-22T00:00:12Z")
        self.assertEqual(result["cpuSeconds"], 5)
        self.assertEqual(result["meanCPUCores"], 0.5)
        self.assertEqual(result["peakWorkingSetMiB"], 2048)

    def test_counter_reset_restart_and_missing_samples_are_rejected(self):
        for samples in ([sample(1, 8), sample(11, 5)],
                        [sample(1, 5), sample(11, 8, restarts=1)], [sample(1, 5)]):
            with self.subTest(samples=samples), self.assertRaises(e.EvidenceError):
                e.resource_summary(samples, "2026-09-22T00:00:00Z", "2026-09-22T00:00:12Z")

    def test_long_sampling_gap_is_rejected(self):
        with self.assertRaisesRegex(e.EvidenceError, "sampling has gaps"):
            e.resource_summary([sample(1, 5), sample(45, 8)],
                               "2026-09-22T00:00:00Z", "2026-09-22T00:00:46Z")


if __name__ == "__main__":
    unittest.main()
