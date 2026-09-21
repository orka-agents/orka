#!/usr/bin/env python3
"""Record or render the second cut using the original terminal workflow."""

import argparse
import importlib.util
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
SCENES = {"01-schedule": 300, "02-discovery": 330, "04-handoff": 120,
          "05-patch": 300, "06-delivery": 210, "07-usage": 300}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["capture", "render"])
    parser.add_argument("scenes", nargs="*", choices=list(SCENES))
    args = parser.parse_args()
    source = ROOT / "demo/06-hackathon/terminal" / (args.mode + ".py")
    spec = importlib.util.spec_from_file_location("first_pass_terminal", source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.HERE = HERE / "terminal"
    module.OUTPUT = ROOT / "bin/hackathon-platform-revised/terminal"
    module.SCENES = SCENES
    sys.argv = [str(source), *(args.scenes or SCENES)]
    module.main()


if __name__ == "__main__":
    main()
