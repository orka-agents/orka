import copy
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import e2e
from acceptance import Acceptance, CheckFailed, FINALIZER


def pod(uid, names):
    return {"metadata": {"name": uid, "uid": uid}, "spec": {"nodeName": "owned-node",
            "containers": [{"name": name} for name in names]},
            "status": {"containerStatuses": [{"name": name, "containerID": "containerd://" + "a" * 64,
                                              "restartCount": 0, "ready": True} for name in names]}}


class RuntimeRecoveryTest(unittest.TestCase):
    def fixture(self):
        run = Mock()
        run.args = SimpleNamespace(agent="human-approval-foundry", backend="foundry")
        run.runtime = {"metadata": {"uid": "runtime-uid", "generation": 1}, "spec": {
            "deployment": {"kubernetesRecovery": {"containerName": "supervisor"}}}}
        run.witness = {"spec": {"operationId": "old-boot"}}
        run.create.return_value = "owned-runtime-loss"
        run.pending.return_value = {"id": "original-approval", "binding": {
            "supervisorBootIDDigest": "sha256:" + e2e.hashlib.sha256(b"old-boot").hexdigest()}}
        original = pod("runtime-pod", ["supervisor", "broker"])
        restarted = copy.deepcopy(original)
        restarted["status"]["containerStatuses"][0].update(containerID="containerd://" + "b" * 64, restartCount=1)
        remote = pod("hosted-pod", ["hosted-agentkit", "local-hosted-transport"])
        cluster = Mock()
        cluster.pod.side_effect = lambda name: remote if name == "human-approval-hosted" else original
        cluster.restarted.return_value = restarted
        return run, cluster, original, restarted, remote

    def test_recovery_uses_controller_retirement_without_reregistration(self):
        run, cluster, original, _, _ = self.fixture()
        frozen = copy.deepcopy(run.runtime)
        request = {"uid": "original-request", "sha256": "original-digest"}
        with patch.object(e2e, "register") as register, \
             patch.object(e2e, "request_evidence", return_value=request), \
             patch.object(e2e, "recovery_result") as result:
            e2e.runtime_recovery(cluster, run)
        register.assert_not_called()
        cluster.apply.assert_not_called()
        cluster.kill_container.assert_called_once_with(original, "supervisor")
        run.recovered.assert_called_once_with(run.witness, proof_kind="foundry-broker-retirement")
        result.assert_called_once_with(run, "owned-runtime-loss", run.pending.return_value, request, "enrolled-runtime-loss")
        self.assertEqual(run.runtime, frozen)

    def test_changed_retained_broker_prevents_success(self):
        run, cluster, _, restarted, _ = self.fixture()
        restarted["status"]["containerStatuses"][1]["restartCount"] = 1
        with patch.object(e2e, "request_evidence", return_value={}):
            with self.assertRaisesRegex(CheckFailed, "retained broker"):
                e2e.runtime_recovery(cluster, run)
        run.recovered.assert_not_called()

    def test_pending_review_must_match_enrolled_boot(self):
        run, cluster, *_ = self.fixture()
        run.pending.return_value["binding"]["supervisorBootIDDigest"] = "sha256:" + e2e.hashlib.sha256(b"unwitnessed-boot").hexdigest()
        with patch.object(e2e, "request_evidence", return_value={}):
            with self.assertRaisesRegex(CheckFailed, "enrolled runtime boot"):
                e2e.runtime_recovery(cluster, run)
        cluster.kill_container.assert_not_called()

    def test_azure_adapter_does_not_substitute_a_local_hosted_process(self):
        run, cluster, *_ = self.fixture()
        cluster.local_hosted = False
        with patch.object(e2e, "request_evidence", return_value={}), patch.object(e2e, "recovery_result"):
            e2e.runtime_recovery(cluster, run)
        self.assertEqual([call.args for call in cluster.pod.call_args_list], [("human-approval-foundry",)])

    def test_foundry_requires_recovery_capability_before_any_registration_write(self):
        cluster = Mock()
        caps = {"runtimeProfileDigest": "profile", "provider": {
            "supportsBrokeredToolApprovals": True, "supportsPermissions": False}}
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            (work / "foundry-profile.json").write_text(json.dumps({"profile": {"digest": "profile"}}))
            with patch.object(e2e, "http_json", return_value=(200, caps)):
                with self.assertRaisesRegex(CheckFailed, "supportsFoundryRecovery"):
                    e2e.register(cluster, work, "foundry", "http://fixture")
        cluster.call.assert_not_called()
        cluster.apply.assert_not_called()


