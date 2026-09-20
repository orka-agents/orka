#!/usr/bin/env python3
"""Validate media, prepare a Resolve Lua driver, and verify the rendered exports."""

import argparse
import datetime
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess


REPOSITORY = Path(__file__).resolve().parents[3]
OUTPUT_ROOT = REPOSITORY / "bin" / "hackathon-first-pass" / "resolve"
DEFAULT_MANIFEST = Path(__file__).with_name("manifest.json")
FPS = 30
WIDTH = 1920
HEIGHT = 1080
MAX_FRAMES = 119 * FPS
SANDBOX_ROOT = Path.home() / "Library" / "Containers" / "com.blackmagic-design.DaVinciResolveLite" / "Data" / "OrkaHackathon"


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def file_digest(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def copy_artifact(source, destination):
    source, destination = Path(source), Path(destination)
    if destination.exists():
        require(file_digest(source) == file_digest(destination),
                "Refusing to replace a different existing artifact: " + str(destination))
        return
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, destination)


def media_probe(path):
    executable = shutil.which("ffprobe") or "/opt/homebrew/bin/ffprobe"
    require(Path(executable).is_file(), "ffprobe is required for media validation")
    result = subprocess.run(
        [executable, "-v", "error", "-show_streams", "-show_format", "-of", "json", str(path)],
        check=True, capture_output=True, text=True,
    )
    return json.loads(result.stdout)


def rate(value):
    numerator, _, denominator = str(value).partition("/")
    return float(numerator) / float(denominator or 1)


def positive_frames(value, context):
    require(isinstance(value, int) and not isinstance(value, bool) and value > 0,
            context + " must be a positive integer frame count")
    return value


def nonnegative_frames(value, context):
    require(isinstance(value, int) and not isinstance(value, bool) and value >= 0,
            context + " must be a nonnegative integer frame count")
    return value


def load_manifest(path, check_media=True):
    path = Path(path).expanduser().resolve()
    manifest = json.loads(path.read_text())
    require(manifest.get("fps", FPS) == FPS, "The first-pass timeline uses 30 fps")
    require(manifest.get("width", WIDTH) == WIDTH and manifest.get("height", HEIGHT) == HEIGHT,
            "The first-pass timeline uses 1920x1080")
    scenes = manifest.get("scenes", [])
    audio = manifest.get("audio", [])
    require(scenes, "The manifest needs at least one video scene")
    require(audio, "The manifest needs the generated voiceover audio")
    timeline_name = manifest.get("timeline_name", "Orka First pass")
    require(isinstance(timeline_name, str) and timeline_name.strip() and
            re.search(r'[\\/:*?"<>|]', timeline_name) is None,
            "timeline_name must omit reserved filename characters")
    output_dir = Path(manifest.get("output_dir", str(OUTPUT_ROOT))).expanduser().resolve()
    require(output_dir == OUTPUT_ROOT or OUTPUT_ROOT in output_dir.parents,
            "Resolve outputs must stay under " + str(OUTPUT_ROOT))
    output_name = manifest.get("output_name", "orka-hackathon-first-pass")
    require(re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", output_name) is not None,
            "output_name must be a simple filename without a directory or extension")
    require(not output_name.endswith((".mp4", ".mov")), "output_name must omit the extension")

    total_frames = 0
    seen_ids = set()
    probes = {}
    for index, scene in enumerate(scenes):
        scene_id = str(scene.get("id", "scene-%02d" % (index + 1)))
        require(scene_id not in seen_ids, "Duplicate scene id: " + scene_id)
        seen_ids.add(scene_id)
        scene["id"] = scene_id
        scene["frames"] = positive_frames(scene["frames"], scene_id + " frames")
        scene["source_start_frame"] = nonnegative_frames(scene.get("source_start_frame", 0),
                                                         scene_id + " source_start_frame")
        scene["record_frame"] = total_frames
        total_frames += scene["frames"]
    require(total_frames <= MAX_FRAMES,
            "Timeline is %.3fs; maximum is 119s" % (total_frames / FPS))

    intervals = {}
    for index, clip in enumerate(audio):
        clip["frames"] = positive_frames(clip["frames"], "audio frames")
        clip["record_frame"] = nonnegative_frames(clip.get("record_frame", 0), "audio record_frame")
        clip["source_start_frame"] = nonnegative_frames(clip.get("source_start_frame", 0),
                                                       "audio source_start_frame")
        clip["track"] = positive_frames(clip.get("track", 1), "audio track")
        end = clip["record_frame"] + clip["frames"]
        require(end <= total_frames, "Audio clip %d extends beyond the video" % (index + 1))
        for start, previous_end in intervals.setdefault(clip["track"], []):
            require(end <= start or clip["record_frame"] >= previous_end,
                    "Audio clips overlap on track %d" % clip["track"])
        intervals[clip["track"]].append((clip["record_frame"], end))

    for kind, clips in (("video", scenes), ("audio", audio)):
        for clip in clips:
            media_path = Path(clip["path"]).expanduser()
            if not media_path.is_absolute():
                media_path = path.parent / media_path
            media_path = media_path.resolve()
            require(media_path.is_file(), "Missing media: " + str(media_path))
            clip["path"] = str(media_path)
            if not check_media:
                continue
            if str(media_path) not in probes:
                probes[str(media_path)] = media_probe(media_path)
            probe = probes[str(media_path)]
            streams = [stream for stream in probe["streams"] if stream["codec_type"] == kind]
            require(streams, "No " + kind + " stream in " + str(media_path))
            stream = streams[0]
            duration = float(stream.get("duration") or probe["format"].get("duration", 0))
            needed = (clip["source_start_frame"] + clip["frames"]) / FPS
            require(duration + 1 / FPS >= needed,
                    "%s is %.3fs, but the manifest needs %.3fs" % (media_path.name, duration, needed))
            if kind == "video":
                require(stream.get("width") == WIDTH and stream.get("height") == HEIGHT,
                        media_path.name + " must be normalized to 1920x1080 before import")
                require(abs(rate(stream["avg_frame_rate"]) - FPS) < 0.001,
                        media_path.name + " must be normalized to constant 30 fps before import")
                require(abs(rate(stream["r_frame_rate"]) - FPS) < 0.001,
                        media_path.name + " has a non-30fps stream rate")

    manifest.update({"fps": FPS, "width": WIDTH, "height": HEIGHT,
                     "output_dir": str(output_dir), "output_name": output_name,
                     "total_frames": total_frames, "duration_seconds": total_frames / FPS,
                     "manifest_path": str(path)})
    return manifest


