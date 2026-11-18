"""Run approval acceptance and crash recovery in one disposable kindctl cluster."""

import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time
from urllib.parse import urlencode

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "examples/human-approval-v2"))
from acceptance import Acceptance, CheckFailed, FINALIZER, http_json, require
import render_runtimes
import recovery_checks as recovery

NAMESPACE = "orka-system"


def wait(description, check, seconds=240):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(1)
    raise CheckFailed("timed out waiting for " + description)


class Cluster:
    def __init__(self, args):
        self.args = args
        require(args.context == "kind-" + args.cluster and bool(os.environ.get("KUBECONFIG")),
                "a scoped kindctl kubeconfig and matching context are required")
        self.command = ["kubectl", "--context", args.context, "-n", NAMESPACE, "--request-timeout=30s"]
        self.nodes = {node["metadata"]["name"] for node in self.json("get", "nodes", "-o", "json")["items"]}
        for node in self.nodes:
            owner = subprocess.check_output(["docker", "inspect", "--format",
                '{{ index .Config.Labels "io.x-k8s.kind.cluster" }}', node], text=True).strip()
            require(owner == args.cluster, "node does not belong to the selected disposable cluster")

    def call(self, *arguments, body=None):
        result = subprocess.run([*self.command, *arguments],
            input=None if body is None else json.dumps(body), capture_output=True, text=True, timeout=90)
        require(result.returncode == 0, "scoped kubectl command failed: " + " ".join(arguments[:3]))
        return result.stdout

    def json(self, *arguments, body=None):
        return json.loads(self.call(*arguments, body=body))

    def apply(self, resource):
        self.call("apply", "-f", "-", body=resource)

    def pod(self, deployment):
        owner = self.json("get", "deployment", deployment, "-o", "json")
        labels = owner["spec"]["selector"]["matchLabels"]
        selector = ",".join(key + "=" + value for key, value in labels.items())
        pods = self.json("get", "pods", "-l", selector, "-o", "json")["items"]
        pods = [pod for pod in pods if not pod["metadata"].get("deletionTimestamp")]
        require(len(pods) == 1 and pods[0]["spec"]["nodeName"] in self.nodes,
                "crash recovery requires one Pod on the owned cluster")
        return pods[0]

    def kill_container(self, pod, name, guard=None):
        current = self.json("get", "pod", pod["metadata"]["name"], "-o", "json")
        before = container_state(pod, name)
        require(current["metadata"]["uid"] == pod["metadata"]["uid"] and
                current["spec"] == pod["spec"] and container_state(current, name) == before,
                "container changed before crash injection")
        identity = before["containerID"].removeprefix("containerd://")
        require(re.fullmatch("[a-f0-9]{64}", identity), "unexpected container identity")
        if guard:
            guard()
        self.last_injection_requested_at = time.time()
        subprocess.run(["docker", "exec", pod["spec"]["nodeName"], "ctr", "--namespace", "k8s.io",
                        "tasks", "kill", "--signal", "SIGKILL", "--all", identity], check=True, timeout=30)

    def restarted(self, deployment, before, name):
        current = self.pod(deployment)
        require(current["metadata"]["uid"] == before["metadata"]["uid"], "crash replaced the Pod and its state volumes")
        old, new = container_state(before, name), container_state(current, name)
        require(new["restartCount"] <= old["restartCount"] + 1, "container restarted more than once")
        return current if (new["ready"] and new["restartCount"] == old["restartCount"] + 1 and
                           new["containerID"] != old["containerID"]) else None


def container_state(pod, name):
    state = next(item for item in pod["status"]["containerStatuses"] if item["name"] == name)
    return {key: state.get(key) for key in ("containerID", "restartCount", "ready")}


