"""Read-only, safe Pod inventory and measured-window reservation allocation.

Snapshots omit environment, commands, annotations, Secrets, and container logs.
Only the explicitly selected demo namespaces are read. Missing lifecycle or Task
identity evidence stops the estimate rather than assigning a zero cost.
"""

from __future__ import annotations

import copy
from datetime import datetime, timezone
from decimal import Decimal, ROUND_CEILING, localcontext
import json
from pathlib import Path
import re
import subprocess

import costs


CONTEXT = "sertac-aks"
NAMESPACES = ("team-payments", "team-payments-runtimes", "orka-efficiency")
APPLICATIONS = {
    ("team-payments", "orka-controller"): "controller",
    ("team-payments", "provider-auth-proxy"): "provider-proxy",
    ("team-payments", "scm-egress-proxy"): "scm-proxy",
    ("team-payments", "efficiency-vekil"): "vekil",
    ("team-payments", "workspace-publisher"): "workspace-publisher",
    ("orka-efficiency", "efficiency-router"): "compatibility-router",
    ("orka-efficiency", "qwen35-2b"): "local-model",
}
LABELS = ("app", "demo.orka.ai/name", "orka.ai/task", "orka.ai/task-type",
          "orka.ai/runtime-pool-uid", "orka.ai/runtime-pool-name", "orka.ai/runtime-pool-namespace")
require = costs.require


def now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def timestamp(value) -> datetime:
    require(isinstance(value, str), "Missing infrastructure timestamp")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise costs.CostError("Invalid infrastructure timestamp") from exc
    require(parsed.tzinfo is not None, "Infrastructure timestamp has no time zone")
    return parsed


def duration(start: datetime, end: datetime) -> Decimal:
    delta = end - start
    return Decimal(delta.days * 86400 + delta.seconds) + Decimal(delta.microseconds) / Decimal(1000000)


def get(kind: str, namespace: str = "") -> dict:
    args = ["kubectl", "--context", CONTEXT, "get", kind, "-o", "json"]
    if namespace:
        args += ["-n", namespace]
    try:
        result = subprocess.run(args, capture_output=True, timeout=45, check=False)
        require(result.returncode == 0, "Infrastructure inventory query failed; raw output withheld")
        return json.loads(result.stdout)
    except (OSError, subprocess.TimeoutExpired, ValueError) as exc:
        raise costs.CostError("Infrastructure inventory query failed; raw output withheld") from exc


def resources(value: dict) -> dict:
    return {key: value[key] for key in ("cpu", "memory") if key in value}


def safe_pod(pod: dict, observed_at: str) -> dict:
    meta, spec, status = pod["metadata"], pod["spec"], pod.get("status", {})

    def container(item):
        return {"name": item["name"], "requests": resources(item.get("resources", {}).get("requests", {})),
                "restartPolicy": item.get("restartPolicy")}

    def state(item):
        states = item.get("state", {})
        return {"name": item["name"], "restartCount": item.get("restartCount"),
                "state": {kind: {key: data[key] for key in ("startedAt", "finishedAt") if key in data}
                          for kind, data in states.items()}}

    scheduled = [x for x in status.get("conditions", []) if x.get("type") == "PodScheduled" and x.get("status") == "True"]
    return {"uid": meta["uid"], "name": meta["name"], "namespace": meta["namespace"],
            "createdAt": meta["creationTimestamp"], "deletionTimestamp": meta.get("deletionTimestamp"),
            "labels": {key: meta.get("labels", {})[key] for key in LABELS if key in meta.get("labels", {})},
            "owners": [{key: owner.get(key) for key in ("kind", "name", "uid", "controller")}
                       for owner in meta.get("ownerReferences", [])],
            "node": spec.get("nodeName"), "scheduledAt": scheduled[0].get("lastTransitionTime") if len(scheduled) == 1 else None,
            "containers": [container(x) for x in spec.get("containers", [])],
            "initContainers": [container(x) for x in spec.get("initContainers", [])],
            "overhead": resources(spec.get("overhead", {})), "podLevelResources": bool(spec.get("resources")),
            "phase": status.get("phase"), "containerStatuses": [state(x) for x in status.get("containerStatuses", [])],
            "initContainerStatuses": [state(x) for x in status.get("initContainerStatuses", [])],
            "observedAt": observed_at}


