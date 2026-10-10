"""Run with python3 -B -m unittest discover -s workers/acp/images/agentkit -v."""

import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import unittest


HERE = Path(__file__).resolve().parent
DIGEST = "a" * 64


class DockerfileTests(unittest.TestCase):
    def test_final_stage_never_executes_source_image(self):
        text = (HERE / "Dockerfile").read_text()
        stages = re.split(r"(?m)^FROM ", text)[1:]
        final = stages[-1]
        self.assertFalse(re.search(r"(?m)^RUN\s", final), "final runtime must have zero RUN instructions")
        self.assertIn("AS runtime-source", text)
        self.assertRegex(text, r"FROM supervisor-builder AS runtime-preparer")
        self.assertRegex(text, r"RUN --mount=from=runtime-source,target=/runtime(?:,ro)?\s")
        self.assertIn("COPY --from=runtime-preparer /out/prepared/ /", final)
        self.assertNotRegex(final, r"COPY --from=runtime-source\s+/\s")


@unittest.skipUnless(os.geteuid() == 0, "ownership normalization requires root, as in the builder")
class OverlayTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "runtime"
        self.overlay = Path(self.temp.name) / "prepared"
        for name in ("opt/agentkit/bin", "agent", "sessions", "usr/share/licenses/orka", "usr/local/bin"):
            (self.root / name).mkdir(parents=True, exist_ok=True)
        for name, mode in (("opt", 0o751), ("usr", 0o711), ("usr/share", 0o750), ("usr/share/licenses", 0o755)):
            path = self.root / name
            path.chmod(mode)
            os.chown(path, 123, 456)
        serve = self.root / "opt/agentkit/bin/agentkit-serve"
        serve.write_text("#!/usr/local/bin/python\n")
        serve.chmod(0o711)
        config = self.root / "agent/agent.yaml"
        config.write_text("agent: fixture\n")
        config.chmod(0o600)
        (self.root / "agent/extra").write_text("keep agent content")
        (self.root / "sessions/existing").write_text("keep session content")
        (self.root / "usr/share/licenses/orka/existing").write_text("keep license content")
        (self.root / "opt/unrelated").write_text("never overlay this")
        (self.root / "usr/unrelated").write_text("never overlay this")
        python = self.root / "usr/local/bin/python3.12"
        python.write_text("fixture executable; never executed")
        python.chmod(0o755)
        (self.root / "usr/local/bin/python").symlink_to("python3.12")
        (self.root / "opt/agentkit/bin/python").symlink_to("/usr/local/bin/python")
        os.chown(self.root / "opt/agentkit/bin/agentkit-serve", 123, 456)

    def prepare(self, image=None, adapter=None):
        helper = HERE / "prepare-overlay.sh"
        self.assertTrue(helper.is_file(), "build-stage overlay helper is missing")
        return subprocess.run(
            ["sh", str(helper), str(self.root), str(self.overlay)],
            env={**os.environ, "AGENTKIT_RUNTIME_IMAGE": image or f"example.invalid/runtime@sha256:{DIGEST}",
                 "AGENTKIT_ADAPTER_DIGEST": adapter or f"sha256:{DIGEST}"},
            capture_output=True, text=True,
        )

    def test_overlay_preserves_contents_and_inherited_parent_metadata(self):
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in ("opt", "usr", "usr/share", "usr/share/licenses"):
            metadata = (self.overlay / name).stat()
            original = (self.root / name).stat()
            self.assertEqual((metadata.st_uid, metadata.st_gid, stat.S_IMODE(metadata.st_mode)),
                             (original.st_uid, original.st_gid, stat.S_IMODE(original.st_mode)), name)
        for name, mode in (("agent", 0o555), ("agent/agent.yaml", 0o444), ("sessions", 0o711),
                           ("opt/agentkit/bin/agentkit-serve", 0o755)):
            self.assertEqual(stat.S_IMODE((self.overlay / name).stat().st_mode), mode, name)
        self.assertEqual((self.overlay / "opt/agentkit/bin/agentkit-serve").stat().st_uid, 0)
        for name in ("agent/extra", "sessions/existing", "usr/share/licenses/orka/existing"):
            self.assertEqual((self.overlay / name).read_bytes(), (self.root / name).read_bytes())
        self.assertEqual(os.readlink(self.overlay / "opt/agentkit/bin/python"), "/usr/local/bin/python")
        for name in ("opt/unrelated", "usr/unrelated", "usr/local"):
            self.assertFalse((self.overlay / name).exists(), name)
        self.assertEqual(stat.S_IMODE((self.root / "agent/agent.yaml").stat().st_mode), 0o600)

    def test_absolute_executable_symlink_uses_source_not_builder(self):
        serve = self.root / "opt/agentkit/bin/agentkit-serve"
        serve.unlink()
        serve.symlink_to("/usr/local/bin/python")
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(os.readlink(self.overlay / "opt/agentkit/bin/agentkit-serve"), "/usr/local/bin/python")

    def test_missing_optional_directories_are_created_with_original_modes(self):
        (self.root / "sessions/existing").unlink()
        (self.root / "sessions").rmdir()
        (self.root / "usr/share/licenses/orka/existing").unlink()
        (self.root / "usr/share/licenses/orka").rmdir()
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        for name, mode in (("sessions", 0o711), ("usr/share/licenses/orka", 0o755)):
            metadata = (self.overlay / name).stat()
            self.assertEqual((metadata.st_uid, metadata.st_gid, stat.S_IMODE(metadata.st_mode)),
                             (0, 0, mode), name)

    def test_config_symlink_inside_agent_preserves_link_and_normalizes_target(self):
        config = self.root / "agent/agent.yaml"
        config.rename(self.root / "agent/config.yaml")
        config.symlink_to("config.yaml")
        os.chown(config, 123, 456, follow_symlinks=False)
        os.chown(self.root / "agent/config.yaml", 123, 456)
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        copied_link = self.overlay / "agent/agent.yaml"
        self.assertEqual(os.readlink(copied_link), "config.yaml")
        self.assertEqual((copied_link.lstat().st_uid, copied_link.lstat().st_gid), (123, 456))
        target = (self.overlay / "agent/config.yaml").stat()
        self.assertEqual((target.st_uid, target.st_gid, stat.S_IMODE(target.st_mode)), (0, 0, 0o444))
        self.assertEqual((self.root / "agent/config.yaml").stat().st_uid, 123)

    def test_relative_parent_traversal_is_clamped_to_source_root(self):
        serve = self.root / "opt/agentkit/bin/agentkit-serve"
        serve.unlink()
        serve.symlink_to("../../../../../../usr/local/bin/python")
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(os.readlink(self.overlay / "opt/agentkit/bin/agentkit-serve"),
                         "../../../../../../usr/local/bin/python")

    def test_absolute_parent_traversal_cannot_use_builder_executable(self):
        serve = self.root / "opt/agentkit/bin/agentkit-serve"
        serve.unlink()
        serve.symlink_to("/../../../../bin/sh")
        (self.root / "bin").mkdir()
        (self.root / "bin/sh").write_text("not executable")
        result = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("agentkit-serve must be executable", result.stderr)

    def test_source_nonexecutable_is_not_hidden_by_builder_executable(self):
        serve = self.root / "opt/agentkit/bin/agentkit-serve"
        serve.unlink()
        serve.symlink_to("/bin/sh")
        (self.root / "bin").mkdir()
        (self.root / "bin/sh").write_text("not executable")
        result = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("agentkit-serve must be executable", result.stderr)

    def test_empty_config_is_rejected(self):
        (self.root / "agent/agent.yaml").write_text("")
        result = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("agent.yaml must be nonempty", result.stderr)

    def test_invalid_or_mismatched_digests_are_rejected(self):
        cases = [("example.invalid/runtime:latest", f"sha256:{DIGEST}"),
                 (f"runtime@sha256:{'A' * 64}", f"sha256:{'A' * 64}"),
                 (f"runtime@sha256:{'a' * 63}", f"sha256:{DIGEST}"),
                 (f"runtime@sha256:{DIGEST}", f"sha256:{'b' * 64}")]
        for image, adapter in cases:
            with self.subTest(image=image, adapter=adapter):
                result = self.prepare(image, adapter)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("digest", result.stderr)

    def test_symlinked_overlay_parent_is_rejected_without_builder_mutation(self):
        (self.root / "opt/agentkit/bin/agentkit-serve").unlink()
        (self.root / "opt/agentkit/bin/python").unlink()
        (self.root / "opt/agentkit/bin").rmdir()
        (self.root / "opt/agentkit").rmdir()
        (self.root / "opt/unrelated").unlink()
        (self.root / "opt").rmdir()
        (self.root / "opt").symlink_to("/usr")
        result = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("overlay directory must not be a symlink", result.stderr)


if __name__ == "__main__":
    unittest.main()
