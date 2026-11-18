import copy
import hashlib
import json
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from acceptance import (Acceptance, CheckFailed, FINALIZER, cancellation_diagnostics, validate_cancellation,
                        validate_cancellation_receipt, validate_pending_approval, validate_runtime)


class RuntimePreflightTest(unittest.TestCase):
    def fixture(self):
        profile = {"digest": "sha256:" + "a" * 64, "providerKind": "agentkit",
                   "adapterName": "agentkit-serve-acp", "adapterDigest": "sha256:" + "b" * 64,
                   "workspaceIntent": "read"}
        expected = {"profile": profile, "mcpPolicy": {
            "allowedTools": ["create-work-order", "read-inventory"], "disallowedTools": [],
            "allowBash": False, "approvalRequiredTools": ["create-work-order"],
        }}
        governance = dict.fromkeys([
            "orkaOwnedWorkspaceDeltas", "promptScopedBrokerAuthorization", "noDirectSCMPublication",
            "orkaOwnedCleanRoomPublication", "exactInstanceFencing", "duplicateSafeMutations", "cancellationSettlement",
        ], True)
        governance.update(mode="strict-governed", trusted=False)
        runtime = {
            "metadata": {"name": "test-runtime", "generation": 3},
            "spec": {"contractVersion": "orka.harness.v2", "deployment": {"mode": "external-endpoint"},
                     "capabilities": {"profile": copy.deepcopy(profile), "mcpPolicy": copy.deepcopy(expected["mcpPolicy"]),
                                      "runtimeInstanceID": "test-instance", "workspaceGovernance": governance}},
            "status": {"ready": True, "observedGeneration": 3, "observedCapabilities": {
                "runtimeInstanceID": "test-instance", "runtimeProfileDigest": profile["digest"],
            }},
        }
        agent = {"metadata": {}, "spec": {"runtime": {"runtimeRef": {"name": "test-runtime"}}}}
        capabilities = {
            "protocol": "orka.harness.v2", "runtimeProfileDigest": profile["digest"],
            "adapterDigests": {"agentkit-serve-acp": profile["adapterDigest"]},
            "provider": {"providerKinds": ["agentkit"], "supportsPermissions": False, "supportsBrokeredToolApprovals": True},
            "limits": {"maxConcurrentPrompts": 2},
        }
        return runtime, agent, capabilities, expected

    def test_capability_response_omits_false_optional_configuration_support(self):
        validate_runtime(*self.fixture(), "agentkit")

    def test_rejects_unqualified_capability_or_changed_policy(self):
        for field, value in [("supportsBrokeredToolApprovals", False), ("supportsPermissions", True)]:
            fixture = self.fixture()
            fixture[2]["provider"][field] = value
            with self.assertRaises(CheckFailed):
                validate_runtime(*fixture, "agentkit")
        for field, value in [("approvalRequiredTools", []), ("allowedTools", ["read-inventory"]), ("allowBash", True)]:
            fixture = self.fixture()
            fixture[0]["spec"]["capabilities"]["mcpPolicy"][field] = value
            with self.assertRaises(CheckFailed):
                validate_runtime(*fixture, "agentkit")

    def test_rejects_stale_conformance_or_different_live_profile(self):
        fixture = self.fixture()
        fixture[0]["status"]["observedGeneration"] = 2
        with self.assertRaises(CheckFailed):
            validate_runtime(*fixture, "agentkit")
        fixture = self.fixture()
        fixture[2]["runtimeProfileDigest"] = "sha256:" + "c" * 64
        with self.assertRaises(CheckFailed):
            validate_runtime(*fixture, "agentkit")


