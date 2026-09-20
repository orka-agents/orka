#!/usr/bin/env python3
"""Lay scene WAVs into a silence-padded voiceover track for the suggested edit."""

import argparse
import json
from pathlib import Path
import wave


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timing", type=Path, default=Path(__file__).with_name("timing.json"))
    parser.add_argument("--audio-dir", type=Path,
                        default=Path(__file__).resolve().parents[3] / "bin/hackathon-first-pass/audio")
    args = parser.parse_args()
    timing = json.loads(args.timing.read_text())
    fps = timing["frames_per_second"]
    lead_frames = timing["voice_lead_in_frames"]
    rate = 48000
    if rate % fps:
        parser.error("Frame rate must divide the sample rate exactly")
    samples_per_frame = rate // fps
    track = bytearray()
    cursor_frames = 0
    placements = []
    for scene in timing["scenes"]:
        source = args.audio_dir / (scene["id"] + ".wav")
        with wave.open(str(source), "rb") as audio:
            if (audio.getframerate(), audio.getnchannels(), audio.getsampwidth()) != (rate, 1, 2):
                raise ValueError(f"Expected 48 kHz mono PCM16 WAV: {source}")
            audio_frames = audio.getnframes()
            data = audio.readframes(audio_frames)
        scene_samples = scene["frames"] * samples_per_frame
        lead_samples = lead_frames * samples_per_frame
        tail_samples = scene_samples - lead_samples - audio_frames
        if tail_samples < 0:
            raise ValueError(f"Voiceover does not fit scene {scene['id']}")
        track.extend(b"\x00\x00" * lead_samples)
        track.extend(data)
        track.extend(b"\x00\x00" * tail_samples)
        placements.append({
            "id": scene["id"],
            "scene_start_frame": cursor_frames,
            "scene_frames": scene["frames"],
            "scene_start_seconds": cursor_frames / fps,
            "voice_start_seconds": (cursor_frames + lead_frames) / fps,
            "voice_duration_seconds": audio_frames / rate,
            "scene_end_seconds": (cursor_frames + scene["frames"]) / fps,
            "path": str(source),
        })
        cursor_frames += scene["frames"]
    output = args.audio_dir / "voiceover-timeline.wav"
    with wave.open(str(output), "wb") as audio:
        audio.setnchannels(1)
        audio.setsampwidth(2)
        audio.setframerate(rate)
        audio.writeframes(track)
    report = {"path": str(output), "duration_seconds": cursor_frames / fps,
              "frames_per_second": fps, "total_frames": cursor_frames,
              "placements": placements}
    (args.audio_dir / "voiceover-timeline.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
