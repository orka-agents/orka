#!/usr/bin/env python3
"""Prepare demo 12 without exposing or replacing the operator's credentials.

Requires kubectl and PyYAML. Rendering is offline; only ``apply`` and ``mode``
change the cluster. Capture gateway evidence before changing its mode: Vekil's
statistics are process-local. The current namespace policy replaces a retired
webhook only for the four new demo namespaces. No command deletes resources.
"""

from __future__ import annotations

import argparse
import base64
import copy
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time
from decimal import Decimal
from urllib.parse import urlsplit

import yaml


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
MANIFESTS = HERE / "manifests"
STATE = ROOT / "bin/efficiency-production/setup.json"
TEAMS = ("team-payments", "team-inventory")
NAMESPACES = (*TEAMS, *(team + "-runtimes" for team in TEAMS), "orka-efficiency")
MANAGER = "orka-efficiency-demo"
OWNER_KEY = "demo.orka.ai/name"
OWNER = "efficiency"
MODEL_NODE = "aks-nodepool1-19390438-vmss000004"
IMAGES = {
    "CONTROLLER_IMAGE": "docker.io/sozercan/orka@sha256:9cfb6bfe463f68f99b5212a73110b4dbeafc1c12635d1f15ab76e0d24b4bcf60",
    "AI_WORKER_IMAGE": "docker.io/sozercan/orka-ai-worker@sha256:54ac3827593f0671e01c0f68ed00617e6852f748fb4e1a7d0eb63e48eb33b4ff",
    "GENERAL_WORKER_IMAGE": "docker.io/sozercan/orka-general-worker@sha256:ebebb9088c52c5a3d6ef192ea26c0173c84ff8bacf81d13ae1ebb33a3acf9be4",
    "VEKIL_IMAGE": "docker.io/sozercan/vekil@sha256:fb9390bae2bb891635e7c7b3750eeaa653416e5a450c85033aa4e7d5ba991afe",
    "AIKIT_IMAGE": "docker.io/sozercan/aikit-qwen35-2b@sha256:0d8d8b0838bcaa808532d3a690d19f9e9095145e29bcb23369133b26078ed234",
}
PROTECTED = (
    ("Deployment", "vekil-system", "vekil"),
    ("PersistentVolumeClaim", "vekil-system", "vekil-token-cache"),
    ("Secret", "vekil-system", "vekil-copilot-github-token"),
)


class SetupError(Exception):
    """Only deliberately safe messages may be passed to this exception."""


class KubernetesError(SetupError):
    def __init__(self, operation: str, conflict: bool = False):
        super().__init__(f"Kubernetes operation failed: {operation}; raw output withheld")
        self.conflict = conflict


def digest(value: object) -> str:
    encoded = value if isinstance(value, bytes) else json.dumps(value, sort_keys=True).encode()
    return hashlib.sha256(encoded).hexdigest()


def message(text: str) -> None:
    print(text, flush=True)


def load_yaml(text: str, description: str) -> object:
    try:
        return yaml.safe_load(text)
    except yaml.YAMLError:
        raise SetupError(f"Cannot parse {description}; source contents withheld") from None


