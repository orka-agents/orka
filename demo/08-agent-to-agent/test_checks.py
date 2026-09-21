"""Offline checks of the demo's evidence failures and controller patch boundary.

These synthetic records are unit-test inputs, never walkthrough output.
"""

import base64
import copy
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import evidence
import prepare


def reference(event_id, when):
    return "a2a1." + ".".join(base64.urlsafe_b64encode(part.encode()).decode().rstrip("=")
                             for part in (event_id, when))


def records(number=1):
    event_id = f"event-{number}"
    when = "2026-09-20T12:00:00.123456789Z"
    event = {"id": event_id, "createdAt": when, "state": "Completed", "threadId": "test-conversation", "gatewayName": "demo-a2a",
             "agentName": "demo-a2a-inventory", "taskName": f"task-{number}", "taskUid": f"uid-{number}",
             "namespace": "demo", "sessionName": "session-1"}
    task = {"metadata": {"name": event["taskName"], "uid": event["taskUid"], "namespace": "demo",
                         "annotations": {"gateway.orka.ai/event-id": event_id}},
            "spec": {"sessionRef": {"name": event["sessionName"]}}, "status": {"phase": "Succeeded"}}
    answer = "We can supply 18 today. The remaining 6 arrive tomorrow."
    a2a = {"id": reference(event_id, when), "contextId": "test-conversation",
           "status": {"state": "TASK_STATE_COMPLETED"},
           "artifacts": [{"artifactId": "final", "parts": [{"text": answer}]}]}
    return a2a, event, task, {"result": answer}


