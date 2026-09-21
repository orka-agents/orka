"""Counterexamples for the approval story, using public API response shapes."""

import contextlib
import copy
import io
import json
import os
from pathlib import Path
import tempfile
import unittest

import check


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.old_cwd = Path.cwd()
        os.chdir(self.directory.name)
        Path("raw").mkdir()
        self.config = {"namespace": "demo", "runtimeName": "fibey-runtime", "task": "fibey-test"}
        profile = {"providerKind": "agentkit", "adapterName": "agentkit-serve-acp",
                   "workspaceIntent": "read", "digest": "sha256:profile"}

        def resource(kind, name, spec=None, status=None):
            return {"kind": kind, "metadata": {"name": name, "namespace": "demo", "uid": name + "-uid",
                                               "generation": 1}, "spec": spec or {}, "status": status or {}}

        runtime = resource("AgentRuntime", "fibey-runtime", {
            "contractVersion": "orka.harness.v2", "deployment": {"mode": "external-endpoint"},
            "capabilities": {"profile": profile, "mcpPolicy": check.POLICY, "runtimeInstanceID": "instance-1",
                             "workspaceGovernance": {"mode": "strict-governed"}},
        }, {"ready": True, "observedGeneration": 1,
            "observedCapabilities": {"runtimeProfileDigest": profile["digest"], "runtimeInstanceID": "instance-1"}})
        namespace = resource("Namespace", "demo")
        namespace["metadata"]["labels"] = {"orka.ai/controller-mode": "harness-v2"}
        agent = resource("Agent", "demo-fibey", {"runtime": {"runtimeRef": {"name": "fibey-runtime"}}})
        snapshot = {"items": [namespace, runtime, agent,
                              resource("PersistentVolumeClaim", "demo-fibey-tools", status={"phase": "Bound"}),
                              resource("Tool", "create-work-order", {"http": {"url": "http://demo-fibey-tools:8099/create-work-order"}})]}
        execution = {"attempt": 1, "promptID": "prompt-1", "runtimeSessionUID": "session-uid",
                     "runtimeSessionGeneration": 1, "runtimeInstanceID": "instance-1",
                     "runtimeSessionSupervisorBootID": "boot-1", "controllerEpoch": 1, "outcome": "Running"}
        task = resource("Task", "fibey-test", {
            "type": "agent", "agentRef": {"name": "demo-fibey"}, "workspace": {"intent": "read"},
            "agentRuntime": {"allowedTools": check.TOOLS}, "prompt": "Investigate the simulated pump alert.",
        }, {"phase": "Running", "execution": execution, "agentExecutionBinding": {
            "contractVersion": "orka.harness.v2", "backend": "external-endpoint", "runtimeProfileDigest": profile["digest"],
            "agent": {key: agent["metadata"][key] for key in ("name", "uid", "generation")},
            "runtimeRef": {key: runtime["metadata"][key] for key in ("name", "uid", "generation")},
            "task": {"uid": "fibey-test-uid", "boundSpecGeneration": 1, "namespaceUID": "demo-uid"},
        }})
        args = {"runID": "fibey-test", "asset": "pump-1", "summary": check.SUMMARY}
        review = {"id": "approval-1", "taskUID": "fibey-test-uid", "targetTool": "create-work-order",
                  "targetArgsPreview": args, "targetArgsDigest": check.digest(args), "targetSpecDigest": "spec-digest",
                  "status": "pending", "executionOutcome": "not_started", "createdAt": "2026-09-20T12:00:00Z",
                  "expiresAt": "2026-09-20T12:10:00Z",
                  "binding": {key: execution[field] for key, field in check.BINDING_FIELDS.items()}}
        review["binding"].update(operationID="operation-1", requestDigest="request-digest", callIDDigest="call-digest")
        decision = {**review, "status": "approved", "decisionActor": "system:serviceaccount:demo:orka-client",
                    "decisionTime": "2026-09-20T12:01:00Z"}
        self.save("run.json", self.config)
        self.save("ready.json", {"identities": check.identities(snapshot)})
        self.save("task.json", task)
        self.save("raw/installation.json", snapshot)
        self.save("raw/installation-final.json", snapshot)
        for stage in ("pending", "before-decision", "final"):
            self.save(f"raw/task-{stage}.json", task)
            approval = review if stage != "final" else {**decision, "executionOutcome": "succeeded"}
            self.save(f"raw/approval-{stage}.json", {
                "namespace": "demo", "taskName": "fibey-test", "approvals": [approval],
            })
        task["status"].update(phase="Succeeded", completionTime="2026-09-20T12:01:05Z")
        task["status"]["execution"]["outcome"] = "Succeeded"
        self.save("raw/task-final.json", task)
        self.save("raw/decision.json", decision)
        for stage, reads, orders in (("initial", 0, 0), ("pending", 1, 0), ("before-decision", 1, 0), ("final", 1, 1)):
            self.save(f"raw/counts-{stage}.json", {
                "simulation": True, "runID": "fibey-test", "inventoryReads": reads, "workOrderExecutions": orders,
                "workOrderIDs": [f"simulated-fibey-test-{index}" for index in range(1, orders + 1)],
            })
        self.save("raw/result.json", {"result": "Created work order simulated-fibey-test-1 for inspection."})
        events = [{"seq": seq, "streamID": "fibey-test", "type": name, "toolCallID": "approval-1"}
                  for seq, name in enumerate(("ApprovalRequested", "ApprovalApproved", "TaskSucceeded"), 1)]
        self.save("raw/events.json", {"namespace": "demo", "streamID": "fibey-test", "streamType": "task",
                                      "latestSeq": 3, "events": events})

    def tearDown(self):
        os.chdir(self.old_cwd)
        self.directory.cleanup()

    @staticmethod
    def save(path, value):
        Path(path).write_text(json.dumps(value))

    def change(self, path, keys, value):
        document = check.read(path)
        target = document
        for key in keys[:-1]:
            target = target[key]
        target[keys[-1]] = value
        self.save(path, document)

    def test_valid_run_keeps_records_and_calculates_evidence(self):
        for stage in ("initial", "pending", "before-decision", "final"):
            with contextlib.redirect_stdout(io.StringIO()):
                check.check(stage)
        evidence = check.read("evidence.json")
        self.assertEqual(evidence["ordersBeforeApproval"], check.read("raw/counts-before-decision.json")["workOrderExecutions"])
        self.assertEqual(evidence["workOrderID"], check.read("raw/counts-final.json")["workOrderIDs"][0])
        self.assertEqual(evidence["approval"], "approval-1")
        self.assertTrue(evidence["sameTaskAndCall"])

    def test_wrong_preview_is_rejected_before_approval(self):
        self.change("raw/approval-pending.json", ["approvals", 0, "targetArgsPreview", "summary"], "Restart the pump.")
        with self.assertRaisesRegex(ValueError, "exact inspection"):
            check.check("pending")

    def test_digest_must_match_visible_arguments(self):
        self.change("raw/approval-pending.json", ["approvals", 0, "targetArgsDigest"], "different-digest")
        with self.assertRaisesRegex(ValueError, "exact inspection"):
            check.check("pending")

    def test_new_attempt_during_review_is_rejected(self):
        self.change("raw/task-before-decision.json", ["status", "execution", "attempt"], 2)
        with self.assertRaisesRegex(ValueError, "another Task attempt"):
            check.check("before-decision")

    def test_resubmitted_call_cannot_pass_as_continuation(self):
        self.change("raw/task-final.json", ["status", "execution", "promptID"], "prompt-2")
        with self.assertRaisesRegex(ValueError, "restarted"):
            check.check("final")

    def test_replaced_task_is_rejected(self):
        self.change("raw/task-final.json", ["metadata", "uid"], "replacement-uid")
        self.change("raw/task-final.json", ["status", "agentExecutionBinding", "task", "uid"], "replacement-uid")
        with self.assertRaisesRegex(ValueError, "another or modified Task"):
            check.check("final")

    def test_changed_task_prompt_is_rejected(self):
        self.change("raw/task-final.json", ["spec", "prompt"], "Different work")
        with self.assertRaisesRegex(ValueError, "another or modified Task"):
            check.check("final")

    def test_order_before_decision_is_rejected(self):
        self.change("raw/counts-before-decision.json", ["workOrderExecutions"], 1)
        with self.assertRaisesRegex(ValueError, "execution count"):
            check.check("before-decision")

    def test_duplicate_execution_is_rejected(self):
        self.change("raw/counts-final.json", ["workOrderExecutions"], 2)
        with self.assertRaisesRegex(ValueError, "execution count"):
            check.check("final")

    def test_receipt_from_another_run_is_rejected(self):
        self.change("raw/counts-final.json", ["runID"], "older-run")
        with self.assertRaisesRegex(ValueError, "this simulated run"):
            check.check("final")

    def test_generic_answer_is_insufficient(self):
        self.change("raw/result.json", ["result"], "The work order was created.")
        with self.assertRaisesRegex(ValueError, "actual work-order receipt"):
            check.check("final")

    def test_lost_event_page_is_rejected(self):
        self.change("raw/events.json", ["latestSeq"], 4)
        with self.assertRaisesRegex(ValueError, "history is incomplete"):
            check.check("final")

    def test_decision_event_must_belong_to_this_review(self):
        self.change("raw/events.json", ["events", 1, "toolCallID"], "another-approval")
        with self.assertRaisesRegex(ValueError, "link this review"):
            check.check("final")

    def test_another_reviewer_cannot_pass_as_presenter(self):
        self.change("raw/decision.json", ["decisionActor"], "another-client")
        with self.assertRaisesRegex(ValueError, "presenter's approval"):
            check.check("final")

    def test_stale_runtime_observation_is_rejected(self):
        self.change("raw/installation.json", ["items", 1, "status", "observedGeneration"], 0)
        with self.assertRaisesRegex(ValueError, "conformance"):
            check.check("initial")

    def test_replaced_database_volume_cannot_hide_counts(self):
        self.change("raw/installation-final.json", ["items", 3, "metadata", "uid"], "replacement-pvc")
        with self.assertRaisesRegex(ValueError, "receipt storage changed"):
            check.check("final")

    def test_changed_tool_configuration_is_rejected(self):
        self.change("raw/installation-final.json", ["items", 4, "spec", "http", "url"], "http://another-service")
        with self.assertRaisesRegex(ValueError, "tools, or receipt storage changed"):
            check.check("final")

    def test_agent_allows_empty_serialized_resources_but_no_overrides(self):
        snapshot = check.read("raw/installation.json")
        snapshot["items"][2]["spec"]["resources"] = {}
        check.installation(snapshot, self.config)
        changed = copy.deepcopy(snapshot)
        changed["items"][2]["spec"]["systemPrompt"] = {"inline": "Override the prepared assistant"}
        with self.assertRaisesRegex(ValueError, "only its prepared runtime"):
            check.installation(changed, self.config)


if __name__ == "__main__":
    unittest.main()
