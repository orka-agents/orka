#!/usr/bin/env python3
"""Prepare genuine terminal captures and separate voice clips for Resolve."""

import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import math
from pathlib import Path
import re
import subprocess
import wave

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / "bin/narrated-demos"
STORYBOARD = Path(__file__).with_name("storyboard.json")
WIDTH, HEIGHT, FPS = 1920, 1080, 30
BACKGROUND = "#1c1e20"
WHITE, MUTED, ACCENT = "#f4f2ec", "#b1b4ba", "#e8a1ee"
FONT = Path("/Library/Fonts/SF-Pro-Display-Regular.otf")
BOLD = Path("/Library/Fonts/SF-Pro-Display-Semibold.otf")
MONO = Path("/Library/Fonts/SF-Mono-Regular.otf")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")


def run(command):
    result = subprocess.run(command, capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(f"{command[0]} failed: {result.stderr[-4000:]}")
    return result.stdout


def probe(path):
    return json.loads(run([
        "ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", str(path),
    ]))


def read_storyboard():
    story = json.loads(STORYBOARD.read_text())
    seen = set()
    for demo in story["demos"]:
        name = demo["id"]
        require(re.fullmatch(r"(?:0[1-5]|0[89]|1[01])-[a-z-]+", name), "Unexpected demo: " + name)
        require(name not in seen, "Duplicate demo: " + name)
        seen.add(name)
    return story


def voice_scenes(demo):
    yield {"id": demo["id"] + "-intro", "text": demo["intro"], "max_frames": 768}
    for index, chapter in enumerate(demo["chapters"], 1):
        yield {"id": f"{demo['id']}-chapter-{index:02d}", "text": chapter["text"], "max_frames": 768}
    yield {"id": demo["id"] + "-outro", "text": demo["outro"], "max_frames": 768}


def plan(story):
    voices = [scene for demo in story["demos"] for scene in voice_scenes(demo)]
    write_json(OUTPUT / "narration.json", {"scenes": voices})
    print(f"Prepared {len(voices)} voice segments for {len(story['demos'])} demos.")


def read_cast(demo):
    path = ROOT / "demo/casts" / (demo["id"] + ".cast")
    lines = [json.loads(line) for line in path.read_text().splitlines()]
    require(lines[0]["version"] == 3, "Expected a v3 capture: " + str(path))
    require(lines[-1][1:] == ["x", "0"], "Capture did not exit successfully: " + str(path))
    markers = [(i, event[2]) for i, event in enumerate(lines[1:], 1) if event[1] == "m"]
    chapters = {}
    for number, (start, title) in enumerate(markers):
        require(title not in chapters, "Duplicate marker: " + title)
        stop = markers[number + 1][0] if number + 1 < len(markers) else len(lines) - 1
        chapters[title] = {"events": lines[start:stop], "first_event": start, "last_event": stop - 1}
    for chapter in demo["chapters"]:
        require(chapter["marker"] in chapters, "Missing recorded chapter: " + chapter["marker"])
    return path, lines[0], chapters


def font(size, bold=False, mono=False):
    return ImageFont.truetype(str(MONO if mono else BOLD if bold else FONT), size)


def text(draw, xy, value, size, color=WHITE, bold=False, mono=False):
    selected = font(size, bold, mono)
    bounds = draw.textbbox(xy, value, font=selected, anchor="lt")
    require(bounds[2] <= WIDTH - 70 and bounds[3] <= HEIGHT - 25, "Text exceeds frame: " + value)
    draw.text(xy, value, font=selected, fill=color, anchor="lt")


def wrapped(draw, xy, value, size, width=1640, color=WHITE, bold=False):
    selected = font(size, bold)
    rows, row = [], ""
    for word in value.split():
        candidate = (row + " " + word).strip()
        if row and draw.textlength(candidate, font=selected) > width:
            rows.append(row)
            row = word
        else:
            row = candidate
    rows.append(row)
    for index, row in enumerate(rows):
        text(draw, (xy[0], xy[1] + index * round(size * 1.2)), row, size, color, bold)
    return xy[1] + len(rows) * round(size * 1.2)


def card(demo, kind, docs_url, output):
    image = Image.new("RGB", (WIDTH, HEIGHT), BACKGROUND)
    draw = ImageDraw.Draw(image)
    text(draw, (134, 94), f"ORKA  /  DEMO {demo['id'][:2]}", 27, ACCENT, mono=True)
    draw.line((134, 166, 1786, 166), fill="#45474c", width=2)
    if kind == "intro":
        bottom = wrapped(draw, (134, 254), demo["title"], 91, width=1640, bold=True)
        wrapped(draw, (139, bottom + 64), demo["scenario"], 49, width=1550, color=MUTED)
        text(draw, (139, 913), "A recorded walkthrough, with waiting time compressed", 27, MUTED)
    else:
        bottom = wrapped(draw, (134, 241), demo["payoff"], 78, width=1630, bold=True)
        wrapped(draw, (139, bottom + 49), demo["next"], 45, width=1570, color=MUTED)
        text(draw, (139, 783), "Explore more and get started", 34, ACCENT, bold=True)
        text(draw, (139, 849), docs_url, 43, mono=True)
    image.save(output)


def chrome(demo, chapter, number, total, output):
    image = Image.new("RGB", (WIDTH, HEIGHT), BACKGROUND)
    draw = ImageDraw.Draw(image)
    text(draw, (84, 19), f"ORKA  /  {demo['id'][:2]}  /  {number:02d} OF {total:02d}", 19, ACCENT, mono=True)
    text(draw, (84, 51), chapter["title"], 43, bold=True)
    text(draw, (84, 109), chapter["cue"], 26, MUTED)
    text(draw, (84, 1036), "Recorded execution  |  Waiting time compressed", 19, MUTED)
    text(draw, (1448, 1036), "orka-agents.github.io/orka/", 19, MUTED)
    image.save(output)


def frame_audio(voice, scene_dir):
    name = voice["id"]
    request_path = OUTPUT / "audio" / (name + "-request.json")
    metadata_path = OUTPUT / "audio" / (name + "-metadata.json")
    request = json.loads(request_path.read_text())
    require(request["input"] == voice["text"], "Voice does not match storyboard: " + name)
    metadata = json.loads(metadata_path.read_text())
    require(metadata["text"] == voice["text"], "Final voice metadata does not match storyboard: " + name)
    require(not metadata["near_frame_cap"], "Voice may be truncated: " + name)
    original = OUTPUT / "audio" / (name + ".wav")
    require(digest(original) == metadata["final"]["sha256"], "Voice digest changed: " + name)
    with wave.open(str(original), "rb") as audio:
        require((audio.getframerate(), audio.getnchannels(), audio.getsampwidth()) == (48000, 1, 2),
                "Expected 48kHz mono PCM16 voice")
        count = audio.getnframes()
        samples = audio.readframes(count)
    frames = math.ceil(count / (48000 / FPS))
    padded = scene_dir / (name + ".wav")
    with wave.open(str(padded), "wb") as audio:
        audio.setnchannels(1)
        audio.setsampwidth(2)
        audio.setframerate(48000)
        audio.writeframes(samples + b"\0\0" * (frames * 1600 - count))
    return padded, frames, metadata


def encode(image, output, frames, terminal=None, factor=1.0):
    command = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y"]
    if terminal:
        info = probe(terminal)["streams"][0]
        scale = min(1752 / info["width"], 868 / info["height"])
        width, height = (int(info[key] * scale) // 2 * 2 for key in ("width", "height"))
        x = (WIDTH - width) // 2
        command.extend(["-ignore_loop", "1", "-i", str(terminal), "-loop", "1",
                        "-framerate", "30", "-i", str(image), "-filter_complex",
                        f"[0:v]setpts={factor:.9f}*(PTS-STARTPTS),scale={width}:{height}:flags=lanczos,fps=30[term];"
                        f"[1:v][term]overlay={x}:154:shortest=1,tpad=stop_mode=clone:stop_duration=600[video]",
                        "-map", "[video]"])
    else:
        command.extend(["-loop", "1", "-framerate", "30", "-i", str(image),
                        "-vf", "fade=t=in:st=0:d=0.3"])
    command.extend(["-an", "-c:v", "libx264", "-crf", "17", "-preset", "fast", "-threads", "4",
                    "-pix_fmt", "yuv420p", "-r", "30", "-frames:v", str(frames),
                    "-movflags", "+faststart", str(output)])
    run(command)
    rendered = probe(output)["streams"][0]
    require(int(rendered["nb_frames"]) == frames, "Scene frame count mismatch: " + str(output))
    require((rendered["width"], rendered["height"]) == (WIDTH, HEIGHT), "Scene dimensions mismatch")
    run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-sseof", "-0.5",
         "-i", str(output), "-frames:v", "1", str(output.with_suffix(".jpg"))])


