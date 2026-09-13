#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
bash -n "$repo_root/examples/fibey-custom-agent-demo/switch-backend.sh"

python3 - "$repo_root" <<'PY'
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(sys.argv.pop())
SCRIPT = ROOT / "examples/fibey-custom-agent-demo/switch-backend.sh"

# Model kubectl's public JSON interface. The repository's manifest tests cover
# YAML decoding; this fixture isolates CREATE and immutable-binding behavior.
KUBECTL = r'''#!/usr/bin/env python3
import copy
import json
import os
from pathlib import Path
import sys

directory = Path(os.environ["FIBEY_TEST_DIRECTORY"])
args = sys.argv[1:]
with (directory / "calls.jsonl").open("a") as log:
    log.write(json.dumps(args) + "\n")
required = ["--context=demo-context", "--namespace=demo-namespace", "--request-timeout=30s"]
if args[:3] != required:
    raise SystemExit("Every request must select the explicit context and namespace")
args = args[3:]
fixtures = json.loads((directory / "fixtures.json").read_text())
scenario = os.environ.get("FIBEY_TEST_SCENARIO", "success")

if args[:2] == ["get", "namespace"]:
    value = fixtures["namespace"]
elif args[:2] == ["get", "agent"]:
    value = fixtures["agent"]
elif args[:2] == ["get", "agentruntime"]:
    value = fixtures["runtime"]
elif args[:2] == ["create", "--dry-run=client"]:
    if Path(args[args.index("-f") + 1]).name != "task.yaml":
        raise SystemExit("Expected the shared task.yaml template")
    value = fixtures["template"]
elif args == ["create", "-f", "-", "-o", "json"]:
    request = json.load(sys.stdin)
    (directory / "submitted.json").write_text(json.dumps(request))
    if scenario in ("already-exists", "ambiguous-create"):
        raise SystemExit("CREATE failed")
    value = copy.deepcopy(request)
    value["metadata"].update(uid="created-task-uid", generation=1)
    (directory / "created.json").write_text(json.dumps(value))
elif args[0] == "wait":
    if scenario == "binding-timeout":
        raise SystemExit("Wait timed out")
    raise SystemExit(0)
elif args[:2] == ["get", "task"]:
    if scenario == "binding-get-failed":
        raise SystemExit("Task GET timed out")
    value = json.loads((directory / "created.json").read_text())
    agent = fixtures["agent"]["metadata"]
    runtime = fixtures["runtime"]
    binding = {
        "contractVersion": "orka.harness.v2",
        "backend": "external-endpoint",
        "task": {"uid": value["metadata"]["uid"], "boundSpecGeneration": 1,
                 "namespaceUID": fixtures["namespace"]["metadata"]["uid"]},
        "agent": {key: agent[key] for key in ("name", "namespace", "uid", "generation")},
        "runtimeRef": {key: runtime["metadata"][key] for key in ("name", "uid", "generation")},
        "runtimeProfileDigest": runtime["spec"]["capabilities"]["profile"]["digest"],
    }
    drift = {
        "agent-recreated": (binding["agent"], "uid", "different-agent"),
        "agent-edited": (binding["agent"], "generation", 4),
        "runtime-recreated": (binding["runtimeRef"], "uid", "different-runtime"),
        "runtime-edited": (binding["runtimeRef"], "generation", 4),
        "profile-drift": (binding, "runtimeProfileDigest", "sha256:" + "b" * 64),
        "namespace-recreated": (binding["task"], "namespaceUID", "different-namespace"),
        "task-recreated": (value["metadata"], "uid", "different-task"),
        "wrong-task-binding": (binding["task"], "uid", "different-task"),
        "wrong-task-generation": (binding["task"], "boundSpecGeneration", 2),
        "wrong-contract-binding": (binding, "contractVersion", "orka.harness.v1"),
        "wrong-backend-binding": (binding, "backend", "runtime-pool"),
    }
    if scenario in drift:
        obj, key, replacement = drift[scenario]
        obj[key] = replacement
    value["status"] = {"agentExecutionBinding": binding}
else:
    raise SystemExit("Unexpected command: " + repr(args))
print(json.dumps(value))
'''


class SubmissionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="fibey-v2-test-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        executable = self.directory / "kubectl"
        executable.write_text(KUBECTL)
        executable.chmod(0o755)
        self.environment = dict(os.environ, PATH=f"{self.directory}:{os.environ['PATH']}",
                                FIBEY_TEST_DIRECTORY=str(self.directory))

    def fixtures(self, backend="agentkit"):
        runtime_name = f"fibey-{backend}-runtime"
        profile = {"digest": "sha256:" + "a" * 64, "providerKind": backend,
                   "adapterName": f"{backend}-serve-acp", "workspaceIntent": "read"}
        governance = {name: True for name in (
            "orkaOwnedWorkspaceDeltas", "promptScopedBrokerAuthorization",
            "noDirectSCMPublication", "orkaOwnedCleanRoomPublication",
            "exactInstanceFencing", "duplicateSafeMutations", "cancellationSettlement")}
        governance.update(mode="strict-governed", trusted=False)
        return {
            "namespace": {"metadata": {"name": "demo-namespace", "uid": "namespace-uid",
                           "labels": {"orka.ai/controller-mode": "harness-v2"}}},
            "agent": {"metadata": {"name": f"fibey-remote-{backend}", "namespace": "demo-namespace",
                       "uid": "agent-uid", "generation": 3},
                      "spec": {"runtime": {"runtimeRef": {"name": runtime_name}}}},
            "runtime": {"metadata": {"name": runtime_name, "uid": "runtime-uid", "generation": 3},
                        "spec": {"contractVersion": "orka.harness.v2", "capabilities": {
                            "runtimeInstanceID": "runtime-instance", "profile": profile,
                            "mcpPolicy": {"allowedTools": [], "disallowedTools": [], "allowBash": False,
                                          "approvalRequiredTools": []}, "workspaceGovernance": governance}},
                        "status": {"ready": True, "observedGeneration": 3, "observedCapabilities": {
                            "runtimeInstanceID": "runtime-instance", "runtimeProfileDigest": profile["digest"]}}},
            "template": {"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
                         "metadata": {"name": "template-name", "labels": {"preserve": "this"}},
                         "spec": {"type": "agent", "agentRef": {"name": "template-agent"},
                                  "prompt": "Preserve the shared incident evidence.\n",
                                  "timeout": "5m", "workspace": {"intent": "read"},
                                  "agentRuntime": {"allowedTools": []}}},
        }

    def run_demo(self, fixtures, backend="agentkit", scenario="success", arguments=None):
        (self.directory / "fixtures.json").write_text(json.dumps(fixtures))
        for name in ("calls.jsonl", "submitted.json", "created.json"):
            (self.directory / name).unlink(missing_ok=True)
        args = arguments if arguments is not None else [backend, "demo-context", "demo-namespace", "fibey-run-01"]
        return subprocess.run(["bash", str(SCRIPT), *args], text=True, capture_output=True,
                              env=dict(self.environment, FIBEY_TEST_SCENARIO=scenario), timeout=15)

    def calls(self):
        path = self.directory / "calls.jsonl"
        return [json.loads(line)[3:] for line in path.read_text().splitlines()] if path.exists() else []

    def mutations(self):
        return [args for args in self.calls() if args[0] not in ("get", "wait")
                and "--dry-run=client" not in args]

    def test_both_backends_preserve_input_and_confirm_binding(self):
        for backend in ("agentkit", "foundry"):
            with self.subTest(backend=backend):
                fixtures = self.fixtures(backend)
                result = self.run_demo(fixtures, backend)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), "task/fibey-run-01")
                self.assertIn("Inference may still be running", result.stderr)
                expected = copy.deepcopy(fixtures["template"])
                expected["metadata"].update(name="fibey-run-01", namespace="demo-namespace")
                expected["spec"]["agentRef"]["name"] = f"fibey-remote-{backend}"
                self.assertEqual(json.loads((self.directory / "submitted.json").read_text()), expected)
                self.assertEqual(self.mutations(), [["create", "-f", "-", "-o", "json"]])

    def test_invalid_arguments_do_not_contact_cluster(self):
        for args in ([], ["agentkit"], ["http", "demo-context", "demo-namespace", "new"],
                     ["agentkit", "", "demo-namespace", "new"]):
            with self.subTest(args=args):
                result = self.run_demo(self.fixtures(), arguments=args)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls(), [])

    def test_preflight_rejects_unready_or_incompatible_runtime(self):
        cases = (
            ("v1 namespace", ("namespace", "metadata", "labels", "orka.ai/controller-mode"), "harness-v1"),
            ("deleting namespace", ("namespace", "metadata", "deletionTimestamp"), "2026-09-08T00:00:00Z"),
            ("wrong Agent mapping", ("agent", "spec", "runtime", "runtimeRef", "name"), "different-runtime"),
            ("deleting Agent", ("agent", "metadata", "deletionTimestamp"), "2026-09-08T00:00:00Z"),
            ("v1 runtime", ("runtime", "spec", "contractVersion"), "orka.harness.v1"),
            ("not Ready", ("runtime", "status", "ready"), False),
            ("stale Ready", ("runtime", "status", "observedGeneration"), 2),
            ("deleting runtime", ("runtime", "metadata", "deletionTimestamp"), "2026-09-08T00:00:00Z"),
            ("changed instance", ("runtime", "status", "observedCapabilities", "runtimeInstanceID"), "old-instance"),
            ("changed profile", ("runtime", "status", "observedCapabilities", "runtimeProfileDigest"), "old-digest"),
            ("wrong provider", ("runtime", "spec", "capabilities", "profile", "providerKind"), "foundry"),
            ("wrong adapter", ("runtime", "spec", "capabilities", "profile", "adapterName"), "different-adapter"),
            ("write intent", ("runtime", "spec", "capabilities", "profile", "workspaceIntent"), "write"),
            ("tools enabled", ("runtime", "spec", "capabilities", "mcpPolicy", "allowedTools"), ["tool"]),
            ("Bash enabled", ("runtime", "spec", "capabilities", "mcpPolicy", "allowBash"), True),
            ("approvals enabled", ("runtime", "spec", "capabilities", "mcpPolicy", "approvalRequiredTools"), ["tool"]),
            ("trusted mode", ("runtime", "spec", "capabilities", "workspaceGovernance", "trusted"), True),
            ("no cancellation settlement", ("runtime", "spec", "capabilities", "workspaceGovernance", "cancellationSettlement"), False),
        )
        for name, path, replacement in cases:
            with self.subTest(case=name):
                fixtures = self.fixtures()
                target = fixtures
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = replacement
                result = self.run_demo(fixtures)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.mutations(), [])

    def test_failed_create_never_retries_or_replaces(self):
        for scenario in ("already-exists", "ambiguous-create"):
            with self.subTest(scenario=scenario):
                result = self.run_demo(self.fixtures(), scenario=scenario)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Inspect Task fibey-run-01", result.stderr)
                self.assertEqual(self.mutations(), [["create", "-f", "-", "-o", "json"]])
                self.assertFalse(any(args[0] == "wait" for args in self.calls()))

    def test_binding_read_failures_leave_the_submitted_task(self):
        for scenario in ("binding-timeout", "binding-get-failed"):
            with self.subTest(scenario=scenario):
                result = self.run_demo(self.fixtures(), scenario=scenario)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("do not resubmit", result.stderr)
                self.assertEqual(self.mutations(), [["create", "-f", "-", "-o", "json"]])

    def test_changed_binding_is_not_reported_as_success(self):
        for scenario in ("agent-recreated", "agent-edited", "runtime-recreated", "runtime-edited",
                         "profile-drift", "namespace-recreated", "task-recreated", "wrong-task-binding",
                         "wrong-task-generation", "wrong-contract-binding", "wrong-backend-binding"):
            with self.subTest(scenario=scenario):
                result = self.run_demo(self.fixtures(), scenario=scenario)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("different binding", result.stderr)
                self.assertEqual(self.mutations(), [["create", "-f", "-", "-o", "json"]])


unittest.main(verbosity=2)
PY