class Forward:
    def __init__(self, cluster, work, service, port):
        self.path = work / "private" / (service + "-forward.log")
        self.log = self.path.open("w")
        self.process = subprocess.Popen([*cluster.command, "port-forward", "--address=127.0.0.1",
            "service/" + service, "0:" + str(port)], stdout=self.log, stderr=subprocess.STDOUT)
        try:
            def ready():
                require(self.process.poll() is None, service + " port-forward exited")
                match = re.search(r"Forwarding from 127\.0\.0\.1:([0-9]+)", self.path.read_text())
                return "http://127.0.0.1:" + match[1] if match else None
            self.url = wait(service + " port-forward", ready, 30)
        except BaseException:
            self.close()
            raise

    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=10)
        self.log.close()


def prepare_secrets(cluster):
    os.umask(0o077)
    cluster.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": NAMESPACE, "labels": {
        "orka.ai/controller-mode": "harness-v2", "pod-security.kubernetes.io/enforce": "baseline"}}})
    for name, keys in {
        "acp-artifact-capability": ["capability-secret"],
        "workspace-publisher-auth": ["controller-token", "operation-capability-secret"],
        "provider-auth-proxy": ["token"], "scm-egress-proxy-auth": ["token"],
        "human-approval-agentkit-auth": ["controller-token", "capability-secret", "provider-token"],
        "human-approval-foundry-auth": ["controller-token", "capability-secret", "provider-token", "continuation-proof", "identity-header"],
    }.items():
        cluster.call("create", "-f", "-", body={"apiVersion": "v1", "kind": "Secret", "metadata": {
            "name": name, "namespace": NAMESPACE}, "stringData": {key: secrets.token_urlsafe(48) for key in keys}})
    tls = cluster.args.work / "private" / "gateway-tls"
    cluster.call("create", "-f", "-", body={"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
        "metadata": {"name": "human-approval-gateway-tls", "namespace": NAMESPACE},
        "stringData": {name: (tls / name).read_text() for name in ("ca.crt", "tls.crt", "tls.key")}})


def api_identity(cluster, work):
    for reviewer in (True, False):
        name = "human-approval-" + ("reviewer" if reviewer else "reader")
        metadata = {"name": name, "namespace": NAMESPACE}
        rules = [{"apiGroups": ["core.orka.ai"], "resources": ["tasks", "agents", "agentruntimes", "tools", "sessions"],
                  "verbs": ["get", "list", "watch"]}]
        if reviewer:
            rules += [
                {"apiGroups": ["core.orka.ai"], "resources": ["tasks"], "verbs": ["create", "delete", "patch"]},
                {"apiGroups": ["core.orka.ai"], "resources": ["tasks/approvals"], "verbs": ["update"]},
                {"apiGroups": ["core.orka.ai"], "resources": ["agents", "agentruntimes", "tools"], "verbs": ["use"]},
            ]
        cluster.apply({"apiVersion": "v1", "kind": "ServiceAccount", "metadata": metadata, "automountServiceAccountToken": False})
        cluster.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata, "rules": rules})
        cluster.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata,
            "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
            "subjects": [{"kind": "ServiceAccount", "name": name, "namespace": NAMESPACE}]})
        token = work / "private" / (name + ".token")
        token.write_text(cluster.call("create", "token", name, "--duration=2h"))
        token.chmod(0o600)


