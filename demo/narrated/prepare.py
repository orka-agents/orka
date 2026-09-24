#!/usr/bin/env python3
"""Prepare genuine terminal captures and narration for Resolve."""

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
AUDIO_RATE, AUDIO_LEAD_FRAMES = 48000, 18
SAMPLES_PER_FRAME = AUDIO_RATE // FPS
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
        require(re.fullmatch(r"(?:0[1-5]|0[89]|1[012])-[a-z-]+", name), "Unexpected demo: " + name)
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


def checked_artifact(value, label):
    require(isinstance(value, dict), "Missing artifact: " + label)
    require(isinstance(value.get("path"), str) and Path(value["path"]).is_absolute(),
            "Expected an absolute artifact path: " + label)
    path = Path(value["path"])
    require(path.is_file(), "Missing artifact file: " + str(path))
    require(digest(path) == value.get("sha256"), "Artifact digest changed: " + label)
    return path


def read_continuous_alignment(demo, path, source_sha):
    """Validate a reviewed partition of one complete narration performance."""
    path = Path(path).resolve()
    raw = path.read_bytes()
    alignment = json.loads(raw)
    require(isinstance(alignment, dict), "Expected a continuous narration alignment object")
    require(type(alignment.get("version")) is int and alignment["version"] in (1, 2),
            "Expected continuous narration alignment version 1 or 2")
    require(alignment.get("demo_id") == demo["id"], "Alignment belongs to another demo")
    voices = list(voice_scenes(demo))
    require(alignment.get("text") == "\n\n".join(voice["text"] for voice in voices),
            "Continuous narration does not match storyboard: " + demo["id"])
    require(isinstance(alignment.get("method"), str) and alignment["method"].strip(),
            "Missing continuous narration alignment method")
    require(isinstance(alignment.get("review"), dict)
            and alignment["review"].get("status") == "accepted",
            "Continuous narration alignment has not been accepted: " + demo["id"])
    if "source_cast_sha256" in alignment:
        require(alignment["source_cast_sha256"] == source_sha, "Alignment source cast changed")
    if "metadata" in alignment:
        checked_artifact(alignment["metadata"], "narration metadata")
    sources = alignment.get("sources", [])
    require(isinstance(sources, list), "Expected a list of narration source artifacts")
    for index, artifact in enumerate(sources):
        checked_artifact(artifact, f"narration source {index + 1}")
    audio_path = checked_artifact(alignment.get("audio"), "continuous narration audio")
    with wave.open(str(audio_path), "rb") as audio:
        require((audio.getframerate(), audio.getnchannels(), audio.getsampwidth(), audio.getcomptype())
                == (AUDIO_RATE, 1, 2, "NONE"), "Expected 48kHz mono PCM16 continuous narration")
        count = audio.getnframes()
        samples = audio.readframes(count)
    require(count > 0 and len(samples) == count * 2, "Empty or truncated continuous narration")
    scenes = alignment.get("scenes")
    require(isinstance(scenes, list) and len(scenes) == len(voices),
            "Continuous narration scene count does not match storyboard")
    end = 0
    for index, (voice, scene) in enumerate(zip(voices, scenes)):
        require(isinstance(scene, dict) and scene.get("id") == voice["id"]
                and scene.get("text") == voice["text"],
                "Continuous narration scene does not match storyboard: " + voice["id"])
        start, stop = scene.get("start_sample"), scene.get("end_sample")
        require(type(start) is int and type(stop) is int and start == end and start < stop <= count,
                "Continuous narration samples must be nonempty and contiguous: " + voice["id"])
        continuous_after = scene.get("continuous_after", False)
        require(type(continuous_after) is bool, "Expected a boolean continuity flag")
        if continuous_after:
            require(alignment["version"] == 2, "Uninterrupted joins require alignment version 2")
            require(0 < index < len(scenes) - 1, "Only a chapter may continue into the next scene")
            require((stop - start) % SAMPLES_PER_FRAME == 0,
                    "Uninterrupted chapter narration must span whole video frames")
        end = stop
    require(end == count, "Continuous narration alignment does not include the complete audio")
    return {"path": str(path), "sha256": hashlib.sha256(raw).hexdigest(),
            "alignment": alignment, "samples": samples, "sample_count": count}


