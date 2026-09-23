import copy
from datetime import datetime, timedelta, timezone
from decimal import Decimal
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import costs
import infrastructure as infra


BASE = datetime(2026, 9, 22, 12, tzinfo=timezone.utc)


def at(seconds):
    return (BASE + timedelta(seconds=seconds)).isoformat().replace("+00:00", "Z")


def container(name="worker", cpu="100m", memory="256Mi"):
    return {"name": name, "requests": {"cpu": cpu, "memory": memory}, "restartPolicy": None}


def pod(uid="pod", namespace="team-payments", *, scheduled=0, observed=130, finished=None,
        labels=None, owners=None):
    state = {"running": {"startedAt": at(scheduled + 1)}} if finished is None else {
        "terminated": {"startedAt": at(scheduled + 1), "finishedAt": at(finished)}}
    return {"uid": uid, "name": uid, "namespace": namespace, "createdAt": at(scheduled),
            "deletionTimestamp": None, "labels": labels or {}, "owners": owners or [],
            "node": "node-1", "scheduledAt": at(scheduled), "containers": [container()],
            "initContainers": [], "overhead": {}, "podLevelResources": False,
            "phase": "Running" if finished is None else "Succeeded",
            "containerStatuses": [{"name": "worker", "restartCount": 0, "state": state}],
            "initContainerStatuses": [], "observedAt": at(observed)}


def node():
    return {"name": "node-1", "uid": "node-uid", "instanceType": "Standard_DS2_v2",
            "region": "westus2", "os": "linux", "allocatable": {"cpu": "1900m", "memory": "5Gi"}}


def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value))


def fixture(root):
    report = {"jobs": {key: {} for key in ("support-01", "support-02", "engineering")}, "phases": {}}
    for phase in costs.PHASES:
        directory = root / phase
        period = {"phase": phase, "startedAt": at(60), "finishedAt": at(120)}
        save(directory / "phase.json", period)
        report["phases"][phase] = {"period": period}
        common = [pod(component, namespace, labels={"app": app, "demo.orka.ai/name": "efficiency"})
                  for (namespace, app), component in infra.APPLICATIONS.items()]
        old = pod("old-worker", finished=50, labels={"orka.ai/task-type": "ai"})
        workers = []
        for index, job in enumerate(report["jobs"]):
            task = {"metadata": {"uid": phase + "-" + job, "name": phase + "-task-" + job,
                                 "namespace": "team-payments"}, "status": {"phase": "Succeeded"}}
            if job != "engineering":
                task["status"]["jobUID"] = phase + "-job-" + job
                workers.append(pod(phase + "-pod-" + job, scheduled=70 + 15 * index, finished=80 + 15 * index,
                    labels={"orka.ai/task-type": "ai", "orka.ai/task": task["metadata"]["name"]},
                    owners=[{"kind": "Job", "uid": task["status"]["jobUID"], "name": job, "controller": True}]))
            else:
                task["status"]["execution"] = {"runtimePoolUID": phase + "-pool-uid", "runtimePoolName": "acp-codex-demo"}
                workers.append(pod(phase + "-runtime", "team-payments-runtimes", scheduled=105,
                    labels={"orka.ai/runtime-pool-uid": phase + "-pool-uid", "orka.ai/runtime-pool-name": "acp-codex-demo",
                            "orka.ai/runtime-pool-namespace": "team-payments"}))
            save(directory / job / "task.json", task)
            report["jobs"][job][phase] = {"taskUID": task["metadata"]["uid"]}
        for relative, observed in (("infrastructure-start.json", 61), ("support-01/infrastructure.json", 81),
                                   ("support-02/infrastructure.json", 96), ("engineering/infrastructure.json", 115),
                                   ("infrastructure-end.json", 130)):
            selected = [copy.deepcopy(item) for item in common + [old] + workers
                        if infra.timestamp(item["scheduledAt"]) < infra.timestamp(at(observed))]
            for item in selected:
                item["observedAt"] = at(observed)
            save(directory / relative, {"schemaVersion": 1, "context": infra.CONTEXT,
                 "startedAt": at(observed - 0.1), "finishedAt": at(observed + 0.1),
                 "pods": selected, "nodes": [node()]})
    return report


