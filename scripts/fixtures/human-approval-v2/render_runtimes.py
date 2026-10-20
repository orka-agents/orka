#!/usr/bin/env python3
"""Render disposable runtime manifests without reading Secrets or contacting Kubernetes."""

import argparse
import hashlib
import json
from pathlib import Path
from urllib.parse import urlsplit


NAMESPACE = "orka-system"
SETUP = None


def environment(values):
    return [{"name": key, "value": str(value)} for key, value in sorted(values.items())]


def secret_environment(name, secret, key):
    return {"name": name, "valueFrom": {"secretKeyRef": {"name": secret, "key": key}}}


def mount(name, path, readonly=False):
    result = {"name": name, "mountPath": path}
    if readonly:
        result["readOnly"] = True
    return result


def empty_volume(name, limit):
    return {"name": name, "emptyDir": {"sizeLimit": limit}}


def security(uid, capabilities=()):
    result = {
        "runAsUser": uid,
        "runAsGroup": uid,
        "runAsNonRoot": uid != 0,
        "allowPrivilegeEscalation": False,
        "readOnlyRootFilesystem": True,
        "privileged": False,
        "capabilities": {"drop": ["ALL"]},
        "seccompProfile": {"type": "RuntimeDefault"},
    }
    if capabilities:
        result["capabilities"]["add"] = list(capabilities)
    return result


def resources(cpu="50m", memory="64Mi", cpu_limit="500m", memory_limit="256Mi"):
    return {
        "requests": {"cpu": cpu, "memory": memory},
        "limits": {"cpu": cpu_limit, "memory": memory_limit},
    }


def probes(handler):
    return {
        "startupProbe": {**handler, "failureThreshold": 60, "periodSeconds": 2, "timeoutSeconds": 3},
        "readinessProbe": {**handler, "failureThreshold": 3, "periodSeconds": 5, "timeoutSeconds": 3},
        "livenessProbe": {**handler, "failureThreshold": 3, "periodSeconds": 10, "timeoutSeconds": 3},
    }


def local_probe(port, path, python="python", ca_file=None):
    scheme = "https" if ca_file else "http"
    context = f", context=ssl.create_default_context(cafile={ca_file!r})" if ca_file else ""
    code = f"import ssl, urllib.request\nwith urllib.request.urlopen('{scheme}://127.0.0.1:{port}{path}', timeout=2{context}) as response:\n    assert response.status == 200\n"
    return {"exec": {"command": [python, "-c", code]}}


def owner_init(image, targets):
    # Fixed volume roots only. Taking root ownership first makes a repeated
    # init safe without CAP_FOWNER, recursive traversal, or shared fsGroup.
    paths = [(path, uid, mode) for _, path, uid, mode in targets]
    code = (
        "import os, stat\n"
        f"for path, uid, mode in {paths!r}:\n"
        "    assert stat.S_ISDIR(os.lstat(path).st_mode)\n"
        "    os.chown(path, 0, 0, follow_symlinks=False)\n"
        "    os.chmod(path, mode, follow_symlinks=False)\n"
        "    os.chown(path, uid, uid, follow_symlinks=False)\n"
    )
    return {
        "name": "prepare-private-directories",
        "image": image,
        "imagePullPolicy": "IfNotPresent",
        "command": ["python", "-c", code],
        "securityContext": security(0, ("CHOWN",)),
        "resources": resources("10m", "16Mi", "100m", "64Mi"),
        "volumeMounts": [mount(name, path) for name, path, _, _ in targets],
    }


