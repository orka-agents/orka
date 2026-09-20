#!/usr/bin/env python3
"""Render real recorded casts into fixed-duration silent Resolve clips."""

import argparse
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[3]
OUTPUT = ROOT / "bin/hackathon-first-pass/terminal"
SCENES = {"01-schedule": 420, "02-discovery": 510, "04-handoff": 210,
          "05-patch": 390, "06-delivery": 420, "07-usage": 210}


def probe(path):
    return json.loads(subprocess.check_output([
        "ffprobe", "-v", "error", "-show_entries",
        "format=duration:stream=codec_name,width,height,pix_fmt,r_frame_rate,nb_frames",
        "-of", "json", str(path)], text=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("scenes", nargs="*", choices=list(SCENES), default=["01-schedule", "02-discovery"])
    args = parser.parse_args()
    for scene in args.scenes:
        cast = OUTPUT / f"{scene}.cast"
        gif = OUTPUT / f"{scene}.gif"
        output = OUTPUT / f"{scene}.mp4"
        if not cast.exists():
            raise RuntimeError(f"Capture this scene first: {scene}")
        subprocess.run([
            "agg", str(cast), str(gif), "--cols", "100", "--rows", "28",
            "--font-size", "24", "--font-family", "SF Mono", "--theme", "monokai",
            "--speed", "1", "--idle-time-limit", "2", "--last-frame-duration", "1.2",
            "--select", "marker:0..", "--fps-cap", "30", "--no-loop", "--quiet",
        ], check=True)
        duration = float(probe(gif)["format"]["duration"])
        target = SCENES[scene] / 30
        # Only timing changes. All displayed output remains the recorded data.
        factor = min(1.0, (target - 0.25) / duration)
        filters = (
            f"setpts={factor:.9f}*(PTS-STARTPTS),"
            "scale=1728:960:force_original_aspect_ratio=decrease:flags=lanczos,"
            "pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color=0x272822,"
            "fps=30,tpad=stop_mode=clone:stop_duration=120"
        )
        subprocess.run([
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
            "-ignore_loop", "1", "-i", str(gif), "-vf", filters,
            "-an", "-c:v", "libx264", "-crf", "17", "-preset", "medium",
            "-pix_fmt", "yuv420p", "-r", "30", "-frames:v", str(SCENES[scene]),
            "-movflags", "+faststart", str(output),
        ], check=True)
        metadata = probe(output)
        metadata.update({"scene": scene, "path": str(output), "source_gif_seconds": duration,
                         "playback_speed": round(1 / factor, 6),
                         "selection": "First chapter marker through the end; final frame held to the scene budget."})
        (OUTPUT / f"{scene}-render.json").write_text(json.dumps(metadata, indent=2) + "\n")
        subprocess.run([
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
            "-ss", str(target - 0.5), "-i", str(output), "-frames:v", "1",
            str(OUTPUT / f"{scene}-frame.png"),
        ], check=True)
        print(json.dumps(metadata), flush=True)


if __name__ == "__main__":
    main()
