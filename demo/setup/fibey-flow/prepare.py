#!/usr/bin/env python3
"""Render demo 14's resources: the alert gateway, the coordinator, and Mark.

The gateway half and controller_patch are copied from demo 08's prepare.py
with this demo's names, so both demos can share a cluster. Demo 13's Foundry
Fibey, Tools, and work-order service are reused unchanged.
"""
import argparse
import json
from pathlib import Path, PurePosixPath
import re
import sys

DEMO = "14-fibey-alert-flow"
ADAPTER = "fibey-alerts-adapter"
GATEWAY = "fibey-alerts"
COORDINATOR = "demo-maintenance-coordinator"
SPECIALIST = "demo-fibey-foundry"
PROVIDER = "demo-foundry-models"
PRESENTER = "mark"
CA_NAME = "fibey-alerts-ca"
CA_PATH = "/etc/orka-demo-fibey-alerts-ca"
ROUTE = {"accountId": "quincy-north", "contextId": "pump-alerts", "senderId": "plant-monitoring"}

COORDINATOR_PROMPT = """\
You are Orka's maintenance coordinator for the Quincy North plant. Each
message is an equipment alert that starts with a lowercase alert ID such as
qn-1234. Follow these steps exactly, once each, in order:

1. Call create_container_task with name "pump-analysis", image
   "{image}", and timeout "5m". Pass no command, args, or workspace. It
   analyzes the asset's sensor history on AKS.
2. Call wait_for_tasks with the task name that create_container_task
   returned (Orka names child tasks; it is not "pump-analysis"). Read the
   analysis from its result. If that task did not succeed, reply that the
   sensor analysis failed and why, and stop: do not delegate.
3. Call delegate_task with agent "{specialist}", timeout "20m", and this
   prompt, replacing <ID>, <ALERT>, and <ANALYSIS> with the alert ID, the
   alert text, and the analysis result copied verbatim:

   Alert <ID>: <ALERT>
   Sensor analysis from Orka's data-analysis job on AKS:
   <ANALYSIS>
   Use the simulated tools for runID <ID> and asset pump-1. Call
   read-inventory exactly once. Then call create-work-order exactly once
   with summary "Inspect the pressure transmitter." Wait for the tool result
   while a person reviews the request. Do not retry an action or treat your
   own words as permission. A successful workOrderID response means a person
   approved the order and it was created. Report the actual workOrderID on
   success, or its denial or error. Keep your final answer under 80 words.

4. Call wait_for_tasks with the task name that delegate_task returned and
   timeout "20m". A person must approve the work order first, so this can
   take several minutes. If it returns before the task finishes, call
   wait_for_tasks again for the same task.
5. Reply in under 80 words of plain text, without Markdown: what the
   analysis found, and the work order ID copied exactly from Fibey's result.
   A workOrderID means a person approved the order and it was then created;
   say so plainly.
   If there is no workOrderID, say why no order was created. Do not claim the
   equipment was inspected or repaired.

Never call any other tool, and never create a work order yourself.
"""


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
            "this demo requires the harness-v2 controller")
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
    volume = {"name": CA_NAME, "configMap": {"name": CA_NAME, "defaultMode": 0o644}}
    mount = {"name": CA_NAME, "mountPath": CA_PATH, "readOnly": True}
    existing_volume = volumes.get(CA_NAME, volume)
    # Kubernetes fills this field when the volume is first stored. Accept the
    # same source before or after defaulting so a second setup is unchanged.
    without_mode = {"name": CA_NAME, "configMap": {"name": CA_NAME}}
    require(existing_volume in (volume, without_mode),
            "controller already uses the demo CA volume name for another source")
    for existing in mounts:
        if existing["name"] == CA_NAME or existing["mountPath"] == CA_PATH:
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


