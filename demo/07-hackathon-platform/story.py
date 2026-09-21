#!/usr/bin/env python3
"""Reframe retained workflow evidence with plain captions and a progress line."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess

from PIL import Image

import cards

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
OUTPUT = ROOT / "bin/hackathon-platform-overview/media"
EVIDENCE = ROOT / "bin/hackathon-platform-overview/evidence"
RETAINED = ROOT / "bin/hackathon-platform-revised"
WIDTH, HEIGHT, FPS = 1920, 1080, 30

VIEWS = [
    {"id": "01-schedule", "source": RETAINED / "terminal/01-schedule.mp4",
     "start": 4.0, "crop": [220, 245, 1320, 366], "placement": [160, 385, 1600],
     "label": "ORKA / SCHEDULED WORK", "title": "Orka starts the scheduled scan.",
     "subtitle": "Security agents begin a source-code review.",
     "note": "Future scheduled runs are paused after this scan.", "phase": 0},
    {"id": "02-discovery", "source": RETAINED / "terminal/02-discovery.mp4",
     "start": 6.0, "crop": [220, 440, 1430, 297], "placement": [136, 411, 1648],
     "label": "SECURITY AGENTS / FINDING", "title": "An image URL can become a server command.",
     "subtitle": "The finding includes evidence from the source code.",
     "note": "Source location: routes/index.js:168", "phase": 1},
    {"id": "03-teams", "source": RETAINED / "teams/03-teams.mp4",
     "start": 1.0, "crop": [80, 200, 1760, 850], "placement": [272, 266, 1376],
     "label": "YOU + ORKA BOT / MICROSOFT TEAMS", "title": "Ask what needs attention.",
     "subtitle": "Read the finding before requesting a fix.", "phase": 2},
    {"id": "04-request", "source": RETAINED / "teams/04-request.mp4",
     "start": 0.0, "crop": [95, 421, 1730, 352], "placement": [140, 410, 1640],
     "label": "YOU / THE DECISION", "title": "You request the fix.",
     "subtitle": "You choose whether Engineering proceeds.", "phase": 2},
    {"id": "04-handoff", "source": RETAINED / "terminal/04-handoff.mp4",
     "start": 1.0, "crop": [220, 205, 1000, 205], "placement": [160, 410, 1560],
     "label": "ORKA / TEAM HANDOFF", "title": "Engineering receives the finding.",
     "subtitle": "Custom demo integration. Engineering's permissions.", "phase": 3},
    {"id": "05-code", "scene": "05-patch", "source": EVIDENCE / "pr-diff.png",
     "crop": [32, 283, 1124, 330], "placement": [136, 344, 1648],
     "label": "ENGINEERING AGENT / PROPOSED FIX", "title": "The engineering agent prepares a fix.",
     "subtitle": "Actual code change in routes/index.js",
     "note": "The URL is passed as data, outside a shell command.", "phase": 3},
    {"id": "05-tests", "scene": "05-patch", "source": RETAINED / "terminal/05-patch.mp4",
     "start": 7.0, "crop": [220, 374, 1485, 228], "placement": [136, 392, 1648],
     "label": "ENGINEERING AGENT / TEST REPORT", "title": "Three focused tests passed.",
     "subtitle": "Reported by the engineering agent",
     "note": "Tests cover URL handling. Full app and database flows were not tested.", "phase": 3},
    {"id": "06-delivery", "source": EVIDENCE / "pr-overview.png",
     "crop": [56, 195, 803, 310], "placement": [133, 288, 1654],
     "label": "GITHUB / PULL REQUEST #36", "title": "The patch and test results. Ready for your review.",
     "subtitle": "Review the proposed code change before merging.", "phase": 4},
    {"id": "07-usage", "source": RETAINED / "terminal/07-usage.mp4",
     "start": 7.0, "crop": [220, 245, 1280, 325], "placement": [136, 362, 1648],
     "label": "ORKA / USAGE", "title": "See reported usage for each task.",
     "subtitle": "Example: one Microsoft Teams request",
     "note": "Missing scan measurements remain visible. Pricing is unavailable.", "phase": 4},
]


def probe(path):
    return json.loads(subprocess.check_output([
        "ffprobe", "-v", "error", "-show_entries",
        "format=duration:stream=codec_type,codec_name,width,height,r_frame_rate,avg_frame_rate,nb_frames",
        "-of", "json", str(path)], text=True))


def verify(path, frames):
    result = probe(path)
    streams = result["streams"]
    if len(streams) != 1 or streams[0]["codec_type"] != "video":
        raise ValueError(f"Expected silent video: {path}")
    stream = streams[0]
    if (stream["width"], stream["height"], stream["r_frame_rate"],
            stream["avg_frame_rate"], int(stream["nb_frames"])) != (WIDTH, HEIGHT, "30/1", "30/1", frames):
        raise ValueError(f"Output does not match the edit: {path}")
    if abs(float(result["format"]["duration"]) - frames / FPS) > 0.001:
        raise ValueError(f"Output duration does not match the edit: {path}")
    return result


def render(view, frames):
    source = view["source"]
    still = source.suffix == ".png"
    if still:
        with Image.open(source) as picture:
            source_size = picture.size
        source_seconds = None
    else:
        metadata = probe(source)
        stream = next(s for s in metadata["streams"] if s["codec_type"] == "video")
        source_size = (stream["width"], stream["height"])
        source_seconds = float(metadata["format"]["duration"])
        if not 0 <= view["start"] < source_seconds:
            raise ValueError(f"Invalid source start: {source}")
    x, y, width, height = view["crop"]
    if min(x, y) < 0 or min(width, height) <= 0 or x + width > source_size[0] or y + height > source_size[1]:
        raise ValueError(f"Crop exceeds source: {source}")
    ox, oy, ow = view["placement"]
    oh = round(height * ow / width / 2) * 2
    if ox < 0 or oy < 260 or ox + ow > WIDTH or oy + oh > 958:
        raise ValueError(f"Evidence overlaps captions or progress: {view['id']}")

    canvas = Image.new("RGBA", (WIDTH * 2, HEIGHT * 2), cards.cards.COLORS["background"])
    cards.text(canvas, (136, 65), view["label"], 27, "blue", "mono")
    title_size = 61
    while cards.cards.font("display", title_size).getlength(view["title"]) / 2 > 1648:
        title_size -= 1
    cards.text(canvas, (132, 128), view["title"], title_size, role="display")
    cards.text(canvas, (137, 217), view["subtitle"], 35, "muted")
    if view.get("note"):
        cards.text(canvas, (138, 911), view["note"], 30, "muted")
    cards.progress(canvas, view["phase"])
    background = OUTPUT / f"{view['id']}-background.png"
    canvas.resize((WIDTH, HEIGHT), Image.Resampling.LANCZOS).convert("RGB").save(background)

    path = OUTPUT / (view["id"] + ".mp4")
    command = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
               "-loop", "1", "-framerate", str(FPS), "-i", str(background)]
    if still:
        command += ["-loop", "1", "-framerate", str(FPS)]
    else:
        command += ["-ss", str(view["start"])]
    command += ["-i", str(source)]
    filters = (f"[1:v]setpts=PTS-STARTPTS,fps={FPS},crop={width}:{height}:{x}:{y},"
               f"scale={ow}:{oh}:flags=lanczos,setsar=1,tpad=stop_mode=clone:stop_duration=120[evidence];"
               f"[0:v][evidence]overlay={ox}:{oy}:shortest=1,format=yuv420p[video]")
    command += ["-filter_complex", filters, "-map", "[video]", "-an", "-c:v", "libx264",
                "-preset", "medium", "-crf", "17", "-pix_fmt", "yuv420p", "-r", str(FPS),
                "-frames:v", str(frames), "-video_track_timescale", "30000",
                "-movflags", "+faststart", str(path)]
    subprocess.run(command, check=True)
    result = verify(path, frames)
    subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
                    "-ss", str(frames / FPS - 0.5), "-i", str(path), "-frames:v", "1",
                    str(OUTPUT / (view["id"] + ".png"))], check=True)
    result.update({"view": {**view, "source": str(source)}, "frames": frames,
                   "source_sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
                   "source_duration_seconds": source_seconds, "ffmpeg_command": command,
                   "fidelity": "Existing video plays at its retained speed, with a final-frame hold when needed. GitHub sources are real browser screenshots held as stills. Evidence pixels are cropped and scaled. Editorial labels are outside the evidence crop."})
    (OUTPUT / (view["id"] + "-metadata.json")).write_text(json.dumps(result, indent=2) + "\n")
    print(f"Verified {view['id']}: {frames} frames", flush=True)


def main():
    scene_ids = list(dict.fromkeys(v.get("scene", v["id"]) for v in VIEWS))
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scene", action="append", choices=scene_ids)
    args = parser.parse_args()
    OUTPUT.mkdir(parents=True, exist_ok=True)
    manifest = json.loads((HERE / "manifest.json").read_text())
    frames = {scene["id"]: scene["frames"] for scene in manifest["scenes"]}
    for view in VIEWS:
        scene_id = view.get("scene", view["id"])
        if args.scene and scene_id not in args.scene:
            continue
        count = frames[scene_id]
        if scene_id == "05-patch":
            count = count // 2 if view["id"] == "05-code" else count - count // 2
        render(view, count)
    if not args.scene or "05-patch" in args.scene:
        concat = OUTPUT / "05-patch-concat.txt"
        concat.write_text("file '05-code.mp4'\nfile '05-tests.mp4'\n")
        subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
                        "-f", "concat", "-safe", "0", "-i", str(concat), "-map", "0:v:0", "-an",
                        "-c:v", "copy", "-video_track_timescale", "30000", "-movflags", "+faststart",
                        str(OUTPUT / "05-patch.mp4")], check=True)
        verify(OUTPUT / "05-patch.mp4", frames["05-patch"])


if __name__ == "__main__":
    main()
