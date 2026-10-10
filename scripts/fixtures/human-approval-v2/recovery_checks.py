"""Enrolled recovery and normal cleanup checks shared by fixture adapters.

The caller supplies a scoped cluster with call/json/pod methods. This module
does not choose a context, inject crashes, re-register a boot, or edit status.
"""

import base64
import copy
from datetime import datetime
import hashlib
import json
import re

from acceptance import CheckFailed, TERMINAL, require


OWNER = "orka.ai/agent-runtime-recovery-uid"
POD_FINALIZER = "orka.ai/agent-runtime-boot-retirement"
TASK_FINALIZER = "orka.ai/cleanup"
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def digest(value, domain=None):
    raw = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
    if domain:
        raw = ("orka.acp." + domain + "\0").encode() + raw
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def identity(value):
    meta = value["metadata"]
    return {key: meta[key] for key in ("name", "namespace", "uid")}


def optional(cluster, kind, name):
    raw = cluster.call("get", kind, name, "--ignore-not-found", "-o", "json")
    return json.loads(raw) if raw.strip() else None


def secret_metadata(cluster):
    # kubectl renders only metadata. No token or retained-auth bytes enter the
    # test process, logs, or report when discovering cleanup authority.
    raw = cluster.call("get", "secrets", "-o",
                       'jsonpath={range .items[*]}{.metadata}{"\\n"}{end}')
    return [{"kind": "Secret", "metadata": json.loads(line)} for line in raw.splitlines() if line.strip()]


def validate_deployment(deployment, provider):
    spec, meta = deployment["spec"], deployment["metadata"]
    pod = spec["template"]["spec"]
    name = "supervisor" if provider == "foundry" else "runtime"
    containers = pod["containers"]
    require(meta.get("uid") and not meta.get("deletionTimestamp") and spec.get("replicas") == 1 and
            spec.get("strategy", {}).get("type") == "Recreate" and not spec.get("paused"),
            "runtime recovery requires an exact live single-replica Recreate Deployment")
    require(not pod.get("initContainers") and not pod.get("ephemeralContainers") and
            not any(pod.get(key) for key in ("hostPID", "hostIPC", "hostNetwork", "shareProcessNamespace")) and
            pod.get("automountServiceAccountToken") is False,
            "runtime recovery requires private namespaces without init or ephemeral containers")
    expected = {"supervisor", "broker"} if provider == "foundry" else {"runtime"}
    require(len(containers) == len(expected) and {item["name"] for item in containers} == expected,
            "runtime recovery container topology is unsupported")
    supervisor = next(item for item in containers if item["name"] == name)
    for item in containers:
        require(DIGEST.fullmatch(item["image"].partition("@")[2]) and not item.get("envFrom"),
                "runtime recovery needs digest-pinned images and explicit environment")
        require(not any(env["name"] == "ORKA_ACP_SUPERVISOR_BOOT_ID" for env in item.get("env", [])),
                "recovery must not configure a fixed supervisor boot ID")
    environment = {env["name"]: env.get("value") for env in supervisor.get("env", [])}
    instance, epoch = environment.get("ORKA_ACP_RUNTIME_INSTANCE_ID"), environment.get("ORKA_ACP_CONTROLLER_EPOCH", "")
    require(instance and epoch.isdecimal() and int(epoch) > 0,
            "recovery needs a stable runtime instance and one literal controller epoch")
    return name, instance, int(epoch)


def enroll(cluster, runtime, provider):
    """Create the registration and bind consent before a Task can be submitted."""
    deployment = cluster.json("get", "deployment", runtime["metadata"]["name"].removesuffix("-runtime"), "-o", "json")
    container, instance, _ = validate_deployment(deployment, provider)
    require(runtime["spec"]["capabilities"].get("supportsDrain") is True,
            "enrolled runtime must advertise authenticated drain")
    value = copy.deepcopy(runtime)
    value["spec"]["capabilities"]["runtimeInstanceID"] = instance
    value["spec"]["deployment"]["kubernetesRecovery"] = {
        "deploymentName": deployment["metadata"]["name"], "deploymentUID": deployment["metadata"]["uid"],
        "containerName": container,
    }
    cluster.call("create", "-f", "-", body=value)
    registered = cluster.json("get", "agentruntime", value["metadata"]["name"], "-o", "json")
    annotations = dict(deployment["metadata"].get("annotations", {}))
    require(not annotations.get(OWNER), "runtime Deployment already has a recovery owner")
    annotations[OWNER] = registered["metadata"]["uid"]
    cluster.json("patch", "deployment", deployment["metadata"]["name"], "--type=json", "--patch-file=/dev/stdin", "-o", "json", body=[
        {"op": "test", "path": "/metadata/uid", "value": deployment["metadata"]["uid"]},
        {"op": "test", "path": "/metadata/resourceVersion", "value": deployment["metadata"]["resourceVersion"]},
        {"op": "add", "path": "/metadata/annotations", "value": annotations},
    ])
    return registered


