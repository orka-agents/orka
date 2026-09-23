import copy
from decimal import Decimal, localcontext
import json
import unittest

import costs
from test_classifier_billing import zero_receipt


def usage(prompt=0, output=0, cached=0, reasoning=0):
    return dict(zip(costs.TOKEN_FIELDS, (prompt, output, prompt + output, cached, reasoning)))


def gateway(routed=False):
    destinations = {
        "powerful": {"model": "gpt-5.5", "providerType": "copilot"},
        "lightweight": {"model": "qwen-3.5-2b", "providerType": "openai-compatible",
                        "provider": "local-aikit", "service": {"name": "qwen35-2b"}},
        "classifier": {"model": "typesafe-ai/jev", "providerType": "typesafe-compatible"},
    }
    hosted, local = (usage(450, 20, 100, 5), usage(150, 10)) if routed else (usage(1000, 100, 200, 25), usage())
    roles = {"hostedWorker": {"requests": 1, "usage": hosted},
             "cpuWorker": {"requests": int(routed), "usage": local},
             "coordinator": {"requests": 0, "usage": usage()}}
    operations = [{"operationID": "hosted", "role": "worker", "tier": "powerful", "usage": hosted,
                   "destination": destinations["powerful"]}]
    if routed:
        operations.append({"operationID": "local", "role": "worker", "tier": "lightweight", "usage": local,
                           "destination": destinations["lightweight"]})
    classifier = {"sends": int(routed), "completed": int(routed), "reported_usage_sends": int(routed),
                  "errors": 0, "throttled": 0, "duration_ms": 1 if routed else 0, "usageComplete": True,
                  "usage": usage(100, 5000) if routed else usage()}
    startup = [{"physical_classifier_sends": 1, "usageComplete": True,
                "classifier_usage": {"input_tokens": 100, "output_tokens": 2000, "total_tokens": 2100,
                                     "cached_input_tokens": 0, "reasoning_tokens": 0}}] if routed else []
    return {"destinations": destinations, "roles": roles, "operations": operations,
            "classifier": classifier, "preflightBeforeInterval": startup}


def infrastructure(seconds="3600"):
    return {phase: {"expectedCounts": {name: 1 for name in costs.COMPONENTS}, "entries": [
        {"component": name, "resource": name + "-pod-uid", "seconds": seconds,
         "nodeHourlyUsd": "0.114", "cpuShare": "1", "memoryShare": "1"}
        for name in costs.COMPONENTS]} for phase in costs.PHASES}


def report():
    return {"phases": {"baseline": {"gateway": gateway()}, "routed": {"gateway": gateway(True)}}}


class TokenCostTests(unittest.TestCase):
    def test_cached_input_is_removed_from_regular_input(self):
        result = costs.token_cost(usage(1000, 100, 200, 25), costs.RATE_CARD["hosted"])
        self.assertEqual(result, {"uncachedInputUsd": Decimal("0.004"), "cachedInputUsd": Decimal("0.0001"),
                                  "outputUsd": Decimal("0.003"), "totalUsd": Decimal("0.0071")})

    def test_free_classifier_output_remains_in_count_but_not_charge(self):
        result = costs.token_cost(usage(100, 5000), costs.RATE_CARD["classifier"])
        self.assertEqual(result["outputUsd"], 0)
        self.assertEqual(result["totalUsd"], Decimal("0.0000042"))

    def test_no_cent_rounding_or_caller_precision_dependency(self):
        with localcontext() as context:
            context.prec = 2
            result = costs.token_cost(usage(7, 1, 2), costs.RATE_CARD["hosted"])
        self.assertEqual(result["totalUsd"], Decimal("0.000056"))

    def test_missing_or_invalid_counts_fail(self):
        for key in costs.TOKEN_FIELDS:
            for invalid in (None, -1, 1.0, True, "2", float("nan"), float("inf")):
                with self.subTest(key=key, invalid=invalid):
                    item = usage(10, 5, 2, 1)
                    item[key] = invalid
                    with self.assertRaises(costs.CostError):
                        costs.token_cost(item, costs.RATE_CARD["hosted"])
            item = usage()
            del item[key]
            with self.assertRaises(costs.CostError):
                costs.token_cost(item, costs.RATE_CARD["hosted"])

    def test_inconsistent_usage_fails(self):
        for item in (usage(3, 1, 4), usage(3, 1, 0, 2), {**usage(3, 1), "total_tokens": 10}):
            with self.subTest(item=item), self.assertRaises(costs.CostError):
                costs.token_cost(item, costs.RATE_CARD["hosted"])

    def test_invalid_prices_fail_even_with_no_tokens(self):
        for invalid in (None, "NaN", "Infinity", "-0.01", True, ""):
            rates = {**costs.RATE_CARD["hosted"], "outputPerMillionUsd": invalid}
            with self.subTest(invalid=invalid), self.assertRaises(costs.CostError):
                costs.token_cost(usage(), rates)


