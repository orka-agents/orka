#!/usr/bin/env python3
"""Exercise prepared v2 runtimes through Orka's real Task and approval APIs."""

import argparse
import copy
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener
import uuid

from simulated_tools import SUMMARY


TOOLS = ["create-work-order", "read-inventory"]
FINALIZER = "human-approval-v2.orka.ai/acceptance-observer"
TERMINAL = {"Succeeded", "Failed", "Cancelled", "OutcomeUnknown"}


class CheckFailed(RuntimeError):
    pass


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, _request, _response, _code, _message, _headers, _url):
        return None


def require(condition, message):
    if not condition:
        raise CheckFailed(message)


def http_json(url, method="GET", body=None, token="", allowed=(200,)):
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if body is not None:
        headers["Content-Type"] = "application/json"
    request = Request(url, data=None if body is None else json.dumps(body).encode(), headers=headers, method=method)
    try:
        response = build_opener(NoRedirect).open(request, timeout=30)
    except HTTPError as error:
        response = error
    except (URLError, TimeoutError) as error:
        raise CheckFailed(f"{method} request did not return a response; inspect the existing Task before retrying") from error
    with response:
        status = response.code
        raw = response.read(2 << 20)
    require(status in allowed, f"{method} request returned HTTP {status}")
    if status != 200 and status != 201:
        return status, None
    try:
        return status, json.loads(raw)
    except (ValueError, UnicodeError) as error:
        raise CheckFailed("API response was not valid JSON") from error


def validate_runtime(runtime, agent, capabilities, expected, backend):
    spec, status = runtime["spec"], runtime.get("status", {})
    registered = spec["capabilities"]
    observed = status.get("observedCapabilities", {})
    profile = registered["profile"]
    require(not runtime["metadata"].get("deletionTimestamp"), "runtime is deleting")
    require(spec.get("contractVersion") == "orka.harness.v2", "runtime must use harness v2")
    require(spec.get("deployment", {}).get("mode") == "external-endpoint", "runtime must be an external endpoint")
    require(status.get("ready") is True and status.get("observedGeneration") == runtime["metadata"]["generation"],
            "runtime has not passed conformance for its current generation")
    require(profile == expected["profile"] and registered["mcpPolicy"] == expected["mcpPolicy"],
            "registration differs from the generated, pinned profile and approval policy")
    require(registered["mcpPolicy"] == {
        "allowedTools": TOOLS, "disallowedTools": [], "allowBash": False,
        "approvalRequiredTools": ["create-work-order"],
    }, "runtime must approve only create-work-order")
    require(profile.get("providerKind") == backend and profile.get("adapterName") == backend + "-serve-acp",
            "runtime uses a different backend")
    require(profile.get("workspaceIntent") == "read", "fixture requires a read workspace")
    require(observed.get("runtimeInstanceID") == registered["runtimeInstanceID"] and
            observed.get("runtimeProfileDigest") == profile["digest"], "runtime observation is stale")
    governance = registered.get("workspaceGovernance", {})
    require(governance.get("mode") == "strict-governed" and not governance.get("trusted", False),
            "runtime requires strict governance")
    for field in ("orkaOwnedWorkspaceDeltas", "promptScopedBrokerAuthorization", "noDirectSCMPublication",
                  "orkaOwnedCleanRoomPublication", "exactInstanceFencing", "duplicateSafeMutations", "cancellationSettlement"):
        require(governance.get(field) is True, f"runtime is missing {field}")
    require(agent["spec"].get("runtime", {}).get("runtimeRef", {}).get("name") == runtime["metadata"]["name"] and
            not agent["metadata"].get("deletionTimestamp"), "Agent does not select the prepared runtime")
    require(capabilities.get("protocol") == "orka.harness.v2" and
            capabilities.get("runtimeProfileDigest") == profile["digest"] and
            capabilities.get("adapterDigests") == {profile["adapterName"]: profile["adapterDigest"]},
            "live runtime protocol, profile, or adapter differs")
    provider = capabilities.get("provider", {})
    require(provider.get("supportsBrokeredToolApprovals") is True and provider.get("supportsPermissions") is False,
            "live runtime lacks the qualified brokered approval capability")
    require(backend in provider.get("providerKinds", []) and capabilities.get("supportsAgentSessionConfiguration", False) is False,
            "live runtime configuration contract differs")
    require(capabilities.get("limits", {}).get("maxConcurrentPrompts", 0) >= 2,
            "fixture needs capacity for an independent prompt during review")