def completed_effect(cluster, kind, aggregate, operation=None):
    effects = cluster.json("get", "externaleffects", "-o", "json")["items"]
    matches = [item for item in effects if item["spec"].get("kind") == kind and
               item["spec"].get("aggregateId") == aggregate and
               (operation is None or item["spec"].get("operationId") == operation)]
    require(len(matches) <= 1, "recovery identity has duplicate durable effects")
    if not matches:
        return None
    effect = matches[0]
    status = effect.get("status", {})
    if status.get("state") in {None, "", "Pending"}:
        return None
    require(status.get("state") == "Succeeded" and isinstance(status.get("response"), dict) and
            status.get("responseDigest") == digest(status["response"]),
            "recovery effect is not a complete immutable observation")
    return effect


def effect_evidence(effect):
    return {**identity(effect), "kind": effect["spec"]["kind"],
            "requestDigest": effect["spec"]["requestDigest"],
            "responseDigest": effect["status"]["responseDigest"]}


def ready_witness(cluster, frozen, provider, previous_boot=None, expected_epoch=None):
    current = cluster.json("get", "agentruntime", frozen["metadata"]["name"], "-o", "json")
    require(identity(current) == identity(frozen) and current["metadata"]["generation"] == frozen["metadata"]["generation"] and
            current["spec"] == frozen["spec"], "recovery replaced or re-registered the original runtime")
    status = current.get("status", {})
    observed = status.get("observedCapabilities", {})
    boot = observed.get("supervisorBootID")
    if not status.get("ready") or status.get("observedGeneration") != current["metadata"]["generation"] or not boot or boot == previous_boot:
        return None
    if expected_epoch is not None and observed.get("controllerEpoch") != expected_epoch:
        return None
    enrollment = current["spec"]["deployment"]["kubernetesRecovery"]
    deployment = cluster.json("get", "deployment", enrollment["deploymentName"], "-o", "json")
    container, instance, epoch = validate_deployment(deployment, provider)
    require(deployment["metadata"]["uid"] == enrollment["deploymentUID"] and
            deployment["metadata"].get("annotations", {}).get(OWNER) == current["metadata"]["uid"] and
            container == enrollment["containerName"] and instance == current["spec"]["capabilities"]["runtimeInstanceID"],
            "recovery Deployment enrollment changed")
    require(observed.get("runtimeInstanceID") == instance and observed.get("controllerEpoch") == epoch,
            "ready runtime has a different instance or Deployment epoch")
    effect = completed_effect(cluster, "agent-runtime-boot-witness", current["metadata"]["uid"], boot)
    if effect is None:
        return None
    witness = effect["status"]["response"]
    pod = cluster.pod(enrollment["deploymentName"])
    state = next(item for item in pod["status"]["containerStatuses"] if item["name"] == container)
    require(witness.get("schemaVersion") == 1 and witness.get("runtimeUID") == current["metadata"]["uid"] and
            witness.get("runtimeGeneration") == current["metadata"]["generation"] and witness.get("spec") == current["spec"] and
            witness.get("deploymentUID") == deployment["metadata"]["uid"] and witness.get("podUID") == pod["metadata"]["uid"] and
            witness.get("containerID") == state["containerID"] and witness.get("restartCount") == state["restartCount"] and
            witness.get("imageID") == state["imageID"] and witness.get("startedAt") == state["state"]["running"]["startedAt"] and
            witness.get("fence", {}).get("supervisorBootID") == boot and witness["fence"].get("runtimeInstanceID") == instance and
            witness["fence"].get("controllerEpoch") == epoch and POD_FINALIZER in pod["metadata"].get("finalizers", []) and
            effect["spec"]["requestDigest"] == digest(witness),
            "ready runtime lacks its exact retained pre-admission boot witness")
    if provider == "foundry":
        broker = witness.get("foundryBroker", {})
        require(broker.get("protocol") == "orka.foundry.broker.v1" and DIGEST.fullmatch(broker.get("ledgerIdentityDigest", "")) and
                broker.get("agentConfigurationDigest") == current["spec"]["capabilities"]["profile"]["agentConfigurationDigest"],
                "Foundry enrollment lacks authenticated broker identity; upgrade the pinned adapter")
    return current, effect