def deployment(name, containers, volumes=(), init=()):
    labels = {"app.kubernetes.io/name": name, "app.kubernetes.io/part-of": "human-approval-v2"}
    pod = {
        "automountServiceAccountToken": False,
        "enableServiceLinks": False,
        "shareProcessNamespace": False,
        "nodeSelector": {"kubernetes.io/os": "linux"},
        "securityContext": {"seccompProfile": {"type": "RuntimeDefault"}},
        "terminationGracePeriodSeconds": 120,
        "containers": containers,
    }
    if volumes:
        pod["volumes"] = list(volumes)
    if init:
        pod["initContainers"] = list(init)
    return {
        "apiVersion": "apps/v1",
        "kind": "Deployment",
        "metadata": {"name": name, "namespace": NAMESPACE, "labels": labels},
        "spec": {
            "replicas": 1,
            "strategy": {"type": "Recreate"},
            "revisionHistoryLimit": 1,
            "progressDeadlineSeconds": 300,
            "selector": {"matchLabels": {"app.kubernetes.io/name": name}},
            "template": {"metadata": {"labels": labels}, "spec": pod},
        },
    }


def service(name, port, target="http"):
    return {
        "apiVersion": "v1",
        "kind": "Service",
        "metadata": {"name": name, "namespace": NAMESPACE},
        "spec": {
            "type": "ClusterIP",
            "selector": {"app.kubernetes.io/name": name},
            "ports": [{"name": target, "port": port, "targetPort": target, "protocol": "TCP"}],
        },
    }


def fixture_resources(fixture):
    model = {
        "name": "model",
        "image": fixture,
        "imagePullPolicy": "IfNotPresent",
        "args": ["model", "--host", "0.0.0.0", "--port", "8100"],
        "ports": [{"name": "http", "containerPort": 8100}],
        "securityContext": security(1000),
        "resources": resources(),
        **probes({"httpGet": {"path": "/health", "port": "http"}}),
    }
    tools = {
        "name": "tools",
        "image": fixture,
        "imagePullPolicy": "IfNotPresent",
        "command": ["python", "/fixture/held_tools.py"],
        "args": ["--host", "0.0.0.0", "--port", "8099", "--state", "/state/simulator.sqlite"],
        "ports": [{"name": "http", "containerPort": 8099}],
        "securityContext": security(1000),
        "resources": resources(),
        "volumeMounts": [mount("simulator-state", "/state")],
        **probes({"httpGet": {"path": "/health", "port": "http"}}),
    }
    return [
        service("human-approval-model", 8100),
        deployment("human-approval-model", [model]),
        service("human-approval-tools", 8099),
        deployment("human-approval-tools", [tools], [empty_volume("simulator-state", "64Mi")],
                   [owner_init(fixture, [("simulator-state", "/state", 1000, 0o700)])]),
    ]