def snapshot() -> dict:
    """Capture only safe inventory fields on the authorized cluster."""
    started, pods = now(), []
    for namespace in NAMESPACES:
        objects = get("pods", namespace)
        observed = now()
        for pod in objects["items"]:
            labels = pod["metadata"].get("labels", {})
            if ((namespace, labels.get("app")) in APPLICATIONS
                    or namespace == "team-payments-runtimes"
                    or namespace == "team-payments" and labels.get("orka.ai/task-type") == "ai"):
                pods.append(safe_pod(pod, observed))
    nodes = []
    for node in get("nodes")["items"]:
        meta, status = node["metadata"], node["status"]
        labels = meta.get("labels", {})
        nodes.append({"name": meta["name"], "uid": meta["uid"],
                      "instanceType": labels.get("node.kubernetes.io/instance-type"),
                      "region": labels.get("topology.kubernetes.io/region"), "os": labels.get("kubernetes.io/os"),
                      "allocatable": resources(status.get("allocatable", {}))})
    return {"schemaVersion": 1, "context": CONTEXT, "startedAt": started, "finishedAt": now(),
            "pods": pods, "nodes": nodes}


def quantity(value, kind: str) -> Decimal:
    require(isinstance(value, str), "Missing CPU or memory resource quantity")
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)([A-Za-z]*)", value)
    require(match is not None, "Invalid or unsupported resource quantity")
    amount, unit = Decimal(match[1]), match[2]
    factors = {"": Decimal(1), "m": Decimal("0.001")} if kind == "cpu" else {
        "": Decimal(1), **{suffix: Decimal(1024) ** i for i, suffix in enumerate(("Ki", "Mi", "Gi", "Ti", "Pi", "Ei"), 1)},
        **{suffix: Decimal(1000) ** i for i, suffix in enumerate(("k", "M", "G", "T", "P", "E"), 1)}}
    require(unit in factors, "Unsupported CPU or memory resource unit")
    result = amount * factors[unit]
    if kind == "cpu":
        require(result * 1000 == (result * 1000).to_integral_value(), "Sub-millicore CPU request is unsupported")
        return result
    return result.to_integral_value(rounding=ROUND_CEILING)


def pod_requests(pod: dict) -> dict:
    require(not pod.get("podLevelResources"), "Pod-level resource overrides are unsupported")
    regular, initial = pod.get("containers"), pod.get("initContainers")
    require(isinstance(regular, list) and regular and isinstance(initial, list), "Missing container resource inventory")
    require(all(not item.get("restartPolicy") for item in regular + initial), "Restartable init sidecars are unsupported")
    names = [item.get("name") for item in regular + initial]
    require(all(isinstance(name, str) and name for name in names) and len(set(names)) == len(names), "Invalid container identities")
    result = {}
    for kind in ("cpu", "memory"):
        running = sum((quantity(item.get("requests", {}).get(kind), kind) for item in regular), Decimal(0))
        initialization = max((quantity(item.get("requests", {}).get(kind), kind) for item in initial), default=Decimal(0))
        overhead = quantity(pod.get("overhead", {}).get(kind, "0"), kind)
        result[kind] = max(running, initialization) + overhead
        require(result[kind] > 0, "CPU and memory reservations must both be present and positive")
    return result


