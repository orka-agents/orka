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

import evidence
import prepare
import run

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
NAMESPACE = "team-payments"
REPOSITORY = "https://github.com/sozercan/orka-demo-inventory"
REVISION = "1e2863c3ad28476e467b29141611be4c56ff3c5a"
NODE_TEST_IMAGE = "docker.io/library/node@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1"
PHASES = ("baseline", "routed")
JOBS = ("support", "engineering")
PROMPTS = {
    "support": (
        'The customer wrote: "Checkout froze, so I clicked Pay again. '
        'Now I see two charges."\n'
        "Write a two-sentence acknowledgement and ask for the order reference."
    ),
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
}


def require(condition, message):
    evidence.require(condition, message)


def agents():
    support = {
        "providerRef": {"name": "semantic-router"},
        "model": {"name": "team-assistant", "maxTokens": 256},
        "systemPrompt": {"inline": (
            "Write a customer support reply in exactly two sentences. "
            "Acknowledge the customer's reported double charge and ask for "
            "their order reference. Do not claim that a refund or investigation "
            "has happened. Use only the supplied facts. Return the reply alone. "
            "Do not call tools."
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
    (root / "customer.txt").write_text(
        'CUSTOMER: "Checkout froze, so I clicked Pay again. Now I see two charges."\n\n'
        "Support: acknowledge the report and ask for the order reference.\n"
        "Payments engineering: stop overlapping retries from charging twice.\n")
    variables = []
    for phase in PHASES:
        for job in JOBS:
            name = f"{job}-{phase}-{suffix}"
            variables.append(f"{job.upper()}_{phase.upper()}={name}")
            spec = {"type": "ai" if job == "support" else "agent", "prompt": PROMPTS[job],
                    "agentRef": {"name": "customer-support" if job == "support" else "payments-engineer"},
                    "timeout": "10m"}
            if job == "engineering":
                branch = f"demos/checkout-{phase}-{suffix}"
                variables.append(f"BRANCH_{phase.upper()}={branch}")
                spec["workspace"] = {"intent": "write", "gitRepo": REPOSITORY, "ref": REVISION,
                                     "publicationCredentialRef": {"name": "payments-repository-write", "key": "token"},
                                     "pushBranch": branch}
            manifest = {"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
                        "metadata": {"name": name, "namespace": NAMESPACE,
                                     "labels": {"demo.orka.ai/name": "12-efficiency-customer-story"}}, "spec": spec}
            (root / f"{job}-{phase}.yaml").write_text(yaml.safe_dump(manifest, sort_keys=False, width=86))
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


def support_checks(answer):
    sentences = [line.strip() for line in re.split(r"(?<=[.!?])\s+", answer.strip()) if line.strip()]
    require(len(sentences) == 2, "Support reply must contain exactly two sentences")
    lower = answer.lower()
    require("order" in lower and any(word in lower for word in ("reference", "number", "id")),
            "Support reply must ask for the order reference")
    require(any(word in lower for word in ("twice", "double", "two charges", "duplicate")),
            "Support reply does not acknowledge the reported double charge")
    require(not re.search(r"\b(refunded|refund (?:has|is|was)|have (?:issued|processed)|already|investigated)\b", lower),
            "Support reply claims an action not established by the customer report")
    require(len(answer.split()) <= 75 and "```" not in answer, "Support reply is not a short customer acknowledgement")
    return ["two sentences", "acknowledges reported double charge", "asks for order reference", "no unsupported completed action"]


def task_manifest(root, phase_name, job):
    return yaml.safe_load((root / f"{job}-{phase_name}.yaml").read_text())


def collect(root, phase_name, job):
    directory = root / phase_name / job
    wanted = task_manifest(root, phase_name, job)
    name = wanted["metadata"]["name"]
    task = json.loads(run.cli("payments", ["task", "get", name, "-o", "json"]))
    run.save(directory / "task.json", task)
    require(task["status"]["phase"] == "Succeeded", "Task did not succeed: " + name)
    require(task["spec"]["prompt"] == PROMPTS[job], "The recorded request changed")
    result = json.loads(run.cli("payments", ["task", "result", name, "-o", "json"]))
    run.save(directory / "result.json", result)
    history = run.events("payments", name, directory)
    after = run.gateway_snapshot("payments")
    run.save(directory / "gateway-after.json", after)
    logs = run.gateway_logs("payments", after["pod"])
    run.save(directory / "operations.json", logs)
    gateway = evidence.gateway_window(run.read(directory / "gateway-before.json"), after, logs,
                                      "off" if phase_name == "baseline" else "enforce",
                                      allow_tool_continuations=job == "engineering",
                                      allow_bounded_context=job == "engineering")
    require(gateway["operations"] and all(row["role"] == "worker" for row in gateway["operations"]),
            "Unexpected model traffic in Task interval")
    require(any(event["type"] == "TaskSucceeded" for event in history), "Missing Task completion event")
    expected_agent = wanted["spec"]["agentRef"]["name"]
    agent = json.loads(run.cli("payments", ["agent", "get", expected_agent, "-o", "json"]))
    run.save(directory / "agent.json", agent)
    require(agent["spec"] == run.read(root / (expected_agent + ".json"))["spec"], "Agent instructions changed")
    require(task["spec"]["agentRef"]["name"] == expected_agent, "Task ran a different Agent")
    checks = []
    if job == "support":
        require(not any(event["type"] == "ToolCallStarted" for event in history), "Support used a tool")
        checks = support_checks(result["result"])
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
    run.save(root / phase_name / "engineering/publication.json", {
        "branch": branch, "commit": subprocess.check_output(["git", "-C", str(target), "rev-parse", "HEAD"], text=True).strip(),
        "parent": parent, "changedFiles": changed, "path": str(target),
    })


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
    report = {"sourceRevision": REVISION, "phases": {}, "jobs": {}}
    samples = [json.loads(line) for line in (root / "resources.jsonl").read_text().splitlines() if line.strip()]
    for phase_name in PHASES:
        directory = root / phase_name
        phase_info = run.read(directory / "phase.json")
        window = evidence.gateway_window(run.read(directory / "gateway-start.json"), run.read(directory / "gateway-end.json"),
                                         run.read(directory / "operations-end.json"), "off" if phase_name == "baseline" else "enforce",
                                         allow_tool_continuations=True, allow_bounded_context=True)
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
                allow_bounded_context=job == "engineering")
            require(actual_gateway == record["gateway"], "Saved route summary differs from raw gateway records")
            operations.extend(actual_gateway["operations"])
            report["jobs"].setdefault(job, {})[phase_name] = record
            history = run.read(directory / job / "events.json")["events"]
            require(any(event["type"] == "TaskSucceeded" for event in history), "Missing saved completion event")
            if job == "support":
                require(not any(event["type"] == "ToolCallStarted" for event in history), "Support used a tool")
                support_checks(run.read(directory / job / "result.json")["result"])
            else:
                tests = run.read(directory / job / "tests.json")
                require(tests["passed"] == 6 and tests["failed"] == 0, "Missing successful regression checks")
                require(hashlib.sha256((directory / job / "tests.txt").read_bytes()).hexdigest() == tests["sha256"],
                        "Saved test output changed")
                publication = run.read(directory / job / "publication.json")
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
        report["phases"][phase_name] = {"gateway": window, "resources": resources}
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
    run.save(root / "verified-report.json", report)
    lines = ["Same jobs, same instructions, same starting code", "", "Job                 Hosted baseline     Routing enabled"]
    for job in JOBS:
        a, b = [report["jobs"][job][phase] for phase in PHASES]
        outcome = "2-sentence reply" if job == "support" else "6 / 6 tests pass"
        lines.append(f"{job:<20}{outcome:<20}{outcome}")
        lines.append(f"  Task elapsed      {a['elapsedSeconds']:>7.1f}s            {b['elapsedSeconds']:>7.1f}s")
    lines += ["", "Run        Local tokens  Hosted tokens  Jev tokens      Jev requests"]
    for phase_name, phase_report in report["phases"].items():
        gateway = phase_report["gateway"]
        classifier = gateway["classifier"]
        reported = str(classifier["usage"]["total_tokens"]) if classifier["usageComplete"] else "unavailable"
        local = gateway["roles"]["cpuWorker"]["usage"]["total_tokens"]
        hosted = gateway["roles"]["hostedWorker"]["usage"]["total_tokens"]
        lines.append(f"{phase_name:<11}{local:<14,}{hosted:<15,}{reported:<16}{classifier['sends']}")
    lines += ["", "Gateway-reported input + output tokens, including cached input.",
              "Counted once. Overlapping Orka counts are not added.",
              "No price comparison is available for these runs."]
    (root / "comparison.txt").write_text("\n".join(lines) + "\n")
    lines = ["Orka token reporting   Hosted baseline   Routing enabled"]
    for job in JOBS:
        labels = []
        for phase_name in PHASES:
            missing = report["phases"][phase_name]["gateway"]["orkaUsage"]["unavailableTasks"]
            uid = report["jobs"][job][phase_name]["taskUID"]
            labels.append("unavailable" if any(row["taskUID"] == uid for row in missing) else "reported")
        lines.append(f"{job:<23}{labels[0]:<18}{labels[1]}")
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
    lines += ["", "These fixed examples do not establish general model quality or dollar savings."]
    (root / "resources.txt").write_text("\n".join(lines) + "\n")
    print("Verified customer reply, repository changes, six tests per run, and recorded routing.")


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
    for action in ("checkout", "finish-tests"):
        command = sub.add_parser(action)
        command.add_argument("phase", choices=PHASES)
    sub.add_parser("verify")
    args = parser.parse_args()
    if args.action == "setup":
        return setup()
    require(args.run_dir is not None, "--run-dir is required")
    root = args.run_dir.resolve()
    if args.action in ("init", "verify"):
        return globals()[args.action](root)
    if args.action in ("begin", "collect"):
        return globals()[args.action](root, args.phase, args.job)
    if args.action == "phase":
        return phase(root, args.phase, args.stage)
    if args.action in ("checkout", "finish-tests"):
        return globals()[args.action.replace("-", "_")](root, args.phase)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, KeyError, prepare.SetupError, subprocess.CalledProcessError) as error:
        # Child command arguments may contain short-lived authentication.
        print("Story stopped: " + (type(error).__name__ if isinstance(error, subprocess.CalledProcessError) else str(error)), file=sys.stderr)
        sys.exit(1)