def exposed_task(cluster, run, task_name, witness_effect):
    task = run.task(task_name)
    execution = task.get("status", {}).get("execution", {})
    if not execution.get("runtimeSessionUID"):
        return None
    effect = completed_effect(cluster, "agent-runtime-session-exposure", run.owned_tasks[task_name])
    if effect is None:
        return None
    exposure = effect["status"]["response"]
    witness = witness_effect["status"]["response"]
    binding = task["status"]["agentExecutionBinding"]
    fence = exposure.get("fence", {})
    require(exposure.get("taskUID") == task["metadata"]["uid"] and exposure.get("attempt") == execution.get("attempt") and
            exposure.get("promptID") == execution.get("promptID") and exposure.get("requestDigest") == execution.get("requestDigest") and
            exposure.get("bindingDigest") == binding.get("bindingDigest") and
            exposure.get("snapshotDigest") == binding.get("snapshot", {}).get("digest") and
            exposure.get("runtimeUID") == run.runtime["metadata"]["uid"] and
            exposure.get("witnessDigest") == witness_effect["spec"]["requestDigest"] and
            all(fence.get(key) == value for key, value in witness["fence"].items()) and
            fence.get("runtimeSessionUID") == execution["runtimeSessionUID"] and
            fence.get("runtimeSessionGeneration") == execution.get("runtimeSessionGeneration"),
            "Task admission lacks its exact enrolled boot exposure")
    return effect


def retirement(cluster, witness_effect, expected_kind=None):
    witness = witness_effect["status"]["response"]
    effect = completed_effect(cluster, "agent-runtime-boot-retirement", witness["runtimeUID"], witness["fence"]["supervisorBootID"])
    if effect is None:
        return None
    proof = effect["status"]["response"]
    kind = proof.get("kind")
    require(effect["spec"]["requestDigest"] == witness_effect["spec"]["requestDigest"] and
            proof.get("schemaVersion") == 1 and proof.get("witnessDigest") == witness_effect["spec"]["requestDigest"] and
            kind in {"authenticated-drain", "kubernetes-container-termination", "foundry-broker-retirement"} and
            (expected_kind is None or kind == expected_kind), "boot retirement does not prove the enrolled owner")
    if kind == "authenticated-drain":
        status = proof.get("drainedStatus", {})
        drain, pressure = status.get("drain", {}), status.get("pressure", {})
        require(not proof.get("containerTermination") and not proof.get("foundryRetirement") and
                status.get("fence") == witness["fence"] and drain.get("requested") is True and
                drain.get("acceptingNewSessions") is False and all(pressure.get(key) == 0 for key in
                ("residentSessions", "activePrompts", "queuedAdmissions", "pendingPermissions", "liveDescendants")) and
                not any(status.get(key) for key in ("sessions", "activePrompts", "pendingPermissions")),
                "boot retirement lacks exact closed and idle drain evidence")
    else:
        terminal = proof.get("containerTermination", {})
        require(not proof.get("drainedStatus") and terminal.get("containerID") == witness["containerID"] and
                terminal.get("startedAt") == witness["startedAt"] and terminal.get("finishedAt"),
                "boot retirement lacks the original container termination")
        require(datetime.fromisoformat(terminal["finishedAt"].replace("Z", "+00:00")) >=
                datetime.fromisoformat(terminal["startedAt"].replace("Z", "+00:00")),
                "boot retirement has an invalid termination time")
        if kind == "foundry-broker-retirement":
            remote = proof.get("foundryRetirement", {})
            request, response, relay = remote.get("request", {}), remote.get("response", {}), remote.get("relay", {})
            broker = witness.get("foundryBroker")
            sealed = response.get("proof", {})
            require(broker and request.get("broker") == broker and request.get("retiredFence") == witness["fence"] and
                    request.get("metadata", {}).get("fence") == relay.get("fence") and
                    relay.get("fence", {}).get("supervisorBootID") != witness["fence"]["supervisorBootID"] and
                    relay.get("foundryBroker") == broker and relay.get("runtimeUID") == witness["runtimeUID"] and
                    response.get("relayFence") == relay.get("fence") and
                    response.get("requestDigest") == request.get("metadata", {}).get("requestDigest") and
                    all(sealed.get(key) == value for key, value in broker.items()) and
                    sealed.get("state") == "retired" and all(sealed.get(key) is True for key in
                    ("sealed", "settlementProven", "retirementProven")) and all(sealed.get(key) == 0 for key in
                    ("activeInvocations", "ambiguousInvocations", "pendingCreates")) and
                    DIGEST.fullmatch(sealed.get("ownerSetDigest", "")) and DIGEST.fullmatch(sealed.get("proofDigest", "")),
                    "Foundry retirement lacks its exact sealed remote owner proof")
        else:
            require(not witness.get("foundryBroker") and not proof.get("foundryRetirement"),
                    "local container death cannot prove remote Foundry retirement")
    return effect