def runtime_resources(provider, profile, supervisor_image, images, epoch, controller_url):
    name = f"human-approval-{provider}"
    secret = name + "-auth"
    values = dict(profile["supervisorEnv"])
    values.update({
        "ORKA_ACP_LISTEN_ADDRESS": ":8080",
        "ORKA_ACP_CONTROLLER_EPOCH": epoch,
        # The registration survives process and Pod replacement. The separate
        # supervisor boot ID remains generated by the runtime on every start.
        "ORKA_ACP_RUNTIME_INSTANCE_ID": name + "-instance",
        "ORKA_ACP_RUNTIME_POOL_UID": name + "-external-pool",
        "ORKA_ACP_RUNTIME_POOL_GENERATION": "1",
        "ORKA_ACP_BROKERED_TOOL_APPROVAL_PROFILE_DIGEST": profile["profile"]["digest"],
        "ORKA_ACP_CONTROLLER_TOKEN_FILE": "/var/run/secrets/orka/auth/controller-token",
        "ORKA_ACP_CAPABILITY_SECRET_FILE": "/var/run/secrets/orka/auth/capability-secret",
        "ORKA_ACP_PROVIDER_TOKEN_FILE": "/var/run/secrets/orka/provider/provider-token",
        "ORKA_ACP_PROVIDER_PROXY_BASE_URL": "http://human-approval-model:8100/v1" if provider == "agentkit" else "http://127.0.0.1:8091/v1",
        "ORKA_ACP_ARTIFACT_API_URL": controller_url,
        "ORKA_ACP_MCP_BROKER_URL": controller_url,
        "ORKA_ACP_WORKSPACE_MAX_ARTIFACT_BYTES": "104857600",
        "ORKA_ACP_TRUST_NAMESPACE": NAMESPACE,
        # The supervisor enforces 0711 on the mount root before launching a
        # child. A nested base could be replaced through a writable mount root.
        "ORKA_ACP_SESSION_BASE_DIR": "/sessions",
    })
    if provider == "foundry":
        values["ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST"] = profile["profile"]["digest"]
    env = environment(values)
    for key, field in (("UID", "uid"), ("NAME", "name"), ("NAMESPACE", "namespace")):
        env.append({"name": f"ORKA_ACP_POD_{key}", "valueFrom": {"fieldRef": {"fieldPath": "metadata." + field}}})
    supervisor = {
        "name": "supervisor" if provider == "foundry" else "runtime",
        "image": supervisor_image,
        "imagePullPolicy": "IfNotPresent",
        "env": env,
        "ports": [{"name": "control", "containerPort": 8080}],
        "securityContext": security(0, ("CHOWN", "KILL", "SETGID", "SETUID")),
        "resources": resources("250m", "512Mi", "2", "4Gi"),
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
        empty_volume("supervisor-sessions", "4Gi"),
        empty_volume("supervisor-tmp", "512Mi"),
        empty_volume("supervisor-home", "256Mi"),
    ]
    containers = [supervisor]
    result = []
    if provider == "foundry":
        foundry_config = (SETUP / "context" / "foundry.json").read_bytes()
        if "sha256:" + hashlib.sha256(foundry_config).hexdigest() != profile["profile"]["agentConfigurationDigest"]:
            raise ValueError("Foundry configuration bytes differ from the registered profile")
        broker = {
            "name": "broker",
            "image": images["foundry-base"]["image"],
            "imagePullPolicy": "IfNotPresent",
            "args": ["--protocol", "broker", "--config", "/agent/foundry.json"],
            "env": environment({
                "ORKA_FOUNDRY_BROKER_ADDR": "127.0.0.1:8091",
                "ORKA_FOUNDRY_BROKER_STATE_DIR": "/broker-state/ledger",
                "ORKA_FOUNDRY_ACP_MODEL": "approval-fixture",
                "ORKA_FOUNDRY_ACP_AGENT_CONFIGURATION_DIGEST": profile["profile"]["agentConfigurationDigest"],
                "AZURE_TOKEN_CREDENTIALS": "ManagedIdentityCredential",
                "IDENTITY_ENDPOINT": "https://human-approval-hosted:8092/metadata/identity/oauth2/token",
                "SSL_CERT_FILE": "/var/run/secrets/orka/gateway-ca/ca.crt",
            }) + [
                secret_environment("ORKA_FOUNDRY_BROKER_BEARER_TOKEN", secret, "provider-token"),
                secret_environment("ORKA_FOUNDRY_BROKER_AGENTKIT_CONTINUATION_PROOF", secret, "continuation-proof"),
                secret_environment("IDENTITY_HEADER", secret, "identity-header"),
            ],
            "securityContext": security(65532),
            "resources": resources("50m", "64Mi", "1", "256Mi"),
            # The broker creates a private 0700 ledger below the fresh writable
            # mount. The immutable configuration is already baked into its image.
            "volumeMounts": [mount("broker-state", "/broker-state"),
                             mount("gateway-ca", "/var/run/secrets/orka/gateway-ca", True)],
            **probes({"exec": {"command": ["/agent-runtime-foundry", "--protocol", "broker", "--health-check"]}}),
        }
        hosted = {
            "name": "hosted-agentkit",
            "image": images["agentkit-hosted"]["image"],
            "imagePullPolicy": "IfNotPresent",
            "env": environment({
                "AGENTKIT_FOUNDRY_BROKERED_MODEL_LOOP": "1",
                "AGENTKIT_FOUNDRY_RESPONSE_STATE_TTL_SECONDS": "1800",
                "AGENTKIT_FOUNDRY_RESPONSE_STATE_FILE": "/hosted-state/responses.json",
                "PYTHONDONTWRITEBYTECODE": "1",
            }) + [secret_environment("AGENTKIT_FOUNDRY_BROKERED_CONTINUATION_PROOF", secret, "continuation-proof")],
            "securityContext": security(1000),
            "resources": resources("100m", "256Mi", "1", "1Gi"),
            "volumeMounts": [mount("hosted-state", "/hosted-state"), mount("hosted-tmp", "/tmp")],
            **probes(local_probe(8088, "/readiness", "/opt/agentkit/bin/python")),
        }
        gateway = {
            "name": "local-hosted-transport",
            "image": images["fixture"]["image"],
            "imagePullPolicy": "IfNotPresent",

            "args": ["gateway", "--host", "0.0.0.0", "--port", "8092", "--hosted-url", "http://127.0.0.1:8088",
                     "--tls-cert", "/var/run/secrets/orka/gateway-tls/tls.crt",
                     "--tls-key", "/var/run/secrets/orka/gateway-tls/tls.key"],
            "ports": [{"name": "http", "containerPort": 8092}],
            "env": [secret_environment("IDENTITY_HEADER", secret, "identity-header")],
            "securityContext": security(1001),
            "resources": resources(),
            "volumeMounts": [mount("gateway-tls", "/var/run/secrets/orka/gateway-tls", True)],
            **probes(local_probe(8092, "/health", ca_file="/var/run/secrets/orka/gateway-tls/ca.crt")),
        }
        containers.append(broker)
        volumes.append(empty_volume("broker-state", "64Mi"))
        volumes.append({"name": "gateway-ca", "secret": {"secretName": "human-approval-gateway-tls",
            "defaultMode": 0o444, "items": [{"key": "ca.crt", "path": "ca.crt"}]}})
        # This represents the remote hosted service. Keeping its process and
        # response state outside the enrolled Pod also survives epoch rollouts.
        hosted_volumes = [empty_volume("hosted-state", "64Mi"), empty_volume("hosted-tmp", "64Mi")]
        hosted_volumes.append({"name": "gateway-tls", "secret": {"secretName": "human-approval-gateway-tls",
            "defaultMode": 0o444, "items": [{"key": key, "path": key} for key in ("ca.crt", "tls.crt", "tls.key")]}})
        hosted_targets = [
            ("hosted-state", "/hosted-state", 1000, 0o700),
            ("hosted-tmp", "/hosted-tmp", 1000, 0o700),
        ]
        result += [service("human-approval-hosted", 8092),
                   deployment("human-approval-hosted", [hosted, gateway], hosted_volumes,
                              [owner_init(images["fixture"]["image"], hosted_targets)])]
    result += [service(name, 8080, "control"), deployment(name, containers, volumes)]
    return result


def main():
    global SETUP
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--setup", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--controller-epoch", required=True)
    parser.add_argument("--controller-api-url", default="http://orka-api.orka-system.svc:8080")
    args = parser.parse_args()
    SETUP = args.setup
    if not args.controller_epoch.isdecimal() or int(args.controller_epoch) <= 0:
        parser.error("controller epoch must be a positive integer")
    parsed = urlsplit(args.controller_api_url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        parser.error("controller API URL must be an HTTP(S) origin without credentials")
    images = json.loads((SETUP / "images.json").read_text())
    composed = json.loads((SETUP / "composed-images.json").read_text())
    items = fixture_resources(images["fixture"]["image"])
    for provider in ("agentkit", "foundry"):
        profile = json.loads((SETUP / f"{provider}-profile.json").read_text())
        items.extend(runtime_resources(provider, profile, composed[provider], images, args.controller_epoch, args.controller_api_url.rstrip("/")))
    result = {"apiVersion": "v1", "kind": "List", "items": items}
    destination = args.output
    destination.write_text(json.dumps(result, indent=2) + "\n")
    print(f"Wrote {len(items)} resources to {destination.name}; no cluster operations performed.")


if __name__ == "__main__":
    main()