def register(cluster, work, provider, url):
    name = "human-approval-" + provider
    runtime_name = name + "-runtime"
    endpoint = "http://" + name + ".orka-system.svc.cluster.local:8080"
    profile = json.loads((work / (provider + "-profile.json")).read_text())
    _, caps = http_json(url + "/v2/capabilities")
    require(caps["runtimeProfileDigest"] == profile["profile"]["digest"] and
            caps["provider"]["supportsBrokeredToolApprovals"] and not caps["provider"]["supportsPermissions"],
            "runtime did not advertise the qualified approval profile")
    if provider == "foundry":
        require(caps.get("supportsFoundryRecovery") is True,
                "Foundry supervisor lacks supportsFoundryRecovery; use the matching upgraded broker and supervisor")
    cluster.call("patch", "secret", name + "-auth", "--type=merge", "--patch-file=/dev/stdin", body={"metadata": {
        "labels": {"orka.ai/agent-runtime-auth": "true", "orka.ai/agent-runtime-name": runtime_name},
        "annotations": {"orka.ai/agent-runtime-endpoint": endpoint}}})
    governance = dict(caps["workspaceGovernance"])
    governance.setdefault("trusted", False)
    registered = recovery.enroll(cluster, {"apiVersion": "core.orka.ai/v1alpha1", "kind": "AgentRuntime",
        "metadata": {"name": runtime_name, "namespace": NAMESPACE}, "spec": {
            "contractVersion": "orka.harness.v2", "deployment": {"mode": "external-endpoint", "endpoint": endpoint},
            "clientAuth": {"controllerBearerTokenSecretRef": {"name": name + "-auth", "key": "controller-token"},
                           "operationCapabilitySecretRef": {"name": name + "-auth", "key": "capability-secret"}},
            "capabilities": {"profile": profile["profile"], "mcpPolicy": profile["mcpPolicy"],
                "limits": caps["limits"], "supportsDrain": caps.get("supportsDrain", False),
                "supportsPublicationFinalization": caps.get("supportsPublicationFinalization", False), "workspaceGovernance": governance}}}, provider)
    registered, witness = wait(provider + " enrolled runtime conformance",
        lambda: recovery.ready_witness(cluster, registered, provider), 360)
    cluster.apply({"apiVersion": "core.orka.ai/v1alpha1", "kind": "Agent", "metadata": {"name": name, "namespace": NAMESPACE},
                   "spec": {"runtime": {"runtimeRef": {"name": runtime_name}}}})
    return registered, witness


class FixtureAcceptance(Acceptance):
    """Preserve evidence until the controller completes normal Task cleanup."""

    def __init__(self, args, cluster, witness):
        super().__init__(args)
        self.cluster = cluster
        self.witness = witness
        self.witnesses = {witness["spec"]["operationId"]: witness}
        self.cleanup_artifacts = {}
        self.cleanup_admissions = {}
        self.cleanup_approvals = {}
        self.report.update(enrollment=[recovery.effect_evidence(witness)], exposures=[], admissions={}, cleanup=[])
        self.save()

    def create(self, suffix, inventory_only=False):
        name = super().create(suffix, inventory_only)
        effect = self.wait("exact enrolled Task exposure", lambda: recovery.exposed_task(self.cluster, self, name, self.witness))
        api_task = self.task(name)
        task = self.kube_json("get", "task", name, "-o", "json")
        require(recovery.identity(task) == recovery.identity(api_task), "Task identity changed while capturing admission")
        # Hash the same stored representation used at cleanup. The typed API
        # omits zero values that Kubernetes CRD defaults retain, such as createPR=false.
        self.cleanup_admissions[name] = recovery.admission_evidence(task, effect, self.witness)
        self.report["admissions"][name] = self.cleanup_admissions[name]
        self.report["exposures"].append({"task": name, **recovery.effect_evidence(effect)})
        self.save()
        return name

    def recovered(self, old_witness, epoch=None, proof_kind=None):
        old_boot = old_witness["spec"]["operationId"]
        proof = self.wait("exact old boot retirement", lambda: recovery.retirement(self.cluster, old_witness, proof_kind), 600)
        registered, witness = self.wait("controller-managed runtime recovery", lambda: recovery.ready_witness(
            self.cluster, self.runtime, self.args.backend, previous_boot=old_boot, expected_epoch=epoch), 600)
        self.runtime, self.witness = registered, witness
        self.witnesses[witness["spec"]["operationId"]] = witness
        self.report["enrollment"].append(recovery.effect_evidence(witness))
        self.report.setdefault("retirement", []).append({**recovery.effect_evidence(proof),
            "bootID": old_boot, "proofKind": proof["status"]["response"]["kind"]})
        self.save()

    def observe(self, name, enabled):
        if enabled:
            # Product finalization can reclaim the event stream. Keep the
            # original call identity before DELETE, not after reclamation.
            approval = self.approval(name)
            self.cleanup_approvals[name] = [recovery._approval_evidence(approval)] if approval else []
            self.cleanup_artifacts[name] = recovery.task_artifacts(self.cluster, self.task(name))
            return super().observe(name, True)

        def cleaned():
            task = self.kube_json("get", "task", name, "-o", "json")
            require(task["metadata"]["uid"] == self.owned_tasks[name], "Task changed during normal cleanup")
            require(FINALIZER in task["metadata"].get("finalizers", []), "cleanup observer disappeared early")
            receipt = recovery.cleanup_receipt(task)
            if receipt is None or set(task["metadata"].get("finalizers", [])) - {FINALIZER}:
                return None
            return task, receipt

        task, receipt = self.wait("normal cleanup receipt and product finalizer release", cleaned, 600)
        artifacts = self.cleanup_artifacts[name]
        record = {"task": name, "uid": self.owned_tasks[name], **receipt,
                  "artifacts": artifacts, "deleted": False, "productFinalizerReleased": True,
                  **recovery.cleanup_evidence(self.cluster, task, artifacts, self.cleanup_approvals[name], self.witnesses,
                                              self.cleanup_admissions[name])}
        self.report["cleanup"].append(record)
        self.save()
        super().observe(name, False)
        self.wait("normal Task cleanup and unchanged retained receipts", lambda: recovery.verify_task_cleanup(self.cluster, record))
        record["deleted"] = True
        self.save()

    def cleanup_tasks(self):
        completed = {item["task"] for item in self.report["cleanup"] if item.get("deleted")}
        for name in self.owned_tasks:
            if name in completed:
                continue
            task = self.settled(name)
            self.observe(name, True)
            recovery.delete_exact(self.cluster, "tasks", task)
            self.observe(name, False)