def validate_pending_approval(approval, run_id, task_uid):
    arguments = {"runID": run_id, "asset": "pump-1", "summary": SUMMARY}
    # These fixture arguments contain only ASCII strings, so sorted compact
    # JSON has the same representation as the broker's canonical arguments.
    digest = hashlib.sha256(json.dumps(arguments, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    require(approval.get("status") == "pending" and approval.get("targetTool") == "create-work-order" and
            approval.get("taskUID") == task_uid and approval.get("targetArgsPreview") == arguments and
            approval.get("targetArgsDigest") == digest and approval.get("targetSpecDigest") and
            approval.get("binding") and approval.get("executionOutcome") == "not_started",
            "review differs from the original, unexecuted simulated work-order proposal")
    created = datetime.fromisoformat(approval["createdAt"].replace("Z", "+00:00"))
    expires = datetime.fromisoformat(approval["expiresAt"].replace("Z", "+00:00"))
    require(0 < (expires - created).total_seconds() <= 600, "review does not have the required bounded expiry")


def validate_cancellation(task, approval, result):
    require(approval.get("status") == "cancelled" and approval.get("executionOutcome") == "not_started" and
            approval.get("executionReason") == "approval_cancelled", "cancelled review lost its unexecuted denial")
    status = task.get("status", {})
    outcome = status.get("execution", {}).get("outcome")
    if outcome == "Cancelled":
        require(status.get("phase") == "Cancelled", "cancelled execution has an inconsistent Task phase")
        return
    # The agent may finish with the broker's denial before prompt cancellation
    # reaches it. Preserve that real Completed event and validate its workspace.
    require(outcome == "Succeeded" and status.get("phase") == "Succeeded" and
            status.get("delivery", {}).get("outcome") == "ReadValidated" and
            "approval_cancelled" in result.get("result", ""),
            "cancellation neither stopped the prompt nor completed with a validated denial result")


class Acceptance:
    def __init__(self, args):
        self.args = args
        self.token = args.api_token_file.read_text().strip()
        require(bool(self.token) and not any(character.isspace() for character in self.token), "API token file is invalid")
        self.prefix = args.task_prefix or f"approval-{args.backend}-{int(time.time())}-{uuid.uuid4().hex[:6]}"
        require(re.fullmatch(r"[a-z0-9][a-z0-9-]{0,42}", self.prefix), "task prefix must be at most 43 DNS-safe characters")
        self.owned_tasks = {}
        self.report = {"backend": args.backend, "namespace": args.namespace, "tasks": [], "checks": [], "passed": False}
        self.runtime = self.kube_json("get", "agentruntime", args.runtime, "-o", "json")
        self.agent = self.kube_json("get", "agent", args.agent, "-o", "json")
        namespace = self.kube_json("get", "namespace", args.namespace, "-o", "json")
        require(namespace["metadata"].get("labels", {}).get("orka.ai/controller-mode") == "harness-v2",
                "namespace must be watched by the harness-v2 controller")
        self.namespace_uid = namespace["metadata"]["uid"]
        _, capabilities = http_json(args.capabilities_url.rstrip("/") + "/v2/capabilities")
        validate_runtime(self.runtime, self.agent, capabilities, json.loads(args.profile_json.read_text()), args.backend)
        self.template = self.kube_json("create", "--dry-run=client", "--validate=false", "-f",
                                       str(Path(__file__).with_name("task.yaml")), "-o", "json")
        _, health = http_json(args.simulator_url.rstrip("/") + "/health")
        require(health.get("simulation") is True, "tool endpoint is not the counted simulator")

    def kube_json(self, *arguments, body=None):
        command = ["kubectl", "--context", self.args.context, "--namespace", self.args.namespace,
                   "--request-timeout=30s", *arguments]
        result = subprocess.run(command, input=None if body is None else json.dumps(body),
                                text=True, capture_output=True, check=False)
        require(result.returncode == 0, "scoped kubectl command failed; inspect the selected namespace")
        return json.loads(result.stdout)

    def api(self, path, method="GET", body=None, allowed=(200,)):
        return http_json(self.args.api_url.rstrip("/") + "/api/v1/" + path + "?" +
                         urlencode({"namespace": self.args.namespace}), method, body, self.token, allowed)

    def wait(self, description, check, seconds=None):
        deadline = time.monotonic() + (self.args.wait_seconds if seconds is None else seconds)
        while time.monotonic() < deadline:
            result = check()
            if result:
                return result
            time.sleep(1)
        raise CheckFailed(f"timed out waiting for {description}")

    def counts(self, run_id, reads, executions):
        _, counts = http_json(self.args.simulator_url.rstrip("/") + "/counts?" + urlencode({"runID": run_id}))
        require(counts.get("simulation") is True and counts.get("runID") == run_id, "simulator returned the wrong run")
        require(counts.get("inventoryReads") == reads and counts.get("workOrderExecutions") == executions,
                f"{run_id}: expected {reads} inventory reads and {executions} executions, got "
                f"{counts.get('inventoryReads')} and {counts.get('workOrderExecutions')}")
        return counts

    def task(self, name):
        _, value = self.api("tasks/" + name)
        if name in self.owned_tasks:
            require(value["metadata"]["uid"] == self.owned_tasks[name], "Task identity changed during acceptance")
        return value

    def create(self, suffix, inventory_only=False):
        name = self.prefix + "-" + suffix
        self.counts(name, 0, 0)
        spec = copy.deepcopy(self.template["spec"])
        spec["agentRef"]["name"] = self.args.agent
        spec["prompt"] = spec["prompt"].replace("__RUN_ID__", name)
        if inventory_only:
            spec["prompt"] = f"Call read-inventory exactly once with runID {name} and asset pump-1. Report its availability. Do not call any other tool."
        self.report["tasks"].append({"name": name})
        self.save()
        _, created = self.api("tasks", "POST", {"name": name, "namespace": self.args.namespace, "spec": spec}, (201,))
        self.owned_tasks[name] = created["metadata"]["uid"]
        self.report["tasks"][-1]["uid"] = created["metadata"]["uid"]
        self.save()
        bound = self.wait("the immutable Task binding", lambda: self.task(name).get("status", {}).get("agentExecutionBinding"))
        require(bound.get("contractVersion") == "orka.harness.v2" and bound.get("backend") == "external-endpoint" and
                bound.get("task", {}).get("uid") == created["metadata"]["uid"] and
                bound.get("task", {}).get("boundSpecGeneration") == created["metadata"]["generation"] and
                bound.get("task", {}).get("namespaceUID") == self.namespace_uid and
                bound.get("runtimeRef", {}).get("name") == self.runtime["metadata"]["name"] and
                bound.get("runtimeRef", {}).get("uid") == self.runtime["metadata"]["uid"] and
                bound.get("runtimeRef", {}).get("generation") == self.runtime["metadata"]["generation"] and
                bound.get("agent", {}).get("name") == self.agent["metadata"]["name"] and
                bound.get("agent", {}).get("namespace") == self.args.namespace and
                bound.get("agent", {}).get("uid") == self.agent["metadata"]["uid"] and
                bound.get("agent", {}).get("generation") == self.agent["metadata"]["generation"] and
                bound.get("runtimeProfileDigest") == self.runtime["spec"]["capabilities"]["profile"]["digest"],
                "Task binding differs from the preflight identities")
        return name

    def approval(self, name):
        _, result = self.api("tasks/" + name + "/approvals")
        values = result.get("approvals", [])
        require(len(values) <= 1, f"{name}: a completed step was repeated or another approval was requested")
        return values[0] if values else None

    def pending(self, name):
        def check():
            approval = self.approval(name)
            require(self.task(name).get("status", {}).get("execution", {}).get("outcome") not in TERMINAL,
                    "Task finished before a pending review was observed")
            return approval
        approval = self.wait("a persisted approval", check)
        validate_pending_approval(approval, name, self.owned_tasks[name])
        self.counts(name, 1, 0)
        return approval

    def still_pending(self, name, original):
        current = self.approval(name)
        require(current, "pending review disappeared")
        validate_pending_approval(current, name, self.owned_tasks[name])
        require(current and current["id"] == original["id"] and current["status"] == "pending" and
                current.get("binding") == original["binding"] and
                current.get("targetSpecDigest") == original["targetSpecDigest"] and
                current.get("expiresAt") == original["expiresAt"], "pending review identity changed or settled early")
        self.counts(name, 1, 0)

    def decision(self, name, approval, decision, allowed=(200,)):
        return self.api(f"tasks/{name}/approvals/{approval['id']}/decision", "POST",
                        {"decision": decision, "reason": "Simulated human approval acceptance"}, allowed)

    def settled(self, name, while_waiting=None):
        def check():
            if while_waiting:
                while_waiting()
            task = self.task(name)
            status = task.get("status", {})
            execution = status.get("execution", {})
            return task if execution.get("outcome") in TERMINAL and status.get("completionTime") else None
        return self.wait("Task settlement", check)

    def observe(self, name, enabled):
        task = self.kube_json("get", "task", name, "-o", "json")
        metadata = task["metadata"]
        require(metadata["uid"] == self.owned_tasks[name], "refusing to update a replaced Task")
        finalizers = metadata.get("finalizers", [])
        updated = [value for value in finalizers if value != FINALIZER]
        if enabled:
            updated.append(FINALIZER)
        self.kube_json("patch", "task", name, "--type=json", "--patch-file=/dev/stdin", "-o", "json", body=[
            {"op": "test", "path": "/metadata/uid", "value": metadata["uid"]},
            {"op": "test", "path": "/metadata/resourceVersion", "value": metadata["resourceVersion"]},
            {"op": "add", "path": "/metadata/finalizers", "value": updated},
        ])

    def run_case(self, scenario):
        name = self.create(scenario)
        original = self.pending(name)
        pending_since = time.monotonic()
        observed_wait = None
        if scenario == "approve":
            other = self.create("independent", inventory_only=True)
            finished = self.settled(other, lambda: self.still_pending(name, original))
            require(finished["status"]["execution"]["outcome"] == "Succeeded", "independent Task did not succeed during review")
            self.counts(other, 1, 0)
            require(self.approval(other) is None, "automatic inventory lookup required approval")
            while time.monotonic() - pending_since < self.args.hold_seconds:
                self.still_pending(name, original)
                time.sleep(1)
            observed_wait = time.monotonic() - pending_since
            _, decided = self.decision(name, original, "approve")
            require(decided["id"] == original["id"] and decided["status"] == "approved", "decision did not approve the original call")
            # An identical decision is idempotent while active. A Task that
            # already settled may reject every further decision as stale.
            self.decision(name, original, "approve", (200, 409))
            self.decision(name, original, "decline", (409,))
        elif scenario == "decline":
            self.still_pending(name, original)
            self.decision(name, original, "decline")
            self.decision(name, original, "approve", (409,))
        elif scenario == "expire":
            def expired():
                self.counts(name, 1, 0)
                current = self.approval(name)
                return current if current and current["status"] == "expired" else None
            self.wait("the real 600-second review expiry", expired, 660)
            self.decision(name, original, "approve", (409, 410))
        elif scenario == "cancel":
            self.observe(name, True)
            self.api("tasks/" + name, "DELETE", allowed=(204,))
        elif scenario == "recovery":
            print(f"Recovery ready: Task {name} is pending. Restart the selected test runtime now; keep the Task and broker ledger.", flush=True)
            def authority_lost():
                self.counts(name, 1, 0)
                # Read-only probes may fail while the operator restarts a process.
                try:
                    task = self.task(name)
                except CheckFailed:
                    return None
                execution = task.get("status", {}).get("execution", {})
                runtime = self.kube_json("get", "agentruntime", self.args.runtime, "-o", "json")
                boot = runtime.get("status", {}).get("observedCapabilities", {}).get("supervisorBootID")
                restarted = bool(boot) and boot != original["binding"]["supervisorBootID"]
                return task if restarted and execution.get("outcome") in TERMINAL else None
            self.wait("the original attempt to settle after runtime recovery", authority_lost)
            self.decision(name, original, "approve", (409, 410))

        task = self.settled(name)
        if scenario == "cancel":
            self.decision(name, original, "approve", (409, 410))
        elif scenario != "recovery":
            require(task["status"]["execution"]["outcome"] == "Succeeded", "runtime did not continue with the final tool outcome")
        final_approval = self.approval(name)
        counts = self.counts(name, 1, 1 if scenario == "approve" else 0)
        if scenario == "approve":
            require(final_approval["status"] == "approved" and final_approval.get("executionOutcome") == "succeeded",
                    "approval execution did not settle successfully")
            _, result = self.api("tasks/" + name + "/result")
            require(counts["workOrderIDs"][0] in result.get("result", ""), "original Task result lost the work-order receipt")
        else:
            require(final_approval.get("executionOutcome") == "not_started", "blocked action was started")
        if scenario == "cancel":
            result = {}
            if task["status"]["execution"]["outcome"] == "Succeeded":
                _, result = self.api("tasks/" + name + "/result")
            validate_cancellation(task, final_approval, result)
        self.report["checks"].append({
            "scenario": scenario, "task": name, "approvalID": original["id"],
            "reviewObservedSeconds": round(observed_wait, 1) if observed_wait is not None else None,
            "approvalStatus": final_approval["status"], "executionOutcome": final_approval.get("executionOutcome"),
            "taskOutcome": task["status"]["execution"]["outcome"], "taskPhase": task["status"]["phase"],
            "deliveryOutcome": task["status"].get("delivery", {}).get("outcome"), "counts": counts,
        })
        self.save()
        if scenario == "cancel":
            self.observe(name, False)
        print(f"Passed {scenario}: {counts['inventoryReads']} lookup, {counts['workOrderExecutions']} work-order executions.", flush=True)

    def save(self):
        self.args.report.parent.mkdir(parents=True, exist_ok=True)
        self.args.report.write_text(json.dumps(self.report, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--backend", choices=("agentkit", "foundry"), required=True)
    parser.add_argument("--api-url", required=True)
    parser.add_argument("--api-token-file", type=Path, required=True)
    parser.add_argument("--capabilities-url", required=True, help="reachable base URL for the selected supervisor's public v2 probes")
    parser.add_argument("--simulator-url", required=True)
    parser.add_argument("--profile-json", type=Path, required=True, help="output of this example's Go profile renderer")
    parser.add_argument("--agent")
    parser.add_argument("--runtime")
    parser.add_argument("--task-prefix")
    parser.add_argument("--scenarios", default="approve,decline,cancel,expire", help="comma-separated approve,decline,cancel,expire,recovery")
    parser.add_argument("--hold-seconds", type=int, default=125, help="minimum pending wait before the approve scenario")
    parser.add_argument("--wait-seconds", type=int, default=900)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    args.agent = args.agent or "human-approval-" + args.backend
    args.runtime = args.runtime or "human-approval-" + args.backend + "-runtime"
    scenarios = args.scenarios.split(",")
    if not 0 <= args.hold_seconds <= 540:
        parser.error("hold seconds must be between 0 and 540")
    if args.wait_seconds <= 0:
        parser.error("wait seconds must be positive")
    if len(scenarios) != len(set(scenarios)) or any(
        value not in {"approve", "decline", "cancel", "expire", "recovery"} for value in scenarios
    ):
        parser.error("scenarios must be unique known names")
    for base in (args.api_url, args.capabilities_url, args.simulator_url):
        parsed = urlsplit(base)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
            parser.error("URLs must be HTTP(S) base URLs without credentials, query, or fragment")
    run = None
    try:
        run = Acceptance(args)
        for scenario in scenarios:
            print(f"Starting {args.backend} {scenario}", flush=True)
            run.run_case(scenario)
        run.report["passed"] = True
        run.report["completedAt"] = datetime.now(timezone.utc).isoformat()
        run.save()
    except (CheckFailed, OSError, ValueError, KeyError) as error:
        if run is not None:
            run.report["error"] = str(error)
            run.save()
        print(f"Acceptance failed: {error}. Preserve the created Tasks and simulator ledger for inspection.", file=sys.stderr)
        return 1
    print(f"Acceptance passed for {args.backend}; evidence is in {args.report}. Tasks are retained except the cancelled Task.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
