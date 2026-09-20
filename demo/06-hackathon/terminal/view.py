#!/usr/bin/env python3
"""Format selected live CLI fields for recording; never synthesize outcomes."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
import textwrap

ROOT = Path(__file__).resolve().parents[3]
STATE = ROOT / "bin/hackathon-first-pass/state"
SCAN = "hackathon-nodejs-goof"
FINDING = "fnd_0afb30da8140"
FIX = "hackathon-fix-0afb30da8140"
SCHEDULE = "hackathon-scheduled-scan"
TRIGGER = "hackathon-scheduled-scan-1789878840"
WHITE, DIM = "\033[38;5;255m", "\033[38;5;245m"
GREEN, RED, RESET = "\033[38;5;114m", "\033[38;5;203m", "\033[0m"


def clean(value):
    return re.sub(r"[\x00-\x1f\x7f-\x9f]", " ", str(value)).strip()


def row(label, value, color=WHITE):
    print(f"{DIM}{label:<22}{RESET}{color}{clean(value)}{RESET}")


def paragraph(value, width=88, limit=None):
    lines = textwrap.wrap(clean(value), width=width)
    if limit and len(lines) > limit:
        lines = lines[:limit]
        lines[-1] = lines[-1].rstrip(" .") + " ..."
    for line in lines:
        print(WHITE + line + RESET)


def cli(*args, team=None, raw=False):
    environment = os.environ.copy()
    if team:
        environment["DEMO_TEAM"] = team
    result = subprocess.run([sys.executable, str(ROOT / "demo/06-hackathon/cli.py"), *args],
                            env=environment, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError("The scoped Orka read failed; no clip should be rendered")
    return result.stdout if raw else json.loads(result.stdout)


def saved_finding():
    record = json.loads((STATE / "finding.json").read_text())
    if record.get("id") != FINDING or record.get("repositoryScan") != SCAN:
        raise RuntimeError("The saved evidence belongs to a different finding")
    return record


def receipt():
    record = json.loads((STATE / "handoff.json").read_text())
    if (record.get("task") != FIX or record.get("finding") != FINDING
            or record.get("sourceTeam") != "orka-system"
            or record.get("destinationTeam") != "orka-pr647-system"
            or not record.get("eventId") or not record.get("sourceTask")):
        raise RuntimeError("A complete cross-team handoff receipt is required")
    return record


def scan_record(data):
    expected = saved_finding()["scanRunID"]
    records = [item for item in data.get("items", []) if item.get("id") == expected]
    if len(records) != 1 or records[0].get("phase") != "succeeded":
        raise RuntimeError("The selected completed scan is not in the live response")
    return records[0]


def schedule(data):
    if data["metadata"]["name"] != SCHEDULE or not data["status"].get("lastScheduleTime"):
        raise RuntimeError("The live parent has no scheduled tick")
    spec, status = data["spec"], data["status"]
    cron = "Every minute (UTC)" if spec.get("schedule") == "* * * * *" and spec.get("timeZone") == "UTC" else spec.get("schedule")
    row("Schedule", cron)
    row("Last scheduled tick", status["lastScheduleTime"])
    row("Further ticks", "Paused after this run" if spec.get("suspend") else "Enabled")


def scan(data, progress=False):
    record = scan_record(data)
    row("Scan outcome", record["phase"], GREEN)
    row("Source areas reviewed", f"{record['reviewedSliceCount']} / {record['sliceCount']}")
    row("Findings retained", record["acceptedFindings"])
    if not progress:
        row("Scan began", record["startedAt"].split(".")[0].replace("Z", "") + " UTC")
        row("Scan completed", record["completedAt"])


def finding(data):
    if data.get("id") != FINDING or data.get("validationStatus") != "validated":
        raise RuntimeError("The selected live finding is not validated")
    row("Finding", data["id"])
    row("Severity", data["severity"].upper(), RED)
    row("Validation", "Validated by scan", GREEN)
    row("Source evidence", f"{data['filePath']}:{data['line']}")
    print()
    paragraph(data["title"])
    paragraph(data["summary"], limit=3)


def handoff(data):
    recorded = receipt()
    meta = data["metadata"]
    annotations = meta.get("annotations", {})
    if (meta.get("name") != FIX or meta.get("namespace") != recorded["destinationTeam"]
            or annotations.get("demo.orka.ai/source-event") != recorded["eventId"]
            or annotations.get("demo.orka.ai/source-finding") != FINDING):
        raise RuntimeError("The Engineering task does not match the handoff receipt")
    row("Authorized request", "Microsoft Teams")
    row("From", "Reliability / " + recorded["sourceTeam"])
    row("To", "Engineering / " + recorded["destinationTeam"])
    row("Finding", recorded["finding"])
    row("Engineering task", meta["name"], GREEN)


def patch(data):
    result = data.get("result", "").strip()
    if not result:
        raise RuntimeError("The coding agent has no recorded result yet")
    if not re.search(r"\b(test|tests|check|checks)\b", result, re.I):
        raise RuntimeError("The recorded result does not mention checks; review the narration")
    # Select actual report bullets, keeping the check outcome and limitation
    # visible. Drop Markdown link destinations, which contain session paths.
    result = re.sub(r"\[([^\]]+)\]\([^)]+\)", r"\1", result).replace("`", "")
    for heading, count in (("Patch summary:", 2), ("Tests:", 2), ("Limitations:", 1)):
        section = re.search(re.escape(heading) + r"\n(.*?)(?=\n\n|\Z)", result, re.S)
        if not section:
            raise RuntimeError("The agent report format changed; select a new verified excerpt")
        bullets = [line for line in section[1].splitlines() if line.startswith("- ")]
        if len(bullets) < count:
            raise RuntimeError("The agent report is missing the expected evidence")
        print(DIM + heading + RESET)
        for line in bullets[:count]:
            paragraph(line)


def delivery(data):
    execution, delivered = data.get("execution", {}), data.get("delivery", {})
    pr = delivered.get("prReceipt", {})
    url = pr.get("url", "")
    sha = delivered.get("verifiedRemoteSHA", "")
    if (data.get("task") != FIX or execution.get("state") != "Succeeded"
            or delivered.get("state") != "VerifiedExact"
            or not sha or sha != delivered.get("expectedCommitSHA")
            or sha != pr.get("headSHA")
            or not re.fullmatch(r"https://github\.com/sozercan/nodejs-goof/pull/[0-9]+", url)):
        raise RuntimeError("Exact publication and a PR receipt are required before this scene")
    result = subprocess.run(["gh", "pr", "view", url, "--json", "number,state,url,headRefOid,title"],
                            capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError("GitHub did not verify the recorded pull request")
    remote = json.loads(result.stdout)
    if remote.get("headRefOid") != sha or remote.get("state") != "OPEN":
        raise RuntimeError("The pull request no longer matches the planned open review state")
    row("Execution", execution["state"], GREEN)
    row("Delivery", delivered["state"], GREEN)
    row("Pull request", f"#{remote['number']} / {remote['state']}")
    row("Verified commit", sha[:12])
    print()
    paragraph(remote["title"])
    paragraph(url)


def usage(data):
    recorded = receipt()
    other = data.get("otherWork", {})
    tasks = other.get("tasks", [])
    matches = [task for task in tasks if task.get("taskName") == recorded["sourceTask"]]
    if len(matches) != 1:
        raise RuntimeError("The selected Teams request is not on this live usage page")
    measured = matches[0]["usage"]
    if measured.get("reportedMeasurements", 0) == 0 or measured.get("completeness") == "unavailable":
        raise RuntimeError("The Teams request has no reported model measurement yet")
    row("Measured work", "One Teams request / Reliability")
    row("Input tokens", f"{measured['inputTokens']:,}")
    row("Output tokens", f"{measured['outputTokens']:,}")
    row("Measurement coverage", measured["completeness"])
    scan_tasks = [task for task in tasks if task.get("taskName", "").startswith(SCAN + "-")]
    if not scan_tasks:
        raise RuntimeError("The live usage page has no matching scan tasks")
    coverage = {task["usage"].get("completeness") for task in scan_tasks}
    row("ACP scan usage", "Unavailable" if coverage == {"unavailable"} else "See per-task measurements")
    row("Model cost", measured["modelCost"])


def ready(scene):
    if scene == "schedule":
        parent = cli("task", "get", SCHEDULE, team="reliability")
        child = cli("task", "get", TRIGGER, team="reliability")
        owners = child["metadata"].get("ownerReferences", [])
        if not any(owner.get("uid") == parent["metadata"]["uid"] for owner in owners):
            raise RuntimeError("The trigger task is not owned by the scheduled parent")
        logs = cli("task", "logs", TRIGGER, team="reliability", raw=True)
        if "Schedule triggered RepositoryScan " + SCAN not in logs:
            raise RuntimeError("The trigger task has no scan creation evidence")
        scan_record(cli("security", "scan", "list", SCAN, "-o", "json", team="reliability"))
    elif scene == "discovery":
        record = cli("security", "finding", "get", FINDING, team="reliability")
        if record.get("validationStatus") != "validated":
            raise RuntimeError("The selected finding must be validated")
    else:
        recorded = receipt()
        task = cli("task", "get", FIX, team="engineering")
        if task["metadata"].get("annotations", {}).get("demo.orka.ai/source-event") != recorded["eventId"]:
            raise RuntimeError("The current task has a different handoff")
        if scene in {"patch", "delivery"} and task.get("status", {}).get("phase") != "Succeeded":
            raise RuntimeError("The Engineering task has not succeeded yet")
        if scene == "usage":
            cli("task", "get", recorded["sourceTask"], team="reliability")


def main():
    if len(sys.argv) < 2:
        raise RuntimeError("Expected a display mode")
    mode = sys.argv[1]
    if mode == "ready":
        ready(sys.argv[2])
        return
    data = json.load(sys.stdin)
    if mode == "progress":
        scan(data, progress=True)
    else:
        {"schedule": schedule, "scan": scan, "finding": finding, "handoff": handoff,
         "patch": patch, "delivery": delivery, "usage": usage}[mode](data)


if __name__ == "__main__":
    try:
        main()
    except (KeyError, ValueError, OSError, RuntimeError) as error:
        raise SystemExit("Recording precondition failed: " + clean(error)) from None