def cleanup_authority(cluster, run):
    recovery.require_unused_runtime(cluster, run.agent, run.runtime)
    authority = recovery.authority_metadata(cluster, run.runtime["metadata"]["uid"])
    require(authority, "enrolled runtime lost its retained cleanup authority before deletion")
    evidence = {"agent": recovery.identity(run.agent), "runtime": recovery.identity(run.runtime),
                "retainedAuthority": [recovery.identity(item) for item in authority], "passed": False}
    run.report["authorityCleanup"] = evidence
    run.save()
    recovery.delete_exact(cluster, "agents", run.agent)
    run.wait("deleted fixture Agent", lambda: recovery.absent(cluster, {"kind": "Agent", **recovery.identity(run.agent)}))
    recovery.require_unused_runtime(cluster, run.agent, run.runtime)
    recovery.delete_exact(cluster, "agentruntimes", run.runtime)
    run.wait("normally finalized enrolled AgentRuntime", lambda: recovery.absent(cluster, {"kind": "AgentRuntime", **recovery.identity(run.runtime)}), 600)
    for secret in authority:
        # Metadata-only lookup avoids reading retained controller credentials.
        run.wait("retired boot authority removal", lambda item=secret: not any(
            current["metadata"]["name"] == item["metadata"]["name"] for current in recovery.secret_metadata(cluster)))
    require(not recovery.authority_metadata(cluster, run.runtime["metadata"]["uid"]),
            "runtime deletion leaked retained boot authority")
    for witness in run.witnesses.values():
        body = witness["status"]["response"]
        pod = recovery.optional(cluster, "pod", body["podName"])
        if pod is not None:
            require(pod["metadata"]["uid"] == body["podUID"] and
                    recovery.POD_FINALIZER not in pod["metadata"].get("finalizers", []),
                    "retired runtime Pod still holds cleanup authority")
    evidence["passed"] = True
    run.save()


def request_evidence(run, task, approval):
    name = "acp-approval-" + hashlib.sha256(approval["id"].encode()).hexdigest()
    secret = run.kube_json("get", "secret", name, "-o", "json")
    owners = secret["metadata"].get("ownerReferences", [])
    require(secret.get("immutable") is True and secret.get("type") == "orka.ai/tool-approval" and
            len(owners) == 1 and owners[0].get("kind") == "Task" and owners[0].get("name") == task and
            owners[0].get("uid") == run.owned_tasks[task] and owners[0].get("controller") is True,
            "original approval request lost its immutable Task ownership")
    return {"name": name, "uid": secret["metadata"]["uid"],
            "sha256": hashlib.sha256(base64.b64decode(secret["data"]["call.json"], validate=True)).hexdigest()}


