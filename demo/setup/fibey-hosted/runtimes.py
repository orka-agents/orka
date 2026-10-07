#!/usr/bin/env python3
"""Render and register demo 13's two Fibey runtimes.

Adapted from Orka's scripts/fixtures/human-approval-v2/render_runtimes.py and
e2e.py. Real transports replace the fixtures: the Orka-hosted Fibey reaches
the model through Orka's provider proxy, and the Foundry bridge calls the
Foundry Hosted Agent with AKS workload identity.

  runtimes.py render   --setup DIR --epoch N --identity-client-id ID > manifests.json
  runtimes.py register --setup DIR --provider agentkit|foundry --capabilities caps.json
"""

import argparse
import hashlib
import json
import subprocess
import sys
from pathlib import Path

NAMESPACE = "orka-system"
DEMO_LABEL = {"demo.orka.ai/name": "13-fibey-hosted-approval"}
PROXY_URL = "http://orka-provider-auth-proxy.orka-system.svc:8080/v1"
CONTROLLER_URL = "http://orka-api.orka-system.svc:8080"
FOUNDRY_SERVICE_ACCOUNT = "fibey-hosted-foundry"
NAMES = {"agentkit": "fibey-on-aks", "foundry": "fibey-on-foundry"}


def env(values):
    return [{"name": key, "value": str(value)} for key, value in sorted(values.items())]


def secret_env(name, secret, key):
    return {"name": name, "valueFrom": {"secretKeyRef": {"name": secret, "key": key}}}


def mount(name, path, readonly=False):
    return {"name": name, "mountPath": path, **({"readOnly": True} if readonly else {})}


def empty(name, limit):
    return {"name": name, "emptyDir": {"sizeLimit": limit}}


def security(uid, capabilities=()):
    result = {
        "runAsUser": uid, "runAsGroup": uid, "runAsNonRoot": uid != 0,
        "allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "privileged": False,
        "capabilities": {"drop": ["ALL"]}, "seccompProfile": {"type": "RuntimeDefault"},
    }
    if capabilities:
        result["capabilities"]["add"] = list(capabilities)
    return result


def resources(cpu, memory, cpu_limit, memory_limit):
    return {"requests": {"cpu": cpu, "memory": memory}, "limits": {"cpu": cpu_limit, "memory": memory_limit}}


def probes(handler):
    return {
        "startupProbe": {**handler, "failureThreshold": 60, "periodSeconds": 2, "timeoutSeconds": 3},
        "readinessProbe": {**handler, "failureThreshold": 3, "periodSeconds": 5, "timeoutSeconds": 3},
        "livenessProbe": {**handler, "failureThreshold": 3, "periodSeconds": 10, "timeoutSeconds": 3},
    }


