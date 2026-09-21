#!/usr/bin/env python3
"""Render the feature overview and workflow cards using the retained demo style."""

import argparse
import importlib.util
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
OUTPUT = ROOT / "bin/hackathon-platform-overview/media"
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
    text(main, (132, 83), "Orka", 96, "pink", "display")
    text(main, (134, 218), "Your agents, working together.", 76, role="display")
    text(main, (137, 324), "Run and govern AI agents across teams.", 38, "muted")
    cards.paste_mascot(main, mark, 1542, 48, 185)
    line(main, [(136, 411), (1784, 411)])
    groups = [
        (3.8, 474, [
            ("Bring your agents", "Native and compatible external agents"),
            ("Coordinate work", "Schedules, events, and team workflows"),
        ]),
        (7.4, 644, [
            ("Choose tools and skills", "Reuse capabilities across workflows"),
            ("Control access", "Permissions for each team and agent"),
        ]),
        (11.2, 814, [
            ("Share reviewed knowledge", "Project memory with human review"),
            ("Track results and usage", "Task outcomes and reported tokens"),
        ]),
    ]
    layers = [(main, 0, 0.3)]
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
    text(main, (136, 102), "ONE EXAMPLE / SECURITY REVIEW", 29, "blue", "mono")
    text(main, (131, 226), "Two teams. One workflow.", 87, role="display")
    text(main, (137, 354), "A scheduled source-code scan. A fix requested from Teams.", 41, "muted")
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
        (366, "Microsoft Teams", "Gateway adapter"),
        (564, "Microsoft Scout", "Adapter for compatible Scout builds"),
        (674, "Telegram", "Gateway adapter"),
        (784, "Slack", "Custom gateway"),
        (894, "Bring-your-own gateway", "Your apps and services"),
    ]
    for y, name, detail in gateways:
        text(inputs, (140, y), name, 43, role="display")
        text(inputs, (143, y + 57), detail, 26, "muted")
        line(inputs, [(670, y + 25), (738, y + 25)], "blue", 2)
    line(inputs, [(738, 391), (738, 919)], "blue", 2)
    arrow(inputs, [(738, 590), (778, 590)])

    upcoming = layer()
    text(upcoming, (143, 466), "Coming soon: multiplayer in Teams", 26, "pink")
    text(upcoming, (143, 506), "Work with Orka together in shared chats.", 25, "muted")

    native = layer()
    arrow(native, [(1050, 590), (1127, 590), (1127, 405), (1159, 405)])
    text(native, (1180, 377), "Native agents", 45, role="display")
    text(native, (1183, 436), "Running in Kubernetes", 32)
    text(native, (1183, 487), "Optional: Agent Sandbox", 27, "muted")
    text(native, (1183, 529), "and Agent Substrate", 27, "muted")

    external = layer()
    agent_rows = [
        (600, "Local agents", "Running in containers on your laptop"),
        (733, "Foundry-hosted agents", "Running in Microsoft Foundry"),
        (866, "Bring-your-own agent", "Connect a compatible runtime"),
    ]
    line(external, [(1127, 590), (1127, 891)], "blue", 2)
    for y, name, detail in agent_rows:
        arrow(external, [(1127, y + 25), (1159, y + 25)])
        text(external, (1180, y), name, 43, role="display")
        text(external, (1183, y + 58), detail, 29, "muted")
    return [(main, 0, 0.3), (inputs, 0.4, 0.3), (upcoming, 5.5, 0.4),
            (native, 10.6, 0.4), (external, 14.8, 0.4)]


def outro(mark):
    main = layer()
    text(main, (134, 99), "Orka", 73, "pink", "display")
    text(main, (134, 243), "One example.", 90, role="display")
    text(main, (134, 358), "Many ways to work.", 90, role="display")
    text(main, (140, 541), "Bring your agents. Connect your channels.", 41)
    text(main, (140, 610), "Build workflows for your organization.", 38, "muted")
    cards.paste_mascot(main, mark, 1410, 185, 420)
    line(main, [(140, 777), (1780, 777)])
    text(main, (140, 840), "https://orka-agents.github.io/orka/", 42, "blue", "mono")
    text(main, (141, 927), "Docs, examples, and integrations", 31)
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
