#!/usr/bin/env python3
"""Task-owned setup for the hackathon recording on sertac-aks.

No credentials are printed or written into source. Existing installations and
unlabelled resources are never replaced. Generated state stays under bin/.
"""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
STATE = ROOT / "bin/hackathon-first-pass/state"
CONTEXT = "sertac-aks"
SERVER = "https://sertac-aks-sertac-aks-9d9ce9-qmrwnhf8.hcp.westus2.azmk8s.io:443"
LABEL = {"demo.orka.ai/name": "06-hackathon"}
RELIABILITY = "orka-system"
ENGINEERING = "orka-pr647-system"
SCAN = "hackathon-nodejs-goof"
SCHEDULE = "hackathon-scheduled-scan"
ROLES = ["source-read", "target-read", "publication", "forge"]
PYTHON = "docker.io/library/python:3.12-slim@sha256:2fe5997d249a808b8eeea52c58a1dbffbba28754dc11699ef5c029f2d818ce79"


def run(args, data=None, check=True):
    result = subprocess.run(args, input=data, text=True, capture_output=True)
    if check and result.returncode:
        # stdin may contain a Secret. Never echo commands, stdin, or raw errors.
        raise RuntimeError(f"{args[0]} operation failed, exit {result.returncode}")
    return result


def kube(*args, data=None, check=True):
    return run(["kubectl", "--context", CONTEXT, *args], data, check)


def verify():
    config = json.loads(kube("config", "view", "-o", "json").stdout)
    context = next(x for x in config["contexts"] if x["name"] == CONTEXT)
    cluster = next(x for x in config["clusters"] if x["name"] == context["context"]["cluster"])
    if cluster["cluster"]["server"] != SERVER:
        raise RuntimeError("sertac-aks endpoint did not match the verified target")
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(STATE, 0o700)


def obj(kind, name, namespace, **fields):
    api = "v1" if kind in {"Secret", "ServiceAccount", "ConfigMap"} else (
        "rbac.authorization.k8s.io/v1" if kind in {"Role", "RoleBinding"}
        else "core.orka.ai/v1alpha1")
    return {"apiVersion": api, "kind": kind,
            "metadata": {"name": name, "namespace": namespace, "labels": LABEL}, **fields}


def apply(resource):
    meta = resource["metadata"]
    old = kube("-n", meta["namespace"], "get", resource["kind"], meta["name"],
               "-o", "json", "--ignore-not-found")
    if old.stdout.strip():
        previous = json.loads(old.stdout)
        if previous["metadata"].get("labels", {}).get("demo.orka.ai/name") != "06-hackathon":
            raise RuntimeError(f"Refusing to change existing {resource['kind']}/{meta['name']}")
        resource["metadata"]["resourceVersion"] = previous["metadata"]["resourceVersion"]
        verb = "replace"
    else:
        verb = "create"
    kube(verb, "-f", "-", data=json.dumps(resource))
    print(f"{resource['kind']}/{meta['name']} ready in {meta['namespace']}")


