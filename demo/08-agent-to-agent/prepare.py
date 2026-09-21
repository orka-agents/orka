#!/usr/bin/env python3
"""Render this demo's resources and a narrowly scoped controller CA patch."""

import argparse
import json
from pathlib import Path, PurePosixPath
import re
import sys


DEMO = "08-agent-to-agent"
ADAPTER = "demo-a2a-adapter"
GATEWAY = "demo-a2a"
AGENT = "demo-a2a-inventory"
CA_PATH = "/etc/orka-demo-a2a-ca"


def require(condition, message):
    if not condition:
        raise ValueError(message)


def flag(args, name, default=None):
    values = []
    for index, arg in enumerate(args):
        if arg.startswith(name + "="):
            values.append(arg.split("=", 1)[1])
        elif arg == name and index + 1 < len(args):
            values.append(args[index + 1])
    require(len(values) <= 1, f"ambiguous controller flag {name}")
    return values[0] if values else default


def controller_patch(deployment, service, namespace, container_name):
    """Read the deployment in memory; never save its arbitrary env values."""
    spec = deployment["spec"]["template"]["spec"]
    containers = [c for c in spec["containers"] if c["name"] == container_name]
    require(len(containers) == 1, "the selected controller container does not exist")
    container = containers[0]
    args = container.get("args", [])
    require(flag(args, "--watch-namespace") == namespace,
            "controller must watch exactly the demo namespace")
    require(flag(args, "--gateway-enabled", "true") == "true", "gateways are disabled")
    require(flag(args, "--controller-mode") == "harness-v2",
            "this demo requires the harness-v2 controller and a working Codex runtime")
    store = PurePosixPath(flag(args, "--store-path", "/data/orka.db"))
    volumes = {v["name"]: v for v in spec.get("volumes", [])}
    mounts = container.get("volumeMounts", [])
    persistent = [m for m in mounts if "persistentVolumeClaim" in volumes.get(m["name"], {})
                  and PurePosixPath(m["mountPath"]) in store.parents and not m.get("subPath")]
    require(len(persistent) == 1, "the gateway database must be on a mounted PVC")
    selector = deployment["spec"]["selector"].get("matchLabels", {})
    require(bool(selector) and not deployment["spec"]["selector"].get("matchExpressions"),
            "controller needs a simple, nonempty Pod selector")
    pod_labels = deployment["spec"]["template"]["metadata"].get("labels", {})
    svc_selector = service["spec"].get("selector", {})
    require(bool(svc_selector) and all(pod_labels.get(k) == v for k, v in svc_selector.items()),
            "the Orka API Service must select this controller")
    require(any(p.get("port") == 8080 for p in service["spec"].get("ports", [])),
            "the Orka API Service must expose port 8080")
    volume = {"name": "demo-a2a-ca", "configMap": {"name": "demo-a2a-ca", "defaultMode": 0o644}}
    mount = {"name": "demo-a2a-ca", "mountPath": CA_PATH, "readOnly": True}
    existing_volume = volumes.get("demo-a2a-ca", volume)
    # Kubernetes fills this field when the volume is first stored. Accept the
    # same source before or after defaulting so a second setup is unchanged.
    without_mode = {"name": "demo-a2a-ca", "configMap": {"name": "demo-a2a-ca"}}
    require(existing_volume in (volume, without_mode),
            "controller already uses the demo CA volume name for another source")
    for existing in mounts:
        if existing["name"] == "demo-a2a-ca" or existing["mountPath"] == CA_PATH:
            require(existing == mount, "controller CA mount collides with another mount")
    env = [e for e in container.get("env", []) if e["name"] == "SSL_CERT_DIR"]
    require(len(env) <= 1 and not any("valueFrom" in e for e in env),
            "SSL_CERT_DIR must be absent or a literal directory list")
    previous = env[0].get("value", "") if env else ""
    directories = (previous or "/etc/ssl/certs:/etc/pki/tls/certs").split(":")
    if CA_PATH not in directories:
        directories.append(CA_PATH)
    patch = {
        "metadata": {k: deployment["metadata"][k] for k in ("uid", "resourceVersion")},
        "spec": {"template": {"spec": {
            "volumes": [volume],
            "containers": [{"name": container_name, "volumeMounts": [mount],
                            "env": [{"name": "SSL_CERT_DIR", "value": ":".join(directories)}]}],
        }}},
    }
    info = {
        "deployment": deployment["metadata"]["name"], "namespace": namespace,
        "uid": deployment["metadata"]["uid"], "container": container_name,
        "selector": selector, "previousCertDir": previous,
        "storeClaim": volumes[persistent[0]["name"]]["persistentVolumeClaim"]["claimName"],
    }
    return patch, info


