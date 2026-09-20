#!/usr/bin/env python3
"""Run the real Orka CLI using a task-owned identity, without changing HOME/config."""

import os
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[2]
team = os.environ.get("DEMO_TEAM", "reliability")
if team not in {"reliability", "engineering"}:
    raise SystemExit("Unknown demo team")
namespace, port = ("orka-system", 18081) if team == "reliability" else ("orka-pr647-system", 18082)
state = ROOT / "bin/hackathon-first-pass/state"
config_dir = state / f"{team}.config"
config_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
os.chmod(config_dir, 0o700)
# This task-owned config stays empty so the kubeconfig tokenFile supplies auth.
config_file = config_dir / "config.yaml"
config_file.write_text("{}\n")
os.chmod(config_file, 0o600)
environment = os.environ.copy()
environment["ORKA_CONFIG_FILE"] = str(config_file)
binary = str(ROOT / "bin/hackathon-first-pass/orka")
os.execve(binary, [binary, "--server", f"http://127.0.0.1:{port}",
                  "--namespace", namespace, "--kubeconfig", str(state / f"{team}.kubeconfig"),
                  *sys.argv[1:]], environment)
