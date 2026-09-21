#!/usr/bin/env python3
"""Render the first-time viewer introduction using the retained demo style."""

import argparse
import importlib.util
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
OUTPUT = ROOT / "bin/hackathon-platform-intro/media"
SOURCE = ROOT / "demo/06-hackathon/media/render_cards.py"
spec = importlib.util.spec_from_file_location("first_pass_cards", SOURCE)
cards = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cards)
layer, text, line = cards.layer, cards.text, cards.line


def arrow(image, points, color="blue"):
    line(image, points, color, 3)
    x, y = points[-1]
    line(image, [(x - 16, y - 11), (x, y), (x - 16, y + 11)], color, 3)


def progress(image, active=None):
    """Keep the human request and final review visible throughout the workflow."""
    line(image, [(136, 979), (1784, 979)], "rule")
    positions = (160, 476, 792, 1232, 1564)
    labels = ("Scan", "Finding", "Human request", "Fix", "Review")
    for index, (x, label) in enumerate(zip(positions, labels)):
        color = "blue" if index == active else "muted"
        if active is not None and index < active:
            color = "green"
        cards.dot(image, x, 1027, 5, color)
        text(image, (x + 20, 1009), label, 31, color,
             "display" if index == active else "body")


def platform(mark):
    main = layer()
    text(main, (132, 104), "Orka", 120, "pink", "display")
    text(main, (134, 301), "Your agents, working together.", 79, role="display")
    text(main, (138, 447), "Run AI agents. Coordinate work across teams.", 48)
    text(main, (138, 540), "With permissions you control.", 48, "green")
    cards.paste_mascot(main, mark, 1460, 85, 265)
    line(main, [(138, 772), (1784, 772)])
    text(main, (140, 824), "Open source", 33, "blue", "mono")
    return [(main, 0, 0.3)]


def scope(mark):
    main = layer()
    text(main, (136, 102), "ONE EXAMPLE / SECURITY REVIEW", 29, "blue", "mono")
    text(main, (131, 226), "Two teams. One workflow.", 87, role="display")
    text(main, (137, 354), "A web app needs a security review.", 41, "muted")
    text(main, (138, 521), "Security agents", 54, "blue", "display")
    text(main, (787, 521), "You in Teams", 49, "pink", "display")
    text(main, (1258, 521), "Engineering agent", 51, "green", "display")
    arrow(main, [(569, 553), (745, 553)])
    arrow(main, [(1105, 553), (1220, 553)], "green")
    text(main, (141, 620), "Find undiscovered security", 33, "muted")
    text(main, (141, 674), "vulnerabilities in source code", 33, "muted")
    text(main, (790, 620), "Request the fix", 34, "muted")
    text(main, (1261, 620), "Prepare and test a fix", 33, "muted")
    text(main, (1261, 674), "for human review", 33, "muted")
    text(main, (140, 846), "Orka coordinates work under each team's permissions.", 37)
    progress(main)
    return [(main, 0, 0.3)]


def gateways(mark):
    main = layer()
    text(main, (132, 85), "Your agents. Your channels.", 78, role="display")
    text(main, (136, 200), "Send requests through your channels. Choose where agents run.", 36, "muted")
    text(main, (138, 301), "GATEWAYS / REQUESTS IN", 27, "blue", "mono")
    text(main, (1180, 301), "AGENTS / WORK RUNS HERE", 27, "blue", "mono")
    text(main, (845, 548), "Orka", 91, "pink", "display")
    text(main, (817, 663), "Coordinate work", 28)
    text(main, (817, 709), "Apply permissions", 28, "muted")
    cards.bracket(main, 795, 532, 639, 22)
    text(main, (138, 997), "Connect through compatible gateway and agent adapters.", 28, "muted")

    inputs = layer()
    gateways = [
        (382, "Microsoft Teams", "Gateway adapter"),
        (492, "Microsoft Scout", "Adapter for compatible Scout builds"),
        (602, "Telegram", "Gateway adapter"),
        (712, "Slack", "Custom gateway"),
        (822, "Bring-your-own gateway", "Your apps and services"),
    ]
    for y, name, detail in gateways:
        text(inputs, (140, y), name, 43, role="display")
        text(inputs, (143, y + 57), detail, 26, "muted")
        line(inputs, [(670, y + 25), (738, y + 25)], "blue", 2)
    line(inputs, [(738, 407), (738, 847)], "blue", 2)
    arrow(inputs, [(738, 590), (778, 590)])

    native = layer()
    arrow(native, [(1050, 590), (1127, 590), (1127, 405), (1159, 405)])
    text(native, (1180, 377), "Native agents", 45, role="display")
    text(native, (1183, 436), "Running in Kubernetes", 32)
    text(native, (1183, 487), "Optional: Agent Sandbox", 27, "muted")
    text(native, (1183, 529), "and Agent Substrate", 27, "muted")

    external = layer()
    agent_rows = [
        (600, "Local agents", "On your laptop, for example in Docker"),
        (733, "Foundry-hosted agents", "Running in Microsoft Foundry"),
        (866, "Bring-your-own agent", "Connect a compatible runtime"),
    ]
    line(external, [(1127, 590), (1127, 891)], "blue", 2)
    for y, name, detail in agent_rows:
        arrow(external, [(1127, y + 25), (1159, y + 25)])
        text(external, (1180, y), name, 43, role="display")
        text(external, (1183, y + 58), detail, 29, "muted")
    return [(main, 0, 0.3), (inputs, 0.4, 0.3),
            (native, 6.0, 0.4), (external, 11.5, 0.4)]


def outro(mark):
    main = layer()
    text(main, (134, 98), "Orka", 77, "pink", "display")
    text(main, (134, 282), "A vulnerability found.", 77, role="display")
    text(main, (134, 404), "A fix tested.", 77, role="display")
    text(main, (134, 537), "A pull request ready for human review.", 68, role="display")
    text(main, (140, 686), "Build your team's next workflow.", 39, "muted")
    cards.paste_mascot(main, mark, 1520, 166, 270)
    line(main, [(140, 802), (1780, 802)])
    text(main, (140, 857), "https://orka-agents.github.io/orka/", 42, "blue", "mono")
    text(main, (141, 939), "Docs, examples, and integrations", 31)
    return [(main, 0, 0.3)]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scene", action="append", choices=["00-platform", "00-scope", "08-gateways", "09-outro"])
    args = parser.parse_args()
    OUTPUT.mkdir(parents=True, exist_ok=True)
    mark = cards.mascot(ROOT / "website/static/img/orka-logo.png")
    timing = json.loads((HERE / "timing.json").read_text())
    frames = {scene["id"]: scene["frames"] for scene in timing["scenes"]}
    scenes = {"00-platform": platform, "00-scope": scope,
              "08-gateways": gateways, "09-outro": outro}
    for scene_id, draw in scenes.items():
        if not args.scene or scene_id in args.scene:
            cards.render(scene_id, draw(mark), frames[scene_id], OUTPUT)


if __name__ == "__main__":
    main()
