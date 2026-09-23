#!/usr/bin/env python3
"""Run the matched workload without recording, retaining every attempt.

The rehearsal reverses the recording's order to reveal obvious order effects.
It uses the same manifests, acceptance checks, public CLI and evidence collector.
"""

import argparse
from http.client import HTTPException
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.request

import run
import story
import prepare


def ready(url, seconds=90):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if response.status == 200:
                    return
        except (urllib.error.URLError, TimeoutError, HTTPException, ConnectionResetError):
            pass
        time.sleep(2)
    raise RuntimeError("A demo service did not become ready")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-dir", type=Path, required=True)
    parser.add_argument("--order", nargs=2, choices=story.PHASES, default=["routed", "baseline"])
    args = parser.parse_args()
    story.require(set(args.order) == set(story.PHASES), "Each mode must run exactly once")
    root = args.run_dir.resolve()
    story.require(not root.exists(), "Preserve previous runs; choose a fresh directory")
    root.parent.mkdir(parents=True, exist_ok=True)
    story.setup()
    processes = []
    with root.with_suffix(".connections.log").open("w") as logs:
        connection = subprocess.Popen([sys.executable, str(story.HERE / "run.py"), "connect"], stdout=logs, stderr=logs)
        processes.append(connection)
        try:
            ready("http://127.0.0.1:18101/healthz")
            ready("http://127.0.0.1:18111/readyz")
            story.require(connection.poll() is None, "Connection helper exited")
            story.init(root)
            run.save(root / "order.json", {"phases": args.order, "purpose": "rehearsal", "selection": "fixed before execution"})
            monitor = subprocess.Popen([sys.executable, str(story.HERE / "run.py"), "--run-dir", str(root), "monitor"], stdout=logs, stderr=logs)
            processes.append(monitor)
            for phase in args.order:
                mode = "off" if phase == "baseline" else "enforce"
                run.kubectl("-n", story.NAMESPACE, "set", "env", "deployment/vekil", "POLICY_ROUTING_MODE=" + mode)
                run.kubectl("-n", story.NAMESPACE, "rollout", "status", "deployment/vekil", "--timeout=180s")
                ready("http://127.0.0.1:18111/readyz")
                print("Starting " + phase, flush=True)
                story.phase(root, phase, "start")
                for job in story.JOBS:
                    name = story.task_manifest(root, phase, job)["metadata"]["name"]
                    story.begin(root, phase, job)
                    run.cli("payments", ["task", "create", "-f", str(root / f"{job}-{phase}.yaml")])
                    run.cli("payments", ["task", "wait", name, "--timeout", "10m"])
                    story.collect(root, phase, job)
                    print(phase + " " + job + " passed", flush=True)
                story.checkout(root, phase)
                command = ["podman", "run", "--rm", "--pull=never", "--network=none", "--read-only",
                           "--cap-drop=all", "--security-opt=no-new-privileges", "--user=65532",
                           "--memory=256m", "--pids-limit=64", "--timeout=60",
                           "-v", str(root / phase / "repository/payments") + ":/checks:ro",
                           story.NODE_TEST_IMAGE, "node", "--test", "/checks/payment.test.mjs"]
                with (root / phase / "engineering/tests.txt").open("w") as output:
                    subprocess.run(command, stdout=output, stderr=subprocess.STDOUT, check=True, timeout=90)
                story.finish_tests(root, phase)
                story.phase(root, phase, "end")
            story.verify(root)
            print((root / "cost-summary.txt").read_text(), flush=True)
        except Exception as error:
            if root.exists():
                run.save(root / "failure.json", {"at": run.now(), "type": type(error).__name__,
                         "message": str(error) if not isinstance(error, subprocess.CalledProcessError) else "Child command failed"})
                try:
                    story.retain_failure(root)
                except Exception as capture_error:
                    run.save(root / "failure-capture.json", {"type": type(capture_error).__name__})
            raise
        finally:
            for process in reversed(processes):
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, ValueError, KeyError, prepare.SetupError, subprocess.CalledProcessError) as error:
        # Command arguments can contain short-lived tokens; never print them.
        print("Rehearsal stopped: " + (type(error).__name__ if isinstance(error, subprocess.CalledProcessError) else str(error)), file=sys.stderr)
        sys.exit(1)