def resources(namespace, installation, image, public_url, api_service, model, ca, controller):
    require(re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", namespace), "invalid namespace")
    labels = {"demo.orka.ai/name": DEMO, "demo.orka.ai/installation": installation}
    pod_labels = {**labels, "app.kubernetes.io/name": ADAPTER}

    def obj(api, kind, name, **fields):
        metadata = {"name": name, "labels": labels}
        if kind != "GatewayClass":
            metadata["namespace"] = namespace
        return {"apiVersion": api, "kind": kind, "metadata": metadata, **fields}

    def rule(group, names, resources_):
        result = {"apiGroups": [group], "resources": resources_, "verbs": ["get"]}
        if names:
            result["resourceNames"] = names
        return result

    config = {
        "listenAddr": ":8443", "tlsCertFile": "/etc/a2a-tls/tls.crt",
        "tlsKeyFile": "/etc/a2a-tls/tls.key", "publicURL": public_url,
        "orkaURL": f"http://{api_service}.{namespace}.svc:8080", "namespace": namespace,
        "gateway": GATEWAY, "binding": AGENT, "agent": AGENT,
        "accountId": "inventory", "contextId": "order-desk", "senderId": "order-desk-app",
        "clientTokenFile": "/etc/a2a-client/token", "inboundTokenFile": "/etc/a2a-inbound/token",
        "outboundTokenFile": "/etc/a2a-outbound/token",
        "readTokenFile": "/var/run/secrets/kubernetes.io/serviceaccount/token",
        "card": {"name": "Inventory advice", "description": "Help the order desk explain stock shortages to customers.",
                 "version": "1.0.0", "skills": [{"id": "stock-advice", "name": "Recommend a customer reply",
                 "description": "Use the supplied stock and delivery facts to suggest a customer reply.",
                 "tags": ["inventory", "text"]}]},
    }
    gateway_api = "gateway.orka.ai/v1alpha1"
    rbac = "rbac.authorization.k8s.io/v1"
    result = [
        obj("core.orka.ai/v1alpha1", "Agent", AGENT, spec={
            "model": {"name": model},
            "runtime": {"type": "codex", "contractVersion": "orka.harness.v2", "defaultMaxTurns": 8,
                        "defaultAllowBash": False, "defaultAllowedTools": ["Glob", "Grep", "Read"],
                        "defaultReasoningEffort": "low"},
            "systemPrompt": {"inline": "You advise the inventory order desk. Use only the facts in the conversation. "
                "Use digits for quantities. When stock is short, say how many can be supplied today and how many remain. "
                "Keep answers brief. Do not run tools, look up outside facts, place orders, or claim to have sent a reply. "
                "Follow-up requests refer to this conversation."},
        }),
        obj(gateway_api, "GatewayClass", f"demo-a2a-{namespace}", spec={
            "contractVersion": "orka.gateway.v1", "category": "http",
            "capabilities": {k: True for k in ("inboundText", "outboundText", "threads", "senderIdentity", "idempotentDelivery")},
        }),
        obj(gateway_api, "Gateway", GATEWAY, spec={
            "gatewayClassName": f"demo-a2a-{namespace}",
            "adapter": {"serviceRef": {"name": ADAPTER, "port": 8443}},
            "inboundAuthRef": {"name": "demo-a2a-inbound", "key": "token"},
            "outboundAuthRef": {"name": "demo-a2a-outbound", "key": "token"},
        }),
        obj(gateway_api, "GatewayBinding", AGENT, spec={
            "gatewayRef": {"name": GATEWAY}, "agentRef": {"name": AGENT},
            "match": {"accountId": "inventory", "contextId": "order-desk", "senderId": "order-desk-app"},
            "senderPolicy": {"mode": "allowlist", "allowedSenderIds": ["order-desk-app"]},
            "session": {"mode": "thread-sender"}, "taskDefaults": {"timeout": "10m"},
        }),
        obj("v1", "ServiceAccount", ADAPTER),
        obj(rbac, "Role", ADAPTER, rules=[
            rule("core.orka.ai", [AGENT], ["agents"]),
            rule("gateway.orka.ai", [GATEWAY], ["gateways"]),
            rule("gateway.orka.ai", [AGENT], ["gatewaybindings"]),
            rule("gateway.orka.ai", [], ["gatewayevents", "gatewaydeliveries"]),
        ]),
        obj(rbac, "RoleBinding", ADAPTER,
            subjects=[{"kind": "ServiceAccount", "name": ADAPTER, "namespace": namespace}],
            roleRef={"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": ADAPTER}),
        obj(rbac, "Role", "demo-a2a-presenter", rules=[
            rule("gateway.orka.ai", [GATEWAY], ["gateways"]),
            rule("gateway.orka.ai", [], ["gatewayevents", "gatewaydeliveries"]),
        ]),
        obj(rbac, "RoleBinding", "demo-a2a-presenter",
            subjects=[{"kind": "ServiceAccount", "name": "orka-client", "namespace": namespace}],
            roleRef={"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "demo-a2a-presenter"}),
        obj("v1", "ConfigMap", "demo-a2a-ca", data={"ca.crt": ca}),
        obj("v1", "ConfigMap", "demo-a2a-config", data={"config.json": json.dumps(config, indent=2)}),
        obj("v1", "Service", ADAPTER, spec={"selector": pod_labels,
            "ports": [{"name": "https", "port": 8443, "targetPort": "https"}]}),
    ]
    mounts = [{"name": "config", "mountPath": "/etc/a2a", "readOnly": True}]
    volumes = [{"name": "config", "configMap": {"name": "demo-a2a-config"}}]
    for name in ("tls", "client", "inbound", "outbound"):
        mounts.append({"name": name, "mountPath": f"/etc/a2a-{name}", "readOnly": True})
        volumes.append({"name": name, "secret": {"secretName": f"demo-a2a-{name}", "defaultMode": 0o440}})
    result.append(obj("apps/v1", "Deployment", ADAPTER, spec={
        "replicas": 1, "selector": {"matchLabels": pod_labels},
        "template": {"metadata": {"labels": pod_labels}, "spec": {
            "serviceAccountName": ADAPTER,
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                                "fsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{"name": "adapter", "image": image, "imagePullPolicy": "IfNotPresent",
                "ports": [{"name": "https", "containerPort": 8443}],
                "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                    "capabilities": {"drop": ["ALL"]}},
                "readinessProbe": {"httpGet": {"path": "/readyz", "port": "https", "scheme": "HTTPS"},
                                   "periodSeconds": 2},
                "resources": {"requests": {"cpu": "50m", "memory": "32Mi"},
                              "limits": {"cpu": "500m", "memory": "128Mi"}},
                "volumeMounts": mounts}], "volumes": volumes,
        }},
    }))
    result.append(obj("networking.k8s.io/v1", "NetworkPolicy", ADAPTER, spec={
        "podSelector": {"matchLabels": pod_labels}, "policyTypes": ["Ingress", "Egress"],
        "ingress": [{"from": [{"podSelector": {"matchLabels": controller["selector"]}}],
                     "ports": [{"protocol": "TCP", "port": 8443}]}],
        "egress": [
            {"to": [{"podSelector": {"matchLabels": controller["selector"]}}],
             "ports": [{"protocol": "TCP", "port": 8080}]},
            {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                     "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
             "ports": [{"protocol": protocol, "port": 53} for protocol in ("UDP", "TCP")]},
        ],
    }))
    return {"apiVersion": "v1", "kind": "List", "items": result}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    patch = commands.add_parser("controller-patch")
    patch.add_argument("--namespace", required=True)
    patch.add_argument("--container", required=True)
    patch.add_argument("--service", type=Path, required=True)
    patch.add_argument("--info", type=Path, required=True)
    render = commands.add_parser("resources")
    for name in ("namespace", "installation", "image", "public-url", "api-service", "model"):
        render.add_argument("--" + name, required=True)
    render.add_argument("--ca", type=Path, required=True)
    render.add_argument("--controller", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "controller-patch":
        output, info = controller_patch(json.load(sys.stdin), json.loads(args.service.read_text()), args.namespace, args.container)
        args.info.write_text(json.dumps(info, indent=2) + "\n")
    else:
        output = resources(args.namespace, args.installation, args.image, args.public_url, args.api_service,
                           args.model, args.ca.read_text(), json.loads(args.controller.read_text()))
    json.dump(output, sys.stdout, indent=2)
    print()


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError) as exc:
        sys.exit(f"A2A preparation stopped: {exc}")
