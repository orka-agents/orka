import copy
import unittest

from acceptance import CheckFailed, validate_cancellation, validate_pending_approval, validate_runtime


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


if __name__ == "__main__":
    unittest.main()
