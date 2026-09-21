#!/usr/bin/env python3
"""Check decoded Resolve exports, narration placement, and scene sample frames."""

import argparse
import hashlib
import json
import math
from pathlib import Path
import subprocess
import wave

import numpy as np
from scipy.signal import correlate, resample_poly

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / "bin/narrated-demos"
RATE, FPS = 8000, 30


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("demo")
    args = parser.parse_args()
    prepared = json.loads((OUTPUT / "exports" / (args.demo + ".report.json")).read_text())
    manifest = prepared["manifest"]
    video = Path(prepared["video_export"])
    check_dir = OUTPUT / "export-qa" / args.demo
    check_dir.mkdir(parents=True, exist_ok=True)
    # Decode the entire video and audio so a valid header cannot hide a broken tail.
    subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-xerror", "-nostdin",
                    "-i", str(video), "-f", "null", "-"], check=True, capture_output=True)
    channels = next(stream["channels"] for stream in prepared["export_probe"]["streams"]
                    if stream["codec_type"] == "audio")
    decoded = subprocess.run([
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-i", str(video),
        "-vn", "-ar", "48000", "-f", "f32le", "-",
    ], capture_output=True, check=True).stdout
    native = np.frombuffer(decoded, dtype="<f4").reshape(-1, channels)
    peak = float(np.max(np.abs(native)))
    require(len(native) > 0 and peak > 0.01, "The export has no audible narration")
    require(peak < 1, "The export audio clips at full scale")
    # Average channels explicitly. FFmpeg's default stereo-to-mono matrix
    # adds 3 dB to identical left/right narration and can falsely report clipping.
    rendered = resample_poly(native.mean(axis=1).astype(np.float64), 1, 6)
    clips = []
    for clip in manifest["audio"]:
        with wave.open(clip["path"], "rb") as source:
            require((source.getframerate(), source.getnchannels(), source.getsampwidth()) == (48000, 1, 2),
                    "Unexpected narration format")
            expected = np.frombuffer(source.readframes(source.getnframes()), dtype="<i2").astype(np.float64) / 32768
        expected = resample_poly(expected, 1, 6)
        start = round(clip["record_frame"] / FPS * RATE)
        margin = round(0.1 * RATE)
        window = rendered[start - margin:start + len(expected) + margin]
        require(len(window) >= len(expected), "Narration is missing from the end of the export")
        scores = correlate(window, expected, mode="valid", method="fft")
        best = int(np.argmax(scores))
        actual = window[best:best + len(expected)]
        similarity = float(np.dot(actual, expected) / math.sqrt(np.dot(actual, actual) * np.dot(expected, expected)))
        shift = (best - margin) / RATE
        require(similarity > 0.95, "Export narration differs from source: " + clip["path"])
        require(abs(shift) <= 1 / FPS, "Export narration is out of position: " + clip["path"])
        clips.append({"path": clip["path"], "correlation": round(similarity, 6), "shift_seconds": shift})
    frames = []
    for index, scene in enumerate(manifest["scenes"]):
        for label, local in (("middle", scene["frames"] / FPS / 2), ("end", scene["frames"] / FPS - 0.5)):
            path = check_dir / (f"{index:02d}-" + scene["id"] + "-" + label + ".jpg")
            time = scene["record_frame"] / FPS + local
            subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
                            "-ss", str(time), "-i", str(video), "-frames:v", "1", str(path)],
                           capture_output=True, check=True)
            frames.append({"time": time, "path": str(path)})
    report = {"status": "verified", "video": str(video), "video_sha256": digest(video),
              "project": prepared["project_export"], "project_sha256": digest(prepared["project_export"]),
              "full_decode": "passed", "audio_peak_dbfs": 20 * math.log10(peak),
              "audio_clips": clips, "sample_frames": frames,
              "visual_review": "Sample frames are ready for visual inspection."}
    (check_dir / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"demo": args.demo, "audio_clips": len(clips), "sample_frames": len(frames),
                      "lowest_audio_correlation": min(clip["correlation"] for clip in clips),
                      "largest_audio_shift": max(abs(clip["shift_seconds"]) for clip in clips)}))


if __name__ == "__main__":
    main()
