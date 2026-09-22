#!/usr/bin/env python3
"""Add the demo's Codex runtime and clean-room publisher to team-payments.

Only the already-owned team-payments installation is changed. Credentials stay
in memory or Kubernetes Secrets. Before the first controller replacement, an
online SQLite backup and every other /data file are copied to a persistent
volume and verified. The private backup and safe receipts stay under bin/.

Run `render-safe` to inspect the additional resources, then `apply`. Stop demo
traffic during apply; the setup refuses to migrate while any Task is active.
It never runs a model, publishes a branch, or creates a pull request.
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
import time
import urllib.error
import urllib.request

import prepare


NS = "team-payments"
RUNTIME_NS = NS + "-runtimes"
DATA_PVC = "efficiency-controller-data"
PUBLISHER_PVC = "efficiency-publisher-data"
PUBLICATION_SECRET = "payments-repository-write"
STATE = prepare.ROOT / "bin/efficiency-production/runtime-setup.json"
PROXY_IMAGE = "docker.io/sozercan/orka@sha256:c68227305afdd2d7752facff9e17412e2e70c3d461894903b21fc7bfd27e0809"
CODEX_IMAGE = "docker.io/sozercan/orka-acp-codex-runtime@sha256:6175a0636e106a036b976f64c8db207dd24ef8041246d6ff2b8632b39451d850"
PUBLISHER_IMAGE = "docker.io/sozercan/orka-workspace-publisher@sha256:04d93c217922661f6e48cf6b37ff5da700c1036b22fb8502fc00aae69b98abeb"
API = f"http://orka-api.{NS}.svc:8080"
AUTH_ROOT = "/var/run/orka"
LABEL_ROLE = "orka.ai/network-role"
SAFE_SOURCE = Path("/Users/sozercan/projects/copilot-proxy/provider-v4.yaml")


def resource(kind: str, name: str, **fields: object) -> dict:
    group = {"Deployment": "apps/v1", "NetworkPolicy": "networking.k8s.io/v1",
             **{role: "rbac.authorization.k8s.io/v1" for role in
                ("Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding")}}
    metadata = {"name": name}
    if kind not in ("ClusterRole", "ClusterRoleBinding"):
        metadata["namespace"] = NS
    return prepare.mark({"apiVersion": group.get(kind, "v1"), "kind": kind,
                         "metadata": metadata, **fields})


def publication_rbac() -> list[dict]:
    # BranchClaims coordinate repository branches across controller namespaces.
    # The uncached control store gets/creates/reclaims claims and updates status;
    # Session cleanup also lists claims. It needs no watch, patch, or spec update.
    name = "orka-efficiency-team-payments-publication"
    return [
        resource("ClusterRole", name, rules=[
            {"apiGroups": ["core.orka.ai"], "resources": ["branchclaims"],
             "verbs": ["get", "list", "create", "delete"]},
            {"apiGroups": ["core.orka.ai"], "resources": ["branchclaims/status"],
             "verbs": ["update"]},
        ]),
        resource("ClusterRoleBinding", name,
                 roleRef={"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": name},
                 subjects=[{"kind": "ServiceAccount", "name": "controller", "namespace": NS}]),
    ]


def env(values: dict[str, str]) -> list[dict]:
    return [{"name": name, "value": value} for name, value in values.items()]


def secret_volume(name: str, keys: list[str]) -> dict:
    return {"name": name, "secret": {"secretName": name, "defaultMode": 0o440,
                                     "items": [{"key": key, "path": key} for key in keys]}}


def secret_mount(name: str, key: str) -> dict:
    # These services require regular files, so use subPath rather than the
    # symlinks at the root of a projected Secret volume.
    return {"name": name, "mountPath": f"{AUTH_ROOT}/{name}/{key}",
            "subPath": key, "readOnly": True}


def deployment(name: str, image: str, command: list[str], args: list[str],
               variables: list[dict], volumes: list[dict], mounts: list[dict],
               health: str = "/readyz") -> dict:
    labels = {"app": name, LABEL_ROLE: name}
    return resource("Deployment", name, spec={
        "replicas": 1, "strategy": {"type": "Recreate"},
        "selector": {"matchLabels": {"app": name}},
        "template": {"metadata": {"labels": labels}, "spec": {
            "serviceAccountName": name, "automountServiceAccountToken": False,
            "enableServiceLinks": False,
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                "runAsGroup": 65532, "fsGroup": 65532,
                                "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{"name": name, "image": image, "command": command,
                "args": args, "env": variables, "ports": [{"name": "http", "containerPort": 8080}],
                "securityContext": {"allowPrivilegeEscalation": False,
                                    "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                "resources": {"requests": {"cpu": "25m", "memory": "64Mi"},
                              "limits": {"cpu": "500m", "memory": "512Mi"}},
                "readinessProbe": {"httpGet": {"path": health, "port": "http"}, "periodSeconds": 5},
                "volumeMounts": mounts}], "volumes": volumes}}})


def peer(role: str, namespace: str | None = None) -> dict:
    result = {"podSelector": {"matchLabels": {LABEL_ROLE: role}}}
    if namespace:
        result["namespaceSelector"] = {"matchLabels": {"kubernetes.io/metadata.name": namespace}}
    return result


def allow(direction: str, peers: list[dict], port: int = 8080) -> dict:
    return {"from" if direction == "ingress" else "to": peers,
            "ports": [{"protocol": "TCP", "port": port}]}


def documents() -> list[dict]:
    names = ("provider-auth-proxy", "scm-egress-proxy", "workspace-publisher")
    docs = [resource("PersistentVolumeClaim", name, spec={"accessModes": ["ReadWriteOnce"],
        "storageClassName": "managed-csi", "resources": {"requests": {"storage": "2Gi"}}})
        for name in (DATA_PVC, PUBLISHER_PVC)]
    docs.extend(publication_rbac())
    for name in names:
        docs.extend([resource("ServiceAccount", name, automountServiceAccountToken=False),
                     resource("Service", name, spec={"selector": {"app": name},
                         "ports": [{"name": "http", "port": 8080, "targetPort": "http"}]})])
    docs.extend([
        resource("Role", "payments-publication-client", rules=[{
            "apiGroups": [""], "resources": ["secrets"],
            "resourceNames": [PUBLICATION_SECRET], "verbs": ["get"]}]),
        resource("RoleBinding", "payments-publication-client",
                 roleRef={"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "payments-publication-client"},
                 subjects=[{"kind": "ServiceAccount", "name": "demo-client", "namespace": NS}]),
    ])
    provider = "provider-auth-proxy"
    docs.append(deployment(provider, PROXY_IMAGE, ["/provider-auth-proxy"], [
        "--listen-address=:8080", f"--upstream-base-url=http://vekil.{NS}.svc:1337",
        f"--token-file={AUTH_ROOT}/{provider}/token"], [],
        [secret_volume(provider, ["token"])], [secret_mount(provider, "token")]))
    scm = "scm-egress-proxy-auth"
    docs.append(deployment("scm-egress-proxy", PROXY_IMAGE, ["/scm-egress-proxy"], [
        "--listen-address=:8080", "--allowed-hosts=github.com", "--forge-api-base-url=https://api.github.com",
        f"--token-file={AUTH_ROOT}/{scm}/token"], [],
        [secret_volume(scm, ["token"])], [secret_mount(scm, "token")]))
    publisher = "workspace-publisher-auth"
    proxy_url = f"http://orka-publisher:$(ORKA_SCM_EGRESS_PROXY_TOKEN)@scm-egress-proxy.{NS}.svc:8080"
    variables = [{"name": "ORKA_SCM_EGRESS_PROXY_TOKEN", "valueFrom": {
        "secretKeyRef": {"name": scm, "key": "token"}}}] + env({
        "HTTPS_PROXY": proxy_url, "https_proxy": proxy_url,
        "NO_PROXY": "localhost,127.0.0.1,.svc,.cluster.local",
        "no_proxy": "localhost,127.0.0.1,.svc,.cluster.local",
        "ORKA_PUBLISHER_SCM_EGRESS_PROXY_REQUIRED": "true",
        "ORKA_PUBLISHER_LISTEN_ADDRESS": ":8080",
        "ORKA_PUBLISHER_CONTROLLER_TOKEN_FILE": f"{AUTH_ROOT}/{publisher}/controller-token",
        "ORKA_PUBLISHER_OPERATION_CAPABILITY_SECRET_FILE": f"{AUTH_ROOT}/{publisher}/operation-capability-secret",
        "ORKA_PUBLISHER_ARTIFACT_AUTHORIZATION_BROKER_URL": API,
        "ORKA_PUBLISHER_ARTIFACT_API_URL": API,
        "ORKA_PUBLISHER_CREDENTIAL_BROKER_URL": API,
        "ORKA_PUBLISHER_TEMP_ROOT": "/tmp/orka-workspace-publisher/runtime",
        "ORKA_PUBLISHER_ALLOWED_SCM_HOSTS": "github.com",
        "ORKA_PUBLISHER_GITHUB_PR_ENABLED": "false",
        "ORKA_PUBLISHER_MAX_CONCURRENT_OPERATIONS": "1",
    })
    docs.append(deployment("workspace-publisher", PUBLISHER_IMAGE,
        ["/usr/local/bin/orka-workspace-publisher"], [], variables,
        [{"name": "data", "persistentVolumeClaim": {"claimName": PUBLISHER_PVC}},
         {"name": "tmp", "emptyDir": {"sizeLimit": "1Gi"}},
         secret_volume(publisher, ["controller-token", "operation-capability-secret"])],
        [{"name": "data", "mountPath": "/data"}, {"name": "tmp", "mountPath": "/tmp/orka-workspace-publisher"},
         secret_mount(publisher, "controller-token"), secret_mount(publisher, "operation-capability-secret")],
        health="/v1/health"))
    dns = {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                   "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
           "ports": [{"protocol": proto, "port": 53} for proto in ("TCP", "UDP")]}
    policies = {
        provider: ([allow("ingress", [peer("provider-client", RUNTIME_NS)])],
                   [dns, allow("egress", [{"podSelector": {"matchLabels": {"app": "efficiency-vekil"}}}], 1337)]),
        "workspace-publisher": ([allow("ingress", [peer("controller")])],
                                [dns, allow("egress", [peer("controller"), peer("scm-egress-proxy")])]),
        "scm-egress-proxy": ([allow("ingress", [peer("workspace-publisher")])],
            [dns, allow("egress", [{"ipBlock": {"cidr": "0.0.0.0/0", "except": [
                "0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
                "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
                "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"]}}], 443)]),
    }
    for name, (incoming, outgoing) in policies.items():
        docs.append(resource("NetworkPolicy", name, spec={"podSelector": {"matchLabels": {"app": name}},
            "policyTypes": ["Ingress", "Egress"], "ingress": incoming, "egress": outgoing}))
    docs.append(resource("NetworkPolicy", "vekil-coding-runtime-access", spec={
        "podSelector": {"matchLabels": {"app": "efficiency-vekil"}}, "policyTypes": ["Ingress"],
        # Ingress policies are additive. Include the two existing native
        # clients as well as the new coding proxy so this is self-contained.
        "ingress": [allow("ingress", [peer(provider),
            {"podSelector": {"matchLabels": {"app": "orka-controller"}}},
            {"podSelector": {"matchLabels": {"orka.ai/task-type": "ai"}}}], 1337)]}))
    return sorted(docs, key=lambda obj: obj["kind"] == "Deployment")


def save(state: dict) -> None:
    STATE.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = STATE.with_suffix(".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as stream:
        json.dump(state, stream, indent=2)
        stream.write("\n")
    os.replace(temporary, STATE)


def bytes_command(kube: prepare.Kubernetes, args: list[str], data: bytes | None = None,
                  timeout: int = 60) -> bytes:
    try:
        result = subprocess.run(["kubectl", "--context", kube.context, *args], input=data,
                                capture_output=True, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise prepare.SetupError("Runtime setup command did not complete; raw output withheld") from None
    if result.returncode:
        raise prepare.SetupError("Runtime setup command failed; raw output withheld")
    return result.stdout


def check_target(kube: prepare.Kubernetes) -> dict:
    if kube.context != "sertac-aks":
        raise prepare.SetupError("This setup is restricted to context sertac-aks")
    for name in (NS, RUNTIME_NS):
        namespace = kube.get("Namespace", name)
        if not prepare.owned(namespace) or namespace["metadata"].get("labels", {}).get("orka.ai/controller-mode") != "harness-v2":
            raise prepare.SetupError("Both payments namespaces must belong to the efficiency demo and use harness v2")
    current = kube.get("Deployment", "orka-controller", NS)
    if not prepare.owned(current) or current["spec"].get("replicas") != 1:
        raise prepare.SetupError("Expected the demo-owned single payments controller")
    container = next((c for c in current["spec"]["template"]["spec"]["containers"] if c["name"] == "manager"), None)
    if not container or container["image"] != prepare.IMAGES["CONTROLLER_IMAGE"]:
        raise prepare.SetupError("Controller image differs from the verified demo pin")
    if "--store-path=/data/orka.db" not in container.get("args", []):
        raise prepare.SetupError("Controller database path differs from the expected /data/orka.db")
    return current


def preservation(kube: prepare.Kubernetes) -> dict:
    result = prepare.preservation(kube, SAFE_SOURCE)
    for name in ("orka-provider-auth-proxy", "orka-workspace-publisher", "orka-scm-egress-proxy"):
        obj = kube.get("Deployment", name, "orka-system")
        result[f"Deployment/orka-system/{name}"] = {"uid": obj["metadata"]["uid"],
            "sha256": prepare.digest(obj["spec"])}
    for name in ("git-credentials", "acp-e2e-forge", "acp-e2e-source-read"):
        obj = kube.get("Secret", name, "default")
        result[f"Secret/default/{name}"] = {"uid": obj["metadata"]["uid"],
            "sha256": prepare.digest(obj.get("data", {}))}
    return result


def ensure_secrets(kube: prepare.Kubernetes, state: dict) -> None:
    for name, keys in {
        "provider-auth-proxy": ["token"], "scm-egress-proxy-auth": ["token"],
        "workspace-publisher-auth": ["controller-token", "operation-capability-secret"],
        "acp-artifact-capability": ["capability-secret"],
    }.items():
        current = kube.get("Secret", name, NS, optional=True)
        if current:
            if not prepare.owned(current) or set(current.get("data", {})) != set(keys):
                raise prepare.SetupError("An existing runtime Secret is unowned or has unexpected keys")
            if any(len(base64.b64decode(current["data"][key])) < 32 for key in keys):
                raise prepare.SetupError("An existing runtime Secret has unusable material")
        else:
            kube.apply(prepare.secret(name, NS, {key: secrets.token_urlsafe(36).encode() for key in keys}), state)
    source = kube.get("Secret", "git-credentials", "default")
    try:
        token = base64.b64decode(source["data"]["token"], validate=True).strip()
        if not token or any(c in token for c in (b"\r", b"\n", b"\x00")):
            raise ValueError
        request = urllib.request.Request("https://api.github.com/repos/sozercan/orka-demo-inventory",
            headers={"Authorization": "Bearer " + token.decode(), "Accept": "application/vnd.github+json",
                     "User-Agent": "orka-efficiency-runtime-setup"})
        with urllib.request.urlopen(request, timeout=20) as response:
            permissions = json.load(response).get("permissions", {})
        if not permissions.get("push"):
            raise ValueError
    except (KeyError, ValueError, UnicodeError, urllib.error.URLError, TimeoutError):
        raise prepare.SetupError("Existing repository credential did not prove write access to the demo repository") from None
    target = kube.get("Secret", PUBLICATION_SECRET, NS, optional=True)
    desired = prepare.secret(PUBLICATION_SECRET, NS, {"token": token})
    if target and (not prepare.owned(target) or target.get("data") != desired["data"]):
        raise prepare.SetupError("Publication Secret differs; refusing to replace existing credentials")
    if not target:
        kube.apply(desired, state)
    state["publication"] = {"source": "default/git-credentials:token", "target": f"{NS}/{PUBLICATION_SECRET}:token",
                            "repository": "sozercan/orka-demo-inventory", "pushPermissionVerified": True}
    save(state)


def task_inventory(kube: prepare.Kubernetes) -> list[dict]:
    tasks = kube.get("tasks.core.orka.ai", namespace=NS)["items"]
    result = []
    for obj in tasks:
        phase = obj.get("status", {}).get("phase")
        if phase not in ("Succeeded", "Failed", "Cancelled"):
            raise prepare.SetupError("Stop demo traffic and let all payments Tasks finish before replacing its controller")
        result.append({"name": obj["metadata"]["name"], "uid": obj["metadata"]["uid"], "phase": phase})
    return sorted(result, key=lambda obj: obj["uid"])


def controller_pod(kube: prepare.Kubernetes) -> dict:
    pods = kube.get("pods", namespace=NS)["items"]
    pods = [p for p in pods if p["metadata"].get("labels", {}).get("app") == "orka-controller"
            and not p["metadata"].get("deletionTimestamp")]
    if len(pods) != 1 or not prepare.owned(pods[0]) or pods[0]["status"].get("phase") != "Running":
        raise prepare.SetupError("Expected exactly one running demo controller pod")
    return pods[0]


def wait_container(kube: prepare.Kubernetes, name: str, container: str, ephemeral: bool = False) -> None:
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        pod = kube.get("pod", name, NS)
        key = "ephemeralContainerStatuses" if ephemeral else "containerStatuses"
        status = next((c for c in pod.get("status", {}).get(key, []) if c["name"] == container), {})
        if status.get("state", {}).get("running"):
            return
        if status.get("state", {}).get("terminated"):
            raise prepare.SetupError("Data migration container stopped before verification")
        time.sleep(2)
    raise prepare.SetupError("Data migration container did not start")


# Only aggregate table counts and content hashes are printed. Database contents
# and artifact bytes travel through captured subprocess pipes, never tool logs.
MANIFEST_JS = r"""
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {DatabaseSync, backup} from 'node:sqlite';
function files(root, rel = '') {
  const result = {};
  for (const name of fs.readdirSync(path.join(root, rel)).sort()) {
    const item = path.join(rel, name), full = path.join(root, item);
    const stat = fs.lstatSync(full);
    if (stat.isSymbolicLink()) throw new Error('symlink in preserved data');
    if (stat.isDirectory()) Object.assign(result, files(root, item));
    else if (stat.isFile()) result[item] = {bytes: stat.size,
      sha256: crypto.createHash('sha256').update(fs.readFileSync(full)).digest('hex')};
    else throw new Error('unsupported preserved file');
  }
  return result;
}
function counts(db) {
  const result = {};
  for (const {name} of db.prepare("SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name").all()) {
    const ident = '"' + name.replaceAll('"', '""') + '"';
    result[name] = db.prepare(`SELECT COUNT(*) AS n FROM ${ident}`).get().n;
  }
  return result;
}
function manifest(root) {
  const db = new DatabaseSync(path.join(root, 'orka.db'), {readOnly: true});
  if (db.prepare('PRAGMA integrity_check').get().integrity_check !== 'ok') throw new Error('database integrity failure');
  const tableCounts = counts(db);
  db.close();
  return {files: files(root), tableCounts};
}
"""
BACKUP_JS = MANIFEST_JS + r"""
const source = '/source', output = '/tmp/orka-preserve';
if (fs.existsSync(output)) fs.rmSync(output, {recursive: true});
fs.mkdirSync(output, {mode: 0o700});
const database = new DatabaseSync(path.join(source, 'orka.db'), {readOnly: true});
await backup(database, path.join(output, 'orka.db'));
database.close();
for (const name of fs.readdirSync(source)) {
  if (['orka.db', 'orka.db-wal', 'orka.db-shm'].includes(name)) continue;
  fs.cpSync(path.join(source, name), path.join(output, name), {recursive: true, dereference: false});
}
console.log(JSON.stringify(manifest(output)));
"""


def node(kube: prepare.Kubernetes, pod: str, container: str, code: str) -> bytes:
    return bytes_command(kube, ["exec", "-n", NS, pod, "-c", container, "--", "node",
        "--no-warnings", "--input-type=module", "-e", code], timeout=120)


def migrate_data(kube: prepare.Kubernetes, state: dict) -> None:
    current = check_target(kube)
    volume = next(v for v in current["spec"]["template"]["spec"]["volumes"] if v["name"] == "data")
    if volume.get("persistentVolumeClaim", {}).get("claimName") == DATA_PVC:
        if not state.get("migration", {}).get("restoredVerified"):
            raise prepare.SetupError("Persistent controller data has no matching preservation receipt")
        return
    if "emptyDir" not in volume:
        raise prepare.SetupError("Refusing to replace an unexpected controller data volume")
    before_tasks = task_inventory(kube)
    pod = controller_pod(kube)
    pod_name, pod_uid = pod["metadata"]["name"], pod["metadata"]["uid"]
    suffix = secrets.token_hex(4)
    backup_name, helper_name = "preserve-" + suffix, "efficiency-data-copy-" + suffix
    prepare.message("Preserving the existing payments database and artifacts before any controller restart")
    ephemeral = {"name": backup_name, "image": CODEX_IMAGE, "command": ["node", "-e", "setTimeout(() => {}, 1800000)"],
        "targetContainerName": "manager", "volumeMounts": [{"name": "data", "mountPath": "/source", "readOnly": True}],
        "securityContext": {"runAsUser": 65532, "runAsGroup": 65532, "runAsNonRoot": True,
                            "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}}}
    pod["spec"].setdefault("ephemeralContainers", []).append(ephemeral)
    kube.run(["replace", "--raw", f"/api/v1/namespaces/{NS}/pods/{pod_name}/ephemeralcontainers", "-f", "-"], pod)
    wait_container(kube, pod_name, backup_name, ephemeral=True)
    expected = json.loads(node(kube, pod_name, backup_name, BACKUP_JS))
    entries = json.loads(node(kube, pod_name, backup_name,
        "import fs from 'node:fs'; console.log(JSON.stringify(fs.readdirSync('/tmp/orka-preserve').sort()));"))
    # Archive the entries, not '.': the PVC mount root belongs to root, so a
    # non-root extractor must not try to restore that directory's timestamps.
    archive = bytes_command(kube, ["exec", "-n", NS, pod_name, "-c", backup_name,
        "--", "tar", "-czf", "-", "-C", "/tmp/orka-preserve", "--", *entries], timeout=120)
    archive_path = STATE.parent / f"payments-data-{pod_uid}-{suffix}.tgz"
    fd = os.open(archive_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(archive)
    state["migration"] = {"sourcePod": pod_name, "sourcePodUID": pod_uid,
        "sourceTasks": before_tasks, "manifest": expected, "backup": str(archive_path),
        "backupSha256": hashlib.sha256(archive).hexdigest(), "pvc": DATA_PVC, "restoredVerified": False}
    save(state)
    helper = resource("Pod", helper_name, spec={"restartPolicy": "Never", "automountServiceAccountToken": False,
        "enableServiceLinks": False, "activeDeadlineSeconds": 900,
        "nodeSelector": {"kubernetes.io/hostname": pod["spec"]["nodeName"]},
        "securityContext": {"runAsUser": 65532, "runAsGroup": 65532, "runAsNonRoot": True,
                            "fsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
        "containers": [{"name": "copy", "image": CODEX_IMAGE, "command": ["node", "-e", "setTimeout(() => {}, 900000)"],
            "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
            "resources": {"requests": {"cpu": "10m", "memory": "64Mi"}, "limits": {"cpu": "250m", "memory": "256Mi"}},
            "volumeMounts": [{"name": "data", "mountPath": "/restore"}]}],
        "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": DATA_PVC}}]})
    kube.apply(helper, state)
    wait_container(kube, helper_name, "copy")
    # A previous interrupted attempt may have restored the same owned volume;
    # current ownership and the still-running emptyDir controller were verified.
    bytes_command(kube, ["exec", "-i", "-n", NS, helper_name, "-c", "copy", "--",
        "tar", "-xzf", "-", "-C", "/restore", "--no-same-owner"], archive, timeout=120)
    restored = json.loads(node(kube, helper_name, "copy", MANIFEST_JS + "\nconsole.log(JSON.stringify(manifest('/restore')));"))
    if restored != expected:
        raise prepare.SetupError("Restored database or artifacts differ; original controller remains running")
    kube.run(["delete", "pod", helper_name, "-n", NS, "--wait=true", "--timeout=60s"], timeout=70)
    if controller_pod(kube)["metadata"]["uid"] != pod_uid or task_inventory(kube) != before_tasks:
        raise prepare.SetupError("Controller or Tasks changed during migration; rerun with demo traffic stopped")
    state["migration"]["restoredVerified"] = True
    save(state)
    prepare.message(f"Verified database integrity, {len(expected['tableCounts'])} table counts, and all preserved file hashes")


def controller_template(current: dict) -> dict:
    template = copy.deepcopy(current["spec"]["template"])
    template["metadata"].setdefault("labels", {})[LABEL_ROLE] = "controller"
    spec = template["spec"]
    manager = next(c for c in spec["containers"] if c["name"] == "manager")
    flags = {
        "acp-codex-runtime-image": CODEX_IMAGE,
        "acp-provider-proxy-base-url": f"http://provider-auth-proxy.{NS}.svc:8080",
        "acp-provider-proxy-namespace": NS,
        "acp-provider-proxy-pod-labels": f"{LABEL_ROLE}=provider-auth-proxy",
        "acp-provider-proxy-token-file": f"{AUTH_ROOT}/provider-auth-proxy/token",
    }
    manager["args"] = [a for a in manager.get("args", []) if a.removeprefix("--").split("=", 1)[0] not in flags]
    manager["args"].extend(f"--{name}={value}" for name, value in flags.items())
    variables = {
        "ORKA_ACP_ARTIFACT_ROOT": "/data/acp-artifacts",
        "ORKA_ACP_ARTIFACT_CAPABILITY_SECRET_FILE": f"{AUTH_ROOT}/acp-artifact-capability/capability-secret",
        "ORKA_WORKSPACE_PUBLISHER_URL": f"http://workspace-publisher.{NS}.svc:8080",
        "ORKA_WORKSPACE_PUBLISHER_CONTROLLER_TOKEN_FILE": f"{AUTH_ROOT}/workspace-publisher-auth/controller-token",
        "ORKA_WORKSPACE_PUBLISHER_CAPABILITY_SECRET_FILE": f"{AUTH_ROOT}/workspace-publisher-auth/operation-capability-secret",
    }
    manager["env"] = [e for e in manager.get("env", []) if e["name"] not in variables] + env(variables)
    auth = {"provider-auth-proxy": ["token"], "acp-artifact-capability": ["capability-secret"],
            "workspace-publisher-auth": ["controller-token", "operation-capability-secret"]}
    spec["volumes"] = [v for v in spec["volumes"] if v["name"] not in {*auth, "data"}]
    spec["volumes"].append({"name": "data", "persistentVolumeClaim": {"claimName": DATA_PVC}})
    manager["volumeMounts"] = [m for m in manager["volumeMounts"] if m["name"] not in auth]
    for name, keys in auth.items():
        spec["volumes"].append(secret_volume(name, keys))
        manager["volumeMounts"].extend(secret_mount(name, key) for key in keys)
    return template


def configure_controller(kube: prepare.Kubernetes, state: dict) -> None:
    current = check_target(kube)
    desired = controller_template(current)
    if desired == current["spec"]["template"]:
        return
    migration = state.get("migration", {})
    if not migration.get("restoredVerified"):
        raise prepare.SetupError("Controller replacement requires verified persistent data")
    inventory = task_inventory(kube)
    volume = next(v for v in current["spec"]["template"]["spec"]["volumes"] if v["name"] == "data")
    already_persistent = volume.get("persistentVolumeClaim", {}).get("claimName") == DATA_PVC
    if not already_persistent and inventory != migration.get("sourceTasks"):
        raise prepare.SetupError("Task inventory changed after the data copy; refresh migration first")
    patch = [{"op": "test", "path": "/metadata/resourceVersion", "value": current["metadata"]["resourceVersion"]},
             {"op": "replace", "path": "/spec/template", "value": desired}]
    kube.run(["patch", "deployment", "orka-controller", "-n", NS, "--type=json", "--patch-file=/dev/stdin"], patch)
    state["controller"] = {"uid": current["metadata"]["uid"], "image": prepare.IMAGES["CONTROLLER_IMAGE"],
                            "templateDigest": prepare.digest(desired), "codexImage": CODEX_IMAGE}
    save(state)


def verify(kube: prepare.Kubernetes, state: dict) -> None:
    current = check_target(kube)
    if controller_template(current) != current["spec"]["template"]:
        raise prepare.SetupError("Controller runtime wiring does not match the requested configuration")
    for name in ("provider-auth-proxy", "scm-egress-proxy", "workspace-publisher", "orka-controller"):
        kube.wait(NS, name, seconds=300)
    old = {obj["uid"]: obj for obj in state.get("migration", {}).get("sourceTasks", [])}
    actual = {obj["metadata"]["uid"]: obj for obj in kube.get("tasks.core.orka.ai", namespace=NS)["items"]}
    if not old or any(uid not in actual or actual[uid].get("status", {}).get("phase") != item["phase"] for uid, item in old.items()):
        raise prepare.SetupError("Previously recorded Tasks were not preserved")
    pod = controller_pod(kube)
    state["readiness"] = {"controllerPod": pod["metadata"]["name"], "controllerPodUID": pod["metadata"]["uid"],
        "preservedTasks": len(old), "deploymentsReady": True, "modelTrafficValidated": False,
        "runtimeContract": "orka.harness.v2", "publicationSecret": PUBLICATION_SECRET}
    save(state)
    prepare.message(f"Payments runtime services are ready; {len(old)} prior Task records and their data were preserved")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", default="sertac-aks")
    parser.add_argument("command", choices=("render-safe", "apply", "verify"))
    args = parser.parse_args()
    if args.command == "render-safe":
        print(json.dumps({"apiVersion": "v1", "kind": "List", "items": documents()}, indent=2))
        return 0
    kube = prepare.Kubernetes(args.context)
    check_target(kube)
    state = json.loads(STATE.read_text()) if STATE.exists() else {"resources": {}, "created_resources": {}}
    state["context"], state["namespace"] = args.context, NS
    before = preservation(kube)
    state["preservationBefore"] = before
    save(state)
    try:
        if args.command == "apply":
            task_inventory(kube)
            ensure_secrets(kube, state)
            for obj in documents():
                kube.apply(obj, state)
            for name in ("provider-auth-proxy", "scm-egress-proxy", "workspace-publisher"):
                kube.wait(NS, name, seconds=300)
            migrate_data(kube, state)
            configure_controller(kube, state)
        verify(kube, state)
    finally:
        after = preservation(kube)
        state["preservationAfter"] = after
        state["protectedResourcesUnchanged"] = before == after
        save(state)
        if before != after:
            raise prepare.SetupError("Protected-resource fingerprints changed; inspect the safe setup receipt")
    prepare.message("Original credentials and shared services are unchanged; no model request or branch publication was performed")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except prepare.SetupError as error:
        print(f"Runtime setup stopped: {error}", file=sys.stderr)
        raise SystemExit(1) from None
