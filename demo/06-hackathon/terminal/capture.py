#!/usr/bin/env python3
"""Record read-only scene scripts with the existing Orka demo helper."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[3]
HERE = Path(__file__).resolve().parent
OUTPUT = ROOT / "bin/hackathon-first-pass/terminal"
SCENES = {
    "01-schedule": 420,
    "02-discovery": 510,
    "04-handoff": 210,
    "05-patch": 390,
    "06-delivery": 420,
    "07-usage": 210,
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("scenes", nargs="*", choices=list(SCENES), default=["01-schedule", "02-discovery"])
    parser.add_argument("--type-speed", type=int, default=72)
    args = parser.parse_args()
    OUTPUT.mkdir(parents=True, exist_ok=True)
    environment = os.environ.copy()
    environment.update({"TYPE_SPEED": str(args.type_speed), "DEMO_AUTO": "1", "TERM": "xterm-256color"})
    for scene in args.scenes:
        raw = OUTPUT / f"{scene}.raw.cast"
        cast = OUTPUT / f"{scene}.cast"
        subprocess.run([
            "asciinema", "rec", str(raw), "--overwrite", "--headless", "--return",
            "--window-size", "100x28", "--idle-time-limit", "2",
            "--capture-env", "TERM,SHELL", "--title", "Orka / " + scene,
            "--command", shlex.join(["bash", str(HERE / f"{scene}.sh")]),
        ], cwd=ROOT, env=environment, check=True)
        shutil.copyfile(raw, cast)
        subprocess.run([sys.executable, str(ROOT / "demo/lib/markers.py"), str(cast)], check=True)
        lines = raw.read_text().splitlines()
        header = json.loads(lines[0])
        if header.get("version") != 3:
            raise RuntimeError("Expected asciicast v3")
        elapsed = sum(json.loads(line)[0] for line in lines[1:] if line.strip())
        metadata = {
            "scene": scene, "target_frames": SCENES[scene], "frames_per_second": 30,
            "recorded_seconds": round(elapsed, 6), "columns": 100, "rows": 28,
            "type_speed": args.type_speed,
            "raw_cast_sha256": hashlib.sha256(raw.read_bytes()).hexdigest(),
            "source": "Live read-only CLI queries and the actual demo bridge receipt where applicable.",
        }
        (OUTPUT / f"{scene}-capture.json").write_text(json.dumps(metadata, indent=2) + "\n")
        print(json.dumps(metadata), flush=True)


if __name__ == "__main__":
    main()
