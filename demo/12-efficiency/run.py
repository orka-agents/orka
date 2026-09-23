#!/usr/bin/env python3
"""Run serial, authenticated requests and retain the evidence for demo 12."""

import argparse
import datetime as dt
import hashlib
from http.client import HTTPException, RemoteDisconnected
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import textwrap
import time
import urllib.error
import urllib.parse
import urllib.request

import yaml

from check import check_data, check_stock_prose, load_function, require
from fixtures import WORKLOADS, agent_spec, client_request

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
CONTEXT = "sertac-aks"
LABEL = "12-efficiency"
ROUTER = "http://127.0.0.1:18090"
ORKA = {"payments": "http://127.0.0.1:18101", "inventory": "http://127.0.0.1:18102"}
VEKIL = {"payments": "http://127.0.0.1:18111", "inventory": "http://127.0.0.1:18112"}
PYTHON_IMAGE = "docker.io/library/python:3.14.7-alpine3.24@sha256:c6ead215bfd31f1e433d968853b7a769989117115b728874824e6c0a27cb96fc"
TOKENS = {}


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def save(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")


def read(path):
    return json.loads(Path(path).read_text())


def kubectl(*args, payload=None, text=True):
    proc = subprocess.run(["kubectl", "--context", CONTEXT, *args],
                          input=json.dumps(payload) if payload is not None else None,
                          capture_output=True, text=True, check=False)
    if proc.returncode:
        raise RuntimeError("kubectl failed: " + proc.stderr.strip())
    return proc.stdout if text else json.loads(proc.stdout)


def token(team):
    if team not in TOKENS:
        TOKENS[team] = kubectl("-n", "team-" + team, "create", "token", "demo-client", "--duration=1h").strip()
    return TOKENS[team]


def cli(team, args):
    cmd = [str(REPO / "bin/orka"), "--server", ORKA[team], "--namespace", "team-" + team,
           "--token", token(team), *args]
    proc = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if proc.returncode:
        # Never format cmd: it contains a short-lived bearer credential.
        raise RuntimeError("Orka command failed: " + proc.stderr.strip())
    return proc.stdout


def http(url, payload=None, team=None, timeout=600):
    headers = {"Content-Type": "application/json"}
    if team:
        headers["Authorization"] = "Bearer " + token(team)
    req = urllib.request.Request(url, data=json.dumps(payload).encode() if payload is not None else None,
                                 headers=headers)
    # A read can wait for kubectl to reconnect after a gateway rollout. A POST
    # is sent once, so a lost acknowledgement can never create duplicate work.
    for attempt in range(10 if payload is None else 1):
        try:
            with urllib.request.urlopen(req, timeout=timeout) as response:
                return json.load(response)
        except (RemoteDisconnected, urllib.error.URLError) as error:
            if payload is not None or isinstance(error, urllib.error.HTTPError) or attempt == 9:
                raise
            time.sleep(2)


def api(team, path, payload=None, query=None):
    q = {"namespace": "team-" + team, **(query or {})}
    return http(ORKA[team] + path + "?" + urllib.parse.urlencode(q), payload, team)


def task_list(team):
    return kubectl("-n", "team-" + team, "get", "tasks", "-o", "json", text=False)["items"]


def gateway_snapshot(team):
    deployment = kubectl("-n", "team-" + team, "get", "deployment", "vekil", "-o", "json", text=False)
    selector = ",".join(k + "=" + v for k, v in deployment["spec"]["selector"]["matchLabels"].items())
    pods = kubectl("-n", "team-" + team, "get", "pods", "-l", selector, "-o", "json", text=False)["items"]
    live = [p for p in pods if not p["metadata"].get("deletionTimestamp") and p["status"]["phase"] == "Running"]
    require(len(live) == 1, "gateway does not have exactly one running Pod")
    pod = live[0]
    container = next(c for c in pod["status"]["containerStatuses"] if c["name"] == "vekil")
    require(container.get("ready") and "running" in container["state"], "gateway container is not ready")
    cm = kubectl("-n", "team-" + team, "get", "configmap", "vekil-providers", "-o", "json", text=False)
    raw = cm["data"]["providers.yaml"]
    config = yaml.safe_load(raw)
    require(all(not p.get("api_key") for p in config["providers"]), "gateway configuration contains an inline credential")
    config_digest = hashlib.sha256(json.dumps(config, sort_keys=True).encode()).hexdigest()
    require(pod["metadata"]["annotations"]["demo.orka.ai/config-digest"] == config_digest,
            "gateway configuration changed without replacing its Pod")
    installed = {"schema_version": config["schema_version"],
                 "model_routes": config["model_routes"], "policy_profiles": config["policy_profiles"],
                 "providers": [{k: p[k] for k in ("id", "type", "trust_domain") if k in p}
                               for p in config["providers"]]}
    local = next(p for p in config["providers"] if p["id"] == "local-aikit")
    local_url = urllib.parse.urlsplit(local["base_url"])
    require(local_url.scheme == "http" and local_url.hostname == "qwen35-2b.orka-efficiency.svc.cluster.local"
            and local_url.port == 8080 and local_url.path == "/v1", "unexpected local model destination")
    next(p for p in installed["providers"] if p["id"] == "local-aikit")["service"] = {
        "namespace": "orka-efficiency", "name": "qwen35-2b", "port": 8080}
    mode = next(e["value"] for c in pod["spec"]["containers"] if c["name"] == "vekil"
                for e in c["env"] if e["name"] == "POLICY_ROUTING_MODE")
    # A private classifier address and all credentials stay out of saved views.
    return {"at": now(), "pod": pod["metadata"]["name"], "podUID": pod["metadata"]["uid"],
            "container": {"restartCount": container["restartCount"], "containerID": container["containerID"],
                          "startedAt": container["state"]["running"]["startedAt"], "imageID": container["imageID"]},
            "configuration": {"uid": cm["metadata"]["uid"], "resourceVersion": cm["metadata"]["resourceVersion"],
                              "sha256": hashlib.sha256(raw.encode()).hexdigest(), "digest": config_digest,
                              "mode": mode, "topology": installed},
            "stats": http(VEKIL[team] + "/stats.json", timeout=30)}


def gateway_logs(team, pod):
    output = kubectl("-n", "team-" + team, "logs", pod)
    records = []
    for line in output.splitlines():
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        fields = value.get("fields", value)
        if fields.get("operation_id") or value.get("msg", fields.get("msg")) == "policy classifier request completed":
            # Operation records are prompt-free. Authentication startup logs
            # are not needed to establish route identity and are not retained.
            records.append(value)
    return records


def events(team, name, directory):
    collected, after, page = [], 0, 0
    while True:
        page += 1
        value = json.loads(cli(team, ["task", "events", name, "--after", str(after), "--limit", "100", "-o", "json"]))
        save(directory / f"events-{page:03}.json", value)
        require(value["streamID"] == name and value["namespace"] == "team-" + team, "wrong Task event stream")
        collected.extend(value["events"])
        latest = value["latestSeq"]
        after = max([e["seq"] for e in collected], default=0)
        if after >= latest and latest:
            break
        require(value["events"], "event history is incomplete")
    require([e["seq"] for e in collected] == list(range(1, latest + 1)), "event sequence has gaps")
    value["afterSeq"], value["events"] = 0, collected
    save(directory / "events.json", value)
    return collected


def wait_task(team, name, timeout=600):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        task = kubectl("-n", "team-" + team, "get", "task", name, "-o", "json", text=False)
        phase = task.get("status", {}).get("phase")
        if phase in {"Succeeded", "Failed", "Cancelled"}:
            require(phase == "Succeeded", f"Task {name} ended in {phase}")
            return task
        time.sleep(2)
    raise RuntimeError("Task did not finish within the demo deadline: " + name)


def parse_answer(text):
    text = text.strip()
    if text.startswith("```json\n") and text.endswith("```"):
        text = text[8:-3].strip()
    elif text.startswith("```\n") and text.endswith("```"):
        text = text[4:-3].strip()
    return json.loads(text)


def apply_agents(structured):
    for team in ORKA:
        existing = kubectl("-n", "team-" + team, "get", "agent", "efficiency-" + team,
                           "--ignore-not-found", "-o", "json")
        if existing.strip():
            require(json.loads(existing)["metadata"].get("labels", {}).get("demo.orka.ai/name") == LABEL,
                    "refusing to change an Agent that does not belong to this demo")
        value = {"apiVersion": "core.orka.ai/v1alpha1", "kind": "Agent",
                 "metadata": {"name": "efficiency-" + team, "namespace": "team-" + team,
                              "labels": {"demo.orka.ai/name": LABEL}},
                 "spec": agent_spec(team, structured)}
        kubectl("apply", "-f", "-", payload=value)
    print("Inventory agent answer format: " + ("stock, shortage, and customer next action" if structured else "one short sentence"))


def validation_task(team, workload, answer, name, directory):
    fn = "charge_once" if workload == "payments-fix" else "reserve"
    load_function(answer.get("code"), fn)
    script = (HERE / "check.py").read_text()
    task = {"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
            "metadata": {"name": name, "namespace": "team-" + team,
                         "labels": {"demo.orka.ai/name": LABEL}},
            "spec": {"type": "container", "image": PYTHON_IMAGE,
                     "command": ["python", "-c", script],
                     "args": [workload, json.dumps(answer)], "timeout": "90s",
                     "retryPolicy": {"maxRetries": 0},
                     "resources": {"requests": {"cpu": "25m", "memory": "64Mi"},
                                   "limits": {"cpu": "250m", "memory": "128Mi"}}}}
    save(directory / "validation-request.json", task)
    save(directory / "validation-created.json", api(team, "/api/v1/tasks", task))
    finished = wait_task(team, name, 180)
    save(directory / "validation-task.json", finished)
    result = json.loads(cli(team, ["task", "result", name, "-o", "json"]))
    save(directory / "validation-result.json", result)
    checked = json.loads(result["result"].strip())
    require(checked.get("passed") is True and checked["workload"] == workload, "code did not pass the container checks")
    return checked


def ask(root, phase, workload):
    item = WORKLOADS[workload]
    team = item["team"]
    directory = root / phase / workload
    directory.mkdir(parents=True, exist_ok=False)
    before_tasks = task_list(team)
    before_uids = {t["metadata"]["uid"] for t in before_tasks}
    save(directory / "tasks-before.json", before_tasks)
    before = gateway_snapshot(team)
    save(directory / "gateway-before.json", before)
    agent = kubectl("-n", "team-" + team, "get", "agent", "efficiency-" + team, "-o", "json", text=False)
    save(directory / "agent.json", agent)
    request = client_request(workload)
    save(directory / "client-request.json", request)
    started = now()
    start = time.monotonic()
    print(f"{team.capitalize()}: {item['title']} ...", flush=True)
    try:
        reply = http(ROUTER + "/openai/v1/chat/completions", request, team, timeout=900)
        save(directory / "client-response.json", reply)
    except urllib.error.HTTPError as exc:
        # Retain the response body without request headers or bearer material.
        (directory / "client-error.txt").write_bytes(exc.read())
        save(directory / "failure.json", {"at": now(), "httpStatus": exc.code})
        raise RuntimeError(f"shared address returned HTTP {exc.code}; inspect saved evidence") from None
    elapsed = time.monotonic() - start
    all_tasks = task_list(team)
    save(directory / "tasks-after.json", all_tasks)
    created = [t for t in all_tasks if t["metadata"]["uid"] not in before_uids]
    require(len(created) == 1, "the application request did not produce exactly one Task")
    task = created[0]
    require(task["spec"]["type"] == "ai", "the coordinator created a different Task type")
    require(task["spec"].get("agentRef", {}).get("name") == "efficiency-" + team, "the configured team Agent was not used")
    require(task["spec"]["prompt"].strip() == item["prompt"].strip(), "the coordinator changed the business request")
    require(not task["spec"].get("ai", {}).get("providerRef"), "coordinator overrode the Agent's provider")
    name = task["metadata"]["name"]
    task = wait_task(team, name)
    save(directory / "task.json", task)
    result = json.loads(cli(team, ["task", "result", name, "-o", "json"]))
    save(directory / "result.json", result)
    history = events(team, name, directory)
    require(not any(e["type"] == "ToolCallStarted" for e in history), "worker used an unexpected tool")
    require(any(e["type"] == "TaskSucceeded" for e in history), "Task terminal event missing")
    after = gateway_snapshot(team)
    save(directory / "gateway-after.json", after)
    require(before["podUID"] == after["podUID"], "gateway restarted during the request")
    save(directory / "gateway-operations.json", gateway_logs(team, after["pod"]))
    content = reply["choices"][0]["message"]["content"]
    require(reply["choices"][0]["finish_reason"] == "stop", "client answer was incomplete")
    if phase == "orchestration-before":
        require(workload == "inventory-stock", "only the inventory format changes")
        checks = check_stock_prose(result["result"])
        require(content.strip() == result["result"].strip(), "client answer differs from Task result")
        checked = {"passed": True, "checks": checks, "checkCount": len(checks)}
        answer = result["result"]
    else:
        answer = parse_answer(result["result"])
        require(parse_answer(content) == answer, "client answer differs from Task result")
        if item["kind"] == "data":
            checks = check_data(workload, answer)
            checked = {"passed": True, "checks": checks, "checkCount": len(checks)}
        else:
            suffix = hashlib.sha256(str(directory).encode()).hexdigest()[:12]
            checked = validation_task(team, workload, answer, "eff-check-" + suffix, directory)
    record = {"phase": phase, "workload": workload, "team": team, "title": item["title"],
              "task": name, "taskUID": task["metadata"]["uid"], "startedAt": started,
              "finishedAt": now(), "clientSeconds": round(elapsed, 3),
              "totalSeconds": round(time.monotonic() - start, 3),
              "requestSHA256": hashlib.sha256(json.dumps(request, sort_keys=True).encode()).hexdigest(),
              "answer": answer, "checks": checked, "workerToolCalls": 0}
    save(directory / "evidence.json", record)
    # This joins actual gateway operations to the Task's model events. A
    # successful answer alone does not establish which model handled it.
    from evidence import verify_request
    verified = verify_request(root, phase, workload)
    save(directory / "verified.json", verified)
    print(f"Task: {name}")
    if item["kind"] == "data":
        print(json.dumps(answer, indent=2) if isinstance(answer, dict) else answer)
    else:
        print("Explanation excerpt: " + textwrap.shorten(answer["explanation"], width=230, placeholder="..."))
        print(f"Validation: {checked['checkCount']} checks passed in an Orka container Task")
    worker = verified["worker"]["destination"]
    print(f"Worker model: {worker['model']} | hosted coordination: {verified['coordinator']['requests']} calls")
    print(f"Actual elapsed time: {elapsed:.1f}s to the answer; {record['totalSeconds']:.1f}s including verification")


def snapshot_phase(root, phase, stage):
    directory = root / phase
    directory.mkdir(parents=True, exist_ok=True)
    if stage == "start":
        require(not (directory / "phase.json").exists(), "phase already started")
        save(directory / "phase.json", {"startedAt": now()})
    phase_info = read(directory / "phase.json")
    if stage == "end":
        phase_info["finishedAt"] = now()
        save(directory / "phase.json", phase_info)
    for team in ORKA:
        gateway = gateway_snapshot(team)
        save(directory / f"{team}-gateway-{stage}.json", gateway)
        save(directory / f"{team}-operations-{stage}.json", gateway_logs(team, gateway["pod"]))
        if stage == "end":
            args = ["usage", "summary", "--from", phase_info["startedAt"],
                    "--until", phase_info["finishedAt"], "-o", "json"]
            save(directory / f"{team}-usage.json", json.loads(cli(team, args)))
            for category in ("unassociated", "other_requests"):
                other_args = ["usage", "other", category, "--from", phase_info["startedAt"],
                              "--until", phase_info["finishedAt"], "--as-of", phase_info["finishedAt"],
                              "--limit", "100", "-o", "json"]
                save(directory / f"{team}-usage-{category}.json", json.loads(cli(team, other_args)))
    print(f"{phase.capitalize()} {stage}: recorded both teams' gateway and usage evidence")


def init(root):
    root.mkdir(parents=True, exist_ok=False)
    os.chmod(root, 0o700)
    save(root / "run.json", {"createdAt": now(), "context": CONTEXT, "router": ROUTER,
                            "namespaces": ["team-payments", "team-inventory"], "demo": LABEL})
    for key, item in WORKLOADS.items():
        (root / (key + ".txt")).write_text(item["prompt"] + "\n")
        save(root / (key + ".request.json"), client_request(key))
    print(root)


def connect():
    forwards = [("orka-efficiency", "orka-compat-router", 18090, 8080),
                ("team-payments", "orka-api", 18101, 8080),
                ("team-inventory", "orka-api", 18102, 8080),
                ("team-payments", "vekil", 18111, 1337),
                ("team-inventory", "vekil", 18112, 1337),
                ("orka-efficiency", "qwen35-2b", 18120, 8080)]
    for _, _, port, _ in forwards:
        with socket.socket() as sock:
            require(sock.connect_ex(("127.0.0.1", port)) != 0, f"dedicated demo port {port} is already in use")
    running = {}
    stopped = False

    def stop(*_):
        nonlocal stopped
        stopped = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        while not stopped:
            for namespace, service, port, target in forwards:
                proc = running.get(port)
                if proc is None or proc.poll() is not None:
                    running[port] = subprocess.Popen(
                        ["kubectl", "--context", CONTEXT, "-n", namespace, "port-forward",
                         "service/" + service, f"{port}:{target}", "--address=127.0.0.1"],
                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            time.sleep(1)
    finally:
        for proc in running.values():
            proc.terminate()
        for proc in running.values():
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()


def monitor(root):
    path = root / "resources.jsonl"
    with path.open("a") as output:
        while True:
            try:
                pods = kubectl("-n", "orka-efficiency", "get", "pods", "-o", "json", text=False)["items"]
                models = [p for p in pods if any(c["image"].startswith("docker.io/sozercan/aikit-qwen35-2b@")
                                               for c in p["spec"]["containers"])]
                for pod in models:
                    node = pod["spec"].get("nodeName")
                    if not node:
                        continue
                    stats = kubectl("get", "--raw", "/api/v1/nodes/" + node + "/proxy/stats/summary", text=False)
                    match = [p for p in stats["pods"] if p["podRef"]["uid"] == pod["metadata"]["uid"]]
                    value = {"at": now(), "node": node, "nodeMemory": stats["node"]["memory"],
                             "pod": pod["metadata"]["name"], "podUID": pod["metadata"]["uid"],
                             "restarts": sum(c.get("restartCount", 0) for c in pod["status"].get("containerStatuses", [])),
                             "stats": match}
                    output.write(json.dumps(value) + "\n")
                    output.flush()
            except (RuntimeError, KeyError, ValueError) as exc:
                output.write(json.dumps({"at": now(), "sampleError": str(exc)}) + "\n")
                output.flush()
            time.sleep(5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-dir", type=Path)
    sub = parser.add_subparsers(dest="action", required=True)
    sub.add_parser("init")
    sub.add_parser("connect")
    sub.add_parser("monitor")
    p = sub.add_parser("agents")
    p.add_argument("format", choices=["plain", "structured"])
    p = sub.add_parser("ask")
    p.add_argument("phase")
    p.add_argument("workload", choices=list(WORKLOADS))
    p = sub.add_parser("phase")
    p.add_argument("name", choices=["baseline", "routed"])
    p.add_argument("stage", choices=["start", "end"])
    p = sub.add_parser("cli")
    p.add_argument("team", choices=list(ORKA))
    p.add_argument("args", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.action == "connect":
        return connect()
    if args.action == "agents":
        return apply_agents(args.format == "structured")
    if args.action == "cli":
        print(cli(args.team, args.args), end="")
        return
    require(args.run_dir is not None, "--run-dir is required")
    root = args.run_dir.resolve()
    if args.action == "init":
        return init(root)
    if args.action == "monitor":
        return monitor(root)
    if args.action == "ask":
        return ask(root, args.phase, args.workload)
    if args.action == "phase":
        return snapshot_phase(root, args.name, args.stage)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, KeyError, urllib.error.URLError, HTTPException) as exc:
        print("Demo stopped: " + str(exc), file=sys.stderr)
        sys.exit(1)
