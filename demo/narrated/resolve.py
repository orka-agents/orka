#!/usr/bin/env python3
"""Use the existing Resolve assembler for the standalone narrated demos."""

import importlib.util
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "demo/06-hackathon/resolve/assemble.py"
spec = importlib.util.spec_from_file_location("demo_resolve", SOURCE)
assembler = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assembler)
assembler.OUTPUT_ROOT = ROOT / "bin/narrated-demos/exports"
assembler.MAX_FRAMES = 10 * 60 * assembler.FPS
assembler.SANDBOX_ROOT = (
    Path.home() / "Library/Containers/com.blackmagic-design.DaVinciResolveLite/Data/OrkaDemos"
)

if __name__ == "__main__":
    assembler.main()
