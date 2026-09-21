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


def scenario(mark):
    main = layer()
    text(main, (132, 116), "THE SCENARIO", 30, "blue", "mono")
    cards.bracket(main, 111, 311, 673)
    text(main, (176, 328), "A web app needs", 98, role="display")
    text(main, (176, 460), "a security review.", 98, "pink", "display")
    text(main, (178, 762), "An intentionally vulnerable demo application", 34, "muted")
    cards.paste_mascot(main, mark, 1360, 254, 475)
    return [(main, 0, 0.3)]


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
    text(main, (171, 467), "Security", 76, "blue", "display")
    text(main, (1110, 467), "Engineering", 76, "green", "display")
    line(main, [(588, 511), (1011, 511)], "rule", 3)
    line(main, [(991, 496), (1011, 511), (991, 526)], "green", 3)
    text(main, (174, 584), "Find source-code flaws", 36, "muted")
    text(main, (1113, 584), "Prepare a fix for review", 36, "muted")
    text(main, (174, 846), "Recorded workflow. Edited for time.", 31, "muted", "mono")
    return [(main, 0, 0.35)]


def gateways(mark):
    main = layer()
    text(main, (132, 113), "Your agents. Your channels.", 78, role="display")
    text(main, (136, 226), "Connect through compatible, extensible integrations.", 37, "muted")
    text(main, (158, 447), "Local agents", 45, role="display")
    text(main, (158, 528), "Foundry agents", 45, role="display")
    text(main, (160, 638), "Compatible agent contracts", 25, "muted", "mono")
    line(main, [(555, 532), (722, 532)], "blue", 3)
    cards.bracket(main, 729, 402, 674, 24)
    text(main, (776, 474), "Orka", 88, "pink", "display")
    line(main, [(1024, 532), (1120, 532)], "blue", 3)
    existing = layer()
    line(existing, [(1120, 432), (1120, 532)], "blue", 3)
    line(existing, [(1120, 432), (1190, 432)], "blue", 3)
    text(existing, (1224, 378), "Teams + Telegram", 45, role="display")
    text(existing, (1226, 449), "Available adapters", 26, "green", "mono")
    custom = layer()
    cards.dashed(custom, (1120, 552), (1120, 710))
    cards.dashed(custom, (1120, 710), (1190, 710))
    text(custom, (1224, 611), "Slack", 45, role="display")
    text(custom, (1224, 680), "Your own integration", 42, role="display")
    text(custom, (1226, 752), "Build a custom gateway", 26, "muted", "mono")
    text(custom, (159, 904), "The gateway protocol is open to your applications and services.", 31, "muted")
    return [(main, 0, 0.35), (existing, 2.8, 0.4), (custom, 5.6, 0.4)]


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
    parser.add_argument("--scene", action="append", choices=["00-scenario", "00-platform", "00-scope", "08-gateways", "09-outro"])
    args = parser.parse_args()
    output = ROOT / "bin/hackathon-platform/media"
    output.mkdir(parents=True, exist_ok=True)
    mark = cards.mascot(Path.home() / "projects/orka/website/static/img/orka-logo.png")
    timing = json.loads((HERE / "timing.json").read_text())
    frames = {scene["id"]: scene["frames"] for scene in timing["scenes"]}
    scenes = {"00-scenario": scenario, "00-platform": platform, "00-scope": scope,
              "08-gateways": gateways, "09-outro": outro}
    for scene_id, draw in scenes.items():
        if not args.scene or scene_id in args.scene:
            cards.render(scene_id, draw(mark), frames[scene_id], output)


if __name__ == "__main__":
    main()