def write_continuous_audio(continuous, scenes, directory, demo_id):
    """Keep every original PCM sample, adding only silence between scene slices."""
    alignment = continuous["alignment"]
    require(len(scenes) == len(alignment["scenes"]), "Continuous audio scene count changed")
    path = directory / (demo_id + "-narration.wav")
    require(path.resolve() != Path(alignment["audio"]["path"]).resolve(),
            "Rendered narration must not replace its source audio")
    placements, offset = [], 0
    for index, (scene, source) in enumerate(zip(scenes, alignment["scenes"])):
        require(scene["id"] == source["id"], "Continuous audio scene order changed")
        frames = scene["frames"]
        count = source["end_sample"] - source["start_sample"]
        continuous_after = source.get("continuous_after", False)
        if continuous_after:
            require(type(frames) is int and frames * SAMPLES_PER_FRAME == count,
                    "An uninterrupted chapter must not insert silence or cut narration: " + scene["id"])
        else:
            require(type(frames) is int and (frames - AUDIO_LEAD_FRAMES) * SAMPLES_PER_FRAME >= count,
                    "Scene is too short for its complete narration: " + scene["id"])
        padding = frames * SAMPLES_PER_FRAME - count
        if index == len(scenes) - 1:
            padding -= AUDIO_LEAD_FRAMES * SAMPLES_PER_FRAME
        placements.append({"id": scene["id"], "source_start_sample": source["start_sample"],
                           "source_end_sample": source["end_sample"],
                           "output_start_sample": offset * SAMPLES_PER_FRAME,
                           "output_end_sample": offset * SAMPLES_PER_FRAME + count,
                           "record_frame": offset + AUDIO_LEAD_FRAMES,
                           "inserted_silence_samples": padding, "continuous_after": continuous_after})
        offset += frames
    frames = offset - AUDIO_LEAD_FRAMES
    temporary = path.with_suffix(".wav.partial")
    try:
        with wave.open(str(temporary), "wb") as audio:
            audio.setnchannels(1)
            audio.setsampwidth(2)
            audio.setframerate(AUDIO_RATE)
            audio.setnframes(frames * SAMPLES_PER_FRAME)
            for placement in placements:
                start, stop = placement["source_start_sample"], placement["source_end_sample"]
                audio.writeframesraw(continuous["samples"][start * 2:stop * 2])
                audio.writeframesraw(b"\0\0" * placement["inserted_silence_samples"])
        with wave.open(str(temporary), "rb") as audio:
            require(audio.getnframes() == frames * SAMPLES_PER_FRAME,
                    "Continuous narration frame count mismatch")
        temporary.replace(path)
    finally:
        temporary.unlink(missing_ok=True)
    clip = {"path": str(path), "record_frame": AUDIO_LEAD_FRAMES, "frames": frames, "track": 1}
    report = {"alignment": continuous["path"], "alignment_sha256": continuous["sha256"],
              "source_audio": alignment["audio"], "source_samples": continuous["sample_count"],
              "sample_rate": AUDIO_RATE, "samples_per_frame": SAMPLES_PER_FRAME,
              "output_audio": str(path), "output_sha256": digest(path), "output_frames": frames,
              "record_frame": AUDIO_LEAD_FRAMES,
              "inserted_silence_samples": sum(item["inserted_silence_samples"] for item in placements),
              "scenes": placements}
    return clip, report


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