class CrashGuardTests(unittest.TestCase):
    def test_expired_inflight_guard_prevents_node_signal(self):
        cluster = e2e.Cluster.__new__(e2e.Cluster)
        original = pod("runtime-pod", ["runtime"])
        cluster.json = Mock(return_value=original)
        with patch.object(e2e.subprocess, "run") as command:
            with self.assertRaisesRegex(CheckFailed, "window expired"):
                cluster.kill_container(original, "runtime", Mock(side_effect=CheckFailed("window expired")))
        command.assert_not_called()

    def test_pod_spec_drift_prevents_node_signal(self):
        cluster = e2e.Cluster.__new__(e2e.Cluster)
        original = pod("runtime-pod", ["runtime"])
        current = copy.deepcopy(original)
        current["spec"]["ephemeralContainers"] = [{"name": "debug"}]
        cluster.json = Mock(return_value=current)
        with patch.object(e2e.subprocess, "run") as command:
            with self.assertRaisesRegex(CheckFailed, "changed before crash"):
                cluster.kill_container(original, "runtime")
        command.assert_not_called()

    def test_unknown_effect_has_one_attempt_and_no_synthetic_receipt(self):
        original = {"metadata": {"name": "effect", "namespace": "orka-system", "uid": "effect-uid"}, "spec": {"operationId": "call"}}
        current = {**copy.deepcopy(original), "status": {"state": "OutcomeUnknown", "attempts": 1}}
        e2e.validate_claim(original, current, "OutcomeUnknown")
        for mutation in ({"attempts": 2}, {"responseDigest": "invented"}, {"response": {}}, {"leaseOwner": "old-owner"}):
            with self.subTest(mutation=mutation):
                changed = copy.deepcopy(current)
                changed["status"].update(mutation)
                with self.assertRaises(CheckFailed):
                    e2e.validate_claim(original, changed, "OutcomeUnknown")