def source_config(path: Path) -> tuple[dict, bytes, dict]:
    try:
        raw = path.read_bytes()
        mode = path.stat().st_mode & 0o777
        source = load_yaml(raw.decode(), "providers source")
        candidates = [p for p in source["providers"] if p.get("id") == "vercel-jev"]
        if len(candidates) != 1:
            raise ValueError
        provider = candidates[0]
        if provider.get("type") != "typesafe-compatible":
            raise ValueError
        endpoint = urlsplit(provider["base_url"])
        if (endpoint.scheme != "https" or not endpoint.hostname or endpoint.username
                or endpoint.password or endpoint.query or endpoint.fragment):
            raise ValueError
        key = provider.get("api_key")
        if not key and provider.get("api_key_env"):
            key = os.environ.get(provider["api_key_env"])
        if not isinstance(key, str) or not key.strip():
            raise ValueError
        routes = [r for r in source["model_routes"]
                  if r.get("internal_purpose") == "policy_classifier"
                  and len(r.get("targets", [])) == 1
                  and r["targets"][0].get("provider") == "vercel-jev"]
        if len(routes) != 1 or not routes[0]["targets"][0].get("upstream_model"):
            raise ValueError
    except (OSError, UnicodeError, AttributeError, KeyError, TypeError, ValueError):
        raise SetupError("Providers source must contain one configured vercel-jev classifier and credential") from None

    config = load_yaml((MANIFESTS / "providers.yaml").read_text(), "demo provider template")
    target = next(p for p in config["providers"] if p["id"] == "vercel-jev")
    # Copy only supported metadata. In particular, never retain the source key.
    for name in ("base_url", "trust_domain", "systemone_path", "auth_type", "auth_header", "auth_prefix"):
        if name in provider:
            target[name] = provider[name]
    target["api_key_env"] = "JEV_API_KEY"
    classifier = next(r for r in config["model_routes"] if r["id"] == "jev-classifier")
    classifier["targets"][0]["upstream_model"] = routes[0]["targets"][0]["upstream_model"]
    return config, key.encode(), {"path": str(path.resolve()), "sha256": digest(raw), "mode": mode}


def owned(obj: dict) -> bool:
    return obj.get("metadata", {}).get("labels", {}).get(OWNER_KEY) == OWNER


def mark(obj: dict) -> dict:
    labels = obj.setdefault("metadata", {}).setdefault("labels", {})
    labels.update({OWNER_KEY: OWNER, "app.kubernetes.io/managed-by": MANAGER})
    if obj.get("kind") == "Deployment":
        obj["spec"]["template"]["metadata"].setdefault("labels", {}).update(labels)
    return obj


def resource_id(obj: dict) -> str:
    meta = obj["metadata"]
    return "/".join((obj["kind"], meta.get("namespace", "_cluster"), meta["name"]))


def template(name: str, **values: str) -> list[dict]:
    text = (MANIFESTS / name).read_text()
    for key, value in {**IMAGES, **values}.items():
        text = text.replace(f"__{key}__", value)
    if "__" in text:
        raise SetupError("Unresolved manifest placeholder")
    return [mark(obj) for obj in yaml.safe_load_all(text) if obj]


def documents(config: dict) -> list[dict]:
    docs = template("shared.yaml") + template("namespace-policy.yaml")
    model_config = (MANIFESTS / "model-config.yaml").read_text()
    docs.append(mark({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {
        "name": "qwen35-config", "namespace": "orka-efficiency"}, "data": {"config.yaml": model_config}}))
    for team in TEAMS:
        docs.extend(template("team.yaml", TEAM=team, DEVELOPER="demo-client"))
        docs.append(mark({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {
            "name": "vekil-providers", "namespace": team}, "data": {
                "providers.yaml": yaml.safe_dump(config, sort_keys=False)}}))
    for obj in docs:
        if obj["kind"] == "Deployment":
            name = obj["metadata"]["name"]
            annotations = obj["spec"]["template"]["metadata"].setdefault("annotations", {})
            if name == "vekil":
                annotations["demo.orka.ai/config-digest"] = digest(config)
            elif name == "qwen35-2b":
                annotations["demo.orka.ai/config-digest"] = digest(model_config)
    # Prerequisites first; Deployments last. Namespace/Secret sequencing is
    # handled by apply_setup so no worker can start before admission is ready.
    return sorted(docs, key=lambda x: (x["kind"] != "Namespace", x["kind"] == "Deployment"))