def render_demo(demo, docs_url):
    directory = OUTPUT / demo["id"]
    directory.mkdir(parents=True, exist_ok=True)
    cast_path, header, chapters = read_cast(demo)
    source_sha = digest(cast_path)
    scenes, audio_clips, provenance = [], [], []
    offset = 0
    voices = list(voice_scenes(demo))
    for index, voice in enumerate(voices):
        scene_id = voice["id"]
        is_card = index in (0, len(voices) - 1)
        chapter = None if is_card else demo["chapters"][index - 1]
        audio_path, audio_frames, audio_metadata = frame_audio(voice, directory)
        output = directory / (scene_id + ".mp4")
        picture = directory / (scene_id + ".png")
        report_path = directory / (scene_id + ".json")
        inputs = {"voice_sha256": digest(audio_path), "source_sha256": source_sha,
                  "demo": demo, "voice": voice, "script_sha256": digest(__file__), "docs_url": docs_url}
        report = json.loads(report_path.read_text()) if report_path.exists() else None
        reusable = report and report.get("inputs") == inputs and output.is_file()
        if reusable:
            require(digest(output) == report["video_sha256"], "Cached video digest changed")
            frames = report["frames"]
        else:
            terminal, factor = None, 1.0
            source = None
            if is_card:
                kind = "intro" if index == 0 else "outro"
                frames = audio_frames + (108 if kind == "outro" else 66)
                card(demo, kind, docs_url, picture)
            else:
                source = chapters[chapter["marker"]]
                split = directory / (scene_id + ".cast")
                # Reset the presentation at a chapter boundary. Every subsequent
                # output byte comes directly from that chapter's recorded events.
                events = [[0, "o", "\u001b[2J\u001b[H\u001b[?25l"]]
                events.extend([[0 if i == 0 else event[0], *event[1:]]
                               for i, event in enumerate(source["events"])])
                events.append([0, "x", "0"])
                split.write_text("\n".join(json.dumps(value) for value in [header, *events]) + "\n")
                terminal = directory / (scene_id + ".gif")
                run(["agg", str(split), str(terminal), "--font-size", "28", "--font-family", "SF Mono",
                     "--line-height", "1.1", "--theme", "monokai", "--idle-time-limit", "2",
                     "--last-frame-duration", "1.2", "--fps-cap", "30", "--no-loop", "--quiet"])
                duration = float(probe(terminal)["format"]["duration"])
                seconds = max(audio_frames / FPS + 2.4, min(duration + 1.2, 40), duration / 2 + 1.2)
                frames = math.ceil(seconds * FPS)
                factor = min(1.0, (frames / FPS - 1.2) / duration)
                chrome(demo, chapter, index, len(demo["chapters"]), picture)
            encode(picture, output, frames, terminal, factor)
            report = {"id": scene_id, "frames": frames, "inputs": inputs, "video_sha256": digest(output),
                      "playback_speed": 1 / factor,
                      "source_events": {k: source[k] for k in ("first_event", "last_event")} if source else None,
                      "audio_metadata": str(OUTPUT / "audio" / (scene_id + "-metadata.json"))}
            write_json(report_path, report)
        title = chapter["title"] if chapter else demo["title"] if index == 0 else "Explore more and get started"
        scenes.append({"id": scene_id, "title": title, "path": str(output), "frames": frames,
                       "note": "Recorded source: " + str(cast_path) if chapter else voice["text"]})
        audio_clips.append({"path": str(audio_path), "record_frame": offset + 18,
                            "frames": audio_frames, "track": 1})
        provenance.append(report)
        offset += frames
        print(f"{scene_id}: {frames / FPS:.2f}s ready", flush=True)
    manifest = {"project_name": "Orka " + demo["id"][:2] + " - " + demo["title"],
                "timeline_name": "Orka " + demo["id"][:2] + " narrated walkthrough",
                "output_name": demo["id"], "scenes": scenes, "audio": audio_clips}
    write_json(directory / "manifest.json", manifest)
    write_json(directory / "provenance.json", {"source_cast": str(cast_path), "source_sha256": source_sha,
               "duration_seconds": offset / FPS, "scenes": provenance})
    print(f"READY {demo['id']}: {offset / FPS:.2f}s", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["plan", "render"])
    parser.add_argument("demos", nargs="*")
    parser.add_argument("--jobs", type=int, default=1)
    args = parser.parse_args()
    story = read_storyboard()
    if args.action == "plan":
        plan(story)
        return
    selected = [demo for demo in story["demos"] if not args.demos or demo["id"] in args.demos]
    require(selected and all(name in [demo["id"] for demo in selected] for name in args.demos),
            "Unknown demo selection")
    require(1 <= args.jobs <= 4, "Use one to four preparation jobs")
    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        futures = [pool.submit(render_demo, demo, story["docs_url"]) for demo in selected]
        for future in futures:
            future.result()


if __name__ == "__main__":
    main()
