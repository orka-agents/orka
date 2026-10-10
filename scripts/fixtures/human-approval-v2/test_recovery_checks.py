import copy
import base64
import hashlib
import json
import unittest
from unittest.mock import Mock

import e2e
from acceptance import CheckFailed
import recovery_checks as recovery


def metadata(name, uid):
    return {"name": name, "namespace": "orka-system", "uid": uid, "generation": 1, "resourceVersion": "7"}


def effect(kind, aggregate, operation, response, request_digest=None):
    return {"apiVersion": "core.orka.ai/v1alpha1", "kind": "ExternalEffect",
            "metadata": metadata(kind + "-record", kind + "-uid"),
            "spec": {"kind": kind, "aggregateId": aggregate, "operationId": operation,
                     "requestDigest": request_digest or recovery.digest(response)},
            "status": {"state": "Succeeded", "response": response, "responseDigest": recovery.digest(response)}}


def canonical_effect(kind, aggregate, operation, response, request_digest=None):
    value = effect(kind, aggregate, operation, response, request_digest)
    parts = ("external-effect", kind, "orka-system", aggregate, operation)
    logical = "external-effect:sha256:" + hashlib.sha256(b"".join(
        str(len(part.encode())).encode() + b":" + part.encode() for part in parts)).hexdigest()
    value["spec"].update(id=logical, identityNamespace="orka-system")
    value["metadata"]["name"] = "external-effect-" + base64.b32encode(
        hashlib.sha256(logical.encode()).digest()).decode().rstrip("=").lower()
    return value


class CleanupCluster:
    """Metadata-only Secret reads and no mutation interface."""

    def __init__(self, values):
        self.values = values

    def json(self, *args):
        kinds = {"externaleffects": "ExternalEffect", "promptattempts": "PromptAttempt",
                 "runtimesessioncontrols": "RuntimeSessionControl"}
        assert args[0] == "get" and args[1] in kinds
        return {"items": [value for value in self.values if value["kind"] == kinds[args[1]]]}

    def call(self, *args):
        assert args[0] == "get"
        if args[1] == "secrets":
            assert ".metadata" in args[-1] and ".data" not in args[-1]
            return "\n".join(json.dumps(value["metadata"]) for value in self.values if value["kind"] == "Secret")
        matches = [value for value in self.values if value["kind"].lower() == args[1].lower() and value["metadata"]["name"] == args[2]]
        assert len(matches) <= 1
        return json.dumps(matches[0]) if matches else ""

    def collect_task(self):
        self.values[:] = [value for value in self.values if value["kind"] == "ExternalEffect"]