class ApprovalPreflightTest(unittest.TestCase):
    @staticmethod
    def fixture():
        return {
            "id": "original-call", "status": "pending", "targetTool": "create-work-order",
            "taskUID": "task-uid", "executionOutcome": "not_started", "binding": {"promptID": "original-prompt"},
            "targetArgsPreview": {"runID": "test-1", "asset": "pump-1", "summary": "Inspect the pressure transmitter."},
            "targetArgsDigest": "03a4bd9bc0ac2659dda376927cf3f8ae5db98d69cb47b9ce7bb3e4d382f82f06",
            "targetSpecDigest": "exact-tool-spec", "createdAt": "2026-09-14T00:00:00Z",
            "expiresAt": "2026-09-14T00:10:00Z",
        }

    def test_accepts_the_exact_unexecuted_simulated_proposal(self):
        validate_pending_approval(self.fixture(), "test-1", "task-uid")

    def test_rejects_a_changed_proposal_before_approval(self):
        for field, value in [
            ("taskUID", "replacement-task"), ("targetTool", "another-tool"),
            ("targetArgsPreview", {"runID": "test-1", "asset": "real-equipment", "summary": "Change equipment"}),
            ("targetArgsDigest", "different-arguments"), ("executionOutcome", "succeeded"),
            ("expiresAt", "2026-09-14T00:10:01Z"),
        ]:
            with self.subTest(field=field):
                approval = self.fixture()
                approval[field] = value
                with self.assertRaises(CheckFailed):
                    validate_pending_approval(approval, "test-1", "task-uid")


class CancellationTest(unittest.TestCase):
    @staticmethod
    def approval():
        return {"status": "cancelled", "executionOutcome": "not_started", "executionReason": "approval_cancelled"}

    def test_accepts_prompt_cancellation_or_completed_denial(self):
        validate_cancellation({"status": {"phase": "Cancelled", "execution": {"outcome": "Cancelled"}}}, self.approval(), {})
        validate_cancellation({"status": {
            "phase": "Succeeded", "execution": {"outcome": "Succeeded"}, "delivery": {"outcome": "ReadValidated"},
        }}, self.approval(), {"result": "The tool returned approval_cancelled."})

    def test_rejects_a_false_delivery_failure_after_completed_denial(self):
        with self.assertRaises(CheckFailed):
            validate_cancellation({"status": {
                "phase": "Failed", "execution": {"outcome": "Succeeded"}, "delivery": {"outcome": "DeliveryConflict"},
            }}, self.approval(), {"result": "The tool returned approval_cancelled."})

    def test_rejects_completion_without_the_cancellation_result_or_with_started_action(self):
        completed = {"status": {
            "phase": "Succeeded", "execution": {"outcome": "Succeeded"}, "delivery": {"outcome": "ReadValidated"},
        }}
        with self.assertRaises(CheckFailed):
            validate_cancellation(completed, self.approval(), {"result": "success"})
        approval = self.approval()
        approval["executionOutcome"] = "succeeded"
        with self.assertRaises(CheckFailed):
            validate_cancellation(completed, approval, {"result": "approval_cancelled"})


