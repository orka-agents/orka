#!/usr/bin/env python3
"""Frame a fresh recording of the retained Orka Bot conversation for Resolve."""

import hashlib
import json
from pathlib import Path
import subprocess

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / "bin/hackathon-platform/teams"
SOURCE = OUTPUT / "orka-bot-second-cut.mov"
WIDTH, HEIGHT, FPS = 1920, 1080, 30
VIEWS = [
    {"id": "03-overview", "source_start": 2, "frames": 120,
     "crop": [648, 752, 2080, 760], "placement": [80, 296, 1760, 644],
     "title": "Ask Orka what needs attention"},
    {"id": "03-detail", "source_start": 4, "frames": 240,
     "crop": [648, 932, 1120, 584], "placement": [180, 226, 1560, 814],
     "title": "Review the saved finding"},
    {"id": "04-request", "source_start": 9, "frames": 90,
     "crop": [1856, 1502, 904, 182], "placement": [100, 426, 1720, 346],
     "title": "Choose the fix"},
]


def probe(path):
    return json.loads(subprocess.check_output([
        "ffprobe", "-v", "error", "-show_entries",
        "format=duration:stream=codec_type,codec_name,width,height,r_frame_rate,avg_frame_rate,nb_frames",
        "-of", "json", str(path)], text=True))


def check_output(path, frames):
    metadata = probe(path)
    streams = metadata["streams"]
    if len(streams) != 1 or streams[0]["codec_type"] != "video":
        raise RuntimeError("Teams clips must contain video only")
    video = streams[0]
    if ((video["width"], video["height"]) != (WIDTH, HEIGHT)
            or video["r_frame_rate"] != "30/1" or video["avg_frame_rate"] != "30/1"
            or int(video["nb_frames"]) != frames
            or abs(float(metadata["format"]["duration"]) - frames / FPS) > 0.001):
        raise RuntimeError("Teams clip does not match the edit")
    return metadata


def main():
    source_probe = probe(SOURCE)
    source_video = next(s for s in source_probe["streams"] if s["codec_type"] == "video")
    fonts = {
        "label": ImageFont.truetype("/Library/Fonts/SF-Mono-Regular.otf", 28),
        "title": ImageFont.truetype("/Library/Fonts/SF-Pro-Display-Semibold.otf", 52),
    }
    views = []
    for view in VIEWS:
        x, y, w, h = view["crop"]
        ox, oy, ow, oh = view["placement"]
        source_end = view["source_start"] + view["frames"] / FPS
        if x + w > source_video["width"] or y + h > source_video["height"]:
            raise RuntimeError("Crop exceeds the actual recording")
        if source_end > float(source_probe["format"]["duration"]):
            raise RuntimeError("Recording does not cover the selected interval")
        background = Image.new("RGB", (WIDTH, HEIGHT), "#272822")
        draw = ImageDraw.Draw(background)
        draw.text((120, 62), "Microsoft Teams / Orka Bot", font=fonts["label"], fill="#5fafff", anchor="lt")
        draw.text((1778, 62), "Recorded conversation", font=fonts["label"], fill="#a5a69e", anchor="rt")
        draw.text((116, 113), view["title"], font=fonts["title"], fill="#eeeeee", anchor="lt")
        draw.line([(120, 195), (1800, 195)], fill="#5f5f87", width=2)
        draw.rectangle((ox - 1, oy - 1, ox + ow, oy + oh), outline="#5f5f87", width=1)
        background_path = OUTPUT / (view["id"] + "-background.png")
        background.save(background_path)
        output = OUTPUT / (view["id"] + ".mp4")
        filters = (
            f"[1:v]setpts=PTS-STARTPTS,fps={FPS},crop={w}:{h}:{x}:{y},"
            f"scale={ow}:{oh}:flags=lanczos,setsar=1[conversation];"
            f"[0:v][conversation]overlay={ox}:{oy}:shortest=1,format=yuv420p[video]"
        )
        command = [
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
            "-loop", "1", "-framerate", str(FPS), "-i", str(background_path),
            "-ss", str(view["source_start"]), "-i", str(SOURCE),
            "-filter_complex", filters, "-map", "[video]", "-an",
            "-c:v", "libx264", "-preset", "medium", "-crf", "17", "-pix_fmt", "yuv420p",
            "-r", str(FPS), "-frames:v", str(view["frames"]),
            "-video_track_timescale", "30000", "-movflags", "+faststart", str(output),
        ]
        subprocess.run(command, check=True)
        metadata = check_output(output, view["frames"])
        subprocess.run([
            "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
            "-ss", "0.5", "-i", str(output), "-frames:v", "1",
            str(OUTPUT / (view["id"] + ".png")),
        ], check=True)
        views.append({**view, "source_end_exclusive": source_end,
                      "path": str(output), "probe": metadata, "ffmpeg_command": command})
        print(f"Verified {view['id']}: {view['frames']} frames", flush=True)
    concat = OUTPUT / "03-teams-concat.txt"
    concat.write_text("file '03-overview.mp4'\nfile '03-detail.mp4'\n")
    conversation = OUTPUT / "03-teams.mp4"
    subprocess.run([
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
        "-f", "concat", "-safe", "0", "-i", str(concat), "-map", "0:v:0", "-an",
        "-c:v", "copy", "-video_track_timescale", "30000", "-movflags", "+faststart", str(conversation),
    ], check=True)
    combined = check_output(conversation, 360)
    metadata = {
        "source": str(SOURCE), "source_sha256": hashlib.sha256(SOURCE.read_bytes()).hexdigest(),
        "source_probe": source_probe, "views": views, "conversation_probe": combined,
        "source_fidelity": "Fresh screen recording of actual retained Orka Bot conversation history. The selected views overlap in source time because the complete conversation is already visible. App content is cropped and scaled at normal speed. No message reconstruction, synthetic typing, UI replacement, or new Teams messages. Labels outside the app crop are editorial overlays. Desktop changes after source second 13 are excluded.",
        "raw_retained": True,
    }
    (OUTPUT / "teams-edit-metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")


if __name__ == "__main__":
    main()