def resources(namespace, installation, adapter_image, analysis_image, model, ca, controller):
    require(re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", namespace), "invalid namespace")
    require(re.fullmatch(r"[^\s@]+@sha256:[a-f0-9]{64}", adapter_image), "adapter image must be digest-pinned")
    labels = {"demo.orka.ai/name": DEMO, "demo.orka.ai/installation": installation}
    pod_labels = {**labels, "app.kubernetes.io/name": ADAPTER}

    def obj(api, kind, name, ns=namespace, **fields):
        metadata = {"name": name, "labels": labels}
        if ns:
            metadata["namespace"] = ns
        return {"apiVersion": api, "kind": kind, "metadata": metadata, **fields}

    def rule(group, resources_, verbs, names=None):
        result = {"apiGroups": [group], "resources": resources_, "verbs": verbs}
        if names:
            result["resourceNames"] = names
        return result

    config = {
        "listenAddr": ":8443", "tlsCertFile": "/etc/a2a-tls/tls.crt", "tlsKeyFile": "/etc/a2a-tls/tls.key",
        "publicURL": "https://127.0.0.1:8443", "orkaURL": f"http://orka-api.{namespace}.svc:8080",
        "namespace": namespace, "gateway": GATEWAY, "binding": COORDINATOR, "agent": COORDINATOR, **ROUTE,
        "clientTokenFile": "/etc/a2a-client/token", "inboundTokenFile": "/etc/a2a-inbound/token",
        "outboundTokenFile": "/etc/a2a-outbound/token",
        "readTokenFile": "/var/run/secrets/kubernetes.io/serviceaccount/token",
        "card": {"name": "Quincy North maintenance", "version": "1.0.0",
                 "description": "Equipment alerts in, reviewed maintenance decisions out.",
                 "skills": [{"id": "equipment-alert", "name": "Handle an equipment alert",
                             "description": "Analyze the asset's history and propose a work order for review.",
                             "tags": ["maintenance", "text"]}]},
    }
    gateway_api, rbac, core = "gateway.orka.ai/v1alpha1", "rbac.authorization.k8s.io/v1", "core.orka.ai/v1alpha1"
    result = [
        obj("v1", "Secret", PROVIDER, type="Opaque",
            stringData={"api-key": "unused-vekil-authenticates-with-workload-identity"}),
        obj(core, "Provider", PROVIDER, spec={
            "type": "openai", "baseURL": "http://vekil.vekil-system.svc.cluster.local:1337/v1",
            "secretRef": {"name": PROVIDER, "key": "api-key"}, "defaultModel": model}),
        obj("networking.k8s.io/v1", "NetworkPolicy", f"{namespace}-ai-workers", ns="vekil-system", spec={
            "podSelector": {"matchLabels": {"app.kubernetes.io/name": "vekil"}}, "policyTypes": ["Ingress"],
            "ingress": [{"from": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": namespace}},
                                   "podSelector": {"matchLabels": {"orka.ai/task-type": "ai"}}}],
                         "ports": [{"port": 1337, "protocol": "TCP"}]}]}),
        obj(core, "Agent", COORDINATOR, spec={
            "providerRef": {"name": PROVIDER}, "model": {"name": model},
            "systemPrompt": {"inline": COORDINATOR_PROMPT.format(image=analysis_image, specialist=SPECIALIST)},
            "coordination": {"enabled": True, "maxDepth": 1, "maxConcurrentChildren": 2,
                             "allowedAgents": [{"name": SPECIALIST}]}}),
        obj(gateway_api, "GatewayClass", f"{GATEWAY}-{namespace}", ns=None, spec={
            "contractVersion": "orka.gateway.v1", "category": "http",
            "capabilities": {k: True for k in ("inboundText", "outboundText", "threads", "senderIdentity",
                                               "idempotentDelivery")}}),
        obj(gateway_api, "Gateway", GATEWAY, spec={
            "gatewayClassName": f"{GATEWAY}-{namespace}",
            "adapter": {"serviceRef": {"name": ADAPTER, "port": 8443}},
            "inboundAuthRef": {"name": f"{GATEWAY}-inbound", "key": "token"},
            "outboundAuthRef": {"name": f"{GATEWAY}-outbound", "key": "token"}}),
        obj(gateway_api, "GatewayBinding", COORDINATOR, spec={
            "gatewayRef": {"name": GATEWAY}, "agentRef": {"name": COORDINATOR}, "match": ROUTE,
            "senderPolicy": {"mode": "allowlist", "allowedSenderIds": [ROUTE["senderId"]]},
            "session": {"mode": "thread-sender"}, "taskDefaults": {"timeout": "30m"}}),
        obj("v1", "ServiceAccount", ADAPTER),
        obj(rbac, "Role", ADAPTER, rules=[
            rule("core.orka.ai", ["agents"], ["get"], [COORDINATOR]),
            rule("gateway.orka.ai", ["gateways"], ["get"], [GATEWAY]),
            rule("gateway.orka.ai", ["gatewaybindings"], ["get"], [COORDINATOR]),
            rule("gateway.orka.ai", ["gatewayevents", "gatewaydeliveries"], ["get"]),
        ]),
        obj(rbac, "RoleBinding", ADAPTER,
            subjects=[{"kind": "ServiceAccount", "name": ADAPTER, "namespace": namespace}],
            roleRef={"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": ADAPTER}),
        # The coordinator's children inherit the gateway sender's identity, so
        # Orka treats them as gateway-owned; reading their results through the
        # API needs get on this Gateway. Workers run as orka-ai-worker.
        obj(rbac, "Role", "demo-fibey-flow-coordinator",
            rules=[rule("gateway.orka.ai", ["gateways"], ["get"], [GATEWAY])]),
        obj(rbac, "RoleBinding", "demo-fibey-flow-coordinator",
            subjects=[{"kind": "ServiceAccount", "name": "orka-ai-worker", "namespace": namespace}],
            roleRef={"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "demo-fibey-flow-coordinator"}),
        # Mark presents and approves. demo-fibey-presenter (demo 13) covers Tasks
        # and approvals; this Role adds gateway records and the usage report.
        # Deciding an approval on gateway-started work also needs update on
        # that Gateway, Orka's operator check for gateway-owned Tasks.
        obj("v1", "ServiceAccount", PRESENTER, automountServiceAccountToken=False),
        obj(rbac, "Role", "demo-fibey-flow-presenter", rules=[
            rule("gateway.orka.ai", ["gateways"], ["get", "update"], [GATEWAY]),
            rule("gateway.orka.ai", ["gatewayevents", "gatewaydeliveries"], ["get", "list"]),
            rule("core.orka.ai", ["repositorymonitors"], ["list"]),
        ]),
        *[obj(rbac, "RoleBinding", f"{role}-{PRESENTER}",
              subjects=[{"kind": "ServiceAccount", "name": PRESENTER, "namespace": namespace}],
              roleRef={"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": role})
          for role in ("demo-fibey-presenter", "demo-fibey-flow-presenter")],
        obj("v1", "ConfigMap", CA_NAME, data={"ca.crt": ca}),
        obj("v1", "ConfigMap", f"{ADAPTER}-config", data={"config.json": json.dumps(config, indent=2)}),
        obj("v1", "Service", ADAPTER, spec={"selector": pod_labels,
            "ports": [{"name": "https", "port": 8443, "targetPort": "https"}]}),
    ]
    mounts = [{"name": "config", "mountPath": "/etc/a2a", "readOnly": True}]
    volumes = [{"name": "config", "configMap": {"name": f"{ADAPTER}-config"}}]
    for name in ("tls", "client", "inbound", "outbound"):
        mounts.append({"name": name, "mountPath": f"/etc/a2a-{name}", "readOnly": True})
        volumes.append({"name": name, "secret": {"secretName": f"{GATEWAY}-{name}", "defaultMode": 0o440}})
    result.append(obj("apps/v1", "Deployment", ADAPTER, spec={
        "replicas": 1, "selector": {"matchLabels": pod_labels},
        "template": {"metadata": {"labels": pod_labels}, "spec": {
            "serviceAccountName": ADAPTER,
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                                "fsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{"name": "adapter", "image": adapter_image, "imagePullPolicy": "IfNotPresent",
                "args": ["-config", "/etc/a2a/config.json"],
                "ports": [{"name": "https", "containerPort": 8443}],
                "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                    "capabilities": {"drop": ["ALL"]}},
                "readinessProbe": {"httpGet": {"path": "/readyz", "port": "https", "scheme": "HTTPS"},
                                   "periodSeconds": 2},
                "resources": {"requests": {"cpu": "50m", "memory": "32Mi"},
                              "limits": {"cpu": "500m", "memory": "128Mi"}},
                "volumeMounts": mounts}], "volumes": volumes}}}))
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
        ]}))
    return {"apiVersion": "v1", "kind": "List", "items": result}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    patch = sub.add_parser("controller-patch", help="read a controller Deployment on stdin")
    patch.add_argument("--namespace", required=True)
    patch.add_argument("--container", required=True)
    patch.add_argument("--service", type=Path, required=True)
    patch.add_argument("--info", type=Path, required=True)
    render = sub.add_parser("resources")
    for name in ("namespace", "installation", "adapter-image", "analysis-image", "model"):
        render.add_argument("--" + name, required=True)
    render.add_argument("--ca", type=Path, required=True)
    render.add_argument("--controller", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "controller-patch":
        result, info = controller_patch(json.load(sys.stdin), json.loads(args.service.read_text()),
                                        args.namespace, args.container)
        args.info.write_text(json.dumps(info, indent=2) + "\n")
        print(json.dumps(result, indent=2))
    else:
        print(json.dumps(resources(args.namespace, args.installation, args.adapter_image, args.analysis_image,
                                   args.model, args.ca.read_text(), json.loads(args.controller.read_text())),
                         indent=2))


if __name__ == "__main__":
    main()
