#!/usr/bin/env python3
"""Generate scene audio with the local AIKit Qwen3-TTS container."""

import argparse
import array
import datetime
import hashlib
import json
import math
from pathlib import Path
import re
import subprocess
import time
import urllib.request
import wave


def audio_metrics(path):
    with wave.open(str(path), "rb") as audio:
        sample_rate = audio.getframerate()
        channels = audio.getnchannels()
        frames = audio.getnframes()
        width = audio.getsampwidth()
        if width != 2:
            raise ValueError(f"Expected PCM16 audio, received {width}-byte samples")
        samples = array.array("h", audio.readframes(frames))
    peak = max(abs(value) for value in samples) / 32768
    rms = math.sqrt(sum(value * value for value in samples) / len(samples)) / 32768
    return {
        "duration_seconds": round(frames / sample_rate, 6),
        "sample_rate": sample_rate,
        "channels": channels,
        "sample_width_bytes": width,
        "rms_dbfs": round(20 * math.log10(rms), 2) if rms else None,
        "peak_dbfs": round(20 * math.log10(peak), 2) if peak else None,
        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
    }


def normalize(raw, output):
    target = "loudnorm=I=-16:TP=-1.5:LRA=7"
    measured = subprocess.run(
        ["ffmpeg", "-hide_banner", "-nostdin", "-i", str(raw), "-af",
         f"{target}:print_format=json", "-f", "null", "-"],
        capture_output=True, text=True, check=True,
    )
    match = re.search(r'\{\s*"input_i".*?\}', measured.stderr, re.S)
    if not match:
        raise ValueError("FFmpeg returned no loudness measurements")
    levels = json.loads(match.group(0))
    measured_filter = (
        f"{target}:measured_I={levels['input_i']}"
        f":measured_TP={levels['input_tp']}"
        f":measured_LRA={levels['input_lra']}"
        f":measured_thresh={levels['input_thresh']}"
        f":offset={levels['target_offset']}:linear=true:print_format=json"
    )
    result = subprocess.run(
        ["ffmpeg", "-hide_banner", "-nostdin", "-y", "-i", str(raw), "-af",
         measured_filter, "-ar", "48000", "-ac", "1", "-c:a", "pcm_s16le", str(output)],
        capture_output=True, text=True, check=True,
    )
    match = re.search(r'\{\s*"input_i".*?\}', result.stderr, re.S)
    return json.loads(match.group(0)) if match else levels


def generate(scene, args, output_dir):
    scene_id = scene["id"]
    if not re.fullmatch(r"[a-z0-9-]+", scene_id):
        raise ValueError(f"Unsafe scene ID: {scene_id}")
    words = len(scene["text"].split())
    max_frames = scene.get("max_frames", min(512, max(160, math.ceil((words / 1.4 + 3) * 12.5))))
    payload = {
        "model": "qwen3-tts",
        "input": scene["text"],
        "voice": args.voice,
        "language": "en",
        "response_format": "wav",
        "params": {"max_frames": str(max_frames)},
    }
    raw = output_dir / f"{scene_id}-raw.wav"
    final = output_dir / f"{scene_id}.wav"
    request_path = output_dir / f"{scene_id}-request.json"
    metadata_path = output_dir / f"{scene_id}-metadata.json"
    reusable = raw.exists() and request_path.exists() and json.loads(request_path.read_text()) == payload
    previous = json.loads(metadata_path.read_text()) if metadata_path.exists() else {}
    generation = previous.get("generation", {})
    if args.force or not reusable:
        encoded = json.dumps(payload).encode()
        request = urllib.request.Request(
            args.endpoint, data=encoded,
            headers={"Content-Type": "application/json"}, method="POST",
        )
        started = time.monotonic()
        with urllib.request.urlopen(request, timeout=300) as response:
            data = response.read()
            if response.status != 200 or not data.startswith(b"RIFF"):
                raise ValueError(f"TTS did not return a WAV for {scene_id}")
            generation = {
                "generated_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                "request_seconds": round(time.monotonic() - started, 3),
                "http_status": response.status,
                "content_type": response.headers.get("Content-Type"),
            }
        raw.write_bytes(data)
        request_path.write_text(json.dumps(payload, indent=2) + "\n")
    raw_metrics = audio_metrics(raw)
    near_cap = raw_metrics["duration_seconds"] >= max_frames / 12.5 - 0.16
    loudness = normalize(raw, final)
    metrics = audio_metrics(final)
    metadata = {
        "id": scene_id,
        "text": scene["text"],
        "word_count": words,
        "generation": generation,
        "max_frames": max_frames,
        "near_frame_cap": near_cap,
        "raw": {"path": str(raw), **raw_metrics},
        "final": {"path": str(final), **metrics},
        "loudness": loudness,
        "recommended_frames_30fps": math.ceil((metrics["duration_seconds"] + 0.6) * 30),
    }
    metadata_path.write_text(json.dumps(metadata, indent=2) + "\n")
    print(json.dumps({"id": scene_id, "seconds": metrics["duration_seconds"],
                      "request_seconds": generation.get("request_seconds"),
                      "near_frame_cap": near_cap}), flush=True)
    return metadata


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, default=Path(__file__).with_name("narration.json"))
    parser.add_argument("--output-dir", type=Path,
                        default=Path(__file__).resolve().parents[3] / "bin/hackathon-first-pass/audio")
    parser.add_argument("--endpoint", default="http://127.0.0.1:18080/v1/audio/speech")
    parser.add_argument("--voice", default="/models/reference.wav")
    parser.add_argument("--scene", action="append", help="Only generate this scene ID; may be repeated")
    parser.add_argument("--force", action="store_true", help="Regenerate matching cached speech")
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)
    scenes = json.loads(args.manifest.read_text())["scenes"]
    selected = [scene for scene in scenes if not args.scene or scene["id"] in args.scene]
    if not selected:
        parser.error("No scene IDs matched")
    results = [generate(scene, args, args.output_dir) for scene in selected]
    report = {
        "status": "Draft narration pending verification against the recorded workflow.",
        "endpoint": args.endpoint,
        "sample_rate": 48000,
        "format": "PCM16 mono WAV",
        "speech_seconds": round(sum(scene["final"]["duration_seconds"] for scene in results), 6),
        "recommended_frames_30fps": sum(scene["recommended_frames_30fps"] for scene in results),
        "scenes": results,
    }
    (args.output_dir / "generation-report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({key: value for key, value in report.items() if key != "scenes"}), flush=True)


if __name__ == "__main__":
    main()