def identity(namespace, name, rules):
    apply(obj("ServiceAccount", name, namespace))
    apply(obj("Role", name, namespace, rules=rules))
    apply(obj("RoleBinding", name, namespace,
              roleRef={"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
              subjects=[{"kind": "ServiceAccount", "name": name, "namespace": namespace}]))
    return kube("-n", namespace, "create", "token", name, "--duration=4h").stdout.strip()


def private_file(path, contents):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as stream:
        stream.write(contents)
    os.chmod(path, 0o600)


def setup():
    verify()
    for team, namespace in [("reliability", RELIABILITY), ("engineering", ENGINEERING)]:
        rules = [
            {"apiGroups": ["core.orka.ai"], "resources": ["tasks", "sessions"],
             "verbs": ["get", "list", "watch", "create"]},
            {"apiGroups": ["core.orka.ai"], "resources": ["agents", "providers", "runtimepools", "repositorymonitors"],
             "verbs": ["get", "list"]},
            {"apiGroups": ["core.orka.ai"], "resources": ["repositoryscans", "repositoryscans/scans",
             "repositoryscans/findings", "repositoryscans/threatmodel", "repositoryscans/slices",
             "repositoryscans/droppedfindings", "securityfindings", "securityfindings/validation"],
             "verbs": ["get", "list"]},
        ]
        if team == "engineering":
            rules.append({"apiGroups": [""], "resources": ["secrets"],
                          "resourceNames": [f"hackathon-github-{role}" for role in ROLES], "verbs": ["get"]})
        else:
            rules.append({"apiGroups": ["gateway.orka.ai"],
                          "resources": ["gateways", "gatewaybindings", "gatewayevents", "gatewaydeliveries"],
                          "verbs": ["get", "list"]})
        token = identity(namespace, f"hackathon-{team}", rules)
        token_path = STATE / f"{team}.token"
        private_file(token_path, token)
        config = {"apiVersion": "v1", "kind": "Config", "current-context": team,
                  "clusters": [{"name": team, "cluster": {"server": SERVER}}],
                  "contexts": [{"name": team, "context": {"cluster": team, "user": team, "namespace": namespace}}],
                  "users": [{"name": team, "user": {"tokenFile": str(token_path)}}]}
        private_file(STATE / f"{team}.kubeconfig", json.dumps(config))
        agent = obj("Agent", f"hackathon-{team}", namespace,
                    spec={"model": {"name": "gpt-5.4-mini"}, "runtime": {
                        "type": "codex", "contractVersion": "orka.harness.v2",
                        "defaultMaxTurns": 60, "defaultReasoningEffort": "medium"}})
        apply(agent)
    # Four explicit credential references. The demo uses the presenter's existing
    # GitHub identity for each role. The ACP child receives none of them.
    gh_token = run(["gh", "auth", "token"]).stdout.strip()
    if not gh_token:
        raise RuntimeError("No existing GitHub authentication is available")
    for role in ROLES:
        apply(obj("Secret", f"hackathon-github-{role}", ENGINEERING,
                  type="Opaque", stringData={"token": gh_token}))
    print("Demo identities and agents ready; existing installations unchanged.")


def schedule():
    verify()
    if kube("-n", RELIABILITY, "get", "task", SCHEDULE, "--ignore-not-found", "-o", "name").stdout.strip():
        raise RuntimeError("This schedule already exists; inspect it instead of replacing its history")
    token = identity(RELIABILITY, "hackathon-scheduler", [
        {"apiGroups": ["core.orka.ai"], "resources": ["repositoryscans"], "verbs": ["get", "create"]}])
    apply(obj("Secret", "hackathon-scheduler-token", RELIABILITY,
              type="Opaque", stringData={"token": token}))
    sha = json.loads(run(["gh", "api", "repos/sozercan/nodejs-goof/commits/main"]).stdout)["sha"]
    scan = obj("RepositoryScan", SCAN, RELIABILITY, spec={
        "provider": "github", "repoURL": "https://github.com/sozercan/nodejs-goof",
        "branch": "main", "ref": sha, "validationMode": "light",
        "validationMaxFindingsPerRun": 2, "maxFindingsPerRun": 5,
        "analysisAgentRef": {"name": "hackathon-reliability"}})
    ca = json.loads(kube("-n", RELIABILITY, "get", "configmap", "kube-root-ca.crt", "-o", "json").stdout)["data"]["ca.crt"]
    code = '''import json, os, ssl, urllib.request, urllib.error
from datetime import datetime, timezone
payload = json.loads(os.environ["DEMO_SCAN"])
ctx = ssl.create_default_context(cadata=os.environ["K8S_CA"])
url = "https://kubernetes.default.svc/apis/core.orka.ai/v1alpha1/namespaces/orka-system/repositoryscans"
request = urllib.request.Request(url, data=json.dumps(payload).encode(), method="POST", headers={
    "Content-Type": "application/json", "Authorization": "Bearer " + os.environ["SCHEDULER_TOKEN"]})
try:
    with urllib.request.urlopen(request, context=ctx, timeout=25) as response:
        result = json.load(response)
    print("Schedule triggered RepositoryScan " + result["metadata"]["name"], flush=True)
    print("Triggered at " + datetime.now(timezone.utc).isoformat(), flush=True)
except urllib.error.HTTPError as error:
    if error.code != 409:
        raise SystemExit("Scanner registration failed, HTTP " + str(error.code))
    print("Demo scan already registered; no duplicate work.", flush=True)
'''
    task = obj("Task", SCHEDULE, RELIABILITY, spec={
        "type": "container", "image": PYTHON, "command": ["python", "-c", code],
        "schedule": "* * * * *", "timeZone": "UTC", "concurrencyPolicy": "Forbid",
        "timeout": "60s", "successfulRunsHistoryLimit": 2,
        "env": [{"name": "DEMO_SCAN", "value": json.dumps(scan)},
                {"name": "K8S_CA", "value": ca},
                {"name": "SCHEDULER_TOKEN", "valueFrom": {
                    "secretKeyRef": {"name": "hackathon-scheduler-token", "key": "token"}}}]})
    private_file(STATE / "repository-scan.json", json.dumps(scan, indent=2))
    apply(task)
    print("Waiting for the next real cron tick. No scan has been started manually.")


def suspend():
    verify()
    current = json.loads(kube("-n", RELIABILITY, "get", "task", SCHEDULE, "-o", "json").stdout)
    meta = current["metadata"]
    if meta.get("labels", {}).get("demo.orka.ai/name") != "06-hackathon":
        raise RuntimeError("Refusing to suspend a task not owned by this demo")
    patch = [
        {"op": "test", "path": "/metadata/uid", "value": meta["uid"]},
        {"op": "test", "path": "/metadata/resourceVersion", "value": meta["resourceVersion"]},
        {"op": "add", "path": "/spec/suspend", "value": True},
    ]
    kube("-n", RELIABILITY, "patch", "task", SCHEDULE, "--type=json", "-p", json.dumps(patch))
    print("Paused future demo ticks; the active scanner continues.")


def access():
    """Add only the read permissions needed by the demo bridge and usage view."""
    verify()
    for team, namespace in [("reliability", RELIABILITY), ("engineering", ENGINEERING)]:
        role = json.loads(kube("-n", namespace, "get", "role", f"hackathon-{team}", "-o", "json").stdout)
        extra = [{"apiGroups": ["core.orka.ai"], "resources": ["repositorymonitors"], "verbs": ["get", "list"]}]
        if team == "reliability":
            extra.append({"apiGroups": ["gateway.orka.ai"],
                          "resources": ["gateways", "gatewaybindings", "gatewayevents", "gatewaydeliveries"],
                          "verbs": ["get", "list"]})
        for rule in extra:
            if rule not in role["rules"]:
                role["rules"].append(rule)
        apply(role)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["setup", "schedule", "suspend", "access"])
    args = parser.parse_args()
    try:
        {"setup": setup, "schedule": schedule, "suspend": suspend, "access": access}[args.action]()
    except (RuntimeError, OSError, KeyError, StopIteration) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