def interval(history: list[dict], start: datetime, end: datetime) -> tuple[datetime, datetime] | None:
    history = sorted(history, key=lambda row: timestamp(row["observedAt"]))
    latest = history[-1]
    require(latest.get("node") and latest.get("scheduledAt"), "Scheduled Pod lifetime evidence is missing")
    scheduled = timestamp(latest["scheduledAt"])
    require(scheduled >= timestamp(latest["createdAt"]), "Pod scheduling precedes creation")
    for old in history:
        for key in ("uid", "name", "namespace", "createdAt", "labels", "owners", "containers", "initContainers", "overhead", "podLevelResources"):
            require(old.get(key) == latest.get(key), "Pod identity or resource reservation changed during sampling")
        require(not old.get("node") or old["node"] == latest["node"], "Pod node changed during sampling")
        require(not old.get("scheduledAt") or old["scheduledAt"] == latest["scheduledAt"], "Pod scheduling timestamp changed")
    statuses = {row["name"]: row for row in latest.get("containerStatuses", [])}
    if latest.get("phase") in ("Succeeded", "Failed"):
        require(set(statuses) == {row["name"] for row in latest["containers"]}, "Terminal Pod lacks complete container statuses")
        ends = [timestamp(row.get("state", {}).get("terminated", {}).get("finishedAt")) for row in statuses.values()]
        finished = max(ends)
        require(scheduled <= finished <= timestamp(latest["observedAt"]), "Invalid terminated Pod lifetime")
    else:
        require(timestamp(latest["observedAt"]) >= end, "Pod disappeared before its reservation end was observed")
        finished = end
    left, right = max(start, scheduled), min(end, finished)
    return (left, right) if left < right else None


def read(path: Path) -> dict:
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError) as exc:
        raise costs.CostError("Missing or invalid infrastructure evidence: " + str(path)) from exc


