#!/usr/bin/env python3
"""Generate one complete Qwen narration performance per standalone demo."""

import argparse
import datetime
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import time
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
STORYBOARD = Path(__file__).with_name("storyboard.json")
GENERATOR = ROOT / "demo/06-hackathon/audio/generate_voiceover.py"
spec = importlib.util.spec_from_file_location("voice_generator", GENERATOR)
generator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(generator)


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")


def voices(demo):
    yield {"id": demo["id"] + "-intro", "text": demo["intro"]}
    for index, chapter in enumerate(demo["chapters"], 1):
        yield {"id": f"{demo['id']}-chapter-{index:02d}", "text": chapter["text"]}
    yield {"id": demo["id"] + "-outro", "text": demo["outro"]}


def make_plan(demo):
    scenes = list(voices(demo))
    return {"demo_id": demo["id"], "scenes": scenes,
            "text": "\n\n".join(scene["text"] for scene in scenes)}


def checked_identity(path):
    identity = json.loads(path.read_text())
    for filename, checksum in (("reference_path", "reference_sha256"),
                               ("config_path", "config_sha256")):
        if digest(identity[filename]) != identity[checksum]:
            raise ValueError("Voice service input changed: " + filename)
    if not re.fullmatch(r"(?:sha256:)?[a-f0-9]{64}", identity["image_id"]):
        raise ValueError("An exact TTS image identity is required")
    return identity


def generate(plan, args, identity):
    directory = args.output_dir / plan["demo_id"]
    directory.mkdir(parents=True, exist_ok=True)
    raw, final = directory / "narration-raw.wav", directory / "narration.wav"
    request_path = directory / "request.json"
    metadata_path = directory / "metadata.json"
    payload = {"model": "qwen3-tts", "input": plan["text"],
               "voice": "/models/reference.wav", "language": "en",
               "response_format": "wav", "params": {"max_frames": str(args.max_frames)}}
    inputs = {"request": payload, "voice_identity": identity,
              "storyboard_sha256": digest(STORYBOARD)}
    if metadata_path.exists():
        old = json.loads(metadata_path.read_text())
        if old.get("inputs") != inputs:
            raise ValueError("Different take exists; choose a new output directory: " + str(directory))
        for name, path in (("raw", raw), ("final", final)):
            if digest(path) != old[name]["sha256"]:
                raise ValueError("Accepted audio changed: " + str(path))
        print(json.dumps({"demo": plan["demo_id"], "reused": True}), flush=True)
        return old
    if raw.exists() or request_path.exists():
        raise ValueError("An unfinished take exists; inspect it before another request: " + str(directory))
    write_json(directory / "plan.json", plan)
    write_json(request_path, payload)
    write_json(directory / "inputs.json", inputs)
    start_utc = datetime.datetime.now(datetime.timezone.utc).isoformat()
    write_json(directory / "request-state.json", {"status": "started", "at": start_utc})
    print(json.dumps({"demo": plan["demo_id"], "status": "generating one complete performance",
                      "words": len(plan["text"].split()), "max_frames": args.max_frames}), flush=True)
    request = urllib.request.Request(args.endpoint, data=json.dumps(payload).encode(),
                                     headers={"Content-Type": "application/json"}, method="POST")
    started = time.monotonic()
    try:
        with urllib.request.urlopen(request, timeout=args.timeout) as response:
            data = response.read()
            if response.status != 200 or not data.startswith(b"RIFF"):
                raise ValueError("TTS returned no WAV")
            response_type = response.headers.get("Content-Type")
        raw.write_bytes(data)
        raw_metrics = generator.audio_metrics(raw)
        if raw_metrics["duration_seconds"] <= 0 or raw_metrics["rms_dbfs"] is None:
            raise ValueError("TTS returned empty or silent audio")
        loudness = generator.normalize(raw, final)
        metrics = generator.audio_metrics(final)
        near_cap = raw_metrics["duration_seconds"] >= args.max_frames / 12.5 - 0.16
        metadata = {"version": 1, "demo_id": plan["demo_id"], "mode": "one request per video",
                    "text": plan["text"], "inputs": inputs,
                    "generation": {"started_utc": start_utc, "request_seconds": round(time.monotonic() - started, 3),
                                   "http_status": 200, "content_type": response_type},
                    "max_frames": args.max_frames, "near_frame_cap": near_cap,
                    "raw": {"path": str(raw.resolve()), **raw_metrics},
                    "final": {"path": str(final.resolve()), **metrics}, "loudness": loudness}
        write_json(metadata_path, metadata)
        write_json(directory / "request-state.json", {"status": "generated; content verification pending"})
        print(json.dumps({"demo": plan["demo_id"], "seconds": metrics["duration_seconds"],
                          "request_seconds": metadata["generation"]["request_seconds"],
                          "near_frame_cap": near_cap}), flush=True)
        return metadata
    except Exception as error:
        write_json(directory / "request-state.json", {"status": "failed; inspect before retrying",
                                                       "error_type": type(error).__name__})
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["plan", "generate"])
    parser.add_argument("demos", nargs="*")
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--identity", type=Path)
    parser.add_argument("--endpoint", default="http://127.0.0.1:18080/v1/audio/speech")
    parser.add_argument("--timeout", type=float, default=3600)
    parser.add_argument("--max-frames", type=int, default=6000)
    args = parser.parse_args()
    all_demos = json.loads(STORYBOARD.read_text())["demos"]
    selected = [demo for demo in all_demos if not args.demos or demo["id"] in args.demos]
    if not selected or set(args.demos) - {demo["id"] for demo in selected}:
        parser.error("Unknown demo selection")
    if not 1 <= args.max_frames <= 8192 or args.timeout <= 0:
        parser.error("Invalid generation budget or timeout")
    plans = [make_plan(demo) for demo in selected]
    if args.action == "plan":
        for plan in plans:
            write_json(args.output_dir / plan["demo_id"] / "plan.json", plan)
        print(json.dumps({"demos": len(plans), "mode": "one complete performance per demo"}))
        return
    if args.identity is None:
        parser.error("--identity is required for generation")
    identity = checked_identity(args.identity)
    for plan in plans:
        generate(plan, args, identity)


if __name__ == "__main__":
    main()
