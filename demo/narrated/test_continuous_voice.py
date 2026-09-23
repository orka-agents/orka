import argparse
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import wave


SPEC = importlib.util.spec_from_file_location("continuous_voice", Path(__file__).with_name("continuous_voice.py"))
voice = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(voice)


class Reply(io.BytesIO):
    status = 200
    headers = {"Content-Type": "audio/wav"}


class ContinuousVoiceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.args = argparse.Namespace(output_dir=self.root, max_frames=6000,
                                       endpoint="http://127.0.0.1:18080/v1/audio/speech", timeout=3600)
        self.plan = voice.make_plan({"id": "01-chat-to-pr", "intro": "First, the scenario.",
                                    "chapters": [{"text": "Now, the work."}], "outro": "Here is the result."})
        self.identity = {"reference_sha256": "a" * 64, "image_id": "b" * 64}
        buffer = io.BytesIO()
        with wave.open(buffer, "wb") as audio:
            audio.setparams((1, 2, 48000, 0, "NONE", "not compressed"))
            audio.writeframes(b"\x10\x00\xf0\xff" * 2400)
        self.wav = buffer.getvalue()

    def normalized_copy(self, source, target):
        target.write_bytes(source.read_bytes())
        return {"test": "already normalized PCM fixture"}

    def test_sends_complete_script_once_and_caches_only_matching_identity(self):
        with patch.object(voice.urllib.request, "urlopen", return_value=Reply(self.wav)) as request, \
                patch.object(voice.generator, "normalize", side_effect=self.normalized_copy):
            metadata = voice.generate(self.plan, self.args, self.identity)
        request.assert_called_once()
        payload = json.loads(request.call_args.args[0].data)
        self.assertEqual(payload["input"], "First, the scenario.\n\nNow, the work.\n\nHere is the result.")
        self.assertEqual(metadata["mode"], "one request per video")
        self.assertEqual(metadata["raw"]["sha256"], metadata["final"]["sha256"])
        with patch.object(voice.urllib.request, "urlopen") as request:
            voice.generate(self.plan, self.args, self.identity)
            request.assert_not_called()
            with self.assertRaisesRegex(ValueError, "Different take exists"):
                voice.generate(self.plan, self.args, {**self.identity, "reference_sha256": "c" * 64})
        self.assertEqual(Path(metadata["raw"]["path"]).read_bytes(), self.wav)

    def test_does_not_repeat_an_unfinished_request(self):
        path = self.root / self.plan["demo_id"] / "request.json"
        path.parent.mkdir()
        path.write_text("{}")
        with patch.object(voice.urllib.request, "urlopen") as request:
            with self.assertRaisesRegex(ValueError, "unfinished take"):
                voice.generate(self.plan, self.args, self.identity)
            request.assert_not_called()

    def test_rejects_modified_voice_reference(self):
        reference, config = self.root / "reference.wav", self.root / "config.yaml"
        reference.write_bytes(self.wav)
        config.write_text("context_size: 8192\n")
        identity = {**self.identity, "reference_path": str(reference),
                    "reference_sha256": voice.digest(reference), "config_path": str(config),
                    "config_sha256": voice.digest(config)}
        path = self.root / "identity.json"
        path.write_text(json.dumps(identity))
        self.assertEqual(voice.checked_identity(path), identity)
        reference.write_bytes(self.wav + b"changed")
        with self.assertRaisesRegex(ValueError, "Voice service input changed"):
            voice.checked_identity(path)

    def test_failure_preserves_single_attempt_evidence(self):
        with patch.object(voice.urllib.request, "urlopen", side_effect=TimeoutError):
            with self.assertRaises(TimeoutError):
                voice.generate(self.plan, self.args, self.identity)
        directory = self.root / self.plan["demo_id"]
        self.assertEqual(json.loads((directory / "request.json").read_text())["input"], self.plan["text"])
        self.assertEqual(json.loads((directory / "request-state.json").read_text())["error_type"], "TimeoutError")
        self.assertFalse((directory / "metadata.json").exists())


if __name__ == "__main__":
    unittest.main()
