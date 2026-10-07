#!/usr/bin/env python3
"""Even out the typing animation in an asciicast v3 recording.

demo/lib/demo.sh types each command one character at a time, sleeping between
characters. On a busy machine those sleeps stretch unevenly and the recording
stutters mid-word. This gives every keystroke after a prompt its command's
median interval. Only the pauses between typed characters change; what the
commands print, and every other interval, is left as recorded.

usage: typing.py <cast-file>   (rewritten in place)
"""

import json
import statistics
import sys

# p() in demo/lib/demo.sh: C_PROMPT, "$", C_CMD, then a space.
PROMPT = "\x1b[38;5;75m$\x1b[38;5;250m "


def is_keystroke(event):
    # A slow reader can catch two or three keystrokes in one event.
    return event[1] == "o" and 0 < len(event[2]) <= 3 and event[2].isprintable()


def rewrite(events):
    commands = 0
    i = 0
    while i < len(events):
        if events[i][1] != "o" or not events[i][2].endswith(PROMPT):
            i += 1
            continue
        j = i + 1
        while j < len(events) and is_keystroke(events[j]):
            j += 1
        typed = events[i + 1 : j]
        if len(typed) > 1:
            step = statistics.median(event[0] / len(event[2]) for event in typed[1:])
            for event in typed:
                event[0] = round(step * len(event[2]), 6)
            commands += 1
        i = j
    return commands


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: typing.py <cast-file>")
    path = sys.argv[1]
    with open(path, encoding="utf-8") as handle:
        lines = handle.readlines()
    if not lines:
        raise SystemExit(f"empty recording: {path}")
    header = json.loads(lines[0])
    if header.get("version") != 3:
        raise SystemExit(f"expected an asciicast v3 recording, got version {header.get('version')!r}")

    events = [json.loads(line) for line in lines[1:] if line.strip()]
    commands = rewrite(events)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(lines[0])
        handle.writelines(json.dumps(event) + "\n" for event in events)
    print(f"{path}: evened typing in {commands} command(s)")


if __name__ == "__main__":
    main()
