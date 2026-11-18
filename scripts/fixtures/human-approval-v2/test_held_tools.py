import http.client
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest

import held_tools


class HeldToolTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.path = Path(self.directory.name) / "calls.sqlite"
        self.server = held_tools.make_server(("127.0.0.1", 0), self.path, max_hold_seconds=3)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.connections = []

    def tearDown(self):
        for connection in self.connections:
            connection.close()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)
        self.directory.cleanup()

    def request(self, path, body=None, read=True):
        connection = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)
        self.connections.append(connection)
        connection.request("GET" if body is None else "POST", path,
                           None if body is None else json.dumps(body), {"Content-Type": "application/json"})
        if not read:
            return connection
        response = connection.getresponse()
        value = json.loads(response.read())
        return response.status, value

    def state(self, run_id, ended=False):
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            _, value = self.request("/admin/state?runID=" + run_id)
            if value["attempts"] and (not ended or value["attempts"][0]["endedAt"] is not None):
                return value
            time.sleep(0.01)
        self.fail("held call was not observed")

    def test_hold_counts_once_and_records_real_disconnect_without_release(self):
        run_id = "held-disconnect"
        self.assertEqual(self.request("/admin/mode", {"runID": run_id, "mode": "hold"})[0], 200)
        connection = self.request("/create-work-order", {"runID": run_id, "asset": "pump-1",
            "summary": "Inspect the pressure transmitter."}, read=False)
        before = self.state(run_id)
        self.assertEqual(before["mode"], "hold")
        self.assertEqual(before["attempts"][0]["ordinal"], 1)
        self.assertIsNone(before["attempts"][0]["endedAt"])
        connection.close()
        after = self.state(run_id, ended=True)
        self.assertEqual(after["attempts"][0]["completion"], "client_disconnected")
        self.assertIsNone(after["releasedAt"])
        self.assertEqual(self.request("/counts?runID=" + run_id)[1]["workOrderExecutions"], 1)
        self.assertEqual(held_tools.Holds(self.path).state(run_id), after)
        self.assertEqual(self.request("/admin/release", {"runID": run_id})[0], 404)

    def test_hold_cannot_be_configured_after_tool_admission(self):
        self.assertEqual(self.request("/read-inventory", {"runID": "already-started", "asset": "pump-1"})[0], 200)
        self.assertEqual(self.request("/admin/mode", {"runID": "already-started", "mode": "hold"})[0], 409)

    def test_normal_execution_remains_counted_and_immediate(self):
        status, value = self.request("/create-work-order", {"runID": "normal", "asset": "pump-1",
            "summary": "Inspect the pressure transmitter."})
        self.assertEqual(status, 200)
        self.assertEqual(value["executionCount"], 1)
        self.assertEqual(self.state("normal", ended=True)["attempts"][0]["completion"], "responded")


if __name__ == "__main__":
    unittest.main()