class Kubernetes:
    def __init__(self, context: str):
        self.context = context

    def run(self, args: list[str], body: object | None = None, timeout: int = 45) -> bytes:
        try:
            result = subprocess.run(["kubectl", "--context", self.context, *args],
                                    input=None if body is None else json.dumps(body).encode(),
                                    capture_output=True, timeout=timeout, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise KubernetesError(args[0]) from None
        if result.returncode:
            conflict = any(word in result.stderr for word in (b"Conflict", b"test failed", b"test operation", b"object has been modified"))
            raise KubernetesError(args[0], conflict)
        return result.stdout

    def get(self, kind: str, name: str = "", namespace: str = "", optional: bool = False) -> dict | None:
        args = ["get", kind]
        if name:
            args.append(name)
        if namespace:
            args.extend(["-n", namespace])
        if optional:
            args.append("--ignore-not-found")
        args.extend(["-o", "json"])
        raw = self.run(args)
        if not raw.strip() and optional:
            return None
        try:
            return json.loads(raw)
        except ValueError:
            raise SetupError("Kubernetes returned an unexpected response; contents withheld") from None

    def apply(self, desired: dict, state: dict) -> None:
        meta = desired["metadata"]
        for attempt in range(4):
            current = self.get(desired["kind"], meta["name"], meta.get("namespace", ""), optional=True)
            if current and not owned(current):
                raise SetupError(f"Refusing to change an unowned resource: {resource_id(desired)}")
            payload = copy.deepcopy(desired)
            if current:
                payload["metadata"]["resourceVersion"] = current["metadata"]["resourceVersion"]
            args = (["apply", "--server-side", f"--field-manager={MANAGER}"] if current
                    else ["create", f"--field-manager={MANAGER}"])
            try:
                result = json.loads(self.run([*args, "-f", "-", "-o", "json"], payload))
                break
            except KubernetesError as error:
                if not error.conflict or attempt == 3:
                    raise
        record = {"id": resource_id(result), "uid": result["metadata"]["uid"]}
        state["resources"][record["id"]] = record
        if current is None:
            state["created_resources"][record["id"]] = record

    def wait(self, namespace: str, name: str, seconds: int = 240) -> None:
        message(f"Waiting for {namespace}/{name} readiness")
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            obj = self.get("Deployment", name, namespace)
            status = obj.get("status", {})
            wanted = obj["spec"].get("replicas", 1)
            if (status.get("observedGeneration", 0) >= obj["metadata"]["generation"]
                    and status.get("updatedReplicas", 0) == wanted
                    and status.get("availableReplicas", 0) == wanted
                    and status.get("replicas", 0) == wanted):
                return
            time.sleep(3)
        raise SetupError(f"Readiness timed out for {namespace}/{name}; inspect its safe diagnostics")


def preservation(kube: Kubernetes, source: Path) -> dict:
    try:
        result = {"providers_source": {"sha256": digest(source.read_bytes()), "mode": source.stat().st_mode & 0o777}}
    except OSError:
        raise SetupError("Cannot fingerprint the original providers source") from None
    for kind, namespace, name in PROTECTED:
        obj = kube.get(kind, name, namespace)
        # Status and resourceVersion legitimately change without operator writes.
        # Hash specifications/data only; raw credentials never leave memory.
        stable = {"uid": obj["metadata"]["uid"], "spec": obj.get("spec"),
                  "data": obj.get("data"), "type": obj.get("type")}
        result[f"{kind}/{namespace}/{name}"] = {"uid": stable["uid"], "sha256": digest(stable)}
    return result


def save_state(state: dict) -> None:
    STATE.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix="setup-", suffix=".tmp", dir=STATE.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(state, stream, indent=2)
            stream.write("\n")
        os.replace(temporary, STATE)
    finally:
        Path(temporary).unlink(missing_ok=True)


def quantity(value: str, cpu: bool = False) -> Decimal:
    units = {"Ki": 1024, "Mi": 1024**2, "Gi": 1024**3, "Ti": 1024**4,
             "n": Decimal("0.000000001"), "u": Decimal("0.000001"), "m": Decimal("0.001"),
             "k": 1000, "M": 1000**2, "G": 1000**3}
    for suffix, multiplier in units.items():
        if value.endswith(suffix):
            answer = Decimal(value[:-len(suffix)]) * multiplier
            return answer * 1000 if cpu else answer
    answer = Decimal(value)
    return answer * 1000 if cpu else answer


def check_capacity(kube: Kubernetes) -> dict:
    nodes = kube.get("nodes")["items"]
    pods = json.loads(kube.run(["get", "pods", "-A", "-o", "json"]))["items"]
    free = {}
    for node in nodes:
        if (node["spec"].get("unschedulable")
                or any(t.get("effect") in ("NoSchedule", "NoExecute") for t in node["spec"].get("taints", []))
                or not any(c["type"] == "Ready" and c["status"] == "True" for c in node["status"].get("conditions", []))):
            continue
        free[node["metadata"]["name"]] = [quantity(node["status"]["allocatable"]["cpu"], True),
                                          quantity(node["status"]["allocatable"]["memory"])]
    for pod in pods:
        node = pod["spec"].get("nodeName")
        if node not in free or pod.get("status", {}).get("phase") in ("Succeeded", "Failed") or owned(pod):
            continue
        for index, key in enumerate(("cpu", "memory")):
            def requested(container: dict) -> Decimal:
                return quantity(container.get("resources", {}).get("requests", {}).get(key, "0"), index == 0)
            steady = sum((requested(c) for c in pod["spec"].get("containers", [])), Decimal(0))
            sidecars, initialization = Decimal(0), Decimal(0)
            for container in pod["spec"].get("initContainers", []):
                request = requested(container)
                if container.get("restartPolicy") == "Always":
                    sidecars += request
                    initialization = max(initialization, sidecars)
                else:
                    initialization = max(initialization, sidecars + request)
            overhead = quantity(pod["spec"].get("overhead", {}).get(key, "0"), index == 0)
            free[node][index] -= max(steady + sidecars, initialization) + overhead
    model = free.get(MODEL_NODE, [Decimal(0), Decimal(0)])
    spare = sum(max(values[0], Decimal(0)) for values in free.values())
    # Static services need 325m; keep another 200m for serial native/validation Jobs.
    if model[0] < 150 or model[1] < 2 * 1024**3 or spare < 525:
        raise SetupError("Insufficient unreserved capacity for the pinned model node and demo workers")
    return {"spare_millicpu": int(spare), "planned_millicpu": 325, "worker_headroom_millicpu": 200,
            "model_node": MODEL_NODE, "model_node_free_millicpu": int(model[0]),
            "model_node_free_memory_mib": int(model[1] / 1024**2)}


def admission_patch(obj: dict) -> tuple[list[dict], list[str]]:
    containers = obj["spec"]["template"]["spec"]["containers"]
    matches = [i for i, c in enumerate(containers) if c["name"] == "admission"]
    if len(matches) != 1:
        raise SetupError("Shared admission container was not found")
    index = matches[0]
    args = containers[index].get("args", [])
    patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": obj["metadata"]["resourceVersion"]}]
    additions = []
    for flag in ("--controller-usernames=", "--task-provenance-trusted-users="):
        positions = [i for i, arg in enumerate(args) if arg.startswith(flag)]
        if len(positions) != 1:
            raise SetupError("Shared admission must have explicit controller and provenance identity flags")
        position = positions[0]
        old = args[position]
        existing = {value.strip() for value in old[len(flag):].split(",")}
        missing = [f"system:serviceaccount:{team}:controller" for team in TEAMS
                   if f"system:serviceaccount:{team}:controller" not in existing]
        if missing:
            path = f"/spec/template/spec/containers/{index}/args/{position}"
            value = old.rstrip(",") + ("," if old[len(flag):].strip(",") else "") + ",".join(missing)
            patch.extend([{"op": "test", "path": path, "value": old}, {"op": "replace", "path": path, "value": value}])
            additions.extend(f"{flag[:-1]}:{username}" for username in missing)
    return patch if len(patch) > 1 else [], additions


def register_admission(kube: Kubernetes, state: dict) -> None:
    for attempt in range(5):
        current = kube.get("Deployment", "orka-admission", "orka-system")
        patch, additions = admission_patch(current)
        if not patch:
            kube.wait("orka-system", "orka-admission")
            return
        try:
            kube.run(["-n", "orka-system", "patch", "deployment", "orka-admission", "--type=json", "--patch-file=/dev/stdin"], patch)
            state.setdefault("admission_identities_added", []).extend(additions)
            save_state(state)
            kube.wait("orka-system", "orka-admission")
            return
        except KubernetesError as error:
            if not error.conflict or attempt == 4:
                raise
    raise SetupError("Admission changed concurrently; no unsafe overwrite was attempted")


def register_namespace_policy(kube: Kubernetes, state: dict, docs: list[dict]) -> None:
    """Keep namespace protection when the shared webhook has a retired path."""
    for obj in docs:
        if obj["kind"] in ("ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"):
            kube.apply(obj, state)
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        policy = kube.get("ValidatingAdmissionPolicy", "orka-efficiency-namespace-mode")
        status = policy.get("status", {})
        if status.get("observedGeneration") == policy["metadata"]["generation"]:
            if status.get("typeChecking", {}).get("expressionWarnings"):
                raise SetupError("The demo namespace policy has CEL type warnings")
            break
        time.sleep(1)
    else:
        raise SetupError("The demo namespace policy was not validated")
    # Older installations kept this webhook after its handler moved to CEL.
    # Leave every other namespace and webhook as-is. The policy above must be
    # installed first, so there is never an unprotected namespace write.
    for attempt in range(5):
        current = kube.get("ValidatingWebhookConfiguration", "orka-admission")
        matches = [(i, w) for i, w in enumerate(current.get("webhooks", []))
                   if w["name"] == "namespaceexecutionmode.core.orka.ai"]
        if not matches:
            return
        index, webhook = matches[0]
        service = webhook["clientConfig"].get("service", {})
        if service != {"name": "orka-admission", "namespace": "orka-system",
                       "path": "/validate-v1-namespace-execution-mode", "port": 443}:
            raise SetupError("Unexpected namespace webhook target; no webhook change was made")
        old = webhook.get("namespaceSelector", {})
        updated = copy.deepcopy(old)
        expressions = updated.setdefault("matchExpressions", [])
        excluded = next((v for v in expressions
                         if v["key"] == "kubernetes.io/metadata.name" and v["operator"] == "NotIn"), None)
        if excluded is None:
            excluded = {"key": "kubernetes.io/metadata.name", "operator": "NotIn", "values": []}
            expressions.append(excluded)
        for team in (*TEAMS, *(team + "-runtimes" for team in TEAMS)):
            if team not in excluded["values"]:
                excluded["values"].append(team)
        if old == updated:
            return
        path = f"/webhooks/{index}/namespaceSelector"
        patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": current["metadata"]["resourceVersion"]},
                 {"op": "test", "path": f"/webhooks/{index}/name", "value": webhook["name"]},
                 {"op": "test", "path": path, "value": old},
                 {"op": "replace", "path": path, "value": updated}]
        try:
            kube.run(["patch", "ValidatingWebhookConfiguration", "orka-admission", "--type=json", "--patch-file=/dev/stdin"], patch)
            state["namespace_admission_migration"] = {"before": old, "after": updated,
                                                     "policy": "orka-efficiency-namespace-mode"}
            save_state(state)
            message("Current namespace protection installed for the four new demo namespaces")
            return
        except KubernetesError as error:
            if not error.conflict or attempt == 4:
                raise