def recovery_result(run, task, pending, before, scenario):
    settled = run.settled(task, lambda: run.counts(task, 1, 0))
    require(settled["status"]["execution"]["outcome"] in {"OutcomeUnknown", "Failed", "Cancelled"},
            "lost prompt incorrectly reported success")
    status, _ = run.decision(task, pending, "approve", (409, 410))
    final = run.approval(task)
    require(final["id"] == pending["id"] and final["binding"] == pending["binding"] and
            final.get("executionOutcome") == "not_started", "recovery changed the review or started its action")
    require(request_evidence(run, task, pending) == before, "recovery replaced the original executable request")
    run.report["checks"].append({"scenario": scenario, "task": task, "approvalID": pending["id"],
        "request": before, "lateDecisionHTTPStatus": status, "counts": run.counts(task, 1, 0),
        "taskOutcome": settled["status"]["execution"]["outcome"], "executionOutcome": final["executionOutcome"]})
    run.save()


def review_permissions(work, run):
    task = run.create("permissions")
    pending = run.pending(task)
    reader = (work / "private/human-approval-reader.token").read_text().strip()
    path = f"/api/v1/tasks/{task}/approvals/{pending['id']}/decision?namespace={NAMESPACE}"
    status, _ = http_json(run.args.api_url + path, "POST", {"decision": "approve"}, reader, (403,))
    run.still_pending(task, pending)
    run.decision(task, pending, "decline")
    run.settled(task)
    run.report["checks"].append({"scenario": "review-permissions", "task": task,
        "readerDecisionHTTPStatus": status, "counts": run.counts(task, 1, 0)})
    run.save()


def failure_diagnostics(run):
    result = []
    for task in run.owned_tasks:
        try:
            status = run.task(task).get("status", {})
            approval = run.approval(task) or {}
            # Keep lifecycle fields only. Task messages, executable inputs,
            # provider responses, and credentials do not belong in CI artifacts.
            result.append({"task": task, "phase": status.get("phase"),
                "taskOutcome": status.get("execution", {}).get("outcome"),
                "approvalStatus": approval.get("status"),
                "executionOutcome": approval.get("executionOutcome"),
                "approvalCreatedAt": approval.get("createdAt"),
                "approvalExpiresAt": approval.get("expiresAt")})
        except CheckFailed:
            result.append({"task": task, "unavailable": True})
    return result


def runtime_recovery(cluster, run):
    task = run.create("runtime-loss")
    pending = run.pending(task)
    before = request_evidence(run, task, pending)
    witness = run.witness
    boot_digest = "sha256:" + hashlib.sha256(witness["spec"]["operationId"].encode()).hexdigest()
    require(boot_digest == pending["binding"]["supervisorBootIDDigest"],
            "pending approval differs from the enrolled runtime boot")
    pod = cluster.pod(run.args.agent)
    container = run.runtime["spec"]["deployment"]["kubernetesRecovery"]["containerName"]
    retained = {item["name"]: container_state(pod, item["name"]) for item in pod["spec"]["containers"] if item["name"] != container}
    remote = cluster.pod("human-approval-hosted") if run.args.backend == "foundry" and getattr(cluster, "local_hosted", True) else None
    cluster.kill_container(pod, container)
    current = wait("restarted enrolled supervisor", lambda: cluster.restarted(run.args.agent, pod, container))
    for name, state in retained.items():
        require(container_state(current, name) == state, "runtime crash restarted the retained " + name)
    if remote is not None:
        after = cluster.pod("human-approval-hosted")
        require(after["metadata"]["uid"] == remote["metadata"]["uid"] and all(
            container_state(after, item["name"]) == container_state(remote, item["name"])
            for item in remote["spec"]["containers"]), "runtime crash replaced the remote hosted fixture")
    run.recovered(witness, proof_kind="foundry-broker-retirement" if run.args.backend == "foundry" else "kubernetes-container-termination")
    recovery_result(run, task, pending, before, "enrolled-runtime-loss")