def retained_cleanup_fixture():
    """One genuine finalized Task, its exposure, approval, and boot witness."""
    task = {"kind": "Task", "metadata": metadata("owned-task", "task-uid"), "spec": {"agentRef": {"name": "fixture"}},
        "status": {"phase": "Succeeded", "completionTime": "2026-09-21T00:01:00Z", "execution": {
            "outcome": "Succeeded", "attempt": 1, "promptID": "prompt-1", "requestDigest": recovery.digest("prompt"),
            "agentRuntimeUID": "runtime-uid", "agentRuntimeName": "fixture-runtime", "runtimeInstanceID": "instance",
            "runtimeSessionSupervisorBootID": "boot-1", "runtimeSessionUID": "session-uid", "runtimeSessionGeneration": 2,
            "controllerEpoch": 17}, "agentExecutionBinding": {"contractVersion": "orka.harness.v2", "backend": "external-endpoint",
            "task": {"uid": "task-uid"}, "runtimeRef": {"uid": "runtime-uid", "generation": 1},
            "bindingDigest": recovery.digest("binding"), "snapshot": {"digest": recovery.digest("snapshot")},
            "runtimeProfileDigest": recovery.digest("profile")}}}
    execution, binding = task["status"]["execution"], task["status"]["agentExecutionBinding"]
    execution["runtimeSessionCleanupDigest"] = recovery.digest({"taskUID": "task-uid", **{
        key: execution[key] for key in ("attempt", "runtimeInstanceID", "runtimeSessionUID", "runtimeSessionGeneration")}},
        "task-runtime-session-cleanup")
    fence = {"runtimeInstanceID": "instance", "supervisorBootID": "boot-1", "controllerEpoch": 17,
        "runtimePoolUID": "pool-uid", "runtimePoolGeneration": 1, "runtimeProfileDigest": recovery.digest("profile"),
        "profileDigestSchemaVersion": 1}
    witness = canonical_effect("agent-runtime-boot-witness", "runtime-uid", "boot-1", {
        "schemaVersion": 1, "runtimeUID": "runtime-uid", "runtimeGeneration": 1, "fence": fence})
    exposure = {"schemaVersion": 1, "namespace": "orka-system", "taskUID": "task-uid", "attempt": 1, "promptID": "prompt-1",
        "requestDigest": execution["requestDigest"], "bindingDigest": binding["bindingDigest"],
        "snapshotDigest": binding["snapshot"]["digest"], "runtimeUID": "runtime-uid", "witnessDigest": witness["spec"]["requestDigest"],
        "fence": {**fence, "runtimeSessionUID": "session-uid", "runtimeSessionGeneration": 2}}
    exposure = canonical_effect("agent-runtime-session-exposure", "task-uid",
        recovery.digest(["task-uid", "1", "prompt-1", "runtime-uid", "boot-1", "session-uid", "2"], "agent-runtime-session-exposure"),
        exposure, recovery.digest(exposure, "runtime-exposure"))
    approval = {"id": "approval-id", "taskUID": "task-uid", "binding": {"taskAttempt": 1, "promptID": "prompt-1",
        "operationID": "operation-1", "runtimeInstanceID": "instance", "supervisorBootID": "boot-1", "controllerEpoch": 17,
        "runtimeSessionUID": "session-uid", "runtimeSessionGeneration": 2, "requestDigest": recovery.digest("call")}}
    receipt = canonical_effect("acp-mcp-tool", "session-uid", "operation-1", {"simulation": True}, approval["binding"]["requestDigest"])
    receipt["metadata"]["labels"] = {"core.orka.ai/task-uid": "task-uid"}
    receipt["status"]["attempts"] = 1
    prompt = {"kind": "PromptAttempt", "metadata": metadata("attempt", "attempt-uid"), "spec": {"taskUid": "task-uid"}}
    secret = {"kind": "Secret", "metadata": {**metadata("request", "request-uid"),
        "ownerReferences": [{"kind": "Task", "uid": "task-uid"}]}}
    cluster = CleanupCluster([task, witness, exposure, receipt, prompt, secret])
    return cluster, task, approval, witness


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        image = "fixture@sha256:" + "a" * 64
        self.runtime = {"metadata": metadata("human-approval-agentkit-runtime", "runtime-uid"), "spec": {
            "capabilities": {"runtimeInstanceID": "stable-instance", "supportsDrain": True,
                             "profile": {"digest": "profile-digest"}},
            "deployment": {"kubernetesRecovery": {"deploymentName": "human-approval-agentkit",
                "deploymentUID": "deployment-uid", "containerName": "runtime"}}},
            "status": {"ready": True, "observedGeneration": 1, "observedCapabilities": {
                "runtimeInstanceID": "stable-instance", "supervisorBootID": "old-boot", "controllerEpoch": 17}}}
        self.deployment = {"metadata": {**metadata("human-approval-agentkit", "deployment-uid"),
                                       "annotations": {recovery.OWNER: "runtime-uid"}},
            "spec": {"replicas": 1, "strategy": {"type": "Recreate"}, "template": {"spec": {
                "automountServiceAccountToken": False, "containers": [{"name": "runtime", "image": image,
                    "env": [{"name": "ORKA_ACP_RUNTIME_INSTANCE_ID", "value": "stable-instance"},
                            {"name": "ORKA_ACP_CONTROLLER_EPOCH", "value": "17"}]}]}}}}
        self.pod = {"metadata": {**metadata("runtime-pod", "pod-uid"), "finalizers": [recovery.POD_FINALIZER]},
                    "status": {"containerStatuses": [{"name": "runtime", "containerID": "containerd://original",
                        "imageID": image, "restartCount": 0, "state": {"running": {"startedAt": "2026-09-21T00:00:00Z"}}}]}}
        self.witness = {"schemaVersion": 1, "runtimeUID": "runtime-uid", "runtimeGeneration": 1,
            "spec": copy.deepcopy(self.runtime["spec"]), "deploymentUID": "deployment-uid", "podUID": "pod-uid",
            "podName": "runtime-pod", "containerID": "containerd://original", "imageID": image, "restartCount": 0,
            "startedAt": "2026-09-21T00:00:00Z", "fence": {"runtimeInstanceID": "stable-instance",
                "supervisorBootID": "old-boot", "controllerEpoch": 17}}
        self.effects = [effect("agent-runtime-boot-witness", "runtime-uid", "old-boot", self.witness)]
        self.cluster = Mock()
        self.cluster.pod.return_value = self.pod

        def read(*args, **_kwargs):
            if args[:2] == ("get", "agentruntime"):
                return self.runtime
            if args[:2] == ("get", "deployment"):
                return self.deployment
            if args[:2] == ("get", "externaleffects"):
                return {"items": self.effects}
            raise AssertionError("unexpected read")

        self.cluster.json.side_effect = read
        self.frozen = copy.deepcopy(self.runtime)

    def test_ready_requires_exact_enrolled_witness(self):
        current, observed = recovery.ready_witness(self.cluster, self.frozen, "agentkit", expected_epoch=17)
        self.assertIs(current, self.runtime)
        self.assertIs(observed, self.effects[0])
        self.assertIsNone(recovery.ready_witness(self.cluster, self.frozen, "agentkit", previous_boot="old-boot"))
        self.assertIsNone(recovery.ready_witness(self.cluster, self.frozen, "agentkit", expected_epoch=18))

    def test_reregistration_or_replacement_is_rejected(self):
        for field, value in (("uid", "new-runtime"), ("generation", 2)):
            with self.subTest(field=field):
                original = self.runtime["metadata"][field]
                self.runtime["metadata"][field] = value
                with self.assertRaisesRegex(CheckFailed, "replaced or re-registered"):
                    recovery.ready_witness(self.cluster, self.frozen, "agentkit")
                self.runtime["metadata"][field] = original

    def test_missing_pod_retention_or_changed_container_is_rejected(self):
        self.pod["metadata"]["finalizers"] = []
        with self.assertRaisesRegex(CheckFailed, "exact retained"):
            recovery.ready_witness(self.cluster, self.frozen, "agentkit")
        self.pod["metadata"]["finalizers"] = [recovery.POD_FINALIZER]
        self.pod["status"]["containerStatuses"][0]["containerID"] = "containerd://replacement"
        with self.assertRaisesRegex(CheckFailed, "exact retained"):
            recovery.ready_witness(self.cluster, self.frozen, "agentkit")

    def test_pending_witness_never_grants_admission(self):
        self.effects[0]["status"] = {"state": "Pending"}
        self.assertIsNone(recovery.ready_witness(self.cluster, self.frozen, "agentkit"))

    def test_corrupt_witness_digest_fails_closed(self):
        self.effects[0]["status"]["responseDigest"] = "sha256:" + "0" * 64
        with self.assertRaisesRegex(CheckFailed, "immutable observation"):
            recovery.ready_witness(self.cluster, self.frozen, "agentkit")

    def test_enrollment_creates_once_and_fences_reciprocal_consent(self):
        self.deployment["metadata"]["annotations"] = {}
        self.cluster.json.side_effect = None
        self.cluster.json.side_effect = [self.deployment, self.runtime, self.deployment]
        created = recovery.enroll(self.cluster, self.frozen, "agentkit")
        self.assertIs(created, self.runtime)
        call = self.cluster.call.call_args
        self.assertEqual(call.args, ("create", "-f", "-"))
        self.assertEqual(call.kwargs["body"]["spec"]["deployment"]["kubernetesRecovery"],
                         self.frozen["spec"]["deployment"]["kubernetesRecovery"])
        operations = self.cluster.json.call_args.kwargs["body"]
        self.assertEqual(operations[:2], [
            {"op": "test", "path": "/metadata/uid", "value": "deployment-uid"},
            {"op": "test", "path": "/metadata/resourceVersion", "value": "7"}])
        self.assertEqual(operations[2]["value"][recovery.OWNER], "runtime-uid")

    def test_unsupported_topology_is_rejected_before_creation(self):
        for mutate in (lambda pod: pod.update(initContainers=[{}]), lambda pod: pod.update(ephemeralContainers=[{}]),
                       lambda pod: pod["containers"].append({"name": "extra", "image": "other"})):
            with self.subTest(mutate=mutate):
                self.deployment = copy.deepcopy(self.deployment)
                original = copy.deepcopy(self.deployment)
                mutate(self.deployment["spec"]["template"]["spec"])
                with self.assertRaises(CheckFailed):
                    recovery.enroll(self.cluster, self.frozen, "agentkit")
                self.cluster.call.assert_not_called()
                self.deployment = original

    def test_retirement_must_match_original_witness_digest(self):
        proof = effect("agent-runtime-boot-retirement", "runtime-uid", "old-boot", {
            "schemaVersion": 1, "kind": "authenticated-drain", "witnessDigest": "sha256:" + "0" * 64})
        self.effects.append(proof)
        with self.assertRaisesRegex(CheckFailed, "enrolled owner"):
            recovery.retirement(self.cluster, self.effects[0])

    def test_container_retirement_requires_exact_original_incarnation(self):
        proof = {"schemaVersion": 1, "kind": "kubernetes-container-termination",
                 "witnessDigest": self.effects[0]["spec"]["requestDigest"], "containerTermination": {
                     "containerID": self.witness["containerID"], "startedAt": self.witness["startedAt"],
                     "finishedAt": "2026-09-21T00:01:00Z"}}
        self.effects.append(effect("agent-runtime-boot-retirement", "runtime-uid", "old-boot", proof,
                                   self.effects[0]["spec"]["requestDigest"]))
        self.assertIs(recovery.retirement(self.cluster, self.effects[0]), self.effects[1])
        for changed in ("containerID", "startedAt"):
            with self.subTest(changed=changed):
                bad = copy.deepcopy(proof)
                bad["containerTermination"][changed] = "another-container-or-boot"
                self.effects[1] = effect("agent-runtime-boot-retirement", "runtime-uid", "old-boot", bad,
                                         self.effects[0]["spec"]["requestDigest"])
                with self.assertRaisesRegex(CheckFailed, "original container"):
                    recovery.retirement(self.cluster, self.effects[0])

    def test_foundry_local_death_is_not_remote_retirement(self):
        witness = copy.deepcopy(self.witness)
        witness["foundryBroker"] = {"protocol": "orka.foundry.broker.v1",
            "ledgerIdentityDigest": "sha256:" + "1" * 64, "agentConfigurationDigest": "sha256:" + "2" * 64}
        self.effects[0] = effect("agent-runtime-boot-witness", "runtime-uid", "old-boot", witness)
        proof = {"schemaVersion": 1, "kind": "kubernetes-container-termination",
                 "witnessDigest": self.effects[0]["spec"]["requestDigest"], "containerTermination": {
                     "containerID": witness["containerID"], "startedAt": witness["startedAt"],
                     "finishedAt": "2026-09-21T00:01:00Z"}}
        self.effects.append(effect("agent-runtime-boot-retirement", "runtime-uid", "old-boot", proof,
                                   self.effects[0]["spec"]["requestDigest"]))
        with self.assertRaisesRegex(CheckFailed, "remote Foundry"):
            recovery.retirement(self.cluster, self.effects[0])
        proof["kind"] = "foundry-broker-retirement"
        self.effects[1] = effect("agent-runtime-boot-retirement", "runtime-uid", "old-boot", proof,
                                 self.effects[0]["spec"]["requestDigest"])
        with self.assertRaisesRegex(CheckFailed, "sealed remote"):
            recovery.retirement(self.cluster, self.effects[0])

    def test_drain_requires_exact_boot_and_zero_live_work(self):
        status = {"fence": self.witness["fence"], "drain": {"requested": True, "acceptingNewSessions": False},
                  "pressure": {key: 0 for key in ("residentSessions", "activePrompts", "queuedAdmissions",
                                                 "pendingPermissions", "liveDescendants")}}
        proof = {"schemaVersion": 1, "kind": "authenticated-drain", "drainedStatus": status,
                 "witnessDigest": self.effects[0]["spec"]["requestDigest"]}
        self.effects.append(effect("agent-runtime-boot-retirement", "runtime-uid", "old-boot", proof,
                                   self.effects[0]["spec"]["requestDigest"]))
        self.assertIs(recovery.retirement(self.cluster, self.effects[0], "authenticated-drain"), self.effects[1])
        with self.assertRaisesRegex(CheckFailed, "enrolled owner"):
            recovery.retirement(self.cluster, self.effects[0], "foundry-broker-retirement")
        status["pressure"]["activePrompts"] = 1
        self.effects[1] = effect("agent-runtime-boot-retirement", "runtime-uid", "old-boot", proof,
                                 self.effects[0]["spec"]["requestDigest"])
        with self.assertRaisesRegex(CheckFailed, "closed and idle"):
            recovery.retirement(self.cluster, self.effects[0])

    def test_secret_discovery_never_requests_secret_data(self):
        meta = metadata("retained-auth", "secret-uid")
        meta["ownerReferences"] = [{"kind": "AgentRuntime", "uid": "runtime-uid"}]
        self.cluster.call.return_value = json.dumps(meta) + "\n"
        self.assertEqual(recovery.authority_metadata(self.cluster, "runtime-uid"), [{"kind": "Secret", "metadata": meta}])
        expression = self.cluster.call.call_args.args[-1]
        self.assertIn(".metadata", expression)
        self.assertNotIn(".data", expression)