def cleanup_receipt(task):
    execution = task.get("status", {}).get("execution", {})
    require(execution.get("outcome") in TERMINAL and task.get("status", {}).get("completionTime"),
            "normal cleanup cannot delete an unsettled Task")
    if not execution.get("runtimeSessionUID"):
        return {"runtimeSessionUID": None}
    body = {"taskUID": task["metadata"]["uid"], "attempt": execution.get("attempt"),
            "runtimeInstanceID": execution.get("runtimeInstanceID"), "runtimeSessionUID": execution["runtimeSessionUID"],
            "runtimeSessionGeneration": execution.get("runtimeSessionGeneration")}
    expected = digest(body, "task-runtime-session-cleanup")
    if execution.get("runtimeSessionCleanupDigest") != expected:
        return None
    return {"runtimeSessionUID": execution["runtimeSessionUID"], "runtimeSessionCleanupDigest": expected}


def delete_exact(cluster, kind, resource):
    require(kind in {"tasks", "agents", "agentruntimes"}, "normal cleanup kind is not allowed")
    live = cluster.json("get", kind, resource["metadata"]["name"], "-o", "json")
    require(identity(live) == identity(resource), "normal cleanup target UID changed")
    if live["metadata"].get("deletionTimestamp"):
        return
    meta = live["metadata"]
    path = f"/apis/core.orka.ai/v1alpha1/namespaces/{meta['namespace']}/{kind}/{meta['name']}"
    # A failed transport is not permission to replay the mutation.
    cluster.call("delete", "--raw", path, "-f", "-", body={"apiVersion": "v1", "kind": "DeleteOptions",
        "preconditions": {"uid": meta["uid"]}, "propagationPolicy": "Background"})


def task_artifacts(cluster, task):
    uid = task["metadata"]["uid"]
    session = task.get("status", {}).get("execution", {}).get("runtimeSessionUID")
    values = secret_metadata(cluster)
    for kind in ("externaleffects", "promptattempts", "runtimesessioncontrols"):
        values.extend(cluster.json("get", kind, "-o", "json")["items"])
    result = []
    for value in values:
        meta, spec = value["metadata"], value.get("spec", {})
        owner = spec.get("owner", {})
        # A Session aggregate may be shared by continuation Tasks. The exact
        # approval Task discovery label takes precedence over that aggregate.
        label = meta.get("labels", {}).get("core.orka.ai/task-uid")
        if value["kind"] == "ExternalEffect" and label and label != uid:
            continue
        if (any(item.get("kind") == "Task" and item.get("uid") == uid for item in meta.get("ownerReferences", [])) or
            meta.get("labels", {}).get("core.orka.ai/task-uid") == uid or
            owner.get("kind") == "Task" and owner.get("uid") == uid or
            spec.get("taskUid") == uid or spec.get("aggregateId") in {uid, session} - {None}):
            result.append({"kind": value["kind"], **identity(value)})
    return result