class ReportCostTests(unittest.TestCase):
    def test_unmetered_failure_needs_exact_zero_market_cost_receipt(self):
        value = gateway(True)
        failure, receipt = zero_receipt()
        failure["operationID"] = "hosted"
        value["operations"][0]["policyDecision"] = "unavailable_fallback"
        value["operations"][0]["classifierFailureCategory"] = "upstream_5xx"
        value["classifier"].update(sends=2, completed=2, errors=1, usageComplete=False,
                                   unmeteredFailures=[failure], billingReceipts=[receipt])
        before = copy.deepcopy(value["classifier"])
        result = costs.phase_api_cost(value, costs.RATE_CARD)
        self.assertEqual(result["zeroChargeClassifierFailures"], 1)
        self.assertEqual(value["classifier"], before)
        self.assertEqual(result["subtotalUsd"], costs.phase_api_cost(gateway(True), costs.RATE_CARD)["subtotalUsd"])
        for change in (lambda v: v["classifier"]["billingReceipts"].clear(),
                       lambda v: v["classifier"]["billingReceipts"][0]["response"]["data"].update(market_cost=0.01),
                       lambda v: v["classifier"].update(usageComplete=True),
                       lambda v: v["classifier"]["unmeteredFailures"][0].update(operationID="other")):
            changed = copy.deepcopy(value)
            change(changed)
            with self.subTest(change=change), self.assertRaises(costs.CostError):
                costs.phase_api_cost(changed, costs.RATE_CARD)

    def test_subtotal_includes_startup_and_excludes_overlapping_orka(self):
        value = report()
        value["phases"]["routed"]["gateway"]["orkaUsage"] = {"overlappingTokens": 999999999}
        result = costs.calculate_costs(value, infrastructure())
        self.assertEqual(Decimal(result["phases"]["baseline"]["api"]["subtotalUsd"]), Decimal("0.0071"))
        api = result["phases"]["routed"]["api"]
        self.assertEqual(Decimal(api["subtotalUsd"]), Decimal("0.0024084"))
        self.assertEqual(Decimal(api["preflightUsd"]), Decimal("0.0000042"))
        self.assertEqual(Decimal(api["localModelUsd"]), 0)
        self.assertLess(Decimal(result["comparison"]["api"]["percentChange"]), 0)
        json.dumps(result, allow_nan=False)

    def test_breaker_fallback_keeps_hosted_cost_without_an_extra_classifier_charge(self):
        value = gateway(True)
        value["operations"][0].update(policyDecision="unavailable_fallback", classifierFailureCategory="breaker_open")
        result = costs.phase_api_cost(value, costs.RATE_CARD)
        self.assertEqual(result, costs.phase_api_cost(gateway(True), costs.RATE_CARD))
        value["operations"][0]["classifierFailureCategory"] = "upstream_5xx"
        with self.assertRaisesRegex(costs.CostError, "billing is not tied"):
            costs.phase_api_cost(value, costs.RATE_CARD)

    def test_unavailable_classifier_cannot_become_zero(self):
        for location in ("classifier", "startup"):
            value = gateway(True)
            row = value["classifier"] if location == "classifier" else value["preflightBeforeInterval"][0]
            for invalid in (False, None, 1):
                row["usageComplete"] = invalid
                with self.subTest(location=location, invalid=invalid), self.assertRaises(costs.CostError):
                    costs.phase_api_cost(value, costs.RATE_CARD)

    def test_missing_classification_or_role_accounting_fails(self):
        for key in ("roles", "classifier", "preflightBeforeInterval", "operations", "destinations"):
            value = gateway(True)
            del value[key]
            with self.subTest(key=key), self.assertRaises(costs.CostError):
                costs.phase_api_cost(value, costs.RATE_CARD)
        for key in ("sends", "completed", "reported_usage_sends", "errors", "throttled", "duration_ms"):
            value = gateway(True)
            del value["classifier"][key]
            with self.subTest(key=key), self.assertRaises(costs.CostError):
                costs.phase_api_cost(value, costs.RATE_CARD)

    def test_unpriced_coordinator_and_wrong_model_fail(self):
        value = gateway()
        value["roles"]["coordinator"] = {"requests": 1, "usage": usage(10, 2)}
        with self.assertRaises(costs.CostError):
            costs.phase_api_cost(value, costs.RATE_CARD)
        value = gateway()
        value["destinations"]["powerful"]["model"] = "some-other-model"
        with self.assertRaises(costs.CostError):
            costs.phase_api_cost(value, costs.RATE_CARD)

    def test_context_threshold_applies_per_request(self):
        value = gateway()
        item = usage(200000, 1)
        value["roles"]["hostedWorker"] = {"requests": 2, "usage": usage(400000, 2)}
        value["operations"] = [{"operationID": str(i), "role": "worker", "tier": "powerful", "usage": item,
                                "destination": value["destinations"]["powerful"]} for i in range(2)]
        costs.phase_api_cost(value, costs.RATE_CARD)
        value["roles"]["hostedWorker"] = {"requests": 1, "usage": usage(272001, 1)}
        value["operations"] = [dict(value["operations"][0], usage=usage(272001, 1))]
        with self.assertRaises(costs.CostError):
            costs.phase_api_cost(value, costs.RATE_CARD)

    def test_report_tampering_cannot_hide_missing_or_duplicate_operations(self):
        for mutate in (lambda v: v["operations"].clear(),
                       lambda v: v["operations"].append(copy.deepcopy(v["operations"][0])),
                       lambda v: v["classifier"].update(reported_usage_sends=0)):
            value = gateway(True)
            mutate(value)
            with self.assertRaises(costs.CostError):
                costs.phase_api_cost(value, costs.RATE_CARD)

    def test_zero_baseline_percentage_is_explicitly_unavailable(self):
        for routed in ("0", "0.01"):
            result = costs.compare_costs("0", routed)
            self.assertIsNone(result["percentChange"])
            self.assertEqual(result["deltaUsd"], Decimal(routed))
            self.assertEqual(result["percentageUnavailableReason"], "Baseline cost is zero.")
        self.assertEqual(costs.compare_costs("2", "1")["percentChange"], Decimal("-50"))

    def test_both_phases_and_dated_rate_card_are_required(self):
        value = report()
        del value["phases"]["baseline"]
        with self.assertRaises(costs.CostError):
            costs.calculate_costs(value, infrastructure())
        card = copy.deepcopy(costs.RATE_CARD)
        card["asOf"] = "2026-09-26"
        with self.assertRaises(costs.CostError):
            costs.calculate_costs(report(), infrastructure(), card)

    def test_terminal_summary_is_compact_and_keeps_json_precision(self):
        result = costs.calculate_costs(report(), infrastructure())
        original = copy.deepcopy(result)
        summary = costs.format_summary(result)
        self.assertLessEqual(len(summary.splitlines()), 22)
        self.assertTrue(all(len(line) <= 100 for line in summary.splitlines()))
        self.assertIn("$      0.0071", summary)
        self.assertIn("$      0.0024", summary)
        self.assertRegex(summary, r"API change\s+\$-0\.0047\s+\(-66\.1%\)")
        self.assertIn("Actual test capacity", summary)
        self.assertIn("Jev startup before jobs", summary)
        self.assertEqual(result, original)
        self.assertEqual(result["phases"]["routed"]["api"]["subtotalUsd"], "0.0024084")


