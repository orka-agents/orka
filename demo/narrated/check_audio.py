#!/usr/bin/env python3
"""Locally transcribe generated narration and flag content mismatches for review."""

import argparse
import contextlib
import hashlib
import io
import json
from pathlib import Path
import re

import mlx_whisper

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / "bin/narrated-demos"
MODEL = "mlx-community/whisper-small.en-mlx"


def tokens(text):
    text = text.lower().replace("orca", "orka").replace("g visor", "gvisor")
    text = re.sub(r"\ba[ -]two[ -]a\b", "a2a", text)
    text = text.replace("four oh four", "404").replace("four o four", "404")
    text = text.replace("twenty four", "24").replace("thirty two", "32")
    numbers = {"zero": "0", "one": "1", "two": "2", "three": "3", "six": "6",
               "eighteen": "18", "twenty": "20"}
    text = text.replace("work-order", "work order").replace("order-desk", "order desk")
    text = re.sub(r"\b(?:" + "|".join(numbers) + r")\b", lambda match: numbers[match[0]], text)
    return re.findall(r"[a-z0-9]+", text)


def edit_distance(first, second):
    previous = list(range(len(second) + 1))
    for row, a in enumerate(first, 1):
        current = [row]
        for column, b in enumerate(second, 1):
            current.append(min(current[-1] + 1, previous[column] + 1,
                               previous[column - 1] + (a != b)))
        previous = current
    return previous[-1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--demo", action="append")
    args = parser.parse_args()
    manifest = json.loads((OUTPUT / "narration.json").read_text())
    qa = OUTPUT / "audio-qa"
    qa.mkdir(exist_ok=True)
    reports = []
    pending = []
    for voice in manifest["scenes"]:
        name = voice["id"]
        if args.demo and not any(name.startswith(demo + "-") for demo in args.demo):
            continue
        path = OUTPUT / "audio" / (name + ".wav")
        metadata_path = OUTPUT / "audio" / (name + "-metadata.json")
        if not metadata_path.exists():
            pending.append(name)
            continue
        metadata = json.loads(metadata_path.read_text())
        if metadata["text"] != voice["text"]:
            raise ValueError("Final audio metadata does not match the current narration: " + name)
        sha = hashlib.sha256(path.read_bytes()).hexdigest()
        if sha != metadata["final"]["sha256"]:
            raise ValueError("Audio does not match its generation record: " + name)
        destination = qa / (name + ".json")
        old = json.loads(destination.read_text()) if destination.exists() else {}
        if old.get("audio_sha256") == sha and old.get("expected") == voice["text"]:
            result = old["transcription"]
        else:
            # This runs on the local Mac. The audio is not uploaded.
            with contextlib.redirect_stderr(io.StringIO()):
                result = mlx_whisper.transcribe(
                    str(path), path_or_hf_repo=MODEL, language="en", verbose=None,
                    temperature=0, condition_on_previous_text=False, word_timestamps=True,
                    initial_prompt="Orka. Fibey. gVisor. Agent Substrate. Kubernetes. Task. Session.",
                )
        expected, actual = tokens(voice["text"]), tokens(result["text"])
        error_rate = edit_distance(expected, actual) / max(1, len(expected))
        ending_distance = edit_distance(expected[-7:], actual[-7:])
        flags = []
        if error_rate > 0.14:
            flags.append("transcription_differs")
        if ending_distance > 2:
            flags.append("check_ending")
        if metadata["near_frame_cap"]:
            flags.append("frame_cap")
        report = {"id": name, "audio_sha256": sha, "expected": voice["text"],
                  "recognized": result["text"].strip(), "word_error_rate": round(error_rate, 4),
                  "flags": flags, "transcription": result,
                  "review": old.get("review") if old.get("audio_sha256") == sha else None}
        destination.write_text(json.dumps(report, indent=2) + "\n")
        reports.append({key: value for key, value in report.items() if key != "transcription"})
        print(json.dumps({"id": name, "word_error_rate": round(error_rate, 4), "flags": flags}), flush=True)
    summary = {"method": "Local speech recognition plus generated-audio integrity checks",
               "model": MODEL, "clips_checked": len(reports), "pending": pending, "clips": reports}
    label = "-".join(args.demo) if args.demo else "all"
    (qa / (label + "-summary.json")).write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps({"checked": len(reports), "pending": len(pending),
                      "flagged": sum(bool(report["flags"]) for report in reports)}), flush=True)


if __name__ == "__main__":
    main()