def controller_epoch(cluster):
    values = cluster.json("get", "controllerepochs", "-o", "json")["items"]
    matches = [item for item in values if item.get("spec", {}).get("name") == "orka-controller"]
    require(len(matches) <= 1, "controller epoch identity is ambiguous")
    return matches[0].get("status", {}).get("epoch") if matches else None


def controller_crash(cluster, epoch, guard=None):
    require(controller_epoch(cluster) == epoch, "controller epoch changed before the crash")
    before = cluster.pod("orka-controller-manager")
    retained = {item["name"]: container_state(before, item["name"]) for item in before["spec"]["containers"] if item["name"] != "manager"}
    cluster.kill_container(before, "manager", guard)
    after = wait("restarted controller", lambda: cluster.restarted("orka-controller-manager", before, "manager"))
    for name, value in retained.items():
        require(container_state(after, name) == value, "controller crash changed an unrelated container")
    def advanced():
        value = controller_epoch(cluster)
        return value if value and value > epoch else None
    return wait("successor controller epoch", advanced)


def pending_controller_recovery(cluster, runs, epoch):
    pending_runs = []
    for run in runs:
        task = run.create("controller-loss")
        pending = run.pending(task)
        pending_runs.append((run, task, pending, request_evidence(run, task, pending), run.witness))
    current_epoch = controller_crash(cluster, epoch)
    for run, task, pending, evidence, witness in pending_runs:
        run.recovered(witness, current_epoch, proof_kind="authenticated-drain")
        recovery_result(run, task, pending, evidence, "enrolled-controller-loss")
    return current_epoch


def simulator_state(run, task):
    _, result = http_json(run.args.simulator_url + "/admin/state?" + urlencode({"runID": task}))
    require(result.get("simulation") is True and result.get("runID") == task and result.get("mode") == "hold" and
            result.get("releasedAt") is None and result.get("releaseStatus") is None, "simulator hold identity changed")
    return result


def validate_claim(original, current, state):
    require(recovery.identity(original) == recovery.identity(current) and original["spec"] == current["spec"],
            "crash recovery changed the original tool effect")
    status = current.get("status", {})
    require(status.get("state") == state and status.get("attempts") == 1 and
            status.get("response") is None and not status.get("responseDigest"),
            "claimed action was retried or acquired a synthetic result")
    if state == "OutcomeUnknown":
        require(not status.get("leaseOwner") and not status.get("leaseExpiresAt"),
                "unknown action still owns an execution lease")


def active_call(run, task, approval, original_effect):
    state = simulator_state(run, task)
    require(len(state["attempts"]) <= 1, "simulator observed repeated tool execution")
    if not state["attempts"]:
        return None
    current = run.approval(task)
    if current.get("executionOutcome") != "running":
        return None
    require(current.get("id") == approval["id"] and current.get("binding") == approval["binding"] and
            current.get("status") == "approved", "held approval identity changed")
    effect = run.kube_json("get", "externaleffect", original_effect["metadata"]["name"], "-o", "json")
    validate_claim(original_effect, effect, "InFlight")
    attempt = state["attempts"][0]
    require(attempt["ordinal"] == 1 and attempt["endedAt"] is None and attempt["completion"] is None and
            0 <= time.time() - attempt["startedAt"] < 120, "held action lacks a safe pre-timeout crash window")
    run.counts(task, 1, 1)
    return attempt


