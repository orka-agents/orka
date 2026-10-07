"""Prepare both Fibey adapter build contexts from one instruction set and Tool list.

Adapted from Orka's scripts/fixtures/human-approval-v2/prepare.py. The model and
the hosted transport are real: the Orka-hosted Fibey reaches the Foundry model
through Orka's provider proxy, and the hosted Fibey runs as a Foundry Hosted
Agent with workload identity. Run inside a venv with AgentKit's common runtime
package installed.
"""

import argparse
import copy
import json
from pathlib import Path
import shutil

import yaml
from agentkit_serve_common.brokered import generate_brokered_tools_from_orka_tool_crds
from agentkit_serve_common.config import AgentSpec

HERE = Path(__file__).resolve().parent


def agent_config(name, model, port, instructions):
    return {
        "abiVersion": "v0",
        "metadata": {"name": name},
        "model": model,
        "instructions": instructions,
        "tools": [],
        "expose": {"openai": True, "port": port},
    }


def write_foundry_config(output, model, endpoint, agent, version):
    config = {"model": model, "toolSchemaMode": "provider-static", "hostedTarget": {
        "projectEndpoint": endpoint, "agentName": agent, "agentVersion": str(version)}}
    (output / "foundry.json").write_text(json.dumps(config, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agentkit", type=Path, required=True)
    parser.add_argument("--orka", type=Path, required=True)
    parser.add_argument("--tools", type=Path, required=True, help="Rendered Orka Tool manifests")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--direct-base-url", required=True, help="Orka provider proxy /v1 URL")
    parser.add_argument("--project-endpoint", required=True)
    parser.add_argument("--hosted-agent", required=True)
    parser.add_argument("--hosted-version", help="Write foundry.json for this concrete hosted version")
    args = parser.parse_args()
    output = args.output
    output.mkdir(parents=True, exist_ok=True)
    if args.hosted_version:
        write_foundry_config(output, args.model, args.project_endpoint, args.hosted_agent, args.hosted_version)
        return

    instructions = (HERE / "fibey-instructions.txt").read_text()
    direct = agent_config("fibey-orka-hosted", {
        "provider": "openai-compatible", "baseURL": args.direct_base_url, "name": args.model,
    }, 8080, instructions)
    hosted = agent_config("fibey-foundry-hosted", {
        "provider": "openai-compatible", "baseURL": args.project_endpoint.rstrip("/") + "/openai/v1",
        "name": args.model,
        "auth": {"type": "workload-identity-token", "audience": "https://ai.azure.com/.default"},
    }, 8088, instructions)
    hosted["brokeredTools"] = generate_brokered_tools_from_orka_tool_crds(
        yaml.safe_load_all(args.tools.read_text()))
    for name, config in [("agent-direct.yaml", direct), ("agent-hosted.yaml", hosted)]:
        AgentSpec.model_validate(copy.deepcopy(config))
        (output / name).write_text(yaml.safe_dump(config, sort_keys=False))

    for folder, package in [("common", "agentkit_serve_common"), ("microsoft-agent-framework", "agentkit_serve")]:
        source = args.agentkit / "runtimes" / folder
        target = output / "runtimes" / folder
        target.mkdir(parents=True, exist_ok=True)
        for filename in ["pyproject.toml", "README.md"]:
            shutil.copyfile(source / filename, target / filename)
        shutil.copytree(source / package, target / package,
                        ignore=shutil.ignore_patterns("__pycache__", "*.pyc"), dirs_exist_ok=True)
    dockerfile = (args.agentkit / "runtimes/microsoft-agent-framework/Dockerfile").read_text()
    dockerfile += "\nUSER 0\nCOPY --chown=0:0 agent-direct.yaml /agent/agent.yaml\nRUN chmod 0444 /agent/agent.yaml\nUSER 1000\n"
    (output / "Dockerfile.agentkit-base").write_text(dockerfile)
    fixtures = args.orka / "scripts/fixtures/human-approval-v2"
    shutil.copyfile(fixtures / "Dockerfile.foundry-base", output / "Dockerfile.foundry-base")
    shutil.copyfile(HERE / "Dockerfile.agentkit-hosted", output / "Dockerfile.agentkit-hosted")
    shutil.copyfile(HERE / "foundry_ingress.py", output / "foundry_ingress.py")


if __name__ == "__main__":
    main()