class CancellationDiagnosticsTest(unittest.TestCase):
    def test_keeps_identity_lifecycle_codes_and_precise_timestamps_without_task_content(self):
        text = "private provider response and credential"
        timestamp = "2026-09-22T19:23:01.123456789Z"
        task = {
            "metadata": {"name": "task", "namespace": "test", "uid": "task-uid", "generation": 2,
                         "resourceVersion": "17", "creationTimestamp": "2026-09-22T19:22:55Z",
                         "deletionTimestamp": timestamp, "annotations": {"private": text}},
            "spec": {"prompt": text},
            "status": {"phase": "Failed", "message": text, "startTime": "2026-09-22T19:23:00+00:00",
                       "completionTime": timestamp, "result": text,
                       "execution": {"outcome": "Failed", "reason": "PromptFailed", "message": text,
                                     "lastTransitionTime": timestamp, "runtimeInstanceID": text},
                       "delivery": {"outcome": "NotRequested", "reason": "Cancelled", "message": text},
                       "conditions": [{"type": "Ready", "status": "False", "reason": "PromptFailed",
                                       "message": text, "lastTransitionTime": timestamp}]},
        }
        snapshot = cancellation_diagnostics(task)
        self.assertEqual(snapshot["metadata"], {key: value for key, value in task["metadata"].items() if key != "annotations"})
        self.assertEqual(snapshot["phase"], "Failed")
        self.assertEqual(snapshot["execution"]["outcome"], "Failed")
        self.assertEqual(snapshot["execution"]["reason"], "PromptFailed")
        self.assertEqual(snapshot["delivery"]["outcome"], "NotRequested")
        self.assertEqual(snapshot["delivery"]["reason"], "Cancelled")
        self.assertEqual(snapshot["conditions"][0]["reason"], "PromptFailed")
        self.assertEqual(snapshot["completionTime"], timestamp)
        self.assertEqual(snapshot["execution"]["lastTransitionTime"], timestamp)
        self.assertEqual(snapshot["conditions"][0]["lastTransitionTime"], timestamp)
        self.assertEqual(snapshot["messageSHA256"], "sha256:" + hashlib.sha256(text.encode()).hexdigest())
        self.assertNotIn(text, json.dumps(snapshot))
        self.assertNotIn("spec", snapshot)
        self.assertNotIn("runtimeInstanceID", snapshot["execution"])

    def test_unknown_codes_are_hashed_and_invalid_timestamps_are_omitted(self):
        text = "UnknownProviderCredentialWithoutWhitespace"
        snapshot = cancellation_diagnostics({
            "metadata": {"name": "task", "uid": "task-uid", "creationTimestamp": text,
                         "deletionTimestamp": "2026-09-22T19:23:01+00:99"},
            "status": {"phase": text, "completionTime": "2026-02-30T00:00:00Z",
                       "execution": {"outcome": text, "reason": text, "message": text,
                                     "lastTransitionTime": "2026-09-22T19:23:01"},
                       "delivery": {"outcome": text, "reason": text},
                       "conditions": [{"type": text, "status": text, "reason": text,
                                       "message": text, "lastTransitionTime": text}]},
        })
        self.assertNotIn(text, json.dumps(snapshot))
        self.assertIsNone(snapshot["phase"])
        self.assertIsNone(snapshot["execution"]["reason"])
        self.assertEqual(snapshot["execution"]["reasonSHA256"], "sha256:" + hashlib.sha256(text.encode()).hexdigest())
        self.assertEqual(snapshot["conditions"][0]["reasonSHA256"], snapshot["execution"]["reasonSHA256"])
        self.assertIsNone(snapshot["metadata"]["creationTimestamp"])
        self.assertIsNone(snapshot["metadata"]["deletionTimestamp"])
        self.assertIsNone(snapshot["completionTime"])
        self.assertIsNone(snapshot["execution"]["lastTransitionTime"])


