#!/usr/bin/env python3
"""Render the revised narrative cards with the original demo palette and type."""

import argparse
import importlib.util
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
SOURCE = ROOT / "demo/06-hackathon/media/render_cards.py"
spec = importlib.util.spec_from_file_location("first_pass_cards", SOURCE)
cards = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cards)
layer, text, line = cards.layer, cards.text, cards.line


def platform(mark):
    main = layer()
    text(main, (132, 83), "Orka", 96, "pink", "display")
    text(main, (134, 218), "Your agents, working together.", 76, role="display")
    text(main, (137, 324), "Open-source execution and governance for AI agents", 38, "muted")
    cards.paste_mascot(main, mark, 1542, 48, 185)
    line(main, [(136, 411), (1784, 411)])
    groups = [
        (4.1, 474, [
            ("Bring your agents", "Compatible local and Foundry integrations"),
            ("Coordinate work", "Schedules, events, and team workflows"),
        ]),
        (8.0, 644, [
            ("Choose tools and skills", "Reuse capabilities across your workflows"),
            ("Control access", "Permissions for each team and agent"),
        ]),
        (12.0, 814, [
            ("Share reviewed knowledge", "Project memory with human review"),
            ("Make work quantifiable", "Task results and reported token usage"),
        ]),
    ]
    layers = [(main, 0, 0.35)]
    for start, y, pair in groups:
        part = layer()
        for x, (title, description) in zip((138, 1006), pair):
            cards.dot(part, x, y + 24, 5, "blue")
            text(part, (x + 27, y), title, 41, role="display")
            text(part, (x + 27, y + 65), description, 29, "muted")
        layers.append((part, start, 0.4))
    return layers


def scope(mark):
    main = layer()
    text(main, (132, 123), "ONE SMALL EXAMPLE", 30, "blue", "mono")
    text(main, (131, 244), "Two teams. One workflow.", 88, role="display")
    text(main, (137, 368), "A web app needs a security review.", 40, "muted")
    text(main, (171, 526), "Security", 76, "blue", "display")
    text(main, (1110, 526), "Engineering", 76, "green", "display")
    line(main, [(588, 570), (1011, 570)], "rule", 3)
    line(main, [(991, 555), (1011, 570), (991, 585)], "green", 3)
    text(main, (174, 648), "Find undiscovered security", 36, "muted")
    text(main, (174, 704), "vulnerabilities in source code", 36, "muted")
    text(main, (1113, 648), "Prepare a fix for review", 36, "muted")
    return [(main, 0, 0.35)]


def gateways(mark):
    main = layer()
    text(main, (132, 113), "Your agents. Your channels.", 78, role="display")
    text(main, (136, 226), "Connect through compatible, extensible integrations.", 37, "muted")
    text(main, (158, 350), "GATEWAYS", 27, "blue", "mono")
    text(main, (1174, 350), "AGENTS", 27, "blue", "mono")
    text(main, (158, 428), "Teams + Telegram", 43, role="display")
    text(main, (160, 493), "Available adapters", 26, "green", "mono")
    line(main, [(586, 462), (635, 462), (635, 568), (740, 568)], "blue", 3)
    line(main, [(722, 555), (740, 568), (722, 581)], "blue", 3)
    cards.bracket(main, 768, 447, 690, 24)
    text(main, (810, 524), "Orka", 88, "pink", "display")
    text(main, (159, 904), "The gateway protocol is open to your applications and services.", 31, "muted")

    custom = layer()
    text(custom, (158, 623), "Slack + your apps", 43, role="display")
    text(custom, (160, 688), "Custom gateways", 26, "muted", "mono")
    cards.dashed(custom, (586, 657), (635, 657))
    cards.dashed(custom, (635, 657), (635, 588))

    native = layer()
    line(native, [(1015, 568), (1104, 568), (1104, 462), (1135, 462)], "blue", 3)
    line(native, [(1117, 449), (1135, 462), (1117, 475)], "blue", 3)
    text(native, (1174, 424), "Native agents", 45, role="display")
    text(native, (1177, 490), "Running in Kubernetes", 33)
    text(native, (1177, 548), "Optionally with Agent Sandbox", 30, "muted")
    text(native, (1177, 593), "and Agent Substrate", 30, "muted")

    external = layer()
    line(external, [(1104, 568), (1104, 730), (1135, 730)], "blue", 3)
    line(external, [(1117, 717), (1135, 730), (1117, 743)], "blue", 3)
    text(external, (1174, 693), "Local + Foundry agents", 42, role="display")
    text(external, (1177, 758), "Compatible integrations", 26, "muted", "mono")
    return [(main, 0, 0.35), (custom, 1.7, 0.4),
            (native, 3.5, 0.4), (external, 9.2, 0.4)]


def outro(mark):
    main = layer()
    text(main, (134, 99), "Orka", 73, "pink", "display")
    text(main, (134, 243), "One example.", 90, role="display")
    text(main, (134, 358), "Many ways to work.", 90, role="display")
    text(main, (140, 541), "Agents  ·  Workflows  ·  Tools  ·  Skills", 37, "muted")
    text(main, (140, 603), "Reviewed memory  ·  Permissions  ·  Token visibility", 34, "muted")
    cards.paste_mascot(main, mark, 1410, 185, 420)
    line(main, [(140, 737), (1780, 737)])
    text(main, (140, 800), "https://orka-agents.github.io/orka/", 43, "blue", "mono")
    text(main, (141, 895), "Explore the docs, examples, and integrations.", 34)
    return [(main, 0, 0.35)]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scene", action="append", choices=["00-platform", "00-scope", "08-gateways", "09-outro"])
    args = parser.parse_args()
    output = ROOT / "bin/hackathon-platform-revised/media"
    output.mkdir(parents=True, exist_ok=True)
    mark = cards.mascot(Path.home() / "projects/orka/website/static/img/orka-logo.png")
    timing = json.loads((HERE / "timing.json").read_text())
    frames = {scene["id"]: scene["frames"] for scene in timing["scenes"]}
    scenes = {"00-platform": platform, "00-scope": scope,
              "08-gateways": gateways, "09-outro": outro}
    for scene_id, draw in scenes.items():
        if not args.scene or scene_id in args.scene:
            cards.render(scene_id, draw(mark), frames[scene_id], output)


if __name__ == "__main__":
    main()