def absent(cluster, item):
    if item["kind"] == "Secret":
        matches = [value for value in secret_metadata(cluster) if value["metadata"]["name"] == item["name"]]
        require(len(matches) <= 1, "cleanup Secret discovery is ambiguous")
        value = matches[0] if matches else None
    else:
        value = optional(cluster, item["kind"], item["name"])
    if value is None:
        return True
    require(value["metadata"]["uid"] == item["uid"], "cleanup name was reused by another owner")
    return False


def _artifact_key(item):
    return item["kind"], item["namespace"], item["name"]


def _text_digest(value):
    return "sha256:" + hashlib.sha256(value.encode()).hexdigest()


def _effect_snapshot(effect):
    """Record immutable identity and receipt hashes without tool results."""
    meta, spec, status = effect["metadata"], effect["spec"], effect.get("status", {})
    require(effect.get("kind") == "ExternalEffect" and not meta.get("deletionTimestamp") and
            not meta.get("ownerReferences") and spec.get("identityNamespace") == meta["namespace"] and
            DIGEST.fullmatch(spec.get("requestDigest", "")), "retained effect identity is incomplete")
    components = ["external-effect", spec.get("kind"), meta["namespace"], spec.get("aggregateId"), spec.get("operationId")]
    require(all(isinstance(value, str) and value for value in components), "retained effect identity is incomplete")
    encoded = b"".join(str(len(value.encode())).encode() + b":" + value.encode() for value in components)
    logical = "external-effect:sha256:" + hashlib.sha256(encoded).hexdigest()
    suffix = base64.b32encode(hashlib.sha256(logical.encode()).digest()).decode().rstrip("=").lower()
    require(spec.get("id") == logical and meta["name"] == "external-effect-" + suffix,
            "retained effect has a different canonical identity")
    require(status.get("state") in {"Succeeded", "Failed", "OutcomeUnknown"} and
            not status.get("leaseOwner") and not status.get("leaseExpiresAt"),
            "retained effect is pending, active, or still leased")
    if status["state"] == "OutcomeUnknown":
        require(status.get("response") is None and not status.get("responseDigest") and status.get("attempts") == 1,
                "unknown effect must retain one attempt without a synthetic receipt")
    else:
        require(isinstance(status.get("response"), dict) and status.get("responseDigest") == digest(status["response"]),
                "retained effect receipt digest is invalid")
    return {"kind": "ExternalEffect", **identity(effect), "effectKind": spec["kind"],
            "specDigest": digest(spec), "requestDigest": spec["requestDigest"],
            "statusDigest": digest(status), "state": status["state"], "attempts": status.get("attempts", 0),
            "responseDigest": status.get("responseDigest"),
            "discoveryDigest": digest({key: meta.get(key) for key in ("labels", "ownerReferences")})}


def task_identity_evidence(task):
    """Keep only execution identity, never prompts or credentials."""
    status, meta = task.get("status", {}), task["metadata"]
    execution, binding = status.get("execution", {}), status.get("agentExecutionBinding", {})
    fields = ("outcome", "attempt", "promptID", "requestDigest", "agentRuntimeUID", "agentRuntimeName",
              "runtimeInstanceID", "runtimeSessionSupervisorBootID", "runtimeSessionUID", "runtimeSessionGeneration",
              "runtimeSessionCleanupDigest", "runtimeSessionProfileDigest", "controllerEpoch")
    return {"kind": "Task", "metadata": identity(task), "specDigest": digest(task.get("spec", {})),
            "bindingRecordDigest": digest(binding), "status": {"completionTime": status.get("completionTime"),
                "phase": status.get("phase"), "execution": {key: execution[key] for key in fields if key in execution},
                "agentExecutionBinding": {**{key: copy.deepcopy(binding[key]) for key in
                    ("contractVersion", "backend", "bindingDigest", "task", "runtimeRef", "runtimeProfileDigest") if key in binding},
                    "snapshot": {"digest": binding.get("snapshot", {}).get("digest")}}}}


def _approval_evidence(approval):
    binding = copy.deepcopy(approval.get("binding", {}))
    # The old run predates public binding digests. Normalize those archived
    # identities, without rewriting the original report or retaining raw IDs.
    for raw, hashed in (("operationID", "operationIDDigest"), ("runtimeInstanceID", "runtimeInstanceIDDigest"),
                        ("supervisorBootID", "supervisorBootIDDigest")):
        if raw in binding:
            expected = _text_digest(binding.pop(raw))
            require(hashed not in binding or binding[hashed] == expected, "approval has contradictory legacy identity")
            binding[hashed] = expected
    return {"id": approval.get("id"), "taskUID": approval.get("taskUID"), "binding": binding}