def inflight_controller_recovery(cluster, runs, epoch):
    prepared = []
    simulator = cluster.pod("human-approval-tools")
    for run in runs:
        name = run.prefix + "-inflight-loss"
        http_json(run.args.simulator_url + "/admin/mode", "POST", {"runID": name, "mode": "hold"})
        task = run.create("inflight-loss")
        approval = run.pending(task)
        prepared.append((run, task, approval, run.approval_effect(task, approval),
                         request_evidence(run, task, approval), run.witness))
    for run, task, approval, *_ in prepared:
        run.still_pending(task, approval)
    for run, task, approval, *_ in prepared:
        run.decision(task, approval, "approve")
    attempts = [run.wait("one claimed held tool call", lambda r=run, n=task, a=approval, e=effect:
                        active_call(r, n, a, e), 60)
                for run, task, approval, effect, _, _ in prepared]

    def guard():
        require(controller_epoch(cluster) == epoch, "controller epoch changed while preparing claimed calls")
        for (run, task, approval, effect, request, _), attempt in zip(prepared, attempts):
            require(active_call(run, task, approval, effect) == attempt,
                    "held execution changed immediately before crash injection")
            require(request_evidence(run, task, approval) == request, "immutable request changed before crash")
        require(time.time() < min(item["startedAt"] for item in attempts) + 120,
                "claimed-call injection window expired")

    injected_at = time.time()
    current_epoch = controller_crash(cluster, epoch, guard)
    injected_at = getattr(cluster, "last_injection_requested_at", injected_at)
    for run, task, approval, original_effect, request, witness in prepared:
        run.recovered(witness, current_epoch, proof_kind="authenticated-drain")
        settled = run.settled(task, lambda: run.counts(task, 1, 1))
        require(settled["status"]["execution"]["outcome"] in {"OutcomeUnknown", "Failed", "Cancelled"},
                "crashed held call incorrectly reported Task success")
        effect = run.kube_json("get", "externaleffect", original_effect["metadata"]["name"], "-o", "json")
        validate_claim(original_effect, effect, "OutcomeUnknown")
        final = run.approval(task)
        require(final.get("id") == approval["id"] and final.get("binding") == approval["binding"] and
                final.get("status") == "approved" and final.get("executionOutcome") == "unknown",
                "claimed approval did not retain its unknown outcome")
        require(request_evidence(run, task, approval) == request, "crash replaced the immutable executable request")
        state = simulator_state(run, task)
        require(len(state["attempts"]) == 1, "crashed call was executed more than once")
        attempt = state["attempts"][0]
        require(attempt["completion"] == "client_disconnected" and
                injected_at - 5 <= attempt["endedAt"] <= injected_at + 30 and attempt["elapsedSeconds"] < 180,
                "held call did not disconnect from the injected controller crash")
        status, _ = run.decision(task, approval, "approve", (409, 410))
        run.report["checks"].append({"scenario": "enrolled-controller-inflight-loss", "task": task,
            "approvalID": approval["id"], "effect": {**recovery.identity(effect), "state": "OutcomeUnknown", "attempts": 1},
            "request": request, "lateDecisionHTTPStatus": status, "executionOutcome": "unknown",
            "taskOutcome": settled["status"]["execution"]["outcome"], "counts": run.counts(task, 1, 1),
            "controllerEpochBefore": epoch, "controllerEpochAfter": current_epoch, "stableSeconds": 0})
        run.save()
    started = time.monotonic()
    while time.monotonic() - started < 30:
        for run, task, approval, effect, request, _ in prepared:
            run.counts(task, 1, 1)
            validate_claim(effect, run.kube_json("get", "externaleffect", effect["metadata"]["name"], "-o", "json"), "OutcomeUnknown")
            require(request_evidence(run, task, approval) == request, "unknown outcome changed its original request")
        time.sleep(1)
    after = cluster.pod("human-approval-tools")
    require(after["metadata"]["uid"] == simulator["metadata"]["uid"] and
            container_state(after, "tools") == container_state(simulator, "tools"), "crash replaced the counted simulator")
    for run in runs:
        run.report["checks"][-1]["stableSeconds"] = 30
        run.save()
    return current_epoch