class ObserverFinalizerTest(unittest.TestCase):
    @staticmethod
    def fixture(finalizers):
        run = object.__new__(Acceptance)
        run.owned_tasks = {"task": "task-uid"}
        task = {"metadata": {"uid": "task-uid", "resourceVersion": "1", "finalizers": finalizers}}
        return run, task

    def test_retries_concurrent_cleanup_without_losing_other_finalizers(self):
        for enabled in (False, True):
            with self.subTest(enabled=enabled):
                run, task = self.fixture(["controller"] + ([] if enabled else [FINALIZER]))
                patches = []

                def kube(*arguments, body=None):
                    if arguments[0] == "get":
                        return copy.deepcopy(task)
                    self.assertEqual(arguments[:3], ("patch", "task", "task"))
                    patches.append(body)
                    if len(patches) == 1:
                        task["metadata"]["resourceVersion"] = "2"
                        task["metadata"]["finalizers"] = ["another-controller"] + ([] if enabled else [FINALIZER])
                        raise CheckFailed("resourceVersion test failed")
                    self.assertEqual(body[0], {"op": "test", "path": "/metadata/uid", "value": "task-uid"})
                    self.assertEqual(body[1], {"op": "test", "path": "/metadata/resourceVersion", "value": "2"})
                    task["metadata"]["finalizers"] = body[2]["value"]
                    return copy.deepcopy(task)

                run.kube_json = kube
                with patch("acceptance.time.sleep"):
                    run.observe("task", enabled)
                self.assertEqual(task["metadata"]["finalizers"], ["another-controller"] + ([FINALIZER] if enabled else []))
                self.assertEqual(len(patches), 2)

    def test_rejects_replaced_task_before_or_during_update(self):
        for during_update in (False, True):
            with self.subTest(during_update=during_update):
                run, task = self.fixture([FINALIZER])
                replacement = copy.deepcopy(task)
                replacement["metadata"].update(uid="replacement", resourceVersion="2")
                responses = [task, CheckFailed("resourceVersion test failed"), replacement] if during_update else [replacement]
                run.kube_json = Mock(side_effect=responses)
                with self.assertRaisesRegex(CheckFailed, "replaced Task"):
                    run.observe("task", False)
                patches = [call for call in run.kube_json.call_args_list if call.args[0] == "patch"]
                self.assertEqual(len(patches), int(during_update))

    def test_propagates_failure_when_task_version_did_not_change(self):
        run, task = self.fixture([FINALIZER])
        failure = CheckFailed("permission denied")
        run.kube_json = Mock(side_effect=[task, failure, task])
        with self.assertRaises(CheckFailed) as raised:
            run.observe("task", False)
        self.assertIs(raised.exception, failure)
        self.assertEqual(run.kube_json.call_count, 3)

    def test_does_not_patch_an_already_settled_observer(self):
        for enabled in (False, True):
            with self.subTest(enabled=enabled):
                run, task = self.fixture(["another-controller"] + ([FINALIZER] if enabled else []))
                run.kube_json = Mock(return_value=task)
                run.observe("task", enabled)
                run.kube_json.assert_called_once_with("get", "task", "task", "-o", "json")