def _validate_exposure(task, effect, witness):
    exposure, body = effect["status"]["response"], witness["status"]["response"]
    spec, execution = effect["spec"], task["status"]["execution"]
    binding, meta = task["status"]["agentExecutionBinding"], task["metadata"]
    fence = exposure.get("fence", {})
    operation = digest([meta["uid"], str(execution.get("attempt")), execution.get("promptID"),
        execution.get("agentRuntimeUID"), execution.get("runtimeSessionSupervisorBootID"),
        execution.get("runtimeSessionUID"), str(execution.get("runtimeSessionGeneration"))], "agent-runtime-session-exposure")
    require(spec.get("kind") == "agent-runtime-session-exposure" and spec.get("aggregateId") == meta["uid"] and
            spec.get("operationId") == operation and spec.get("requestDigest") == digest(exposure, "runtime-exposure") and
            effect["status"].get("state") == "Succeeded" and effect["status"].get("attempts", 0) == 0 and
            exposure.get("schemaVersion") == 1 and exposure.get("namespace") == meta["namespace"] and
            exposure.get("taskUID") == meta["uid"] and exposure.get("attempt") == execution.get("attempt") and
            exposure.get("promptID") == execution.get("promptID") and exposure.get("requestDigest") == execution.get("requestDigest") and
            exposure.get("bindingDigest") == binding.get("bindingDigest") and
            exposure.get("snapshotDigest") == binding.get("snapshot", {}).get("digest") and
            binding.get("contractVersion") == "orka.harness.v2" and binding.get("backend") == "external-endpoint" and
            binding.get("task", {}).get("uid") == meta["uid"] and
            exposure.get("runtimeUID") == execution.get("agentRuntimeUID") == binding.get("runtimeRef", {}).get("uid") and
            body.get("runtimeUID") == exposure.get("runtimeUID") and
            body.get("runtimeGeneration") == binding.get("runtimeRef", {}).get("generation") and
            exposure.get("witnessDigest") == witness["spec"].get("requestDigest") == digest(body) and
            witness["spec"].get("kind") == "agent-runtime-boot-witness" and
            witness["spec"].get("aggregateId") == exposure["runtimeUID"] and
            witness["spec"].get("operationId") == fence.get("supervisorBootID") and
            body.get("fence") and all(fence.get(key) == value for key, value in body["fence"].items()) and
            fence.get("runtimeInstanceID") == execution.get("runtimeInstanceID") and
            fence.get("supervisorBootID") == execution.get("runtimeSessionSupervisorBootID") and
            fence.get("runtimeSessionUID") == execution.get("runtimeSessionUID") and
            fence.get("runtimeSessionGeneration") == execution.get("runtimeSessionGeneration") and
            fence.get("runtimeProfileDigest") == binding.get("runtimeProfileDigest"),
            "retained exposure does not prove the exact Task, Session, and enrolled boot")


def _validate_approval(task, effect, approval):
    binding, execution = approval["binding"], task["status"]["execution"]
    spec, status, meta = effect["spec"], effect["status"], task["metadata"]
    require(approval.get("id") and approval.get("taskUID") == meta["uid"] and
            effect["metadata"].get("labels", {}).get("core.orka.ai/task-uid") == meta["uid"] and
            spec.get("kind") == "acp-mcp-tool" and spec.get("aggregateId") == execution.get("runtimeSessionUID") and
            binding.get("requestDigest") == spec.get("requestDigest") and
            binding.get("operationIDDigest") == _text_digest(spec["operationId"]) and
            binding.get("taskAttempt") == execution.get("attempt") and binding.get("promptID") == execution.get("promptID") and
            binding.get("runtimeSessionUID") == execution.get("runtimeSessionUID") and
            binding.get("runtimeSessionGeneration") == execution.get("runtimeSessionGeneration") and
            binding.get("controllerEpoch") == execution.get("controllerEpoch") and
            binding.get("runtimeInstanceIDDigest") == _text_digest(execution.get("runtimeInstanceID", "")) and
            binding.get("supervisorBootIDDigest") == _text_digest(execution.get("runtimeSessionSupervisorBootID", "")),
            "retained approval receipt does not match the original Task and call")
    if status["state"] == "Failed":
        response = status["response"]
        exact = response.get("approvalID") == approval["id"] and not response.get("externalEffectID") and not response.get("requestDigest")
        abandoned = (not response.get("approvalID") and response.get("externalEffectID") == spec["id"] and
                     response.get("requestDigest") == spec["requestDigest"] and response.get("code") == "approval_stale")
        require(status.get("attempts", 0) in {0, 1} and response.get("isError") is True and (exact or abandoned) and
                response.get("code") in {"approval_declined", "approval_expired", "approval_cancelled", "approval_stale"},
                "retained denial does not prove this unexecuted approval")
    else:
        require(status.get("attempts") == 1, "retained action receipt has an unexpected execution count")