class CleanupObserverTests(unittest.TestCase):
    def fixture(self):
        from test_recovery_checks import retained_cleanup_fixture
        cluster, task, approval, witness = retained_cleanup_fixture()
        run = e2e.FixtureAcceptance.__new__(e2e.FixtureAcceptance)
        run.owned_tasks = {"owned-task": "task-uid"}
        run.cleanup_artifacts = {"owned-task": e2e.recovery.task_artifacts(cluster, task)}
        run.cleanup_admissions = {"owned-task": e2e.recovery.admission_evidence(task, cluster.values[2], witness)}
        run.cleanup_approvals = {"owned-task": [e2e.recovery._approval_evidence(approval)]}
        run.witnesses = {"boot-1": witness}
        run.report = {"cleanup": []}
        run.save = Mock()
        run.cluster = cluster
        run.approval = Mock(return_value=approval)
        task["metadata"].update(finalizers=[FINALIZER], deletionTimestamp="now")
        run.kube_json = Mock(return_value=task)

        def bounded_wait(_description, check, _seconds=None):
            result = check()
            if not result:
                raise CheckFailed("condition not yet satisfied")
            return result

        run.wait = bounded_wait
        return run, task

    def test_observer_is_retained_until_product_finalizer_and_receipt_complete(self):
        for missing in ("receipt", "finalizer"):
            with self.subTest(missing=missing):
                run, task = self.fixture()
                if missing == "receipt":
                    task["status"]["execution"].pop("runtimeSessionCleanupDigest")
                else:
                    task["metadata"]["finalizers"].append(e2e.recovery.TASK_FINALIZER)
                with patch.object(Acceptance, "observe") as parent:
                    with self.assertRaisesRegex(CheckFailed, "not yet"):
                        run.observe("owned-task", False)
                parent.assert_not_called()

    def test_observer_removal_follows_receipt_capture_and_normal_deletion(self):
        run, _ = self.fixture()
        run.approval.side_effect = AssertionError("event stream already reclaimed")
        with patch.object(Acceptance, "observe", side_effect=lambda *_args: run.cluster.collect_task()) as parent:
            run.observe("owned-task", False)
        parent.assert_called_once_with("owned-task", False)
        self.assertTrue(run.report["cleanup"][0]["deleted"])
        self.assertTrue(run.report["cleanup"][0]["runtimeSessionCleanupDigest"].startswith("sha256:"))
        self.assertEqual(len(run.report["cleanup"][0]["retainedEffects"]), 2)
        self.assertEqual(len(run.cluster.values), 3, "the observer must preserve the witness and both receipts")
        run.approval.assert_not_called()

    def test_approval_identity_is_saved_before_delete_reclaims_events(self):
        run, task = self.fixture()
        run.task = Mock(return_value=task)
        with patch.object(Acceptance, "observe") as parent:
            run.observe("owned-task", True)
        parent.assert_called_once_with("owned-task", True)
        run.approval.assert_called_once_with("owned-task")
        self.assertEqual(run.cleanup_approvals["owned-task"][0]["id"], "approval-id")
        self.assertNotIn("operationID", run.cleanup_approvals["owned-task"][0]["binding"])

    def test_invalid_retained_receipt_keeps_observer_and_failure_evidence(self):
        run, _ = self.fixture()
        run.cluster.values[3]["status"]["state"] = "Pending"
        with patch.object(Acceptance, "observe") as parent:
            with self.assertRaisesRegex(CheckFailed, "pending"):
                run.observe("owned-task", False)
        parent.assert_not_called()
        self.assertEqual(run.report["cleanup"], [])

    def test_receipt_loss_after_observer_release_does_not_report_cleanup_pass(self):
        run, _ = self.fixture()

        def incorrectly_collect(*_args):
            run.cluster.collect_task()
            run.cluster.values.pop()

        with patch.object(Acceptance, "observe", side_effect=incorrectly_collect):
            with self.assertRaisesRegex(CheckFailed, "disappeared"):
                run.observe("owned-task", False)
        self.assertFalse(run.report["cleanup"][0]["deleted"])

    def test_creation_captures_sanitized_admission_before_recovery(self):
        run, task = self.fixture()
        run.witness = run.witnesses["boot-1"]
        run.report.update(exposures=[], admissions={})
        run.task = Mock(return_value=task)
        task["spec"]["prompt"] = "private fixture prompt"
        task["status"]["execution"]["message"] = "private runtime diagnostic"
        with patch.object(Acceptance, "create", return_value="owned-task"), patch.object(
                e2e.recovery, "exposed_task", return_value=run.cluster.values[2]):
            run.create("sample")
        evidence = run.report["admissions"]["owned-task"]
        self.assertNotIn("private", json.dumps(evidence))
        self.assertEqual(evidence["status"]["execution"]["runtimeSessionUID"], "session-uid")

    def test_admission_and_cleanup_use_stored_spec_despite_api_omissions(self):
        run, task = self.fixture()
        run.witness = run.witnesses["boot-1"]
        run.report.update(exposures=[], admissions={})
        # The CRD defaults this false field, but the typed API omits it.
        task["spec"]["workspace"] = {"intent": "read", "createPR": False}
        api_task = copy.deepcopy(task)
        del api_task["spec"]["workspace"]["createPR"]
        run.task = Mock(return_value=api_task)
        with patch.object(Acceptance, "create", return_value="owned-task"), patch.object(
                e2e.recovery, "exposed_task", return_value=run.cluster.values[2]):
            run.create("sample")
        with patch.object(Acceptance, "observe", side_effect=lambda *_args: run.cluster.collect_task()):
            run.observe("owned-task", False)
        self.assertEqual(run.report["admissions"]["owned-task"]["specDigest"], e2e.recovery.digest(task["spec"]))
        self.assertTrue(run.report["cleanup"][0]["deleted"])

    def test_admission_rejects_kubernetes_task_replacement(self):
        run, task = self.fixture()
        run.witness = run.witnesses["boot-1"]
        run.report.update(exposures=[], admissions={})
        run.task = Mock(return_value=task)
        replaced = copy.deepcopy(task)
        replaced["metadata"]["uid"] = "replacement-uid"
        run.kube_json.return_value = replaced
        with patch.object(Acceptance, "create", return_value="owned-task"), patch.object(
                e2e.recovery, "exposed_task", return_value=run.cluster.values[2]):
            with self.assertRaisesRegex(CheckFailed, "identity changed while capturing admission"):
                run.create("sample")
        self.assertEqual(run.report["admissions"], {})

    def test_cleanup_rejects_actual_spec_or_binding_drift_after_raw_admission(self):
        mutations = {
            "spec": lambda task: task["spec"]["workspace"].update(intent="write"),
            "binding": lambda task: task["status"]["agentExecutionBinding"]["snapshot"].update(digest="changed"),
        }
        for field, mutate in mutations.items():
            with self.subTest(field=field):
                run, task = self.fixture()
                run.witness = run.witnesses["boot-1"]
                run.report.update(exposures=[], admissions={})
                task["spec"]["workspace"] = {"intent": "read", "createPR": False}
                run.task = Mock(return_value=copy.deepcopy(task))
                with patch.object(Acceptance, "create", return_value="owned-task"), patch.object(
                        e2e.recovery, "exposed_task", return_value=run.cluster.values[2]):
                    run.create("sample")
                mutate(task)
                with patch.object(Acceptance, "observe") as parent:
                    with self.assertRaisesRegex(CheckFailed, "spec or immutable binding changed"):
                        run.observe("owned-task", False)
                parent.assert_not_called()
                self.assertEqual(run.report["cleanup"], [])


if __name__ == "__main__":
    unittest.main()