class EvidenceChecks(unittest.TestCase):
    def test_final_report_rejects_unchanged_adapter_and_additional_tasks(self):
        first, event1, task1, result1 = records()
        followup, event2, task2, result2 = records(2)
        def pod(uid):
            return {"items": [{"metadata": {"uid": uid}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}}]}
        with tempfile.TemporaryDirectory() as directory:
            raw = Path(directory) / "raw"
            raw.mkdir()
            files = {"first-admission": first, "retry": first, "first-completed-event": event1,
                     "retry-event": event1, "retry-task": task1, "tasks-after-first": {"items": [task1]},
                     "tasks-after-retry": {"items": [task1]}, "followup-completed-event": event2,
                     "first-answer": first, "followup-answer": followup, "first-task": task1, "followup-task": task2,
                     "first-result": result1, "followup-result": result2, "tasks-before-restart": {"items": [task1, task2]},
                     "tasks-after-restart": {"items": [task1, task2]}, "pods-before-restart": pod("before"),
                     "pods-after-restart": pod("after"), "after-restart-answer": followup, "after-restart-event": event2}
            for name, value in files.items():
                (raw / (name + ".json")).write_text(json.dumps(value))
            with contextlib.redirect_stdout(io.StringIO()):
                evidence.report(raw)
            self.assertEqual(evidence.read(raw.parent / "evidence.json")["newTasksAfterRestart"], 0)
            (raw / "pods-after-restart.json").write_text(json.dumps(pod("before")))
            with self.assertRaisesRegex(ValueError, "different adapter"):
                evidence.report(raw)
            (raw / "pods-after-restart.json").write_text(json.dumps(pod("after")))
            (raw / "tasks-after-restart.json").write_text(json.dumps({"items": [task1, task2, records(3)[2]]}))
            with self.assertRaisesRegex(ValueError, "exactly two"):
                evidence.report(raw)

    def test_correlates_nanosecond_admission_and_rejects_replaced_task(self):
        a2a, event, task, _ = records()
        evidence.correlate(a2a, event, task)
        task["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(ValueError, "UID"):
            evidence.correlate(a2a, event, task)

    def test_public_reference_does_not_cross_readmission(self):
        a2a, event, task, _ = records()
        event["createdAt"] = "2026-09-20T12:00:00.123456790Z"
        with self.assertRaisesRegex(ValueError, "admission"):
            evidence.correlate(a2a, event, task)

    def test_reply_requires_success_and_exact_saved_answer(self):
        a2a, _, _, result = records()
        self.assertEqual(evidence.reply(a2a, result), result["result"])
        result["result"] = "A different answer"
        with self.assertRaisesRegex(ValueError, "different"):
            evidence.reply(a2a, result)
        a2a["status"]["state"] = "TASK_STATE_WORKING"
        with self.assertRaisesRegex(ValueError, "completed"):
            evidence.reply(a2a, result)

    def test_generic_reply_is_not_conversation_evidence(self):
        a2a, _, _, result = records()
        result["result"] = "We can offer available stock and arrange a later delivery."
        a2a["artifacts"][0]["parts"][0]["text"] = result["result"]
        with self.assertRaisesRegex(ValueError, "quantities"):
            evidence.reply(a2a, result)

    def test_retry_fails_when_the_snapshot_contains_duplicate_work(self):
        a2a, event, task, _ = records()
        with tempfile.TemporaryDirectory() as directory:
            raw = Path(directory)
            files = {"first-admission": a2a, "retry": a2a, "first-completed-event": event,
                     "retry-event": event, "retry-task": task,
                     "tasks-after-first": {"items": [task]}, "tasks-after-retry": {"items": [task]}}
            for name, value in files.items():
                (raw / (name + ".json")).write_text(json.dumps(value))
            self.assertEqual(evidence.retry_check(raw), 1)
            (raw / "tasks-after-retry.json").write_text(json.dumps({"items": [task, records(2)[2]]}))
            with self.assertRaisesRegex(ValueError, "identities"):
                evidence.retry_check(raw)


class ControllerPreparation(unittest.TestCase):
    def setUp(self):
        self.deployment = {"metadata": {"name": "controller", "uid": "controller-uid", "resourceVersion": "7"},
            "spec": {"selector": {"matchLabels": {"app": "controller"}}, "template": {
                "metadata": {"labels": {"app": "controller"}}, "spec": {
                    "volumes": [{"name": "store", "persistentVolumeClaim": {"claimName": "db"}}],
                    "containers": [{"name": "manager", "args": ["--watch-namespace=demo", "--controller-mode=harness-v2"],
                        "env": [{"name": "SSL_CERT_DIR", "value": "/existing/ca"},
                                {"name": "UNRELATED_SETTING", "value": "must-not-be-copied"}],
                        "volumeMounts": [{"name": "store", "mountPath": "/data"}]}]}}}}
        self.service = {"spec": {"selector": {"app": "controller"}, "ports": [{"port": 8080}]}}

    def test_additive_patch_preserves_trust_and_excludes_unrelated_values(self):
        patch, info = prepare.controller_patch(self.deployment, self.service, "demo", "manager")
        rendered = json.dumps(patch)
        self.assertNotIn("must-not-be-copied", rendered)
        self.assertEqual(patch["metadata"], {"uid": "controller-uid", "resourceVersion": "7"})
        self.assertEqual(patch["spec"]["template"]["spec"]["containers"][0]["env"][0]["value"],
                         "/existing/ca:" + prepare.CA_PATH)
        self.assertEqual(info["storeClaim"], "db")

    def test_refuses_other_namespace_ephemeral_store_and_foreign_mount(self):
        with self.assertRaisesRegex(ValueError, "namespace"):
            prepare.controller_patch(self.deployment, self.service, "other", "manager")
        ephemeral = copy.deepcopy(self.deployment)
        ephemeral["spec"]["template"]["spec"]["volumes"][0] = {"name": "store", "emptyDir": {}}
        with self.assertRaisesRegex(ValueError, "PVC"):
            prepare.controller_patch(ephemeral, self.service, "demo", "manager")
        self.deployment["spec"]["template"]["spec"]["volumes"].append({"name": "demo-a2a-ca", "secret": {"secretName": "foreign"}})
        with self.assertRaisesRegex(ValueError, "another source"):
            prepare.controller_patch(self.deployment, self.service, "demo", "manager")

    def test_repeat_setup_preserves_the_stored_controller_template(self):
        patch, _ = prepare.controller_patch(self.deployment, self.service, "demo", "manager")
        applied = copy.deepcopy(self.deployment)
        pod = applied["spec"]["template"]["spec"]
        changes = patch["spec"]["template"]["spec"]
        pod["volumes"].extend(changes["volumes"])
        pod["volumes"][-1]["configMap"]["defaultMode"] = 420
        pod["containers"][0]["volumeMounts"].extend(changes["containers"][0]["volumeMounts"])
        pod["containers"][0]["env"] = changes["containers"][0]["env"]
        repeated, _ = prepare.controller_patch(applied, self.service, "demo", "manager")
        self.assertEqual(repeated, patch)
        pod["volumes"][-1]["configMap"]["items"] = [{"key": "other", "path": "ca.crt"}]
        with self.assertRaisesRegex(ValueError, "another source"):
            prepare.controller_patch(applied, self.service, "demo", "manager")


if __name__ == "__main__":
    unittest.main()