def calculate(root: Path, verified_report: dict) -> dict:
    """Build cost inputs, binding selected worker Pods to saved successful Tasks."""
    root = Path(root)
    jobs = costs.mapping(verified_report.get("jobs"), "verified jobs")
    require("engineering" in jobs and len(jobs) > 1, "Missing engineering or support jobs")
    result = {}
    for phase in costs.PHASES:
        directory = root / phase
        period = read(directory / "phase.json")
        require(period == verified_report["phases"][phase].get("period"), "Phase timing differs from the verified report")
        start, end = timestamp(period.get("startedAt")), timestamp(period.get("finishedAt"))
        require(start < end, "Invalid infrastructure measurement window")
        paths = [directory / "infrastructure-start.json", *[directory / job / "infrastructure.json" for job in jobs],
                 directory / "infrastructure-end.json"]
        samples = [read(path) for path in paths]
        histories, nodes = {}, {}
        for sample in samples:
            require(sample.get("schemaVersion") == 1 and sample.get("context") == CONTEXT, "Wrong infrastructure snapshot target or schema")
            require(timestamp(sample["startedAt"]) <= timestamp(sample["finishedAt"]), "Reversed infrastructure snapshot interval")
            for node in sample["nodes"]:
                previous = nodes.setdefault(node["name"], node)
                require(previous == node, "Node identity or allocatable capacity changed during measurement")
            seen = set()
            for pod in sample["pods"]:
                require(pod["uid"] not in seen and pod["namespace"] in NAMESPACES, "Duplicate or out-of-scope Pod in snapshot")
                seen.add(pod["uid"])
                require(timestamp(sample["startedAt"]) <= timestamp(pod["observedAt"]) <= timestamp(sample["finishedAt"]), "Pod observation is outside its snapshot")
                histories.setdefault(pod["uid"], []).append(pod)
        tasks = {}
        for job in jobs:
            task = read(directory / job / "task.json")
            require(task["metadata"]["uid"] == jobs[job][phase]["taskUID"] and task.get("status", {}).get("phase") == "Succeeded",
                    "Infrastructure Task identity does not match the verified result")
            tasks[job] = task
        native = {task["status"].get("jobUID"): task["metadata"]["name"] for job, task in tasks.items() if job != "engineering"}
        require(None not in native and "" not in native and len(native) == len(tasks) - 1, "Missing or duplicate native Job UID")
        execution = tasks["engineering"]["status"].get("execution", {})
        pool_uid, pool_name = execution.get("runtimePoolUID"), execution.get("runtimePoolName")
        require(pool_uid and pool_name, "Engineering Task lacks its runtime pool identity")
        selected, matched_jobs, matched_pool = [], set(), False
        for uid, history in histories.items():
            pod = max(history, key=lambda item: timestamp(item["observedAt"]))
            span = interval(history, start, end)
            if span is None:
                continue
            namespace, labels = pod["namespace"], pod["labels"]
            component = APPLICATIONS.get((namespace, labels.get("app")))
            if component:
                require(labels.get("demo.orka.ai/name") == "efficiency", "Common service Pod is not demo-owned")
            elif namespace == "team-payments" and labels.get("orka.ai/task-type") == "ai":
                owners = [owner for owner in pod["owners"] if owner["kind"] == "Job" and owner["controller"] is True]
                require(len(owners) == 1 and owners[0]["uid"] in native
                        and labels.get("orka.ai/task") == native[owners[0]["uid"]], "Unassociated native worker overlaps the measured phase")
                matched_jobs.add(owners[0]["uid"])
                component = "native-worker"
            elif namespace == "team-payments-runtimes":
                require(labels.get("orka.ai/runtime-pool-namespace") == "team-payments"
                        and str(labels.get("orka.ai/runtime-pool-name", "")).startswith("acp-codex-"), "Unpriced or unowned runtime Pod")
                if labels.get("orka.ai/runtime-pool-uid") == pool_uid:
                    require(labels["orka.ai/runtime-pool-name"] == pool_name, "Runtime pool name and UID disagree")
                    matched_pool = True
                component = "codex-runtime"
            else:
                raise costs.CostError("Unknown infrastructure component in selected namespace")
            selected.append((component, uid, pod, span))
        require(matched_jobs == set(native), "A support Task has no measured Job Pod")
        require(matched_pool, "Engineering Task has no measured runtime Pod")
        expected = {name: sum(component == name for component, *_ in selected) for name in costs.COMPONENTS}
        entries = []
        with localcontext() as context:
            context.prec = 50
            for component, uid, pod, (left, right) in selected:
                node = nodes.get(pod["node"])
                require(node is not None, "Missing node allocation evidence")
                reference = costs.RATE_CARD["nodeReference"]
                require((node["instanceType"], node["region"], node["os"]) ==
                        (reference["sku"], reference["region"], reference["operatingSystem"].lower()), "Node has no verified Linux retail price")
                reserved = pod_requests(pod)
                allocatable = {kind: quantity(node.get("allocatable", {}).get(kind), kind) for kind in ("cpu", "memory")}
                require(all(0 < reserved[kind] <= allocatable[kind] for kind in reserved), "Pod reservation exceeds its node's allocatable capacity")
                entries.append({"component": component, "resource": f"{pod['namespace']}/{pod['name']}@{uid}",
                    "seconds": format(duration(left, right), "f"), "nodeHourlyUsd": reference["retailHourlyUsd"],
                    "cpuShare": format(reserved["cpu"] / allocatable["cpu"], "f"),
                    "memoryShare": format(reserved["memory"] / allocatable["memory"], "f"),
                    "podUID": uid, "nodeUID": node["uid"], "startedAt": left.isoformat(), "finishedAt": right.isoformat(),
                    "requested": {key: format(value, "f") for key, value in reserved.items()},
                    "allocatable": {key: format(value, "f") for key, value in allocatable.items()}})
        result[phase] = {"expectedCounts": expected, "entries": entries,
                         "selectedPodUIDs": sorted(uid for _, uid, *_ in selected), "period": copy.deepcopy(period)}
        costs.infrastructure_cost(result[phase], phase)
    return result