def _execution_changed(terminal, admission):
    require(identity(terminal) == identity(admission) and terminal["specDigest"] == admission["specDigest"] and
            terminal["bindingRecordDigest"] == admission["bindingRecordDigest"] and
            terminal["status"]["agentExecutionBinding"] == admission["status"]["agentExecutionBinding"],
            "Task spec or immutable binding changed after admission")
    before, after = admission["status"]["execution"], terminal["status"]["execution"]
    # Successor controllers update the mutable controllerEpoch even when the
    # original session still owns cleanup. The exposure keeps its old epoch.
    fields = ("attempt", "promptID", "requestDigest", "agentRuntimeUID", "runtimeInstanceID",
              "runtimeSessionSupervisorBootID", "runtimeSessionUID", "runtimeSessionGeneration")
    return any(before.get(key) != after.get(key) for key in fields)


def admission_evidence(task, effect, witness):
    saved = task_identity_evidence(task)
    _effect_snapshot(effect)
    _effect_snapshot(witness)
    _validate_exposure(saved, effect, witness)
    return saved


def cleanup_evidence(cluster, task, artifacts, approvals, witnesses, admission=None):
    """Capture exact retained receipts after normal finalization, before GC.

    ExternalEffects are durable idempotency/audit records, not Task children.
    All other discovered artifacts must disappear with normal Task cleanup.
    """
    require(TASK_FINALIZER not in task["metadata"].get("finalizers", []) and cleanup_receipt(task) is not None,
            "Task lacks genuine cleanup receipt or product finalizer release")
    saved_task = task_identity_evidence(task)
    admission = copy.deepcopy(admission if admission is not None else saved_task)
    changed = _execution_changed(saved_task, admission)
    identities = {_artifact_key(item): item for item in artifacts}
    require(len(identities) == len(artifacts) and all(item["kind"] in {
        "ExternalEffect", "Secret", "PromptAttempt", "RuntimeSessionControl"} and
        item["namespace"] == task["metadata"]["namespace"] for item in artifacts), "cleanup artifact inventory is invalid")
    retained = []
    for item in artifacts:
        if item["kind"] != "ExternalEffect":
            continue
        live = optional(cluster, "externaleffect", item["name"])
        require(live is not None and identity(live) == {key: item[key] for key in ("name", "namespace", "uid")},
                "recorded immutable effect disappeared or was replaced")
        record = _effect_snapshot(live)
        if record["effectKind"] == "agent-runtime-session-exposure":
            witness = witnesses.get(live["status"]["response"].get("fence", {}).get("supervisorBootID"))
            require(witness is not None, "retained exposure has no recorded admission witness")
            record["witness"] = _effect_snapshot(witness)
            _validate_exposure(admission, live, witness)
            if changed:
                proof = retirement(cluster, witness)
                require(proof is not None, "changed terminal execution requires genuine original boot retirement")
                record["retirement"] = _effect_snapshot(proof)
        else:
            require(record["effectKind"] == "acp-mcp-tool", "unexpected Task effect cannot be exempted from cleanup")
            matches = [_approval_evidence(value) for value in approvals if value.get("binding", {}).get("requestDigest") == record["requestDigest"]]
            require(len(matches) == 1, "retained effect lacks one original approval binding")
            record["approval"] = matches[0]
            _validate_approval(admission, live, record["approval"])
        retained.append(record)
    require(not admission["status"]["execution"].get("runtimeSessionUID") or
            sum(item["effectKind"] == "agent-runtime-session-exposure" for item in retained) == 1,
            "Task cleanup lacks its exact immutable session exposure")
    return {"taskEvidence": saved_task, "admissionEvidence": admission, "retainedEffects": retained}