def runtime(provider, setup, epoch, client_id):
    name = NAMES[provider]
    secret = name + "-auth"
    profile = json.loads((setup / f"{provider}-profile.json").read_text())
    images = json.loads((setup / "images.json").read_text())
    values = dict(profile["supervisorEnv"])
    values.update({
        "ORKA_ACP_LISTEN_ADDRESS": ":8080",
        "ORKA_ACP_CONTROLLER_EPOCH": epoch,
        "ORKA_ACP_RUNTIME_INSTANCE_ID": name + "-instance",
        "ORKA_ACP_RUNTIME_POOL_UID": name + "-external-pool",
        "ORKA_ACP_RUNTIME_POOL_GENERATION": "1",
        "ORKA_ACP_BROKERED_TOOL_APPROVAL_PROFILE_DIGEST": profile["profile"]["digest"],
        "ORKA_ACP_CONTROLLER_TOKEN_FILE": "/var/run/secrets/orka/auth/controller-token",
        "ORKA_ACP_CAPABILITY_SECRET_FILE": "/var/run/secrets/orka/auth/capability-secret",
        "ORKA_ACP_PROVIDER_TOKEN_FILE": "/var/run/secrets/orka/provider/provider-token",
        "ORKA_ACP_PROVIDER_PROXY_BASE_URL": PROXY_URL if provider == "agentkit" else "http://127.0.0.1:8091/v1",
        "ORKA_ACP_ARTIFACT_API_URL": CONTROLLER_URL,
        "ORKA_ACP_MCP_BROKER_URL": CONTROLLER_URL,
        "ORKA_ACP_WORKSPACE_MAX_ARTIFACT_BYTES": "104857600",
        "ORKA_ACP_TRUST_NAMESPACE": NAMESPACE,
        "ORKA_ACP_SESSION_BASE_DIR": "/sessions",
    })
    if provider == "foundry":
        values["ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST"] = profile["profile"]["digest"]
    supervisor_env = env(values)
    for key, field in (("UID", "uid"), ("NAME", "name"), ("NAMESPACE", "namespace")):
        supervisor_env.append({"name": f"ORKA_ACP_POD_{key}", "valueFrom": {"fieldRef": {"fieldPath": "metadata." + field}}})
    supervisor = {
        "name": "supervisor" if provider == "foundry" else "runtime",
        "image": images[provider],
        "imagePullPolicy": "IfNotPresent",
        "env": supervisor_env,
        "ports": [{"name": "control", "containerPort": 8080}],
        "securityContext": security(0, ("CHOWN", "KILL", "SETGID", "SETUID")),
        "resources": resources("100m", "256Mi", "1", "2Gi"),
        "volumeMounts": [
            mount("supervisor-auth", "/var/run/secrets/orka/auth", True),
            mount("supervisor-provider", "/var/run/secrets/orka/provider", True),
            mount("supervisor-sessions", "/sessions"),
            mount("supervisor-tmp", "/tmp"),
            mount("supervisor-home", "/home/worker"),
        ],
        **probes({"httpGet": {"path": "/v2/health", "port": "control"}}),
    }
    volumes = [
        {"name": "supervisor-auth", "secret": {"secretName": secret, "defaultMode": 0o400,
          "items": [{"key": key, "path": key} for key in ("controller-token", "capability-secret")]}},
        {"name": "supervisor-provider", "secret": {"secretName": secret, "defaultMode": 0o400,
          "items": [{"key": "provider-token", "path": "provider-token"}]}},
        empty("supervisor-sessions", "4Gi"), empty("supervisor-tmp", "512Mi"), empty("supervisor-home", "256Mi"),
    ]
    containers = [supervisor]
    labels = {"app.kubernetes.io/name": name, "app.kubernetes.io/part-of": "fibey-hosted-approval", **DEMO_LABEL}
    pod_labels = dict(labels)
    pod = {
        "automountServiceAccountToken": False,
        "enableServiceLinks": False,
        "shareProcessNamespace": False,
        "nodeSelector": {"kubernetes.io/os": "linux"},
        "securityContext": {"seccompProfile": {"type": "RuntimeDefault"}},
        "terminationGracePeriodSeconds": 120,
        "containers": containers,
        "volumes": volumes,
    }
    items = []
    if provider == "foundry":
        config = (setup / "context" / "foundry.json").read_bytes()
        if "sha256:" + hashlib.sha256(config).hexdigest() != profile["profile"]["agentConfigurationDigest"]:
            raise SystemExit("Foundry configuration bytes differ from the generated profile")
        model = json.loads(config)["model"]
        containers.append({
            "name": "broker",
            "image": images["foundry-base"],
            "imagePullPolicy": "IfNotPresent",
            "args": ["--protocol", "broker", "--config", "/agent/foundry.json"],
            "env": env({
                "ORKA_FOUNDRY_BROKER_ADDR": "127.0.0.1:8091",
                "ORKA_FOUNDRY_BROKER_STATE_DIR": "/broker-state/ledger",
                "ORKA_FOUNDRY_ACP_MODEL": model,
                "ORKA_FOUNDRY_ACP_AGENT_CONFIGURATION_DIGEST": profile["profile"]["agentConfigurationDigest"],
            }) + [
                secret_env("ORKA_FOUNDRY_BROKER_BEARER_TOKEN", secret, "provider-token"),
                secret_env("ORKA_FOUNDRY_BROKER_AGENTKIT_CONTINUATION_PROOF", secret, "continuation-proof"),
            ],
            "securityContext": security(65532),
            "resources": resources("50m", "64Mi", "1", "256Mi"),
            "volumeMounts": [mount("broker-state", "/broker-state")],
            **probes({"exec": {"command": ["/agent-runtime-foundry", "--protocol", "broker", "--health-check"]}}),
        })
        volumes.append(empty("broker-state", "64Mi"))
        # The broker reaches Foundry as this workload identity; the supervisor
        # and ACP child never hold an Azure credential.
        pod["serviceAccountName"] = FOUNDRY_SERVICE_ACCOUNT
        pod_labels["azure.workload.identity/use"] = "true"
        items.append({"apiVersion": "v1", "kind": "ServiceAccount", "automountServiceAccountToken": False,
                      "metadata": {"name": FOUNDRY_SERVICE_ACCOUNT, "namespace": NAMESPACE, "labels": dict(DEMO_LABEL),
                                   "annotations": {"azure.workload.identity/client-id": client_id}}})
    else:
        # The provider proxy admits orka-runtimes Pods by default; admit this
        # external runtime too so its model calls still pass the proxy's bearer check.
        items.append({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                      "metadata": {"name": name + "-provider-client", "namespace": NAMESPACE, "labels": dict(DEMO_LABEL)},
                      "spec": {"podSelector": {"matchLabels": {"orka.ai/network-role": "provider-auth-proxy"}},
                               "policyTypes": ["Ingress"],
                               "ingress": [{"from": [{"podSelector": {"matchLabels": {"app.kubernetes.io/name": name}}}],
                                            "ports": [{"port": 8080, "protocol": "TCP"}]}]}})
    items.append({"apiVersion": "v1", "kind": "Service",
                  "metadata": {"name": name, "namespace": NAMESPACE, "labels": dict(DEMO_LABEL)},
                  "spec": {"type": "ClusterIP", "selector": {"app.kubernetes.io/name": name},
                           "ports": [{"name": "control", "port": 8080, "targetPort": "control", "protocol": "TCP"}]}})
    items.append({"apiVersion": "apps/v1", "kind": "Deployment",
                  "metadata": {"name": name, "namespace": NAMESPACE, "labels": labels},
                  "spec": {"replicas": 1, "strategy": {"type": "Recreate"}, "revisionHistoryLimit": 1,
                           "progressDeadlineSeconds": 300,
                           "selector": {"matchLabels": {"app.kubernetes.io/name": name}},
                           "template": {"metadata": {"labels": pod_labels}, "spec": pod}}})
    return items