class CancellationReceiptTest(unittest.TestCase):
    @staticmethod
    def fixture():
        task = {"metadata": {"namespace": "test", "name": "task", "uid": "task-uid",
                             "deletionTimestamp": "2026-09-20T00:00:00Z"},
                "status": {"phase": "Cancelled", "execution": {"outcome": "Cancelled"}}}
        approval = {"id": "approval", "taskUID": "task-uid", "binding": {
            "runtimeSessionUID": "session", "operationIDDigest": "sha256:" + hashlib.sha256(b"operation").hexdigest(),
            "requestDigest": "sha256:" + "a" * 64}}
        before = {"metadata": {"name": "effect", "uid": "effect-uid"},
                  "spec": {"kind": "acp-mcp-tool", "identityNamespace": "test", "aggregateId": "session",
                           "operationId": "operation", "requestDigest": approval["binding"]["requestDigest"]},
                  "status": {"state": "Pending"}}
        after = copy.deepcopy(before)
        response = {"isError": True, "code": "approval_cancelled", "approvalID": "approval",
                    "error": "Tool execution was cancelled before tool execution."}
        digest = "sha256:" + hashlib.sha256(json.dumps(response, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        after["status"] = {"state": "Failed", "attempts": 0, "response": response, "responseDigest": digest}
        return task, approval, before, after

    def test_verifies_unexecuted_receipt_after_timeline_cleanup(self):
        evidence = validate_cancellation_receipt(*self.fixture())
        self.assertEqual(evidence["executionOutcome"], "not_started")
        self.assertEqual(evidence["attempts"], 0)
        self.assertNotIn("response", evidence)

    def test_rejects_replaced_or_started_actions_and_invalid_receipts(self):
        changes = [
            (0, ("metadata", "uid"), "replacement-task"),
            (0, ("metadata", "deletionTimestamp"), None),
            (0, ("status", "phase"), "Failed"),
            (2, ("status", "state"), "InFlight"),
            (3, ("metadata", "uid"), "replacement-effect"),
            (3, ("spec", "requestDigest"), "changed-digest"),
            (3, ("spec", "operationId"), "another-operation"),
            (3, ("status", "state"), "Succeeded"),
            (3, ("status", "state"), "OutcomeUnknown"),
            (3, ("status", "attempts"), 1),
            (3, ("status", "leaseOwner"), "executor"),
            (3, ("status", "response"), {}),
            (3, ("status", "response", "approvalID"), "another-approval"),
            (3, ("status", "response", "code"), "approval_stale"),
            (3, ("status", "responseDigest"), "sha256:" + "0" * 64),
        ]
        for index, path, value in changes:
            with self.subTest(index=index, path=path, value=value):
                fixture = self.fixture()
                target = fixture[index]
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = value
                with self.assertRaises(CheckFailed):
                    validate_cancellation_receipt(*fixture)

    def test_cancel_case_survives_erased_timeline_and_result(self):
        for completed in (False, True):
            with self.subTest(completed=completed):
                task, approval, before, after = self.fixture()
                if completed:
                    task["status"] = {"phase": "Succeeded", "execution": {"outcome": "Succeeded"},
                                      "delivery": {"outcome": "ReadValidated"}}
                run = object.__new__(Acceptance)
                run.create = Mock(return_value="task")
                run.pending = Mock(return_value=approval)
                run.approval_effect = Mock(return_value=before)
                run.observe = Mock()
                run.api = Mock(side_effect=[(204, None), (404, None)] if completed else [(204, None)])
                run.settled = Mock(return_value=task)
                run.decision = Mock(return_value=(404, None))
                run.kube_json = Mock(return_value=after)
                run.approval = Mock(return_value=None)
                run.counts = Mock(return_value={"inventoryReads": 1, "workOrderExecutions": 0})
                run.save = Mock()
                run.report = {"checks": []}
                run.run_case("cancel")
                check = run.report["checks"][0]
                self.assertIsNone(check["approvalStatus"])
                self.assertEqual(check["cancellationReceipt"]["code"], "approval_cancelled")
                self.assertEqual(check["lateDecisionHTTPStatus"], 404)
                self.assertEqual(run.report["cancellationDiagnostics"][0]["metadata"]["uid"], "task-uid")
                self.assertEqual(run.report["cancellationDiagnostics"][0]["execution"]["outcome"],
                                 "Succeeded" if completed else "Cancelled")
                run.observe.assert_any_call("task", False)

    def test_failed_task_diagnostics_are_saved_before_assertions_without_accepting_failure(self):
        task, approval, before, after = self.fixture()
        text = "provider failure with private credentials"
        task["status"] = {"phase": "Failed", "execution": {"outcome": "Failed", "reason": "PromptFailed", "message": text},
                          "delivery": {"outcome": "NotRequested"}, "completionTime": "2026-09-20T00:00:01Z"}
        with TemporaryDirectory() as directory:
            run = object.__new__(Acceptance)
            run.args = SimpleNamespace(report=Path(directory) / "report.json")
            run.report = {"checks": [], "passed": False}
            run.create = Mock(return_value="task")
            run.pending = Mock(return_value=approval)
            run.approval_effect = Mock(return_value=before)
            run.observe = Mock()
            run.api = Mock(return_value=(204, None))
            run.settled = Mock(return_value=task)
            run.kube_json = Mock(return_value=after)
            run.approval = Mock()

            def late_decision(*_args):
                # The first post-settlement operation sees evidence already on
                # disk, even if its transport or the receipt assertion fails.
                saved = json.loads(run.args.report.read_text())
                self.assertEqual(saved["cancellationDiagnostics"][0]["execution"]["reason"], "PromptFailed")
                self.assertNotIn(text, run.args.report.read_text())
                return 404, None

            run.decision = Mock(side_effect=late_decision)
            with self.assertRaisesRegex(CheckFailed, "cancellation neither stopped the prompt"):
                run.run_case("cancel")
            saved = json.loads(run.args.report.read_text())
            self.assertFalse(saved["passed"])
            self.assertEqual(saved["checks"], [])
            self.assertEqual(saved["cancellationDiagnostics"][0]["metadata"]["uid"], approval["taskUID"])
            self.assertEqual(saved["cancellationDiagnostics"][0]["phase"], "Failed")
            run.observe.assert_called_once_with("task", True)
            run.approval.assert_not_called()


if __name__ == "__main__":
    unittest.main()