def verify_output(report_path):
    report_path = Path(report_path).resolve()
    report = json.loads(report_path.read_text())
    manifest = report["manifest"]
    video_path = Path(manifest["output_dir"]) / (manifest["output_name"] + ".mp4")
    require(video_path.is_file(), "The Resolve MP4 does not exist yet")
    probe = media_probe(video_path)
    video = next((s for s in probe["streams"] if s["codec_type"] == "video"), None)
    audio = next((s for s in probe["streams"] if s["codec_type"] == "audio"), None)
    require(video and audio, "The export must contain both video and narration")
    require(video["codec_name"] == "h264", "The export must use H.264 video")
    require(audio["codec_name"] == "aac", "The export must use AAC audio")
    require(video["width"] == WIDTH and video["height"] == HEIGHT, "Wrong export dimensions")
    require(abs(rate(video["avg_frame_rate"]) - FPS) < 0.001, "Wrong export frame rate")
    require(int(video["nb_frames"]) == manifest["total_frames"], "Wrong export frame count")
    duration = float(probe["format"]["duration"])
    require(duration < 120, "The delivered video must be shorter than 120 seconds")
    project_export = Path(report["project_export"])
    require(project_export.is_file() and project_export.stat().st_size > 0, "The Resolve project export is missing")
    if report.get("delivery_dir"):
        delivery_dir = Path(report["delivery_dir"])
        report["render_video_export"] = str(video_path)
        report["render_project_export"] = str(project_export)
        delivered_video = delivery_dir / video_path.name
        delivered_project = delivery_dir / project_export.name
        copy_artifact(video_path, delivered_video)
        copy_artifact(project_export, delivered_project)
        video_path = delivered_video
        report["project_export"] = str(delivered_project)
    report.update({"status": "verified", "video_export": str(video_path),
                   "export_duration_seconds": duration, "export_probe": probe})
    if report_path.name.endswith(".prepared.json"):
        report_path = report_path.with_name(manifest["output_name"] + ".report.json")
    write_json(report_path, report)
    print("Verified Resolve export: %s, %.3fs, 1920x1080 H.264/AAC at 30fps" % (video_path, duration))
    return report


