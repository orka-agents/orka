"""Prepare real adapter images and deterministic model/transport inputs for E2E."""

import argparse
import copy
import json
from pathlib import Path
import shutil
import subprocess

import yaml
from agentkit_serve_common.brokered import generate_brokered_tools_from_orka_tool_crds
from agentkit_serve_common.config import AgentSpec


def prepare_gateway_tls(directory):
    """Create a per-run CA and serving key outside image and evidence inputs."""
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    directory.chmod(0o700)
    serving = directory / "serving.conf"
    serving.write_text(
        "[req]\nprompt = no\ndistinguished_name = name\n"
        "[name]\nCN = human-approval-hosted\n"
        "[serving]\nbasicConstraints = critical, CA:false\n"
        "keyUsage = critical, digitalSignature, keyEncipherment\n"
        "extendedKeyUsage = serverAuth\n"
        "subjectAltName = DNS:human-approval-hosted,DNS:human-approval-hosted.orka-system.svc,"
        "DNS:human-approval-hosted.orka-system.svc.cluster.local,IP:127.0.0.1\n"
    )
    commands = [
        ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-sha256", "-days", "2",
         "-subj", "/CN=human-approval-fixture-ca", "-addext", "basicConstraints=critical,CA:TRUE",
         "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", str(directory / "ca.key"),
         "-out", str(directory / "ca.crt")],
        ["req", "-new", "-newkey", "rsa:2048", "-nodes", "-sha256", "-config", str(serving),
         "-keyout", str(directory / "tls.key"), "-out", str(directory / "tls.csr")],
        ["x509", "-req", "-sha256", "-days", "2", "-in", str(directory / "tls.csr"),
         "-CA", str(directory / "ca.crt"), "-CAkey", str(directory / "ca.key"), "-CAcreateserial",
         "-extfile", str(serving), "-extensions", "serving", "-out", str(directory / "tls.crt")],
        ["verify", "-CAfile", str(directory / "ca.crt"), str(directory / "tls.crt")],
    ]
    for command in commands:
        subprocess.run(["openssl", *command], capture_output=True, check=True, timeout=30)
    for name in ("ca.key", "tls.key"):
        (directory / name).chmod(0o600)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agentkit", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[3]
    example = root / "examples/human-approval-v2"
    output = args.output
    output.mkdir(parents=True, exist_ok=True)
    base = yaml.safe_load((example / "agent-hosted.yaml.example").read_text())
    base["model"] = {"provider": "openai-compatible", "baseURL": "http://human-approval-model:8100/v1", "name": "approval-fixture"}
    base["tools"] = []
    direct = copy.deepcopy(base)
    direct["metadata"]["name"] = "human-approval-direct"
    direct["expose"]["port"] = 8080
    hosted = copy.deepcopy(base)
    hosted["brokeredTools"] = generate_brokered_tools_from_orka_tool_crds(
        yaml.safe_load_all((example / "tools.yaml").read_text()))
    for name, config in [("agent-direct.yaml", direct), ("agent-hosted.yaml", hosted)]:
        AgentSpec.model_validate(config)
        (output / name).write_text(yaml.safe_dump(config, sort_keys=False))
    foundry = {"model": "approval-fixture", "toolSchemaMode": "provider-static", "hostedTarget": {
        "projectEndpoint": "https://human-approval-hosted:8092/api/projects/fixture",
        "agentName": "human-approval-hosted", "agentVersion": "1"}}
    (output / "foundry.json").write_text(json.dumps(foundry, indent=2) + "\n")
    prepare_gateway_tls(output.parent / "private" / "gateway-tls")
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
    for name in ["Dockerfile.agentkit-hosted", "Dockerfile.foundry-base", "Dockerfile.fixture", "runtime_fixture.py", "held_tools.py"]:
        shutil.copyfile(Path(__file__).with_name(name), output / name)
    shutil.copyfile(example / "simulated_tools.py", output / "simulated_tools.py")


if __name__ == "__main__":
    main()