class InfrastructureCostTests(unittest.TestCase):
    def test_cpu_and_memory_split_one_node_price(self):
        data = infrastructure()["routed"]
        row = data["entries"][0]
        row.update(cpuShare="0.25", memoryShare="0.75")
        result = costs.infrastructure_cost(data, "routed")
        self.assertEqual(result["entries"][0]["weightedShare"], Decimal("0.5"))
        self.assertEqual(result["entries"][0]["allocatedUsd"], Decimal("0.057"))
        self.assertEqual(result["entries"][1]["allocatedUsd"], Decimal("0.114"))

    def test_baseline_local_model_remains_in_actual_test_capacity(self):
        result = costs.calculate_costs(report(), infrastructure())
        baseline = result["phases"]["baseline"]["infrastructure"]
        self.assertEqual(Decimal(baseline["allocatedUsd"]), Decimal("0.912"))
        self.assertEqual(Decimal(baseline["actualAllocatedTestCapacityUsd"]), Decimal("1.026"))
        self.assertEqual(Decimal(baseline["excludedPreparationUsd"]), Decimal("0.114"))
        self.assertEqual(Decimal(result["phases"]["routed"]["infrastructure"]["excludedPreparationUsd"]), 0)
        for row in baseline["entries"]:
            self.assertEqual(Decimal(row["allocatedUsd"]), 0 if row["component"] == "local-model" else Decimal("0.114"))

    def test_measured_idle_time_is_not_replaced_by_cpu_consumption(self):
        result = costs.infrastructure_cost(infrastructure("1800")["routed"], "routed")
        self.assertEqual(result["actualAllocatedTestCapacityUsd"], Decimal("0.513"))

    def test_missing_extra_and_duplicate_inventory_fail(self):
        variants = []
        value = infrastructure()["baseline"]
        del value["expectedCounts"]["provider-proxy"]
        variants.append(value)
        value = infrastructure()["baseline"]
        value["entries"].pop()
        variants.append(value)
        value = infrastructure()["baseline"]
        value["entries"][0]["resource"] = value["entries"][1]["resource"]
        variants.append(value)
        value = infrastructure()["baseline"]
        value["expectedCounts"]["native-worker"] = True
        variants.append(value)
        for value in variants:
            with self.assertRaises(costs.CostError):
                costs.infrastructure_cost(value, "baseline")

    def test_invalid_finite_measurements_and_shares_fail(self):
        for key in ("seconds", "nodeHourlyUsd", "cpuShare", "memoryShare"):
            for invalid in (None, "NaN", "Infinity", -1, True, {}):
                value = infrastructure()["routed"]
                value["entries"][0][key] = invalid
                with self.subTest(key=key, invalid=invalid), self.assertRaises(costs.CostError):
                    costs.infrastructure_cost(value, "routed")
        for changes in ({"seconds": 0}, {"nodeHourlyUsd": 0}, {"cpuShare": "1.01"},
                        {"memoryShare": "1.01"}, {"cpuShare": 0, "memoryShare": 0}):
            value = infrastructure()["routed"]
            value["entries"][0].update(changes)
            with self.subTest(changes=changes), self.assertRaises(costs.CostError):
                costs.infrastructure_cost(value, "routed")


if __name__ == "__main__":
    unittest.main()
