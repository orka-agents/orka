"""Schedule suspension must stay within the demo's owned resources."""

import contextlib
import io
import json
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import cluster


class SuspensionTest(unittest.TestCase):
    def task(self, labels):
        return SimpleNamespace(stdout=json.dumps({"metadata": {
            "uid": "scheduled-task-1", "resourceVersion": "42", "labels": labels,
        }, "spec": {}}))

    def test_unowned_schedule_is_never_patched(self):
        for labels in ({}, {"demo.orka.ai/name": "another-demo"}):
            with self.subTest(labels=labels), patch.object(cluster, "verify"), \
                    patch.object(cluster, "kube", return_value=self.task(labels)) as kube:
                with self.assertRaisesRegex(RuntimeError, "not owned"):
                    cluster.suspend()
                self.assertEqual(kube.call_count, 1)
                self.assertEqual(kube.call_args.args,
                                 ("-n", cluster.RELIABILITY, "get", "task", cluster.SCHEDULE, "-o", "json"))

    def test_owned_schedule_patch_is_fenced_to_the_observed_task(self):
        with patch.object(cluster, "verify"), \
                patch.object(cluster, "kube", return_value=self.task(cluster.LABEL)) as kube, \
                contextlib.redirect_stdout(io.StringIO()):
            cluster.suspend()
        args = kube.call_args.args
        self.assertEqual(args[:7], ("-n", cluster.RELIABILITY, "patch", "task",
                                   cluster.SCHEDULE, "--type=json", "-p"))
        self.assertEqual(json.loads(args[7]), [
            {"op": "test", "path": "/metadata/uid", "value": "scheduled-task-1"},
            {"op": "test", "path": "/metadata/resourceVersion", "value": "42"},
            {"op": "add", "path": "/spec/suspend", "value": True},
        ])


if __name__ == "__main__":
    unittest.main()