def registration(provider, setup, caps):
    name = NAMES[provider]
    profile = json.loads((setup / f"{provider}-profile.json").read_text())
    if caps["runtimeProfileDigest"] != profile["profile"]["digest"] or \
            not caps["provider"]["supportsBrokeredToolApprovals"] or caps["provider"]["supportsPermissions"]:
        raise SystemExit(f"{name} did not advertise the qualified approval profile")
    governance = dict(caps["workspaceGovernance"])
    governance.setdefault("trusted", False)
    return {"apiVersion": "core.orka.ai/v1alpha1", "kind": "AgentRuntime",
            "metadata": {"name": name + "-runtime", "namespace": NAMESPACE, "labels": dict(DEMO_LABEL)},
            "spec": {
                "contractVersion": "orka.harness.v2",
                "deployment": {"mode": "external-endpoint",
                               "endpoint": f"http://{name}.{NAMESPACE}.svc.cluster.local:8080"},
                "clientAuth": {"controllerBearerTokenSecretRef": {"name": name + "-auth", "key": "controller-token"},
                               "operationCapabilitySecretRef": {"name": name + "-auth", "key": "capability-secret"}},
                "capabilities": {"runtimeInstanceID": name + "-instance",
                                 "profile": profile["profile"], "mcpPolicy": profile["mcpPolicy"],
                                 "limits": caps["limits"], "supportsDrain": caps.get("supportsDrain", False),
                                 "supportsPublicationFinalization": caps.get("supportsPublicationFinalization", False),
                                 "workspaceGovernance": governance}}}


def secret(name, runtime, source, proof):
    import base64
    import secrets as tokens
    data = {key: tokens.token_urlsafe(48) for key in ("controller-token", "capability-secret")}
    if source == "proxy":
        raw = subprocess.run(["kubectl", "-n", NAMESPACE, "get", "secret", "provider-auth-proxy", "-o", "json"],
                             check=True, capture_output=True, text=True).stdout
        data["provider-token"] = base64.b64decode(json.loads(raw)["data"]["token"]).decode()
    else:
        data["provider-token"] = tokens.token_urlsafe(48)
        data["continuation-proof"] = Path(proof).read_text()
    return {"apiVersion": "v1", "kind": "Secret", "metadata": {
        "name": name + "-auth", "namespace": NAMESPACE,
        "labels": {"orka.ai/agent-runtime-auth": "true", "orka.ai/agent-runtime-name": runtime, **DEMO_LABEL},
        "annotations": {"orka.ai/agent-runtime-endpoint": f"http://{name}.{NAMESPACE}.svc.cluster.local:8080"}},
        "stringData": data}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    secret_parser = sub.add_parser("secret")
    secret_parser.add_argument("--name", required=True)
    secret_parser.add_argument("--runtime", required=True)
    secret_parser.add_argument("--source", choices=("proxy", "random"), required=True)
    secret_parser.add_argument("--proof", type=Path, required=True)
    render = sub.add_parser("render")
    render.add_argument("--setup", type=Path, required=True)
    render.add_argument("--epoch", required=True)
    render.add_argument("--identity-client-id", required=True)
    register = sub.add_parser("register")
    register.add_argument("--setup", type=Path, required=True)
    register.add_argument("--provider", choices=sorted(NAMES), required=True)
    register.add_argument("--capabilities", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "secret":
        json.dump(secret(args.name, args.runtime, args.source, args.proof), sys.stdout)
    elif args.command == "render":
        if not args.epoch.isdecimal() or int(args.epoch) <= 0:
            parser.error("controller epoch must be a positive integer")
        items = []
        for provider in ("agentkit", "foundry"):
            items.extend(runtime(provider, args.setup, args.epoch, args.identity_client_id))
        json.dump({"apiVersion": "v1", "kind": "List", "items": items}, sys.stdout, indent=2)
    else:
        caps = json.loads(args.capabilities.read_text())
        json.dump(registration(args.provider, args.setup, caps), sys.stdout, indent=2)


if __name__ == "__main__":
    main()