def verify_task_cleanup(cluster, record):
    """Read-only proof of transient removal and unchanged retained receipts.

    False means normal garbage collection is still pending. Missing, replaced,
    changed, or nonterminal immutable effects fail immediately.
    """
    task, admission, effects = record["taskEvidence"], record["admissionEvidence"], record["retainedEffects"]
    changed = _execution_changed(task, admission)
    receipt = cleanup_receipt(task)
    require(record.get("productFinalizerReleased") is True and record.get("task") == task["metadata"]["name"] and
            record.get("uid") == task["metadata"]["uid"] and receipt is not None and
            all(record.get(key) == value for key, value in receipt.items()), "cleanup receipt evidence changed")
    artifacts = {_artifact_key(item): item for item in record["artifacts"]}
    retained = {_artifact_key(item): item for item in effects}
    require(len(artifacts) == len(record["artifacts"]) and len(retained) == len(effects) and
            set(retained) == {key for key, value in artifacts.items() if value["kind"] == "ExternalEffect"},
            "cleanup retained-effect inventory differs from the recorded artifacts")
    for key, expected in retained.items():
        require(all(expected.get(field) == artifacts[key].get(field) for field in ("kind", "name", "namespace", "uid")),
                "cleanup retained-effect identity differs from the original artifact")
        live = optional(cluster, "externaleffect", expected["name"])
        require(live is not None and _effect_snapshot(live) == {key: value for key, value in expected.items()
                if key not in {"approval", "witness", "retirement"}}, "retained immutable effect disappeared, changed, or was replaced")
        if expected["effectKind"] == "agent-runtime-session-exposure":
            witness = optional(cluster, "externaleffect", expected["witness"]["name"])
            require(witness is not None and _effect_snapshot(witness) == expected["witness"], "retained admission witness changed")
            _validate_exposure(admission, live, witness)
            if changed:
                proof = retirement(cluster, witness)
                require(proof is not None and _effect_snapshot(proof) == expected.get("retirement"),
                        "changed terminal execution lacks its original boot retirement")
        else:
            require(expected["effectKind"] == "acp-mcp-tool", "unexpected retained Task effect")
            _validate_approval(admission, live, expected["approval"])
    require(not admission["status"]["execution"].get("runtimeSessionUID") or
            sum(item["effectKind"] == "agent-runtime-session-exposure" for item in effects) == 1,
            "Task cleanup lacks its exact immutable session exposure")
    for current in task_artifacts(cluster, task) + task_artifacts(cluster, admission):
        require(artifacts.get(_artifact_key(current)) == current, "unrecorded or replaced Task artifact appeared during cleanup")
    removed = absent(cluster, {"kind": "Task", **identity(task)})
    for item in artifacts.values():
        if item["kind"] != "ExternalEffect":
            removed = absent(cluster, item) and removed
    return removed


def authority_metadata(cluster, runtime_uid):
    return [item for item in secret_metadata(cluster) if any(
        owner.get("kind") == "AgentRuntime" and owner.get("uid") == runtime_uid
        for owner in item["metadata"].get("ownerReferences", []))]


def require_unused_runtime(cluster, agent, runtime):
    """Refuse authority deletion while any Task or Session still references it."""
    for task in cluster.json("get", "tasks", "-o", "json")["items"]:
        binding = task.get("status", {}).get("agentExecutionBinding", {})
        agent_ref, runtime_ref = binding.get("agent", {}), binding.get("runtimeRef", {})
        require(not (task.get("spec", {}).get("agentRef", {}).get("name") == agent["metadata"]["name"] or
                     agent_ref.get("uid") == agent["metadata"]["uid"] or
                     runtime_ref.get("uid") == runtime["metadata"]["uid"] or
                     runtime_ref.get("name") == runtime["metadata"]["name"]),
                "runtime authority still has a referencing Task")
    for control in cluster.json("get", "runtimesessioncontrols", "-o", "json")["items"]:
        require(control.get("status", {}).get("lineage", {}).get("runtimeIdentity") != runtime["metadata"]["uid"],
                "runtime authority still has a referencing Session")
