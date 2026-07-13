import importlib.util
import json
import subprocess
import unittest
from pathlib import Path

MODULE_PATH = Path(__file__).with_name("fetch_task_events.py")
SPEC = importlib.util.spec_from_file_location("fetch_task_events", MODULE_PATH)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class FakeRunner:
    def __init__(self, pages):
        self.pages = pages
        self.commands = []

    def __call__(self, command, **kwargs):
        self.commands.append((command, kwargs))
        after = int(command[command.index("--after") + 1])
        payload = self.pages[after]
        return subprocess.CompletedProcess(command, 0, json.dumps(payload), "")


class FetchTaskEventsTests(unittest.TestCase):
    def test_fetches_until_latest_sequence(self):
        runner = FakeRunner(
            {
                0: {
                    "namespace": "ns",
                    "streamType": "task",
                    "streamID": "task-a",
                    "afterSeq": 0,
                    "latestSeq": 3,
                    "events": [{"seq": 1}, {"seq": 2}],
                },
                2: {
                    "namespace": "ns",
                    "streamType": "task",
                    "streamID": "task-a",
                    "afterSeq": 2,
                    "latestSeq": 3,
                    "events": [{"seq": 3}],
                },
            }
        )

        payload = MODULE.fetch_task_events("task-a", "ns", runner=runner)

        self.assertEqual([event["seq"] for event in payload["events"]], [1, 2, 3])
        self.assertEqual(payload["latestSeq"], 3)
        self.assertEqual(payload["afterSeq"], 0)
        self.assertEqual(len(runner.commands), 2)
        self.assertIn("1000", runner.commands[0][0])

    def test_accepts_empty_stream(self):
        runner = FakeRunner({0: {"afterSeq": 0, "latestSeq": 0, "events": []}})
        payload = MODULE.fetch_task_events("task-a", "ns", runner=runner)
        self.assertEqual(payload["events"], [])

    def test_rejects_empty_truncated_page(self):
        runner = FakeRunner({0: {"afterSeq": 0, "latestSeq": 2, "events": []}})
        with self.assertRaisesRegex(RuntimeError, "stopped at sequence 0"):
            MODULE.fetch_task_events("task-a", "ns", runner=runner)

    def test_rejects_sequence_gaps(self):
        runner = FakeRunner(
            {0: {"afterSeq": 0, "latestSeq": 2, "events": [{"seq": 2}]}}
        )
        with self.assertRaisesRegex(ValueError, "sequence 2, expected 1"):
            MODULE.fetch_task_events("task-a", "ns", runner=runner)


if __name__ == "__main__":
    unittest.main()
