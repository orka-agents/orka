#!/usr/bin/env python3
"""Short terminal views derived from the installation and verified run records."""

import argparse
import json
from pathlib import Path
import sys
import textwrap

import yaml

from evidence import aggregate, profile, read, require, terminal_map
from run import CONTEXT, ORKA, ROUTER, kubectl, save


def connections(root):
    config = kubectl("-n", "orka-efficiency", "get", "configmap", "orka-compat-router", "-o", "json", text=False)
    routes = yaml.safe_load(config["data"]["routes.yaml"])["namespaces"]
    expected = {"team-" + team: f"http://orka-api.team-{team}.svc:8080" for team in ORKA}
    require(routes == expected, "Shared address has unexpected team routes")
    saved = {"context": CONTEXT, "address": ROUTER + "/openai/v1", "router": config, "teams": {}}
    for team in ORKA:
        namespace = "team-" + team
        controller = kubectl("-n", namespace, "get", "deployment", "orka-controller", "-o", "json", text=False)
        args = controller["spec"]["template"]["spec"]["containers"][0]["args"]
        require("--controller-mode=harness-v2" in args and "--watch-namespace=" + namespace in args,
                "Team controller is not using its own harness-v2 namespace")
        require(controller["status"].get("availableReplicas") == 1, "Team controller is unavailable")
        saved["teams"][team] = {"controllerUID": controller["metadata"]["uid"], "arguments": args}
    save(root / "connections.json", saved)
    print("One application address: " + saved["address"])
    print("This terminal forwards that address from sertac-aks.\n")
    print("Developer identity          Orka installation")
    for team in ORKA:
        print(f"{('team-' + team + '/demo-client'):<28} team-{team}")
    print("\nApplication model name: platform/coordinator")
    print("Each team keeps its own Agents, Tasks, and usage records.")


def agent(root):
    value = kubectl("-n", "team-inventory", "get", "agent", "efficiency-inventory", "-o", "json", text=False)
    save(root / ("agent-" + value["metadata"]["resourceVersion"] + ".json"), value)
    print("Saved Agent: " + value["metadata"]["name"])
    print("Model connection: " + value["spec"]["providerRef"]["name"] + "/" + value["spec"]["model"]["name"])
    print("Stock instruction:")
    print(textwrap.fill(value["spec"]["systemPrompt"]["inline"].splitlines()[-1], width=88))


def routes(root):
    destinations = None
    for team in ORKA:
        snapshot = read(root / "routed" / f"{team}-gateway-start.json")
        state = profile(snapshot)
        require(state["effective_mode"] == "enforce" and state["preflight_state"] == "ready",
                "Routing assessment is not ready")
        _, current = terminal_map(snapshot)
        require(destinations is None or destinations == current, "Team gateway destinations differ")
        destinations = current
        print(f"{team.capitalize()} gateway: routing enabled; Jev connection checked")
    print("\nConfigured destinations behind team-assistant:")
    print("  Local worker:  " + destinations["lightweight"]["model"] + " through AIKit on cluster CPU")
    print("  Hosted worker: " + destinations["powerful"]["model"])
    print("  Coordinator:   " + destinations["coordinator"]["model"] + " stays hosted")
    print("\nJev assesses the request. The gateway records the route actually used.")


def outcomes(report):
    print("Same requests and Agent instructions; separate checked Tasks.\n")
    print("Request                Hosted baseline  Routing enabled  Checks")
    for row in report["comparisons"]:
        before, after = row["baseline"], row["routed"]
        print(f"{row['workload']:<22} {before['worker']['destination']['model']:<16} "
              f"{after['worker']['destination']['model']:<16} {before['checkCount']} / {after['checkCount']} passed")
    print("\nChecks cover these examples, including overlapping requests and retries.")


def usage(report):
    print("Orka usage summary: Other team usage for these requests.")
    print("Gateway records identify the model behind each call. Count usage once.\n")
    print("Run       Team       Local tokens  Hosted worker  Coordination  Assessment")
    for phase, value in report["phases"].items():
        for team, window in value["teams"].items():
            roles = window["roles"]
            classifier = window["classifier"]
            counts = [roles[key]["usage"]["total_tokens"] for key in ("cpuWorker", "hostedWorker", "coordinator")]
            assessed = str(classifier["usage"]["total_tokens"]) if classifier["usageComplete"] else "unavailable"
            print(f"{phase:<9} {team:<10} {counts[0]:<13,} {counts[1]:<14,} {counts[2]:<13,} {assessed}")
    print("\nReported input + output tokens. Cached input is included.")
    print("Assessment is separate from Orka's model totals; unavailable is not zero.")
    print("Startup assessment checks sit outside the request totals:")
    for team, window in report["phases"]["routed"]["teams"].items():
        for row in window["preflightBeforeInterval"]:
            measured = (str(row["classifier_usage"]["total_tokens"]) + " reported tokens"
                        if row["usageComplete"] else "token usage unavailable")
            print(f"  {team}: {row['physical_classifier_sends']} sends, "
                  f"{measured}")


def resources(report):
    print("Actual seconds from application request to answer:\n")
    print("Request                Baseline  Routed")
    for row in report["comparisons"]:
        print(f"{row['workload']:<22} {row['baseline']['clientSeconds']:>7.1f}  {row['routed']['clientSeconds']:>6.1f}")
    print("\nLocal model service, including idle time during each sampled interval:")
    for phase, value in report["phases"].items():
        sample = value["resources"]
        print(f"{phase:<9} {sample['cpuSeconds']:.1f} CPU seconds / {sample['sampledSeconds']:.1f}s sampled; "
              f"{sample['peakWorkingSetMiB']:.0f} MiB peak working memory")
    print("\nCPU and memory are real costs. These measurements do not establish dollar savings.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-dir", type=Path, required=True)
    parser.add_argument("view", choices=["connections", "agent", "routes", "outcomes", "usage", "resources", "ending"])
    args = parser.parse_args()
    root = args.run_dir.resolve()
    if args.view in ("connections", "agent", "routes"):
        return globals()[args.view](root)
    report = aggregate(root)
    save(root / "verified-report.json", report)
    if args.view in ("outcomes", "usage", "resources"):
        return globals()[args.view](report)
    print("Same inventory question, with updated platform instructions:")
    print("Before: " + report["orchestration"]["before"])
    print("After:")
    print(json.dumps(report["orchestration"]["after"], indent=2))
    print("\nAgent instructions shape the answer. Gateway policy chooses the model.")
    print("The checks and usage records show what those choices produced.")
    print("\nWhat experience would you build for your teams?")
    print("https://orka-agents.github.io/orka/")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, KeyError, TypeError) as exc:
        print("View stopped: " + str(exc), file=sys.stderr)
        sys.exit(1)