class ProjectionTests(unittest.TestCase):
    def test_safe_projection_drops_secrets_commands_and_status_messages(self):
        private = "SHOULD-NOT-BE-SAVED"
        source = {"metadata": {"uid": "uid", "name": "pod", "namespace": "team-payments",
                    "creationTimestamp": at(0), "annotations": {"example": private},
                    "labels": {"app": "orka-controller", "private": private}},
                  "spec": {"nodeName": "node-1", "containers": [{"name": "main", "image": private,
                    "command": [private], "env": [{"name": "TOKEN", "value": private}],
                    "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}}}]},
                  "status": {"phase": "Running", "message": private,
                    "conditions": [{"type": "PodScheduled", "status": "True", "lastTransitionTime": at(1)}],
                    "containerStatuses": [{"name": "main", "restartCount": 0,
                       "state": {"running": {"startedAt": at(2), "message": private}}}]}}
        result = infra.safe_pod(source, at(3))
        self.assertNotIn(private, json.dumps(result))
        self.assertEqual(result["labels"], {"app": "orka-controller"})
        self.assertEqual(result["containers"][0]["requests"], {"cpu": "100m", "memory": "128Mi"})
        self.assertEqual(result["scheduledAt"], at(1))

    def test_snapshot_queries_only_explicit_namespaces_and_node_sizing(self):
        queries = []

        def get(kind, namespace=""):
            queries.append((kind, namespace))
            if kind == "nodes":
                return {"items": [{"metadata": {"name": "node-1", "uid": "node-uid", "labels": {
                    "node.kubernetes.io/instance-type": "Standard_DS2_v2", "topology.kubernetes.io/region": "westus2",
                    "kubernetes.io/os": "linux"}}, "status": {"allocatable": {"cpu": "1900m", "memory": "5Gi"}}}]}
            return {"items": []}

        with patch.object(infra, "get", side_effect=get):
            result = infra.snapshot()
        self.assertEqual(queries, [("pods", namespace) for namespace in infra.NAMESPACES] + [("nodes", "")])
        self.assertEqual(result["nodes"], [node()])
        self.assertEqual(result["context"], "sertac-aks")


class ReservationTests(unittest.TestCase):
    def test_resource_quantities_use_cpu_cores_and_memory_bytes(self):
        for value, expected in (("50m", "0.05"), ("2", "2"), ("0.125", "0.125")):
            self.assertEqual(infra.quantity(value, "cpu"), Decimal(expected))
        for value, expected in (("256Mi", 268435456), ("1Gi", 1073741824), ("1M", 1000000), ("1.5", 2)):
            self.assertEqual(infra.quantity(value, "memory"), expected)
        for value in (None, 1, True, "NaN", "Infinity", "-1", "", "1e3", "1unknown"):
            for kind in ("cpu", "memory"):
                with self.subTest(value=value, kind=kind), self.assertRaises(costs.CostError):
                    infra.quantity(value, kind)
        with self.assertRaises(costs.CostError):
            infra.quantity("0.0001", "cpu")

    def test_regular_sum_maximum_init_and_overhead(self):
        value = pod()
        value["containers"] = [container("main", "100m", "128Mi"), container("side", "50m", "64Mi")]
        value["initContainers"] = [container("clone", "500m", "64Mi"), container("setup", "200m", "256Mi")]
        value["overhead"] = {"cpu": "10m", "memory": "8Mi"}
        self.assertEqual(infra.pod_requests(value), {"cpu": Decimal("0.510"), "memory": Decimal(264 * 1024 * 1024)})

    def test_missing_requests_and_unsupported_reservation_models_fail(self):
        changes = [lambda v: v["containers"][0]["requests"].pop("memory"),
                   lambda v: v["containers"][0]["requests"].update(cpu="NaN"),
                   lambda v: v["containers"].append(copy.deepcopy(v["containers"][0])),
                   lambda v: v.update(podLevelResources=True),
                   lambda v: v["initContainers"].append({**container("init"), "restartPolicy": "Always"}),
                   lambda v: v["containers"][0]["requests"].update(cpu="0")]
        for change in changes:
            value = pod()
            change(value)
            with self.subTest(change=change), self.assertRaises(costs.CostError):
                infra.pod_requests(value)


class LifetimeTests(unittest.TestCase):
    def test_running_idle_capacity_and_terminal_lifetimes_are_clipped(self):
        start, end = infra.timestamp(at(60)), infra.timestamp(at(120))
        self.assertEqual(infra.interval([pod()], start, end), (start, end))
        self.assertEqual(infra.interval([pod(scheduled=70, finished=90)], start, end),
                         (infra.timestamp(at(70)), infra.timestamp(at(90))))
        self.assertEqual(infra.interval([pod(scheduled=70, finished=125)], start, end),
                         (infra.timestamp(at(70)), end))
        self.assertIsNone(infra.interval([pod(finished=50)], start, end))

    def test_running_pod_disappearance_cannot_be_guessed(self):
        with self.assertRaisesRegex(costs.CostError, "disappeared"):
            infra.interval([pod(observed=100)], infra.timestamp(at(60)), infra.timestamp(at(120)))

    def test_terminal_status_and_timestamp_gaps_fail(self):
        changes = [lambda v: v.update(containerStatuses=[]),
                   lambda v: v["containerStatuses"][0]["state"]["terminated"].pop("finishedAt"),
                   lambda v: v.update(scheduledAt=None),
                   lambda v: v.update(createdAt=at(100)),
                   lambda v: v.update(observedAt=at(80))]
        for change in changes:
            value = pod(scheduled=70, finished=90)
            change(value)
            with self.subTest(change=change), self.assertRaises(costs.CostError):
                infra.interval([value], infra.timestamp(at(60)), infra.timestamp(at(120)))

    def test_resource_node_and_identity_drift_fail(self):
        changes = [lambda v: v["containers"][0]["requests"].update(cpu="200m"),
                   lambda v: v.update(node="different-node"),
                   lambda v: v["labels"].update(app="different"),
                   lambda v: v.update(scheduledAt=at(10))]
        for change in changes:
            early, late = pod(observed=80), pod(observed=130)
            change(late)
            with self.subTest(change=change), self.assertRaises(costs.CostError):
                infra.interval([early, late], infra.timestamp(at(60)), infra.timestamp(at(120)))


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.report = fixture(self.root)

    def mutate(self, relative, change):
        path = self.root / "baseline" / relative
        value = json.loads(path.read_text())
        change(value)
        save(path, value)

    def test_full_inventory_binds_every_task_and_includes_idle_services(self):
        result = infra.calculate(self.root, self.report)
        for phase, value in result.items():
            self.assertEqual(value["expectedCounts"]["native-worker"], 2)
            self.assertEqual(value["expectedCounts"]["codex-runtime"], 1)
            self.assertEqual(len(value["entries"]), 10)
            self.assertEqual(len(value["selectedPodUIDs"]), 10)
            for entry in value["entries"]:
                expected_seconds = "10" if entry["component"] == "native-worker" else (
                    "15" if entry["component"] == "codex-runtime" else "60")
                self.assertEqual(entry["seconds"], expected_seconds)
                self.assertEqual(Decimal(entry["memoryShare"]), Decimal("0.05"))
                self.assertEqual(entry["nodeHourlyUsd"], "0.114")
                self.assertNotIn("old-worker", entry["resource"])
            self.assertEqual(value["period"], self.report["phases"][phase]["period"])
        json.dumps(result, allow_nan=False)

    def test_native_job_uid_and_task_name_must_both_match(self):
        path = "support-01/task.json"
        self.mutate(path, lambda v: v["status"].update(jobUID="wrong-job-uid"))
        with self.assertRaisesRegex(costs.CostError, "Unassociated native worker"):
            infra.calculate(self.root, self.report)
        self.report = fixture(self.root)
        self.mutate(path, lambda v: v["metadata"].update(name="different-task"))
        with self.assertRaisesRegex(costs.CostError, "Unassociated native worker"):
            infra.calculate(self.root, self.report)

    def test_runtime_pool_uid_and_name_must_both_match(self):
        for changes in ({"runtimePoolUID": "wrong"}, {"runtimePoolName": "wrong"}):
            self.report = fixture(self.root)
            self.mutate("engineering/task.json", lambda v: v["status"]["execution"].update(changes))
            with self.subTest(changes=changes), self.assertRaises(costs.CostError):
                infra.calculate(self.root, self.report)

    def test_missing_task_worker_or_common_component_fails(self):
        for identity in ("baseline-pod-support-01", "baseline-runtime", "provider-proxy"):
            self.report = fixture(self.root)
            for path in (self.root / "baseline").rglob("infrastructure*.json"):
                value = json.loads(path.read_text())
                value["pods"] = [item for item in value["pods"] if item["uid"] != identity]
                save(path, value)
            with self.subTest(identity=identity), self.assertRaises(costs.CostError):
                infra.calculate(self.root, self.report)

    def test_missing_snapshot_or_phase_time_mismatch_fails(self):
        path = self.root / "baseline" / "support-02" / "infrastructure.json"
        path.unlink()
        with self.assertRaises(costs.CostError):
            infra.calculate(self.root, self.report)
        self.report = fixture(self.root)
        self.report["phases"]["baseline"]["period"] = {"startedAt": at(50), "finishedAt": at(120)}
        with self.assertRaisesRegex(costs.CostError, "timing differs"):
            infra.calculate(self.root, self.report)

    def test_missing_node_unknown_price_and_allocation_drift_fail(self):
        for change in (lambda v: v.update(nodes=[]),
                       lambda v: v["nodes"][0].update(instanceType="unpriced-size"),
                       lambda v: v["nodes"][0]["allocatable"].update(cpu="50m")):
            self.report = fixture(self.root)
            for path in (self.root / "baseline").rglob("infrastructure*.json"):
                self.mutate(path.relative_to(self.root / "baseline"), change)
            with self.subTest(change=change), self.assertRaises(costs.CostError):
                infra.calculate(self.root, self.report)
        self.report = fixture(self.root)
        self.mutate("infrastructure-end.json", lambda v: v["nodes"][0]["allocatable"].update(memory="6Gi"))
        with self.assertRaisesRegex(costs.CostError, "capacity changed"):
            infra.calculate(self.root, self.report)

    def test_wrong_context_duplicate_pod_and_invalid_observation_fail(self):
        for change in (lambda v: v.update(context="wrong-cluster"),
                       lambda v: v["pods"].append(copy.deepcopy(v["pods"][0])),
                       lambda v: v["pods"][0].update(observedAt=at(1000))):
            self.report = fixture(self.root)
            self.mutate("infrastructure-end.json", change)
            with self.subTest(change=change), self.assertRaises(costs.CostError):
                infra.calculate(self.root, self.report)


if __name__ == "__main__":
    unittest.main()
