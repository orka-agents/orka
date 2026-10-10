#!/usr/bin/env python3
"""Exercise the exact-source patch without downloading or running a model."""
import pathlib
import subprocess
import tempfile
import unittest

PATCH = pathlib.Path(__file__).with_name("patch.py")
GRPC_ANCHOR = '#endif\n                task.id_slot = json_value(data, "id_slot", -1);'
CONTEXT_ANCHOR = '''                        if (!is_user_start && !is_score_boundary && !near_prompt_end) {
                            do_checkpoint = false;
                        }'''


class CheckpointPatchTest(unittest.TestCase):
    def prepare(self, root):
        backend = root / "backend/cpp/llama-cpp"
        server = backend / "llama.cpp/tools/server"
        staged = backend / "llama.cpp/tools/grpc-server"
        server.mkdir(parents=True)
        staged.mkdir(parents=True)
        grpc = backend / "grpc-server.cpp"
        context = server / "server-context.cpp"
        grpc.write_text(GRPC_ANCHOR + "\n" + GRPC_ANCHOR)
        context.write_text(CONTEXT_ANCHOR)
        (staged / "grpc-server.cpp").write_text(grpc.read_text())
        (staged / "server-context.cpp").write_text(context.read_text())
        return grpc, context, staged

    def run_patch(self, root):
        return subprocess.run(
            ["python3", str(PATCH), str(root)], capture_output=True, check=False
        )

    def test_both_completion_paths_and_staged_sources(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            grpc, context, staged = self.prepare(root)
            self.assertEqual(self.run_patch(root).returncode, 0)
            self.assertEqual(grpc.read_text().count("task.params.message_spans ="), 2)
            self.assertIn("slot.task->type != SERVER_TASK_TYPE_COMPLETION", context.read_text())
            self.assertIn("!is_score_boundary", context.read_text())
            self.assertEqual(grpc.read_text(), (staged / grpc.name).read_text())
            self.assertEqual(context.read_text(), (staged / context.name).read_text())
            self.assertNotEqual(self.run_patch(root).returncode, 0)

    def test_mismatch_does_not_partially_patch(self):
        for target in ("grpc", "context", "staged"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                grpc, context, staged = self.prepare(root)
                if target == "grpc":
                    grpc.write_text(GRPC_ANCHOR)
                elif target == "context":
                    context.write_text("unexpected source")
                else:
                    (staged / "server-context.cpp").unlink()
                before = grpc.read_text(), context.read_text()
                self.assertNotEqual(self.run_patch(root).returncode, 0)
                self.assertEqual(before, (grpc.read_text(), context.read_text()))


if __name__ == "__main__":
    unittest.main()
