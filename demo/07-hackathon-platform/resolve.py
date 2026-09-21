#!/usr/bin/env python3
"""Reuse the verified Resolve assembler with a separate output directory."""

import importlib.util
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "demo/06-hackathon/resolve/assemble.py"
spec = importlib.util.spec_from_file_location("first_pass_resolve", SOURCE)
assembler = importlib.util.module_from_spec(spec)
spec.loader.exec_module(assembler)
assembler.OUTPUT_ROOT = (ROOT / "bin/hackathon-platform-intro/resolve").resolve()
assembler.DEFAULT_MANIFEST = Path(__file__).with_name("manifest.json")

if __name__ == "__main__":
    assembler.main()
