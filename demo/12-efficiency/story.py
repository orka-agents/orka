#!/usr/bin/env python3
"""Offscreen preparation and evidence for the customer checkout walkthrough.

The recorded commands use the Orka CLI, kubectl, Git, and Podman. This helper
retains their inputs and checks records before the narration advances.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

import yaml

import classifier_billing
import evidence
import infrastructure
import prepare
import run

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
NAMESPACE = "team-payments"
REPOSITORY = "https://github.com/sozercan/orka-demo-inventory"
REVISION = "1e2863c3ad28476e467b29141611be4c56ff3c5a"
NODE_TEST_IMAGE = "docker.io/library/node@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1"
PHASES = ("baseline", "routed")
CUSTOMERS = json.loads((HERE / "customers.json").read_text())
SUPPORT_JOBS = tuple(customer["id"] for customer in CUSTOMERS)
JOBS = (*SUPPORT_JOBS, "engineering")
PROMPTS = {
    customer["id"]: ('Customer report: "' + customer["report"] + '"\n'
                     "Order reference: " + (customer["orderReference"] or "not supplied") + ".\n"
                     "Support has not checked the account or taken any action.\n"
                     "Draft the first reply to this customer in two sentences, using only these facts.\n"
                     + ("In sentence one, acknowledge the reported duplicate charges for order "
                        + customer["orderReference"] + ". In sentence two, only thank the customer "
                        "for sharing the details. Do not ask for an order reference."
                        if customer["orderReference"] else
                        "In sentence one, acknowledge the reported duplicate charges. "
                        "In sentence two, ask the customer to share their order reference.")
                     + " Do not promise any action.")
    for customer in CUSTOMERS
}
PROMPTS.update({
    "engineering": (
        "Debug the repository's intermittent double-charge bug. Investigate "
        "payments/charge.mjs and its failing tests to find the cause, then fix it. Concurrent retries "
        "of one event ID must make one charge and return the same receipt. "
        "A failed attempt must allow a later retry, and different event IDs "
        "must remain independent. Preserve the public function signature and "
        "all existing tests. Run node --test payments/payment.test.mjs before "
        "and after the fix. Change only payments/charge.mjs. End with a short "
        "summary and the actual test counts."
    ),
})


def require(condition, message):
    evidence.require(condition, message)


def agents():
    support = {
        "providerRef": {"name": "semantic-router"},
        "model": {"name": "team-assistant", "maxTokens": 256, "temperature": 0},
        "systemPrompt": {"inline": (
            "Draft a first customer support acknowledgement in exactly two short sentences. "
            "Sentence one expresses sympathy for the reported duplicate charges and includes "
            "the exact order reference if supplied. Sentence two asks for the missing order "
            "reference, or thanks the customer for the details when it is already supplied. "
            "Use only the supplied facts. Do not mention refunds, promise an investigation "
            "or future action, or claim anyone checked the account. Do not call tools. "
            "Example with a reference: I'm sorry you saw duplicate charges for order ORD-9998. "
            "Thank you for sharing the details. "
            "Example without a reference: I'm sorry you saw duplicate charges after checkout froze. "
            "Could you share your order reference? "
            "Use the current customer's details, never the example reference. Return only the reply."
        )},
        "tools": [],
        "resources": {"requests": {"cpu": "100m", "memory": "256Mi"},
                      "limits": {"cpu": "1", "memory": "512Mi"}},
    }
    engineering = {
        "model": {"name": "team-assistant"},
        "runtime": {"type": "codex", "contractVersion": "orka.harness.v2",
                    "defaultMaxTurns": 20, "defaultAllowBash": True},
        "systemPrompt": {"inline": (
            "You are the payments engineer. Work in the repository Orka has "
            "prepared. Read the payment code and tests, make the smallest "
            "correct fix, and run the tests. Preserve all tests. Never commit, "
            "push, change Git configuration, or open a pull request. Orka "
            "publishes the verified change. Keep your final answer under "
            "80 words and report the actual test results."
        )},
    }
    return {"customer-support": support, "payments-engineer": engineering}


def setup():
    """Update only the two story Agents, Provider, and owned payments gateway."""
    if subprocess.run(["podman", "image", "exists", NODE_TEST_IMAGE], capture_output=True).returncode:
        subprocess.run(["podman", "pull", "--quiet", NODE_TEST_IMAGE], check=True)
    kube = prepare.Kubernetes("sertac-aks")
    state = json.loads(prepare.STATE.read_text())
    require(state["context"] == "sertac-aks"
            and kube.get("Namespace", "kube-system")["metadata"]["uid"] == state["cluster_uid"],
            "The setup belongs to a different cluster")
    source = Path(state["providers_source"]["path"])
    before = prepare.preservation(kube, source)
    resources = {"resources": {}, "created_resources": {}}
    for kind, name, spec in [
        ("Provider", "semantic-router", {
            "type": "anthropic", "baseURL": "http://vekil.team-payments.svc.cluster.local:1337",
            "secretRef": {"name": "provider-key", "key": "api-key"}, "defaultModel": "team-assistant",
        }),
        *(("Agent", name, spec) for name, spec in agents().items()),
    ]:
        current = kube.get(kind, name, NAMESPACE, optional=True)
        if current and kind == "Agent" and name == "customer-support" and current["spec"] != spec:
            require(prepare.owned(current), "Refusing an unowned support Agent")
            require({k: v for k, v in current["spec"].items() if k not in ("systemPrompt", "model")}
                    == {k: v for k, v in spec.items() if k not in ("systemPrompt", "model")}
                    and {k: v for k, v in current["spec"]["model"].items() if k != "temperature"}
                    == {k: v for k, v in spec["model"].items() if k != "temperature"},
                    "Support Agent differs outside the revised instructions and sampling setting")
            # Earlier create operations own this field as an Update, which can
            # conflict with server-side Apply even under the same manager name.
            # Change only the intended prompt, fenced to the version inspected.
            kube.run(["-n", NAMESPACE, "patch", "agent", name, "--type=json", "--patch-file=/dev/stdin"], [
                {"op": "test", "path": "/metadata/resourceVersion", "value": current["metadata"]["resourceVersion"]},
                {"op": "replace", "path": "/spec/systemPrompt", "value": spec["systemPrompt"]},
                {"op": "replace", "path": "/spec/model", "value": spec["model"]},
            ])
        kube.apply(prepare.mark({"apiVersion": "core.orka.ai/v1alpha1", "kind": kind,
                                "metadata": {"name": name, "namespace": NAMESPACE}, "spec": spec}), resources)
    configmap = kube.get("ConfigMap", "vekil-providers", NAMESPACE)
    require(prepare.owned(configmap), "Refusing an unowned gateway configuration")
    config = yaml.safe_load(configmap["data"]["providers.yaml"])
    require(all(not provider.get("api_key") for provider in config["providers"]),
            "Gateway configuration unexpectedly contains an inline credential")
    # Codex sends tool definitions and repository context. Use the supported
    # overall budget; Vekil still bounds individual background messages.
    policy = config["policy_profiles"][0]
    classifier = policy["classifier"]
    classifier.update({"max_request_bytes": 65536, "recent_turns": 8})
    # The ACP client supplies an effort preference. The gateway owns the
    # effective setting and keeps local Qwen's configured reasoning disabled.
    for tier, effort in (("lightweight", "none"), ("powerful", "medium")):
        policy[tier]["reasoning_effort"] = effort
        route = next(route for route in config["model_routes"] if route["id"] == policy[tier]["route"])
        route["reasoning_effort"] = [effort]
    kube.run(["-n", NAMESPACE, "patch", "configmap", "vekil-providers", "--type=json", "--patch-file=/dev/stdin"], [
        {"op": "test", "path": "/metadata/resourceVersion", "value": configmap["metadata"]["resourceVersion"]},
        {"op": "replace", "path": "/data/providers.yaml", "value": yaml.safe_dump(config, sort_keys=False)},
    ])
    deployment = kube.get("Deployment", "vekil", NAMESPACE)
    require(prepare.owned(deployment), "Refusing an unowned gateway")
    containers = deployment["spec"]["template"]["spec"]["containers"]
    number = next(i for i, c in enumerate(containers) if c["name"] == "vekil")
    position = next(i for i, e in enumerate(containers[number]["env"]) if e["name"] == "POLICY_ROUTING_MODE")
    kube.run(["-n", NAMESPACE, "patch", "deployment", "vekil", "--type=json", "--patch-file=/dev/stdin"], [
        {"op": "test", "path": "/metadata/resourceVersion", "value": deployment["metadata"]["resourceVersion"]},
        {"op": "add", "path": "/spec/template/metadata/annotations/demo.orka.ai~1config-digest",
         "value": prepare.digest(config)},
        {"op": "replace", "path": f"/spec/template/spec/containers/{number}/env/{position}/value", "value": "off"},
    ])
    kube.wait(NAMESPACE, "vekil")
    after = prepare.preservation(kube, source)
    require(before == after, "Original configuration or credentials changed")
    run.save(ROOT / "bin/efficiency-production/customer-story-setup.json", {
        "context": "sertac-aks", "namespace": NAMESPACE, "preserved": True,
        "before": before, "after": after, "resources": resources, "sourceRevision": REVISION,
    })
    print("Customer support, payments engineering, and semantic-router are ready; hosted baseline selected.")


def init(root):
    root.mkdir(parents=True, exist_ok=False, mode=0o700)
    suffix = time.strftime("%m%d%H%M%S", time.gmtime()) + "-" + os.urandom(2).hex()
    run.save(root / "run.json", {"createdAt": run.now(), "context": "sertac-aks", "namespace": NAMESPACE,
                               "repository": REPOSITORY, "sourceRevision": REVISION, "suffix": suffix})
    run.save(root / "customers.json", CUSTOMERS)
    (root / "customers.txt").write_text(
        "TWENTY SYNTHETIC CUSTOMER REPORTS. ONE CHECKOUT PROBLEM.\n\n"
        + "\n\n".join(f"{row['id']}: {row['report']}" for row in CUSTOMERS[:3])
        + "\n\n17 more reports are in customers.json.\n"
        "Support: write an individual factual reply to each customer.\n"
        "Engineering: fix the payment retry bug once.\n")
    variables = []
    for phase in PHASES:
        (root / ("support-" + phase)).mkdir()
        for job in JOBS:
            name = f"{job}-{phase}-{suffix}"
            support = job in SUPPORT_JOBS
            variables.append(f"{job.upper().replace('-', '_')}_{phase.upper()}={name}")
            spec = {"type": "ai" if support else "agent", "prompt": PROMPTS[job],
                    "agentRef": {"name": "customer-support" if support else "payments-engineer"},
                    "timeout": "10m"}
            if job == "engineering":
                branch = f"demos/checkout-{phase}-{suffix}"
                variables.append(f"BRANCH_{phase.upper()}={branch}")
                spec["workspace"] = {"intent": "write", "gitRepo": REPOSITORY, "ref": REVISION,
                                     "publicationCredentialRef": {"name": "payments-repository-write", "key": "token"},
                                     "pushBranch": branch}
            manifest = {"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
                        "metadata": {"name": name, "namespace": NAMESPACE,
                                     "labels": {"demo.orka.ai/name": "12-efficiency-twenty-customers"}}, "spec": spec}
            (root / f"{job}-{phase}.yaml").write_text(yaml.safe_dump(manifest, sort_keys=False, width=86))
            if support:
                (root / ("support-" + phase) / (name + ".yaml")).write_text(
                    yaml.safe_dump(manifest, sort_keys=False, width=86))
        (root / phase).mkdir()
    for job, prompt in PROMPTS.items():
        (root / (job + "-request.txt")).write_text(prompt + "\n")
    variables.append(f"SOURCE_REVISION={REVISION}")
    variables.append(f"NODE_TEST_IMAGE={NODE_TEST_IMAGE}")
    (root / "names.sh").write_text("\n".join(variables) + "\n")
    version = subprocess.check_output([
        "podman", "run", "--rm", "--pull=never", "--network=none", "--read-only",
        "--cap-drop=all", "--security-opt=no-new-privileges", "--user=65532",
        "--memory=256m", "--pids-limit=64", "--timeout=60", NODE_TEST_IMAGE, "node", "--version",
    ], text=True).strip()
    run.save(root / "test-runtime.json", {"image": NODE_TEST_IMAGE, "node": version})
    for name in agents():
        value = json.loads(run.cli("payments", ["agent", "get", name, "-o", "json"]))
        run.save(root / (name + ".json"), value)
    provider = json.loads(run.cli("payments", ["provider", "get", "semantic-router", "-o", "json"]))
    run.save(root / "provider.json", provider)
    snapshot = run.gateway_snapshot("payments")
    policy, destinations = evidence.terminal_map(snapshot)
    excerpt = {"public_model": policy["public_id"],
               "classifier": destinations["classifier"]["model"],
               "lightweight": destinations["lightweight"]["model"] + " via AIKit on cluster CPU",
               "powerful": destinations["powerful"]["model"] + " hosted"}
    (root / "routing.yaml").write_text("# Configured Vekil semantic router destinations\n" + yaml.safe_dump(excerpt, sort_keys=False))
    run.save(root / "infrastructure-initial.json", infrastructure.snapshot())
    print(str(root))


def phase(root, name, stage):
    directory = root / name
    path = directory / "phase.json"
    value = {"phase": name} if stage == "start" else run.read(path)
    value["startedAt" if stage == "start" else "finishedAt"] = run.now()
    run.save(path, value)
    snapshot = run.gateway_snapshot("payments")
    run.save(directory / ("gateway-" + stage + ".json"), snapshot)
    run.save(directory / ("operations-" + stage + ".json"), run.gateway_logs("payments", snapshot["pod"]))
    run.save(directory / ("infrastructure-" + stage + ".json"), infrastructure.snapshot())
    if stage == "end":
        flags = ["--from", value["startedAt"], "--until", value["finishedAt"], "--as-of", value["finishedAt"], "-o", "json"]
        for category in ("summary", "other-unassociated", "other-other_requests"):
            args = ["usage", *(category.split("-", 1)), *flags]
            filename = "payments-usage" + ("" if category == "summary" else "-" + category.removeprefix("other-"))
            run.save(directory / (filename + ".json"), json.loads(run.cli("payments", args)))


def begin(root, phase_name, job):
    directory = root / phase_name / job
    directory.mkdir(exist_ok=False)
    run.save(directory / "started.json", {"at": run.now()})
    run.save(directory / "gateway-before.json", run.gateway_snapshot("payments"))


def support_checks(answer, job):
    customer = next(row for row in CUSTOMERS if row["id"] == job)
    sentences = [line.strip() for line in re.split(r"(?<=[.!?])\s+", answer.strip()) if line.strip()]
    require(len(sentences) == 2, "Support reply must contain exactly two sentences")
    lower = answer.lower().replace("\u2019", "'")
    reference = customer["orderReference"]
    if reference:
        references = re.findall(r"(?<![A-Za-z0-9_-])ORD-[A-Za-z0-9_-]+", answer, re.IGNORECASE)
        require(set(references) == {reference}, "Support reply lost the supplied order reference or named a different order")
        require(not re.search(r"\b(?:provide|share|send|confirm|tell|what)\b[^.!?]{0,50}order (?:reference|number|id)", lower),
                "Support asked for an order reference it already had")
    else:
        require("order" in lower and any(word in lower for word in ("reference", "number", "id"))
                and re.search(r"\b(?:provide|share|send|confirm|tell|what|may|could|please)\b", lower),
                "Support reply must ask for the missing order reference")
        require(not re.search(r"ORD-\d+", answer), "Support invented an order reference")
        require(not re.search(r"order (?:reference|number|id) (?:that )?you (?:provided|supplied|shared)", lower),
                "Support claims the missing reference was supplied")
    require(re.search(r"twice|double|two [^.!?]{0,25}(?:charges|payments)|duplicate", lower),
            "Support reply does not acknowledge the reported double charge")
    require(not re.search(r"\b(refund\w*|have (?:issued|processed)|already|investigated|"
                          r"(?:will|we'll|i'll) [^.!?]{0,35}investigat)", lower),
            "Support reply claims an action not established by the customer report")
    require(not re.search(r"\b(?:will|shall|[a-z]+'ll|going to)\b", lower),
            "Support reply promises an action not established by the customer report")
    require(not re.search(r"\b(?:we|i|our team|our support)(?:'ve| have| has| had|'re| are|'m| am| is)? "
                          r"(?:review(?:ed|ing)|check(?:ed|ing)|locat(?:ed|ing)|investigat(?:ed|ing)|"
                          r"start(?:ed|ing)|initiat(?:ed|ing)|look(?:ed|ing) into)\b", lower),
            "Support claims an account action was taken")
    require(len(answer.split()) <= 75 and "```" not in answer, "Support reply is not a short customer acknowledgement")
    return ["two sentences", "acknowledges reported double charge",
            "uses supplied order reference" if reference else "asks for missing order reference",
            "no unsupported refund or investigation"]


def task_manifest(root, phase_name, job):
    return yaml.safe_load((root / f"{job}-{phase_name}.yaml").read_text())


def classifier_receipts(root, gateway, *, fetch=False):
    failures = gateway["classifier"].get("unmeteredFailures", [])
    if fetch:
        state = run.read(prepare.STATE)
        receipts = classifier_billing.fetch_receipts(failures, state["providers_source"]["path"],
                                                     root / "classifier-billing")
    else:
        receipts = [run.read(root / "classifier-billing" / (row["generationID"] + ".json")) for row in failures]
    classifier_billing.verify_receipts(failures, receipts)
    gateway["classifier"]["billingReceipts"] = receipts


def collect(root, phase_name, job):
    directory = root / phase_name / job
    wanted = task_manifest(root, phase_name, job)
    name = wanted["metadata"]["name"]
    task = json.loads(run.cli("payments", ["task", "get", name, "-o", "json"]))
    run.save(directory / "task.json", task)
    history = run.events("payments", name, directory)
    after = run.gateway_snapshot("payments")
    run.save(directory / "gateway-after.json", after)
    logs = run.gateway_logs("payments", after["pod"])
    run.save(directory / "operations.json", logs)
    require(task["status"]["phase"] == "Succeeded", "Task did not succeed: " + name)
    require(task["status"]["attempts"] == 1, "Task retried; retain this attempt but do not present it as a clean comparison")
    require(task["spec"]["prompt"] == PROMPTS[job], "The recorded request changed")
    result = json.loads(run.cli("payments", ["task", "result", name, "-o", "json"]))
    run.save(directory / "result.json", result)
    gateway = evidence.gateway_window(run.read(directory / "gateway-before.json"), after, logs,
                                      "off" if phase_name == "baseline" else "enforce",
                                      allow_tool_continuations=job == "engineering",
                                      allow_bounded_context=job == "engineering", allow_classifier_fallback=True)
    classifier_receipts(root, gateway, fetch=True)
    require(gateway["operations"] and all(row["role"] == "worker" for row in gateway["operations"]),
            "Unexpected model traffic in Task interval")
    require(any(event["type"] == "TaskSucceeded" for event in history), "Missing Task completion event")
    expected_agent = wanted["spec"]["agentRef"]["name"]
    agent = json.loads(run.cli("payments", ["agent", "get", expected_agent, "-o", "json"]))
    run.save(directory / "agent.json", agent)
    require(agent["spec"] == run.read(root / (expected_agent + ".json"))["spec"], "Agent instructions changed")
    require(task["spec"]["agentRef"]["name"] == expected_agent, "Task ran a different Agent")
    checks = []
    if job in SUPPORT_JOBS:
        require(not any(event["type"] == "ToolCallStarted" for event in history), "Support used a tool")
        checks = support_checks(result["result"], job)
    else:
        require(task["spec"]["workspace"]["ref"] == REVISION, "Engineering used a different source revision")
        require(task["spec"]["workspace"]["intent"] == "write", "Engineering has no writable repository")
    started = task["metadata"]["creationTimestamp"]
    ended = task["status"]["completionTime"]
    record = {"task": name, "taskUID": task["metadata"]["uid"], "phase": phase_name, "job": job,
              "startedAt": started, "finishedAt": ended,
              "elapsedSeconds": round((evidence.timestamp(ended) - evidence.timestamp(started)).total_seconds(), 2),
              "checks": checks, "gateway": gateway}
    run.save(directory / "evidence.json", record)
    run.save(directory / "infrastructure.json", infrastructure.snapshot())
    route_lines = ["Recorded model calls for " + name, "", "Routing tier  Actual destination      Calls"]
    for tier in ("lightweight", "powerful"):
        rows = [row for row in gateway["operations"] if row["tier"] == tier]
        if rows:
            choice = tier if phase_name == "routed" else "baseline"
            route_lines.append(f"{choice:<13} {rows[0]['destination']['model']:<23} {len(rows)}")
    classifier = gateway["classifier"]
    continuations = sum(row["policyDecision"] == "replay_binding" for row in gateway["operations"])
    route_lines += ["", f"Classifier requests: {classifier['sends']}",
                    f"Classifier elapsed: {classifier['duration_ms'] / 1000:.2f}s total"]
    fallbacks = sum(row["policyDecision"] == "unavailable_fallback" for row in gateway["operations"])
    if fallbacks:
        route_lines.append(f"Classifier unavailable: {fallbacks}; hosted fallback used.")
    skipped = sum(row["classifierFailureCategory"] == "breaker_open" for row in gateway["operations"])
    if skipped:
        route_lines.append(f"Circuit breaker: {skipped} requests used hosted without calling Jev.")
    if continuations:
        route_lines.append(f"Tool continuations kept with the selected model: {continuations}")
    if phase_name == "routed" and any(row["classifierInputTruncated"]
                                    for row in gateway["operations"] if row["policyDecision"] == "classified"):
        route_lines.append("Classifier view: shortened context, as reported by Vekil.")
    (root / f"{job}-{phase_name}-routes.txt").write_text("\n".join(route_lines) + "\n")


def checkout(root, phase_name):
    manifest = task_manifest(root, phase_name, "engineering")
    branch = manifest["spec"]["workspace"]["pushBranch"]
    target = root / phase_name / "repository"
    require(not target.exists(), "Verification checkout already exists")
    subprocess.run([
        "git", "clone", "--depth", "2", "--single-branch", "--branch", branch,
        REPOSITORY, str(target),
    ], capture_output=True, check=True)
    changed = subprocess.check_output(["git", "-C", str(target), "diff", "--name-only", REVISION, "HEAD"], text=True).splitlines()
    require(changed == ["payments/charge.mjs"], "Published branch changed files outside the payment fix")
    parent = subprocess.check_output(["git", "-C", str(target), "rev-parse", "HEAD^"], text=True).strip()
    require(parent == REVISION, "Published change does not start at the agreed source revision")
    for filename in ("payments/payment.test.mjs", "payments/README.md"):
        require((target / filename).read_bytes() == (HERE / "sample" / filename).read_bytes(), "Published tests were modified")
    # Only this source directory is mounted into the unprivileged test container.
    # The enclosing run directory retains its private permissions.
    (target / "payments").chmod(0o755)
    for filename in ("charge.mjs", "payment.test.mjs", "README.md"):
        path = target / "payments" / filename
        require(path.is_file() and not path.is_symlink(), "Published source must be a regular file")
        path.chmod(0o644)
    publication = {
        "branch": branch, "commit": subprocess.check_output(["git", "-C", str(target), "rev-parse", "HEAD"], text=True).strip(),
        "parent": parent, "changedFiles": changed, "path": str(target),
    }
    publication_checks(run.read(root / phase_name / "engineering/task.json"), publication)
    run.save(root / phase_name / "engineering/publication.json", publication)


def publication_checks(task, publication):
    delivery = task["status"].get("delivery", {})
    require(delivery.get("state") == "VerifiedExact"
            and delivery.get("verifiedRemoteSHA") == publication["commit"]
            and delivery.get("expectedCommitSHA") == publication["commit"]
            and delivery.get("startingSHA") == publication["parent"] == REVISION
            and delivery.get("branch") == publication["branch"] == task["spec"]["workspace"]["pushBranch"],
            "Published commit does not match the Task's verified delivery")


def finish_tests(root, phase_name):
    path = root / phase_name / "engineering/tests.txt"
    output = path.read_text()
    require(re.search(r"(?:#|ℹ) tests 6\b", output) and re.search(r"(?:#|ℹ) pass 6\b", output)
            and re.search(r"(?:#|ℹ) fail 0\b", output), "Independent payment regression tests did not all pass")
    run.save(root / phase_name / "engineering/tests.json", {"passed": 6, "failed": 0,
             "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
             "command": "node --test /checks/payment.test.mjs", "execution": "podman",
             **run.read(root / "test-runtime.json")})


def verify(root):
    import costs
    require(run.read(root / "customers.json") == CUSTOMERS, "Customer fixture changed")
    report = {"sourceRevision": REVISION, "customerCount": len(CUSTOMERS),
              "supportConcurrency": 1, "phases": {}, "jobs": {}}
    samples = [json.loads(line) for line in (root / "resources.jsonl").read_text().splitlines() if line.strip()]
    for phase_name in PHASES:
        directory = root / phase_name
        phase_info = run.read(directory / "phase.json")
        window = evidence.gateway_window(run.read(directory / "gateway-start.json"), run.read(directory / "gateway-end.json"),
                                         run.read(directory / "operations-end.json"), "off" if phase_name == "baseline" else "enforce",
                                         allow_tool_continuations=True, allow_bounded_context=True,
                                         allow_classifier_fallback=True)
        classifier_receipts(root, window)
        operations = []
        for job in JOBS:
            record = run.read(directory / job / "evidence.json")
            task = run.read(directory / job / "task.json")
            require(task["status"]["phase"] == "Succeeded"
                    and task["metadata"]["uid"] == record["taskUID"]
                    and task["metadata"]["name"] == record["task"]
                    and task["spec"]["prompt"] == PROMPTS[job],
                    "Saved Task does not match the completed job")
            actual_gateway = evidence.gateway_window(
                run.read(directory / job / "gateway-before.json"),
                run.read(directory / job / "gateway-after.json"),
                run.read(directory / job / "operations.json"),
                "off" if phase_name == "baseline" else "enforce", allow_tool_continuations=job == "engineering",
                allow_bounded_context=job == "engineering", allow_classifier_fallback=True)
            classifier_receipts(root, actual_gateway)
            require(actual_gateway == record["gateway"], "Saved route summary differs from raw gateway records")
            operations.extend(actual_gateway["operations"])
            report["jobs"].setdefault(job, {})[phase_name] = record
            history = run.read(directory / job / "events.json")["events"]
            require(any(event["type"] == "TaskSucceeded" for event in history), "Missing saved completion event")
            if job in SUPPORT_JOBS:
                require(not any(event["type"] == "ToolCallStarted" for event in history), "Support used a tool")
                support_checks(run.read(directory / job / "result.json")["result"], job)
            else:
                tests = run.read(directory / job / "tests.json")
                require(tests["passed"] == 6 and tests["failed"] == 0, "Missing successful regression checks")
                require(hashlib.sha256((directory / job / "tests.txt").read_bytes()).hexdigest() == tests["sha256"],
                        "Saved test output changed")
                publication = run.read(directory / job / "publication.json")
                publication_checks(task, publication)
                require(publication["parent"] == REVISION
                        and task["spec"]["workspace"]["ref"] == REVISION
                        and publication["branch"] == task["spec"]["workspace"]["pushBranch"],
                        "Published fix does not belong to this Task or source revision")
        require({op["operationID"] for op in operations} == {op["operationID"] for op in window["operations"]}
                and len(operations) == len(window["operations"]), "Unaccounted gateway work between Tasks")
        requests = [{"task": report["jobs"][job][phase_name]["task"],
                     "taskUID": report["jobs"][job][phase_name]["taskUID"],
                     "worker": {"usage": evidence.add_tokens(
                         operation["usage"] for operation in report["jobs"][job][phase_name]["gateway"]["operations"])}}
                    for job in JOBS]
        window["orkaUsage"] = evidence.orka_usage(directory, "payments", requests, window, phase_info,
            allow_unreported_tasks=(report["jobs"]["engineering"][phase_name]["taskUID"],))
        resources = evidence.resource_summary(samples, phase_info["startedAt"], phase_info["finishedAt"])
        support_records = [report["jobs"][job][phase_name] for job in SUPPORT_JOBS]
        require(all(evidence.timestamp(a["finishedAt"]) <= evidence.timestamp(b["startedAt"])
                    for a, b in zip(support_records, support_records[1:])), "Support concurrency changed")
        report["phases"][phase_name] = {"gateway": window, "resources": resources,
            "period": phase_info,
            "supportElapsedSeconds": round((evidence.timestamp(support_records[-1]["finishedAt"])
                - evidence.timestamp(support_records[0]["startedAt"])).total_seconds(), 2)}
    for job in JOBS:
        first, second = [report["jobs"][job][phase] for phase in PHASES]
        require(first["taskUID"] != second["taskUID"], "The comparison reused a Task")
        for filename in ("agent.json",):
            a = run.read(root / "baseline" / job / filename)
            b = run.read(root / "routed" / job / filename)
            require(a["spec"] == b["spec"] and a["metadata"]["uid"] == b["metadata"]["uid"], "The Agent changed across runs")
        a = run.read(root / "baseline" / job / "gateway-before.json")
        b = run.read(root / "routed" / job / "gateway-before.json")
        require(a["configuration"]["digest"] == b["configuration"]["digest"]
                and a["container"]["imageID"] == b["container"]["imageID"], "Routing comparison changed more than the mode")
    routed = report["phases"]["routed"]["gateway"]["operations"]
    require({op["tier"] for op in routed} == {"lightweight", "powerful"}, "Both model destinations were not demonstrated")
    allocation = infrastructure.calculate(root, report)
    run.save(root / "infrastructure.json", allocation)
    report["costs"] = costs.calculate_costs(report, allocation)
    run.save(root / "verified-report.json", report)
    lines = ["Same 20 reports, instructions, starting code and tests", "Support concurrency: one Task at a time in both runs", "",
             "Outcome                   Hosted baseline   Routing enabled",
             "Customer replies checked  20 / 20           20 / 20",
             "Payment tests passed      6 / 6             6 / 6", "",
             "Elapsed time              Hosted baseline   Routing enabled   Change"]
    for title, values in (
            ("Support batch", [report["phases"][p]["supportElapsedSeconds"] for p in PHASES]),
            ("Engineering Task", [report["jobs"]["engineering"][p]["elapsedSeconds"] for p in PHASES])):
        a, b = values
        require(a > 0, "Elapsed baseline is zero")
        lines.append(f"{title:<26}{a:>7.1f}s          {b:>7.1f}s          {(b-a)/a*100:+.1f}%")
    lines += ["", "Run        Local tokens  Hosted tokens  Jev tokens      Jev requests"]
    for phase_name, phase_report in report["phases"].items():
        gateway = phase_report["gateway"]
        classifier = gateway["classifier"]
        reported = str(classifier["usage"]["total_tokens"]) + ("*" if not classifier["usageComplete"] else "")
        local = gateway["roles"]["cpuWorker"]["usage"]["total_tokens"]
        hosted = gateway["roles"]["hostedWorker"]["usage"]["total_tokens"]
        lines.append(f"{phase_name:<11}{local:<14,}{hosted:<15,}{reported:<16}{classifier['sends']}")
    lines += ["", "Gateway-reported input + output tokens, including cached input.",
              "Counted once. Overlapping Orka counts are not added."]
    failures = report["phases"]["routed"]["gateway"]["classifier"]["unmeteredFailures"]
    if failures:
        lines += [f"* {len(failures)} failed Jev calls lack token counts; exact billing receipts show zero charge.",
                  "Those requests used hosted fallback. Their hosted usage is included."]
    skipped = sum(row["classifierFailureCategory"] == "breaker_open" for row in routed)
    if skipped:
        lines.append(f"Circuit breaker: {skipped} more requests used hosted without calling Jev.")
    (root / "comparison.txt").write_text("\n".join(lines) + "\n")
    lines = ["Orka token reporting   Hosted baseline   Routing enabled"]
    for job in ("support-01", "engineering"):
        labels = []
        for phase_name in PHASES:
            missing = report["phases"][phase_name]["gateway"]["orkaUsage"]["unavailableTasks"]
            uid = report["jobs"][job][phase_name]["taskUID"]
            labels.append("unavailable" if any(row["taskUID"] == uid for row in missing) else "reported")
        title = "Support Tasks" if job in SUPPORT_JOBS else "Engineering Task"
        lines.append(f"{title:<23}{labels[0]:<18}{labels[1]}")
    lines += ["", "The comparison uses Vekil's recorded counts for both jobs.",
              "Unavailable Orka measurements have no numeric value."]
    (root / "usage-notes.txt").write_text("\n".join(lines) + "\n")
    lines = ["Local model resources, including idle time in each sampled interval", ""]
    for phase_name, phase_report in report["phases"].items():
        sample = phase_report["resources"]
        lines.append(f"{phase_name:<10} {sample['cpuSeconds']:.1f} CPU seconds / {sample['sampledSeconds']:.1f}s observed; "
                     f"{sample['peakWorkingSetMiB']:.0f} MiB peak working memory")
        for row in phase_report["gateway"]["preflightBeforeInterval"]:
            usage = str(row["classifier_usage"]["total_tokens"]) + " tokens" if row["usageComplete"] else "tokens unavailable"
            lines.append(f"Classifier startup check outside the jobs: {row['physical_classifier_sends']} request; {usage}.")
    lines += ["", "Resource measurements cover this example, including idle time during each interval."]
    (root / "resources.txt").write_text("\n".join(lines) + "\n")
    # The terminal displays this derived report with ordinary cat, not a demo-only CLI.
    (root / "cost-summary.txt").write_text(costs.format_summary(report["costs"]))
    replies = []
    for job in SUPPORT_JOBS:
        replies.append({"customer": next(c for c in CUSTOMERS if c["id"] == job),
                        **{p: run.read(root / p / job / "result.json")["result"] for p in PHASES}})
    run.save(root / "all-replies.json", replies)
    for phase_name in PHASES:
        records = [report["jobs"][j][phase_name] for j in SUPPORT_JOBS]
        tiers = {tier: sum(any(op["tier"] == tier for op in row["gateway"]["operations"]) for row in records)
                 for tier in ("lightweight", "powerful")}
        fallbacks = sum(op["policyDecision"] == "unavailable_fallback" for row in records
                        for op in row["gateway"]["operations"])
        (root / f"support-{phase_name}-routes.txt").write_text(
            f"Actual destinations for {len(records)} support Tasks\n\n"
            f"Local Qwen3.5 2B: {tiers['lightweight']}\nHosted GPT-5.5: {tiers['powerful']}\n"
            + (f"Hosted fallback when Jev was unavailable: {fallbacks}\n" if fallbacks else ""))
    print("Verified all 20 replies, repository changes, six tests per run, routing and cost estimates.")


def batch_summary(root, phase_name):
    """Print only verified batch records, before the full comparison is available."""
    records = [run.read(root / phase_name / job / "evidence.json") for job in SUPPORT_JOBS]
    lines = [f"Completed and checked: {len(records)} / {len(CUSTOMERS)} customer replies", ""]
    for tier in ("lightweight", "powerful"):
        count = sum(any(op["tier"] == tier for op in row["gateway"]["operations"]) for row in records)
        model = "local Qwen3.5 2B" if tier == "lightweight" else "hosted GPT-5.5"
        lines.append(f"{model}: {count}")
    fallbacks = sum(op["policyDecision"] == "unavailable_fallback" for row in records
                    for op in row["gateway"]["operations"])
    if fallbacks:
        lines.append(f"Hosted fallback when Jev was unavailable: {fallbacks}")
    (root / f"support-{phase_name}-routes.txt").write_text("\n".join(lines) + "\n")


def retain_failure(root):
    """Keep raw accounting if a CLI or acceptance check aborts the run."""
    if not root.exists():
        return
    snapshot = run.gateway_snapshot("payments")
    run.save(root / "failure-gateway.json", snapshot)
    run.save(root / "failure-operations.json", run.gateway_logs("payments", snapshot["pod"]))
    run.save(root / "failure-infrastructure.json", infrastructure.snapshot())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-dir", type=Path)
    sub = parser.add_subparsers(dest="action", required=True)
    sub.add_parser("setup")
    sub.add_parser("init")
    for action in ("begin", "collect"):
        command = sub.add_parser(action)
        command.add_argument("phase", choices=PHASES)
        command.add_argument("job", choices=JOBS)
    command = sub.add_parser("phase")
    command.add_argument("phase", choices=PHASES)
    command.add_argument("stage", choices=("start", "end"))
    for action in ("checkout", "finish-tests", "batch-summary"):
        command = sub.add_parser(action)
        command.add_argument("phase", choices=PHASES)
    sub.add_parser("verify")
    sub.add_parser("retain-failure")
    args = parser.parse_args()
    if args.action == "setup":
        return setup()
    require(args.run_dir is not None, "--run-dir is required")
    root = args.run_dir.resolve()
    if args.action in ("init", "verify", "retain-failure"):
        return globals()[args.action.replace("-", "_")](root)
    if args.action in ("begin", "collect"):
        return globals()[args.action](root, args.phase, args.job)
    if args.action == "phase":
        return phase(root, args.phase, args.stage)
    if args.action in ("checkout", "finish-tests", "batch-summary"):
        return globals()[args.action.replace("-", "_")](root, args.phase)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, KeyError, prepare.SetupError, subprocess.CalledProcessError) as error:
        # Child command arguments may contain short-lived authentication.
        print("Story stopped: " + (type(error).__name__ if isinstance(error, subprocess.CalledProcessError) else str(error)), file=sys.stderr)
        sys.exit(1)