def run_acceptance(cluster, args):
    work = args.work
    api_identity(cluster, work)
    cluster.call("apply", "-f", str(ROOT / "examples/human-approval-v2/tools.yaml"))
    # Keep the Tool timeout below the four-minute approval execution budget.
    # The held-call crash must happen well before this independent timeout.
    cluster.call("patch", "tool", "create-work-order", "--type=merge", "--patch-file=/dev/stdin",
                 body={"spec": {"http": {"timeout": "239s"}}})
    epoch = wait("controller epoch", lambda: controller_epoch(cluster))
    render_runtimes.SETUP = work
    images = json.loads((work / "images.json").read_text())
    composed = json.loads((work / "composed-images.json").read_text())
    items = render_runtimes.fixture_resources(images["fixture"]["image"])
    for provider in ("agentkit", "foundry"):
        profile = json.loads((work / (provider + "-profile.json")).read_text())
        items += render_runtimes.runtime_resources(provider, profile, composed[provider], images, str(epoch),
                                                   "http://orka-api.orka-system.svc:8080")
    cluster.apply({"apiVersion": "v1", "kind": "List", "items": items})
    for name in ("model", "tools", "hosted", "agentkit", "foundry"):
        subprocess.run([*cluster.command, "rollout", "status", "deployment/human-approval-" + name,
                        "--timeout=300s"], check=True, timeout=320)
    forwards, runs = [], []
    try:
        def forward(name, port):
            value = Forward(cluster, work, name, port)
            forwards.append(value)
            return value.url
        api = forward("orka-api", 8080)
        simulator = forward("human-approval-tools", 8099)
        for provider in ("agentkit", "foundry"):
            url = forward("human-approval-" + provider, 8080)
            _, witness = register(cluster, work, provider, url)
            run_args = argparse.Namespace(backend=provider, context=args.context, namespace=NAMESPACE,
                api_url=api, simulator_url=simulator, capabilities_url=url,
                api_token_file=work / "private/human-approval-reviewer.token",
                profile_json=work / (provider + "-profile.json"), agent="human-approval-" + provider,
                runtime="human-approval-" + provider + "-runtime", task_prefix="ci-" + provider,
                wait_seconds=300, hold_seconds=125, report=work / "evidence" / (provider + ".json"))
            runs.append(FixtureAcceptance(run_args, cluster, witness))

        def scenarios(run):
            review_permissions(work, run)
            for scenario in ("approve", "decline", "cancel", "expire"):
                print("Starting " + run.args.backend + " " + scenario, flush=True)
                run.run_case(scenario)
        # Both backends must survive the ordinary proxy timeout and the full
        # 600-second review expiry. Independent counters isolate each run.
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(scenarios, run) for run in runs]
            for future in futures:
                future.result()

        for run in runs:
            runtime_recovery(cluster, run)
        current_epoch = pending_controller_recovery(cluster, runs, epoch)
        current_epoch = inflight_controller_recovery(cluster, runs, current_epoch)
        # Cluster teardown is not evidence of normal cleanup. Keep the
        # controller and supervisors alive until receipts and finalizers finish.
        for run in runs:
            run.cleanup_tasks()
            cleanup_authority(cluster, run)
        cleanup = {"passed": True, "taskCount": sum(len(run.owned_tasks) for run in runs),
                   "normallyDeletedTasks": sum(len(run.report["cleanup"]) for run in runs),
                   "agentRuntimesDeleted": [run.report["authorityCleanup"]["runtime"] for run in runs],
                   "retainedBootAuthorityCount": 0}
        require(cleanup["taskCount"] == cleanup["normallyDeletedTasks"], "some fixture Tasks lack normal cleanup evidence")
        (work / "evidence" / "cleanup.json").write_text(json.dumps(cleanup, indent=2) + "\n")
        for run in runs:
            run.report.update(passed=True, completedAt=datetime.now(timezone.utc).isoformat(),
                              controllerEpochBefore=epoch, controllerEpochAfter=current_epoch, transport="local-fixture")
            run.save()
    except BaseException as error:
        (work / "evidence" / "failure.json").write_text(json.dumps({"passed": False,
            "error": str(error), "normalCleanupPassed": False}, indent=2) + "\n")
        for run in runs:
            run.report["error"] = str(error)
            run.report["diagnostics"] = failure_diagnostics(run)
            run.save()
        raise
    finally:
        for value in reversed(forwards):
            value.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("secrets", "run"))
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--context", required=True)
    parser.add_argument("--cluster", required=True)
    args = parser.parse_args()
    cluster = Cluster(args)
    if args.mode == "secrets":
        prepare_secrets(cluster)
    else:
        run_acceptance(cluster, args)


if __name__ == "__main__":
    main()