def render_demo(demo, docs_url, continuous_dir=None):
    directory = OUTPUT / demo["id"]
    directory.mkdir(parents=True, exist_ok=True)
    cast_path, header, chapters = read_cast(demo)
    source_sha = digest(cast_path)
    scenes, audio_clips, provenance = [], [], []
    offset = 0
    voices = list(voice_scenes(demo))
    continuous = (read_continuous_alignment(demo, Path(continuous_dir) / demo["id"] / "alignment.json",
                                          source_sha) if continuous_dir is not None else None)
    for index, voice in enumerate(voices):
        scene_id = voice["id"]
        is_card = index in (0, len(voices) - 1)
        chapter = None if is_card else demo["chapters"][index - 1]
        if continuous:
            audio_scene = continuous["alignment"]["scenes"][index]
            audio_frames = math.ceil((audio_scene["end_sample"] - audio_scene["start_sample"])
                                     / SAMPLES_PER_FRAME)
            voice_sha = continuous["alignment"]["audio"]["sha256"]
        else:
            audio_path, audio_frames, _ = frame_audio(voice, directory)
            voice_sha = digest(audio_path)
        output = directory / (scene_id + ".mp4")
        picture = directory / (scene_id + ".png")
        report_path = directory / (scene_id + ".json")
        inputs = {"voice_sha256": voice_sha, "source_sha256": source_sha,
                  "demo": demo, "voice": voice, "script_sha256": digest(__file__), "docs_url": docs_url}
        if continuous:
            inputs["continuous_alignment_sha256"] = continuous["sha256"]
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
                max_speed = chapter.get("max_speed", 2)
                require(isinstance(max_speed, (int, float)) and not isinstance(max_speed, bool)
                        and 1 <= max_speed <= 4, "Chapter playback speed must be between one and four")
                if continuous and audio_scene.get("continuous_after", False):
                    # Keep this join byte-contiguous in the one voice track. Fit
                    # the video to the speech instead of adding a pause to it.
                    frames = audio_frames
                    require(frames / FPS >= duration / max_speed + 1.2,
                            "Uninterrupted narration is too short for readable video: " + scene_id)
                else:
                    seconds = max(audio_frames / FPS + 2.4, min(duration + 1.2, 40), duration / max_speed + 1.2)
                    frames = math.ceil(seconds * FPS)
                factor = min(1.0, (frames / FPS - 1.2) / duration)
                chrome(demo, chapter, index, len(demo["chapters"]), picture)
            encode(picture, output, frames, terminal, factor)
            report = {"id": scene_id, "frames": frames, "inputs": inputs, "video_sha256": digest(output),
                      "playback_speed": 1 / factor,
                      "source_events": {k: source[k] for k in ("first_event", "last_event")} if source else None}
            if continuous:
                report["audio_alignment"] = {"path": continuous["path"], "sha256": continuous["sha256"],
                                             "start_sample": audio_scene["start_sample"],
                                             "end_sample": audio_scene["end_sample"]}
            else:
                report["audio_metadata"] = str(OUTPUT / "audio" / (scene_id + "-metadata.json"))
            write_json(report_path, report)
        title = chapter["title"] if chapter else demo["title"] if index == 0 else "Explore more and get started"
        scenes.append({"id": scene_id, "title": title, "path": str(output), "frames": frames,
                       "note": "Recorded source: " + str(cast_path) if chapter else voice["text"]})
        if not continuous:
            audio_clips.append({"path": str(audio_path), "record_frame": offset + AUDIO_LEAD_FRAMES,
                                "frames": audio_frames, "track": 1})
        provenance.append(report)
        offset += frames
        print(f"{scene_id}: {frames / FPS:.2f}s ready", flush=True)
    evidence = {"source_cast": str(cast_path), "source_sha256": source_sha,
                "duration_seconds": offset / FPS, "scenes": provenance}
    if continuous:
        clip, evidence["continuous_narration"] = write_continuous_audio(continuous, scenes, directory, demo["id"])
        audio_clips.append(clip)
    manifest = {"project_name": "Orka " + demo["id"][:2] + " - " + demo["title"],
                "timeline_name": "Orka " + demo["id"][:2] + " narrated walkthrough",
                "output_name": demo["id"], "scenes": scenes, "audio": audio_clips}
    write_json(directory / "manifest.json", manifest)
    write_json(directory / "provenance.json", evidence)
    print(f"READY {demo['id']}: {offset / FPS:.2f}s", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["plan", "render"])
    parser.add_argument("demos", nargs="*")
    parser.add_argument("--jobs", type=int, default=1)
    parser.add_argument("--continuous-dir", type=Path,
                        help="Render one narration track from PATH/<demo-id>/alignment.json")
    args = parser.parse_args()
    story = read_storyboard()
    if args.action == "plan":
        require(args.continuous_dir is None, "--continuous-dir is only supported for render")
        plan(story)
        return
    selected = [demo for demo in story["demos"] if not args.demos or demo["id"] in args.demos]
    require(selected and all(name in [demo["id"] for demo in selected] for name in args.demos),
            "Unknown demo selection")
    require(1 <= args.jobs <= 4, "Use one to four preparation jobs")
    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        futures = [pool.submit(render_demo, demo, story["docs_url"], args.continuous_dir) for demo in selected]
        for future in futures:
            future.result()


if __name__ == "__main__":
    main()