class CleanupTests(unittest.TestCase):
    def task(self):
        execution = {"outcome": "OutcomeUnknown", "attempt": 1, "runtimeInstanceID": "runtime-instance",
                     "runtimeSessionUID": "session-uid", "runtimeSessionGeneration": 1}
        task = {"metadata": metadata("owned-task", "task-uid"), "status": {"execution": execution, "completionTime": "now"}}
        body = {"taskUID": "task-uid", **{key: execution[key] for key in
            ("attempt", "runtimeInstanceID", "runtimeSessionUID", "runtimeSessionGeneration")}}
        execution["runtimeSessionCleanupDigest"] = recovery.digest(body, "task-runtime-session-cleanup")
        return task

    def test_unknown_outcome_requires_exact_cleanup_receipt(self):
        task = self.task()
        self.assertEqual(recovery.cleanup_receipt(task)["runtimeSessionUID"], "session-uid")
        task["status"]["execution"].pop("runtimeSessionCleanupDigest")
        self.assertIsNone(recovery.cleanup_receipt(task))

    def test_receipt_for_old_task_uid_is_rejected(self):
        task = self.task()
        task["metadata"]["uid"] = "reused-task"
        self.assertIsNone(recovery.cleanup_receipt(task))

    def test_active_task_is_not_cleanup_evidence(self):
        task = self.task()
        task["status"]["execution"]["outcome"] = "Running"
        with self.assertRaisesRegex(CheckFailed, "unsettled"):
            recovery.cleanup_receipt(task)

    def test_delete_rechecks_uid_and_uses_precondition(self):
        task, cluster = self.task(), Mock()
        cluster.json.return_value = task
        recovery.delete_exact(cluster, "tasks", task)
        self.assertEqual(cluster.call.call_args.kwargs["body"]["preconditions"], {"uid": "task-uid"})
        cluster.call.reset_mock()
        cluster.json.return_value = copy.deepcopy(task)
        cluster.json.return_value["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(CheckFailed, "UID changed"):
            recovery.delete_exact(cluster, "tasks", task)
        cluster.call.assert_not_called()

    def test_delete_never_retries_uncertain_transport_or_deletes_artifacts(self):
        task, cluster = self.task(), Mock()
        cluster.json.return_value = task
        cluster.call.side_effect = CheckFailed("transport uncertain")
        with self.assertRaisesRegex(CheckFailed, "uncertain"):
            recovery.delete_exact(cluster, "tasks", task)
        self.assertEqual(cluster.call.call_count, 1)
        with self.assertRaisesRegex(CheckFailed, "kind is not allowed"):
            recovery.delete_exact(cluster, "secrets", task)

    def test_any_referencing_task_prevents_runtime_authority_deletion(self):
        cluster = Mock()
        agent = {"metadata": metadata("fixture-agent", "agent-uid")}
        runtime = {"metadata": metadata("fixture-runtime", "runtime-uid")}
        for ref in ({"spec": {"agentRef": {"name": "fixture-agent"}}},
                    {"status": {"agentExecutionBinding": {"runtimeRef": {"uid": "runtime-uid"}}}}):
            with self.subTest(ref=ref):
                cluster.json.return_value = {"items": [{"metadata": metadata("outside-prefix-task", "outside-uid"), **ref}]}
                with self.assertRaisesRegex(CheckFailed, "referencing Task"):
                    recovery.require_unused_runtime(cluster, agent, runtime)
        cluster.call.assert_not_called()

    def test_referencing_session_prevents_authority_deletion(self):
        cluster = Mock()
        agent = {"metadata": metadata("fixture-agent", "agent-uid")}
        runtime = {"metadata": metadata("fixture-runtime", "runtime-uid")}
        cluster.json.side_effect = [{"items": []}, {"items": [{"status": {"lineage": {"runtimeIdentity": "runtime-uid"}}}]}]
        with self.assertRaisesRegex(CheckFailed, "referencing Session"):
            recovery.require_unused_runtime(cluster, agent, runtime)
        cluster.call.assert_not_called()


class RetainedReceiptTests(unittest.TestCase):
    def setUp(self):
        self.cluster, self.task, self.approval, self.witness = retained_cleanup_fixture()
        self.artifacts = recovery.task_artifacts(self.cluster, self.task)

    def capture(self):
        evidence = recovery.cleanup_evidence(self.cluster, self.task, self.artifacts, [self.approval], {"boot-1": self.witness})
        return {"task": "owned-task", "uid": "task-uid", **recovery.cleanup_receipt(self.task),
            "artifacts": self.artifacts, "productFinalizerReleased": True,
            **evidence}

    def test_normal_cleanup_preserves_exact_terminal_effects(self):
        record = self.capture()
        self.assertFalse(recovery.verify_task_cleanup(self.cluster, record))
        self.cluster.collect_task()
        self.assertTrue(recovery.verify_task_cleanup(self.cluster, record))
        self.assertEqual(len(record["retainedEffects"]), 2)
        self.assertNotIn("simulation", json.dumps(record))
        self.assertNotIn("operationID", record["retainedEffects"][1]["approval"]["binding"])

    def test_pending_missing_replaced_changed_and_leased_effects_fail(self):
        mutations = (lambda value: value["status"].update(state="Pending"),
            lambda value: value["metadata"].update(uid="replacement"),
            lambda value: value["spec"].update(requestDigest=recovery.digest("different")),
            lambda value: value["status"].update(responseDigest=recovery.digest("different")),
            lambda value: value["status"].update(response={"changed": True}),
            lambda value: value["status"].update(attempts=2),
            lambda value: value["status"].update(leaseOwner="live"),
            lambda value: value["metadata"].update(labels={"core.orka.ai/task-uid": "different"}))
        for mutate in mutations:
            self.setUp()
            record = self.capture()
            self.cluster.collect_task()
            mutate(self.cluster.values[-1])
            with self.subTest(mutation=mutate), self.assertRaises(CheckFailed):
                recovery.verify_task_cleanup(self.cluster, record)
        self.setUp()
        record = self.capture()
        self.cluster.collect_task()
        self.cluster.values.pop()
        with self.assertRaisesRegex(CheckFailed, "disappeared"):
            recovery.verify_task_cleanup(self.cluster, record)

    def test_capture_rejects_unbound_succeeded_receipt_or_wrong_task(self):
        for mutate in (lambda value: value.update(taskUID="other"),
                       lambda value: value["binding"].update(requestDigest=recovery.digest("other")),
                       lambda value: value["binding"].update(operationID="other"),
                       lambda value: value["binding"].update(supervisorBootID="other"),
                       lambda value: value["binding"].update(runtimeSessionGeneration=3)):
            self.setUp()
            mutate(self.approval)
            with self.subTest(mutation=mutate), self.assertRaises(CheckFailed):
                self.capture()

    def test_exposure_requires_exact_task_session_binding_and_witness(self):
        for target, field, changed in (("body", "taskUID", "other"), ("body", "bindingDigest", recovery.digest("other")),
            ("body", "witnessDigest", recovery.digest("other")), ("fence", "runtimeSessionUID", "other"),
            ("fence", "runtimeSessionGeneration", 3), ("fence", "controllerEpoch", 18), ("fence", "supervisorBootID", "other")):
            self.setUp()
            exposure = self.cluster.values[2]
            body = exposure["status"]["response"]
            (body if target == "body" else body["fence"])[field] = changed
            exposure["spec"]["requestDigest"] = recovery.digest(body, "runtime-exposure")
            exposure["status"]["responseDigest"] = recovery.digest(body)
            with self.subTest(field=field), self.assertRaises(CheckFailed):
                self.capture()

    def test_cleanup_requires_genuine_receipt_and_released_product_finalizer(self):
        for missing in ("receipt", "product"):
            self.setUp()
            if missing == "receipt":
                self.task["status"]["execution"].pop("runtimeSessionCleanupDigest")
            else:
                self.task["metadata"]["finalizers"] = [recovery.TASK_FINALIZER]
            with self.subTest(missing=missing), self.assertRaises(CheckFailed):
                self.capture()

    def test_unknown_keeps_one_attempt_without_fabricating_a_result(self):
        receipt = self.cluster.values[3]
        receipt["status"] = {"state": "OutcomeUnknown", "attempts": 1}
        record = self.capture()
        self.cluster.collect_task()
        self.assertTrue(recovery.verify_task_cleanup(self.cluster, record))
        receipt["status"]["response"] = {"madeUp": True}
        with self.assertRaisesRegex(CheckFailed, "synthetic receipt"):
            self.capture()

    def test_denial_receipt_requires_matching_approval_and_definitive_code(self):
        receipt = self.cluster.values[3]
        response = {"isError": True, "code": "approval_stale", "approvalID": "approval-id"}
        receipt["status"] = {"state": "Failed", "response": response, "responseDigest": recovery.digest(response)}
        self.capture()
        for field, value in (("approvalID", "other"), ("code", "tool_failed"), ("isError", False)):
            saved = response[field]
            response[field] = value
            receipt["status"]["responseDigest"] = recovery.digest(response)
            with self.subTest(field=field), self.assertRaises(CheckFailed):
                self.capture()
            response[field] = saved

    def test_pre_execution_denial_after_claim_retains_one_attempt(self):
        receipt = self.cluster.values[3]
        response = {"isError": True, "code": "approval_stale", "approvalID": "approval-id"}
        receipt["status"] = {"state": "Failed", "attempts": 1, "response": response, "responseDigest": recovery.digest(response)}
        self.capture()
        receipt["status"]["attempts"] = 2
        with self.assertRaisesRegex(CheckFailed, "unexecuted approval"):
            self.capture()

    def test_controller_epoch_projection_preserves_original_admission_fence(self):
        admission = recovery.admission_evidence(self.task, self.cluster.values[2], self.witness)
        self.task["status"]["execution"]["controllerEpoch"] = 18
        evidence = recovery.cleanup_evidence(self.cluster, self.task, self.artifacts, [self.approval], {"boot-1": self.witness}, admission)
        self.assertEqual(evidence["admissionEvidence"]["status"]["execution"]["controllerEpoch"], 17)
        self.assertNotIn("retirement", evidence["retainedEffects"][0])

    def test_cleared_planned_execution_requires_exact_boot_retirement(self):
        admission = recovery.admission_evidence(self.task, self.cluster.values[2], self.witness)
        self.task["status"]["execution"].update(runtimeInstanceID="", runtimeSessionUID="", runtimeSessionGeneration=0, controllerEpoch=18)
        with self.assertRaisesRegex(CheckFailed, "genuine original boot retirement"):
            recovery.cleanup_evidence(self.cluster, self.task, self.artifacts, [self.approval], {"boot-1": self.witness}, admission)
        response = {"schemaVersion": 1, "kind": "authenticated-drain", "witnessDigest": self.witness["spec"]["requestDigest"],
            "drainedStatus": {"fence": self.witness["status"]["response"]["fence"],
                "drain": {"requested": True, "acceptingNewSessions": False},
                "pressure": {key: 0 for key in ("residentSessions", "activePrompts", "queuedAdmissions", "pendingPermissions", "liveDescendants")}}}
        proof = canonical_effect("agent-runtime-boot-retirement", "runtime-uid", "boot-1", response, self.witness["spec"]["requestDigest"])
        self.cluster.values.append(proof)
        evidence = recovery.cleanup_evidence(self.cluster, self.task, self.artifacts, [self.approval], {"boot-1": self.witness}, admission)
        record = {"task": "owned-task", "uid": "task-uid", **recovery.cleanup_receipt(self.task),
            "artifacts": self.artifacts, "productFinalizerReleased": True, **evidence}
        self.cluster.collect_task()
        self.assertTrue(recovery.verify_task_cleanup(self.cluster, record))
        proof["status"]["response"]["drainedStatus"]["pressure"]["activePrompts"] = 1
        proof["status"]["responseDigest"] = recovery.digest(proof["status"]["response"])
        with self.assertRaises(CheckFailed):
            recovery.verify_task_cleanup(self.cluster, record)

    def test_new_or_reused_transient_artifact_prevents_completion(self):
        record = self.capture()
        self.cluster.collect_task()
        self.cluster.values.append({"kind": "Secret", "metadata": {**metadata("unexpected", "new-uid"),
            "ownerReferences": [{"kind": "Task", "uid": "task-uid"}]}})
        with self.assertRaisesRegex(CheckFailed, "unrecorded"):
            recovery.verify_task_cleanup(self.cluster, record)

    def test_other_task_approval_on_shared_session_is_not_reclaimed(self):
        other = canonical_effect("acp-mcp-tool", "session-uid", "other-call", {"simulation": True})
        other["metadata"]["labels"] = {"core.orka.ai/task-uid": "other-task"}
        self.cluster.values.append(other)
        self.assertEqual(recovery.task_artifacts(self.cluster, self.task), self.artifacts)


if __name__ == "__main__":
    unittest.main()
