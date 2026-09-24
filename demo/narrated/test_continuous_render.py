"""Sample preservation and timing checks without encoding any production media."""

from contextlib import ExitStack, redirect_stdout
import copy
import io
import json
import math
from pathlib import Path
import struct
import tempfile
import unittest
from unittest import mock
import wave

import prepare


class ContinuousRenderTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.output = self.root / "output"
        self.output.mkdir()
        patcher = mock.patch.multiple(prepare, ROOT=self.root, OUTPUT=self.output)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.demo = {
            "id": "01-chat-to-pr", "title": "One recorded task",
            "intro": "First, explain the scenario.",
            "chapters": [{"marker": "Run the task", "title": "Run the task",
                          "cue": "Inspect the recorded result.", "text": "This is the complete result."}],
            "outro": "Explore the documentation and get started.",
        }
        self.voices = list(prepare.voice_scenes(self.demo))
        self.cast = self.root / "demo/casts/01-chat-to-pr.cast"
        self.cast.parent.mkdir(parents=True)
        events = [{"version": 3, "width": 100, "height": 28},
                  [0, "m", "Run the task"], [0.1, "o", "$ orka task list\r\nSucceeded\r\n"],
                  [0, "x", "0"]]
        self.cast.write_text("\n".join(json.dumps(item) for item in events) + "\n")
        self.cast_sha = prepare.digest(self.cast)
        self.continuous_dir = self.root / "continuous"
        self.alignment_path = self.continuous_dir / self.demo["id"] / "alignment.json"
        self.alignment_path.parent.mkdir(parents=True)
        self.source_audio = self.alignment_path.with_name("normalized.wav")
        self.counts = [1601, 3205, 2407]
        count = sum(self.counts)
        self.samples = struct.pack(f"<{count}h", *range(1, count + 1))
        self.write_wave(self.source_audio, self.samples)
        self.alignment = {
            "version": 1, "demo_id": self.demo["id"],
            "text": "\n\n".join(voice["text"] for voice in self.voices),
            "audio": {"path": str(self.source_audio), "sha256": prepare.digest(self.source_audio)},
            "scenes": [], "method": "Reviewed silence boundaries", "review": {"status": "accepted"},
            "source_cast_sha256": self.cast_sha,
        }
        offset = 0
        for voice, count in zip(self.voices, self.counts):
            self.alignment["scenes"].append({"id": voice["id"], "text": voice["text"],
                                             "start_sample": offset, "end_sample": offset + count})
            offset += count
        self.write_alignment()

    @staticmethod
    def write_wave(path, samples, rate=48000, channels=1, width=2):
        with wave.open(str(path), "wb") as audio:
            audio.setnchannels(channels)
            audio.setsampwidth(width)
            audio.setframerate(rate)
            audio.writeframes(samples)

    def write_alignment(self, value=None):
        prepare.write_json(self.alignment_path, self.alignment if value is None else value)

    def read_alignment(self):
        return prepare.read_continuous_alignment(self.demo, self.alignment_path, self.cast_sha)

    def assert_sample_preservation(self, clip, report, scenes):
        with wave.open(clip["path"], "rb") as audio:
            self.assertEqual((audio.getframerate(), audio.getnchannels(), audio.getsampwidth()),
                             (48000, 1, 2))
            self.assertEqual(audio.getnframes(), clip["frames"] * 1600)
            rendered = audio.readframes(audio.getnframes())
        self.assertEqual(clip["record_frame"], 18)
        self.assertEqual(clip["track"], 1)
        self.assertEqual(clip["frames"] + clip["record_frame"], sum(scene["frames"] for scene in scenes))
        recovered, offset, padding = [], 0, 0
        for scene, placement in zip(scenes, report["scenes"]):
            start, stop = placement["output_start_sample"], placement["output_end_sample"]
            self.assertEqual(start, offset * 1600)
            self.assertEqual(placement["record_frame"], offset + 18)
            original = self.samples[placement["source_start_sample"] * 2:placement["source_end_sample"] * 2]
            self.assertEqual(rendered[start * 2:stop * 2], original)
            recovered.append(rendered[start * 2:stop * 2])
            silence = placement["inserted_silence_samples"]
            self.assertEqual(rendered[stop * 2:(stop + silence) * 2], b"\0\0" * silence)
            offset += scene["frames"]
            padding += silence
        self.assertEqual(b"".join(recovered), self.samples)
        self.assertEqual(len(rendered), len(self.samples) + padding * 2)
        self.assertEqual(report["source_samples"], sum(self.counts))
        self.assertEqual(report["inserted_silence_samples"], padding)
        self.assertEqual(prepare.digest(self.source_audio), self.alignment["audio"]["sha256"])
        self.assertEqual(prepare.digest(Path(clip["path"])), report["output_sha256"])

    def test_one_frame_exact_track_preserves_every_source_sample(self):
        scenes = [{"id": voice["id"], "frames": frames}
                  for voice, frames in zip(self.voices, (24, 26, 25))]
        directory = self.output / self.demo["id"]
        directory.mkdir()
        clip, report = prepare.write_continuous_audio(self.read_alignment(), scenes, directory, self.demo["id"])
        self.assert_sample_preservation(clip, report, scenes)
        self.assertEqual(list(directory.glob("*.wav")), [Path(clip["path"])])
        self.assertFalse(list(directory.glob("*.partial")))

    def test_rejects_stale_unreviewed_or_incomplete_alignment(self):
        cases = [
            ("version", lambda value: value.update(version=3)),
            ("boolean version", lambda value: value.update(version=True)),
            ("demo", lambda value: value.update(demo_id="02-sandbox")),
            ("script", lambda value: value.update(text="An older script.")),
            ("method", lambda value: value.update(method="")),
            ("review", lambda value: value.update(review={"status": "pending"})),
            ("missing review", lambda value: value.pop("review")),
            ("cast", lambda value: value.update(source_cast_sha256="0" * 64)),
            ("audio digest", lambda value: value["audio"].update(sha256="0" * 64)),
            ("relative audio", lambda value: value["audio"].update(path="normalized.wav")),
            ("scene count", lambda value: value["scenes"].pop()),
            ("scene order", lambda value: value["scenes"].reverse()),
            ("scene text", lambda value: value["scenes"][1].update(text="Different words.")),
            ("scene identity", lambda value: value["scenes"][1].update(id="another-scene")),
            ("leading gap", lambda value: value["scenes"][0].update(start_sample=1)),
            ("gap", lambda value: value["scenes"][1].update(start_sample=1602)),
            ("overlap", lambda value: value["scenes"][1].update(start_sample=1600)),
            ("empty scene", lambda value: value["scenes"][0].update(end_sample=0)),
            ("fractional sample", lambda value: value["scenes"][0].update(start_sample=0.0)),
            ("boolean sample", lambda value: value["scenes"][0].update(start_sample=False)),
            ("omitted tail", lambda value: value["scenes"][-1].update(end_sample=sum(self.counts) - 1)),
            ("excess tail", lambda value: value["scenes"][-1].update(end_sample=sum(self.counts) + 1)),
        ]
        for label, mutate in cases:
            with self.subTest(label=label):
                value = copy.deepcopy(self.alignment)
                mutate(value)
                self.write_alignment(value)
                with self.assertRaises(ValueError):
                    self.read_alignment()

    def test_checks_optional_metadata_and_source_hashes(self):
        metadata = self.alignment_path.with_name("metadata.json")
        metadata.write_text('{"generation": "one complete performance"}\n')
        artifact = {"path": str(metadata), "sha256": prepare.digest(metadata)}
        self.alignment.update(metadata=artifact, sources=[dict(artifact)])
        self.write_alignment()
        self.assertEqual(self.read_alignment()["samples"], self.samples)
        for key in ("metadata", "sources"):
            with self.subTest(key=key):
                value = copy.deepcopy(self.alignment)
                entry = value[key][0] if key == "sources" else value[key]
                entry["sha256"] = "0" * 64
                self.write_alignment(value)
                with self.assertRaisesRegex(ValueError, "Artifact digest changed"):
                    self.read_alignment()

    def test_rejects_wrong_wave_format_and_incomplete_sample_bytes(self):
        for rate, channels, width in ((24000, 1, 2), (48000, 2, 2), (48000, 1, 1)):
            with self.subTest(rate=rate, channels=channels, width=width):
                self.write_wave(self.source_audio, self.samples, rate, channels, width)
                self.alignment["audio"]["sha256"] = prepare.digest(self.source_audio)
                self.write_alignment()
                with self.assertRaisesRegex(ValueError, "48kHz mono PCM16"):
                    self.read_alignment()
        for kind in ("empty", "truncated"):
            with self.subTest(kind=kind):
                self.write_wave(self.source_audio, b"" if kind == "empty" else self.samples)
                if kind == "truncated":
                    self.source_audio.write_bytes(self.source_audio.read_bytes()[:-4])
                self.alignment["audio"]["sha256"] = prepare.digest(self.source_audio)
                self.write_alignment()
                with self.assertRaisesRegex(ValueError, "Empty or truncated"):
                    self.read_alignment()

    def test_rejects_visual_timing_that_would_cut_speech(self):
        scenes = [{"id": voice["id"], "frames": 24} for voice in self.voices]
        scenes[1]["frames"] = 20  # Two usable frames cannot contain 3,205 samples.
        directory = self.output / self.demo["id"]
        directory.mkdir()
        with self.assertRaisesRegex(ValueError, "too short"):
            prepare.write_continuous_audio(self.read_alignment(), scenes, directory, self.demo["id"])
        self.assertFalse(list(directory.iterdir()))

    def render_with_stub_video(self, continuous_dir=None, duration=200):
        encodes = []

        def encode(picture, output, frames, terminal=None, factor=1):
            output.write_bytes(f"stub video: {output.name}, {frames}, {factor}".encode())
            encodes.append({"frames": frames, "factor": factor})

        def run(command):
            self.assertEqual(command[0], "agg")
            Path(command[2]).write_bytes(b"stub terminal animation")

        with ExitStack() as stack:
            stack.enter_context(mock.patch.object(prepare, "encode", side_effect=encode))
            stack.enter_context(mock.patch.object(prepare, "card"))
            stack.enter_context(mock.patch.object(prepare, "chrome"))
            stack.enter_context(mock.patch.object(prepare, "probe", return_value={"format": {"duration": duration}}))
            stack.enter_context(mock.patch.object(prepare, "run", side_effect=run))
            stack.enter_context(redirect_stdout(io.StringIO()))
            if continuous_dir:
                stack.enter_context(mock.patch.object(prepare, "frame_audio", side_effect=AssertionError("chapter audio used")))
            prepare.render_demo(self.demo, "https://orka-agents.github.io/orka/", continuous_dir)
        directory = self.output / self.demo["id"]
        return (json.loads((directory / "manifest.json").read_text()),
                json.loads((directory / "provenance.json").read_text()), encodes)

    def test_render_preserves_speed_limits_recording_and_one_audio_track(self):
        for speed in (1, 2, 4):
            with self.subTest(speed=speed):
                self.demo["chapters"][0]["max_speed"] = speed
                manifest, provenance, encodes = self.render_with_stub_video(self.continuous_dir)
                self.assertEqual(len(manifest["audio"]), 1)
                self.assert_sample_preservation(manifest["audio"][0], provenance["continuous_narration"],
                                                manifest["scenes"])
                self.assertEqual(prepare.digest(self.cast), self.cast_sha)
                chapter = provenance["scenes"][1]
                self.assertEqual(chapter["source_events"], {"first_event": 1, "last_event": 2})
                self.assertLessEqual(chapter["playback_speed"], speed)
                self.assertEqual(manifest["scenes"][1]["frames"], math.ceil((200 / speed + 1.2) * 30))
                self.assertEqual(len(encodes), 3)
                split = self.output / self.demo["id"] / (self.voices[1]["id"] + ".cast")
                self.assertIn([0.1, "o", "$ orka task list\r\nSucceeded\r\n"],
                              [json.loads(line) for line in split.read_text().splitlines()])
        _, _, encodes = self.render_with_stub_video(self.continuous_dir)
        self.assertEqual(encodes, [], "Unchanged video scenes should be reused")

    def configure_uninterrupted_chapter(self, frames=66):
        self.counts = [1601, frames * 1600, 2407]
        count = sum(self.counts)
        self.samples = struct.pack(f"<{count}h", *(i % 30000 + 1 for i in range(count)))
        self.write_wave(self.source_audio, self.samples)
        self.alignment["version"] = 2
        self.alignment["audio"]["sha256"] = prepare.digest(self.source_audio)
        offset = 0
        for scene, size in zip(self.alignment["scenes"], self.counts):
            scene.update(start_sample=offset, end_sample=offset + size)
            offset += size
        self.alignment["scenes"][1]["continuous_after"] = True
        self.write_alignment()

    def test_uninterrupted_chapter_retains_audio_without_a_silent_splice(self):
        self.configure_uninterrupted_chapter()
        manifest, provenance, _ = self.render_with_stub_video(self.continuous_dir, duration=1)
        self.assertEqual(manifest["scenes"][1]["frames"], 66)
        proof = provenance["continuous_narration"]
        self.assertEqual(proof["scenes"][1]["inserted_silence_samples"], 0)
        self.assertEqual(proof["scenes"][1]["output_end_sample"], proof["scenes"][2]["output_start_sample"])
        self.assert_sample_preservation(manifest["audio"][0], proof, manifest["scenes"])
        wrong = copy.deepcopy(manifest["scenes"])
        wrong[1]["frames"] += 1
        with self.assertRaisesRegex(ValueError, "must not insert silence"):
            prepare.write_continuous_audio(self.read_alignment(), wrong, self.output / self.demo["id"], self.demo["id"])

    def test_uninterrupted_video_must_still_obey_the_speed_limit(self):
        self.configure_uninterrupted_chapter()
        with self.assertRaisesRegex(ValueError, "too short for readable video"):
            self.render_with_stub_video(self.continuous_dir, duration=20)

    def test_rejects_invalid_uninterrupted_joins(self):
        self.configure_uninterrupted_chapter()
        cases = [
            lambda v: v.update(version=1),
            lambda v: v["scenes"][1].update(continuous_after="true"),
            lambda v: (v["scenes"][1].update(end_sample=v["scenes"][1]["end_sample"] + 1),
                       v["scenes"][2].update(start_sample=v["scenes"][2]["start_sample"] + 1)),
            lambda v: v["scenes"][0].update(continuous_after=True),
            lambda v: v["scenes"][-1].update(continuous_after=True),
        ]
        for mutate in cases:
            value = copy.deepcopy(self.alignment)
            mutate(value)
            self.write_alignment(value)
            with self.assertRaises(ValueError):
                self.read_alignment()

    def test_default_render_still_uses_separate_frame_padded_clips(self):
        audio_dir = self.output / "audio"
        audio_dir.mkdir()
        offset = 0
        for voice, count in zip(self.voices, self.counts):
            original = audio_dir / (voice["id"] + ".wav")
            self.write_wave(original, self.samples[offset * 2:(offset + count) * 2])
            prepare.write_json(audio_dir / (voice["id"] + "-request.json"), {"input": voice["text"]})
            prepare.write_json(audio_dir / (voice["id"] + "-metadata.json"),
                               {"text": voice["text"], "near_frame_cap": False,
                                "final": {"sha256": prepare.digest(original)}})
            offset += count
        manifest, provenance, _ = self.render_with_stub_video()
        self.assertEqual(len(manifest["audio"]), len(self.voices))
        self.assertNotIn("continuous_narration", provenance)
        self.assertEqual([scene["frames"] for scene in manifest["scenes"]], [68, 3036, 110])
        sample_offset, frame_offset = 0, 0
        for scene, clip, count in zip(manifest["scenes"], manifest["audio"], self.counts):
            self.assertEqual(clip["record_frame"], frame_offset + 18)
            self.assertEqual(clip["frames"], math.ceil(count / 1600))
            with wave.open(clip["path"], "rb") as audio:
                self.assertEqual(audio.getnframes(), clip["frames"] * 1600)
                rendered = audio.readframes(audio.getnframes())
            self.assertEqual(rendered[:count * 2], self.samples[sample_offset * 2:(sample_offset + count) * 2])
            self.assertEqual(rendered[count * 2:], b"\0\0" * (clip["frames"] * 1600 - count))
            sample_offset += count
            frame_offset += scene["frames"]
        self.assertTrue(all("audio_metadata" in scene for scene in provenance["scenes"]))
        self.assertFalse((self.output / self.demo["id"] / (self.demo["id"] + "-narration.wav")).exists())


if __name__ == "__main__":
    unittest.main()
