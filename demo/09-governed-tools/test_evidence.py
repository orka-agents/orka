#!/usr/bin/env python3
"""Corrupt captured-response fixtures and ensure the public checker fails closed."""

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from prepare import prepare

HERE = Path(__file__).resolve().parent


class EvidenceTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        (self.root / "raw").mkdir()
        prepare(self.root, "test", "evidence-test", "test-provider", "test-model")
        self.config = json.loads((self.root / "run.json").read_text())
        empty = {"instanceId": "test-instance", "runId": "evidence-test", "receipts": [], "orderRequests": 0,
                 "ordersCreated": 0, "totalOrderRequests": 0, "totalOrdersCreated": 0}
        self.write("supplier-before", empty)
        receipt = copy.deepcopy(empty)
        receipt["receipts"] = [{"runId": "evidence-test", "method": "GET", "path": "/v1/stock", "status": 200,
                                "credentialAccepted": True, "orderCreated": False, "item": "replacement-filter", "available": 32}]
        self.write("supplier-lookup", receipt)
        self.write("supplier-after", receipt)
        for action, outcome in (("lookup", "ToolCallCompleted"), ("order", "ToolCallFailed")):
            task = json.loads((self.root / f"{action}.json").read_text())
            task["metadata"]["uid"] = f"test-{action}-uid"
            task["status"] = {"phase": "Succeeded"}
            self.write(f"{action}-task", task)
            self.write(f"{action}-result", {"result": "32 filters are available, covering the request for 20." if action == "lookup" else "The gateway refused the order."})
            tool = self.config["stockTool" if action == "lookup" else "orderTool"]
            events = [{"seq": 1, "type": "ToolCallStarted", "toolName": tool, "toolCallID": action},
                      {"seq": 2, "type": outcome, "toolName": tool, "toolCallID": action,
                       "summary": "tool call completed" if action == "lookup" else "gateway returned HTTP 404"},
                      {"seq": 3, "type": "TaskSucceeded"}]
            self.write(f"{action}-events", {"streamID": self.config[action + "Task"], "namespace": "test", "latestSeq": 3, "events": events})
        allowed = {"demo_request": "/v1/stock?run=evidence-test&item=replacement-filter", "demo_method": "GET", "demo_status": 200}
        denied = {"demo_request": "/v1/orders?run=evidence-test", "demo_method": "POST", "demo_status": 404}
        (self.root / "raw/gateway-lookup.jsonl").write_text(json.dumps(allowed) + "\n")
        (self.root / "raw/gateway-order.jsonl").write_text(json.dumps(allowed) + "\n" + json.dumps(denied) + "\n")

    def tearDown(self):
        self.directory.cleanup()

    def write(self, name, body):
        (self.root / "raw" / f"{name}.json").write_text(json.dumps(body))

    def mutate(self, name, fn):
        body = json.loads((self.root / "raw" / f"{name}.json").read_text())
        fn(body)
        self.write(name, body)

    def check(self, expected=2, action="summary"):
        result = subprocess.run([sys.executable, str(HERE / "evidence.py"), action], cwd=self.root, text=True, capture_output=True)
        self.assertEqual(result.returncode, expected, result.stderr)
        return result

    def test_complete_correlated_evidence(self):
        result = self.check(expected=0)
        self.assertIn("0 order requests; 0 orders created", result.stdout)
        self.check(expected=1, action="order-result")

    def test_network_error_does_not_count_as_gateway_refusal(self):
        self.mutate("order-events", lambda d: d["events"][1].update(summary="connection refused"))
        self.check()

    def test_gateway_log_is_required(self):
        (self.root / "raw/gateway-order.jsonl").write_text("")
        self.check()

    def test_gateway_log_from_another_run_is_rejected(self):
        path = self.root / "raw/gateway-order.jsonl"
        path.write_text(path.read_text().replace("run=evidence-test", "run=other-test"))
        self.check()

    def test_missing_tool_attempt_is_rejected(self):
        self.mutate("order-events", lambda d: d["events"].pop(0))
        self.check()

    def test_supplier_receiving_failed_order_is_rejected(self):
        self.mutate("supplier-after", lambda d: d.update(totalOrderRequests=1, orderRequests=1))
        self.check()

    def test_supplier_restart_cannot_erase_order_evidence(self):
        self.mutate("supplier-after", lambda d: d.update(instanceId="new-empty-instance"))
        self.check()

    def test_unrelated_tool_result_is_rejected(self):
        self.mutate("order-events", lambda d: d["events"][1].update(toolCallID="another-call"))
        self.check()

    def test_stock_without_authentication_is_rejected(self):
        self.mutate("supplier-lookup", lambda d: d["receipts"][0].update(credentialAccepted=False))
        self.check()


if __name__ == "__main__":
    unittest.main()