def lua_literal(value):
    if isinstance(value, str):
        escaped = []
        for byte in value.encode("utf-8"):
            if 32 <= byte < 127 and byte not in (34, 92):
                escaped.append(chr(byte))
            else:
                escaped.append("\\%03d" % byte)
        return '"' + "".join(escaped) + '"'
    if value is True:
        return "true"
    if value is False:
        return "false"
    if value is None:
        return "nil"
    if isinstance(value, (int, float)):
        return str(value)
    if isinstance(value, list):
        return "{" + ",".join(lua_literal(item) for item in value) + "}"
    if isinstance(value, dict):
        return "{" + ",".join("[" + lua_literal(key) + "]=" + lua_literal(child)
                              for key, child in value.items()) + "}"
    raise TypeError("Unsupported Lua literal: " + repr(value))


def prepare_lua(manifest_path, sandbox=False):
    manifest = load_manifest(manifest_path)
    output_dir = Path(manifest["output_dir"])
    output_dir.mkdir(parents=True, exist_ok=True)
    manifest["resolved_project_name"] = manifest.get("project_name", "Orka Hackathon First Pass") + \
        " " + datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    source_map = {}
    delivery_dir = None
    if sandbox:
        delivery_dir = str(output_dir)
        sandbox_dir = SANDBOX_ROOT / manifest["output_name"]
        for extension in (".mp4", ".drp", ".report.json"):
            require(not (sandbox_dir / (manifest["output_name"] + extension)).exists(),
                    "A sandbox export already exists. Choose a new output_name.")
        for clip in manifest["scenes"] + manifest["audio"]:
            original = clip["path"]
            if original not in source_map:
                target = sandbox_dir / "media" / ("%02d-" % (len(source_map) + 1) + Path(original).name)
                copy_artifact(original, target)
                source_map[original] = str(target)
            clip["path"] = source_map[original]
        manifest["output_dir"] = str(sandbox_dir)
    driver = output_dir / (manifest["output_name"] + ".console.lua")
    assembler = Path(__file__).with_suffix(".lua")
    driver.write_text("-- Generated by the checked media preflight.\n"
                      "_orka_assembler = (function()\n" + assembler.read_text() + "\nend)()\n"
                      "_orka_render_report = _orka_assembler.assemble(" + lua_literal(manifest) + ")\n")
    prepared = {
        "status": "prepared", "manifest": manifest, "project_name": manifest["resolved_project_name"],
        "project_export": str(Path(manifest["output_dir"]) / (manifest["output_name"] + ".drp")),
        "assembly_engine": "DaVinci Resolve internal Lua SDK",
    }
    if sandbox:
        sandbox_driver = Path(manifest["output_dir"]) / driver.name
        sandbox_driver.write_bytes(driver.read_bytes())
        driver = sandbox_driver
        prepared.update({"delivery_dir": delivery_dir, "source_media_map": source_map})
    write_json(output_dir / (manifest["output_name"] + ".prepared.json"), prepared)
    print("Preflight passed: %.3fs. In Resolve's Lua Console run:" % manifest["duration_seconds"])
    print("dofile(" + lua_literal(str(driver)) + ")")
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--preflight", metavar="MANIFEST")
    mode.add_argument("--verify-output", metavar="REPORT")
    mode.add_argument("--prepare-lua", metavar="MANIFEST")
    parser.add_argument("manifest", nargs="?", default=str(DEFAULT_MANIFEST))
    parser.add_argument("--sandbox", action="store_true", help="Stage media and Lua inside App Store Resolve's task sandbox")
    args = parser.parse_args()
    if args.preflight:
        manifest = load_manifest(args.preflight)
        print("Preflight passed: %d scenes, %d audio clips, %.3fs at 1920x1080/30fps" %
              (len(manifest["scenes"]), len(manifest["audio"]), manifest["duration_seconds"]))
        return manifest
    if args.verify_output:
        return verify_output(args.verify_output)
    return prepare_lua(args.prepare_lua or args.manifest, sandbox=args.sandbox)


if __name__ == "__main__":
    main()