def secret(name: str, namespace: str, values: dict[str, bytes]) -> dict:
    return mark({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": name, "namespace": namespace},
                 "type": "Opaque", "data": {key: base64.b64encode(value).decode() for key, value in values.items()}})


def ensure_key(kube: Kubernetes, state: dict, name: str, namespace: str, key: str, value: bytes) -> None:
    current = kube.get("Secret", name, namespace, optional=True)
    if current:
        if not owned(current) or not current.get("data", {}).get(key):
            raise SetupError(f"Existing {namespace}/{name} is not a valid demo-owned key")
        if name == "snapshot-key":
            try:
                if len(base64.b64decode(current["data"][key], validate=True)) != 32:
                    raise ValueError
            except ValueError:
                raise SetupError(f"Existing {namespace}/snapshot-key has an invalid key length") from None
        record = {"id": resource_id(current), "uid": current["metadata"]["uid"]}
        state["resources"][record["id"]] = record
        return
    obj = secret(name, namespace, {key: value})
    obj["immutable"] = True
    kube.apply(obj, state)


def apply_setup(kube: Kubernetes, state: dict, config: dict, jev_key: bytes) -> None:
    docs = documents(config)
    # Refuse collisions before touching shared admission or creating resources.
    for obj in docs:
        current = kube.get(obj["kind"], obj["metadata"]["name"], obj["metadata"].get("namespace", ""), optional=True)
        if current and not owned(current):
            raise SetupError(f"Refusing to change an unowned resource: {resource_id(obj)}")
    state["capacity"] = check_capacity(kube)
    original = kube.get("Secret", "vekil-copilot-github-token", "vekil-system")
    try:
        copilot = base64.b64decode(original["data"]["token"], validate=True)
        if not copilot:
            raise ValueError
    except (KeyError, ValueError):
        raise SetupError("Original Copilot Secret has no usable token; nothing was changed") from None
    message("Target verified: sertac-aks; existing Vekil and credentials will be preserved")
    register_namespace_policy(kube, state, docs)
    for obj in docs:
        if obj["kind"] == "Namespace":
            kube.apply(obj, state)
    register_admission(kube, state)
    for team in TEAMS:
        ensure_key(kube, state, "snapshot-key", team, "key", secrets.token_bytes(32))
        ensure_key(kube, state, "provider-key", team, "api-key", secrets.token_urlsafe(32).encode())
        auth = secret("vekil-auth", team, {"copilot-token": copilot, "jev-api-key": jev_key})
        kube.apply(auth, state)
        for obj in docs:
            if (obj["kind"] == "Deployment" and obj["metadata"].get("namespace") == team
                    and obj["metadata"]["name"] == "vekil"):
                obj["spec"]["template"]["metadata"]["annotations"]["demo.orka.ai/auth-digest"] = digest(auth["data"])
    for obj in docs:
        if obj["kind"] not in ("Namespace", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"):
            kube.apply(obj, state)
            save_state(state)
    kube.wait("orka-efficiency", "qwen35-2b", seconds=660)
    for team in TEAMS:
        kube.wait(team, "vekil")
        kube.wait(team, "orka-controller")
    kube.wait("orka-efficiency", "orka-compat-router")
    state["gateway_modes"] = {team: "off" for team in TEAMS}
    state["status"] = "ready"
    message("Both teams, their gateways, the CPU model, and the shared router are ready; routing is off")


def set_mode(kube: Kubernetes, state: dict, mode: str) -> None:
    message("Changing gateway mode; earlier statistics must already have been captured")
    for team in TEAMS:
        for attempt in range(5):
            obj = kube.get("Deployment", "vekil", team)
            if not owned(obj):
                raise SetupError(f"Refusing to change an unowned gateway in {team}")
            containers = obj["spec"]["template"]["spec"]["containers"]
            index = next(i for i, c in enumerate(containers) if c["name"] == "vekil")
            env = containers[index]["env"]
            position = next(i for i, item in enumerate(env) if item["name"] == "POLICY_ROUTING_MODE")
            old = env[position].get("value")
            if old == mode:
                break
            path = f"/spec/template/spec/containers/{index}/env/{position}/value"
            patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": obj["metadata"]["resourceVersion"]},
                     {"op": "test", "path": path, "value": old}, {"op": "replace", "path": path, "value": mode}]
            try:
                kube.run(["-n", team, "patch", "deployment", "vekil", f"--field-manager={MANAGER}",
                          "--type=json", "--patch-file=/dev/stdin"], patch)
                break
            except KubernetesError as error:
                if not error.conflict or attempt == 4:
                    raise
        kube.wait(team, "vekil")
        state.setdefault("gateway_modes", {})[team] = mode
        save_state(state)
    state["status"] = "ready"
    message(f"Both gateways are ready in {mode} mode")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True, choices=["sertac-aks"])
    parser.add_argument("--providers-source", type=Path)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("render-safe", help="render offline manifests, omitting Secrets and private classifier endpoints")
    commands.add_parser("apply", help="deploy only the owned demo resources and append controller admission identities")
    mode_parser = commands.add_parser("mode", help="change both gateways after the caller has saved their evidence")
    mode_parser.add_argument("mode", choices=["off", "enforce"])
    args = parser.parse_args()
    if args.command in ("apply", "render-safe") and not args.providers_source:
        raise SetupError("--providers-source is required for apply and render-safe")
    if args.command == "render-safe":
        config, _, _ = source_config(args.providers_source)
        safe = copy.deepcopy(config)
        provider = next(p for p in safe["providers"] if p["id"] == "vercel-jev")
        provider["base_url"] = "https://classifier.invalid/operator-supplied"
        for name in ("auth_header", "auth_prefix", "systemone_path"):
            if name in provider:
                provider[name] = "operator-supplied"
        print(yaml.safe_dump_all(documents(safe), sort_keys=False))
        return 0

    kube = Kubernetes(args.context)
    cluster_uid = kube.get("Namespace", "kube-system")["metadata"]["uid"]
    if STATE.exists():
        try:
            state = json.loads(STATE.read_text())
        except (OSError, ValueError):
            raise SetupError("Cannot read the previous safe setup record") from None
        if state.get("cluster_uid") != cluster_uid or state.get("context") != args.context:
            raise SetupError("Setup record belongs to a different cluster")
    else:
        state = {"schema": 1, "context": args.context, "cluster_uid": cluster_uid,
                 "resources": {}, "created_resources": {}, "images": IMAGES}
    source = args.providers_source or Path(state.get("providers_source", {}).get("path", ""))
    if not source.is_file():
        raise SetupError("The original providers source is required to verify preservation")
    config, jev_key, fingerprint = source_config(source)
    state["providers_source"] = fingerprint
    before = preservation(kube, source)
    state["preservation_before"] = before
    state["status"] = "applying" if args.command == "apply" else "changing-mode"
    save_state(state)
    try:
        if args.command == "apply":
            apply_setup(kube, state, config, jev_key)
        else:
            set_mode(kube, state, args.mode)
    except Exception:
        state["status"] = "incomplete"
        raise
    finally:
        after = preservation(kube, source)
        state["preservation_after"] = after
        state["preserved"] = before == after
        state["updated_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        save_state(state)
        if before != after:
            raise SetupError("Preservation check changed; inspect the safe setup fingerprints")
    message("Original providers file, Vekil deployment, token cache PVC, and Copilot Secret are unchanged")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SetupError as error:
        print(f"Error: {error}", file=sys.stderr)
        sys.exit(1)
    except (Exception, KeyboardInterrupt) as error:
        # Parser/provider/Kubernetes exceptions can contain private payloads.
        print(f"Setup did not complete ({type(error).__name__}); private diagnostics withheld", file=sys.stderr)
        sys.exit(1)
