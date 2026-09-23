import json
from pathlib import Path
import tempfile
import threading
import unittest
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from simulated_tools import Ledger, SUMMARY, make_server


class SimulatedToolsTest(unittest.TestCase):
    def test_counts_survive_restart_and_expose_duplicate_execution(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "calls.sqlite"
            for round_index in range(2):
                server = make_server(("127.0.0.1", 0), Ledger(state))
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                base = f"http://127.0.0.1:{server.server_port}"
                try:
                    if round_index == 0:
                        self.call(base + "/read-inventory", {"runID": "test-1", "asset": "pump-1"})
                    before = self.call(base + "/counts?runID=test-1")
                    self.assertEqual(before["inventoryReads"], 1)
                    self.assertEqual(before["workOrderExecutions"], round_index)
                    receipt = self.call(base + "/create-work-order", {
                        "runID": "test-1", "asset": "pump-1", "summary": SUMMARY,
                    })
                    self.assertEqual(receipt["executionCount"], round_index + 1)
                    self.assertEqual(receipt["workOrderID"], f"simulated-test-1-{round_index + 1}")
                finally:
                    server.shutdown()
                    server.server_close()
                    thread.join()
            self.assertEqual(Ledger(state).counts("test-1")["workOrderExecutions"], 2)

    def test_invalid_call_never_reaches_execution_counter(self):
        with tempfile.TemporaryDirectory() as directory:
            ledger = Ledger(Path(directory) / "calls.sqlite")
            server = make_server(("127.0.0.1", 0), ledger)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                for invalid in [
                    {"runID": "test-1", "asset": "real-equipment", "summary": SUMMARY},
                    {"runID": "test-1", "asset": "pump-1", "summary": "Change real equipment"},
                    {"runID": "test-1", "asset": "pump-1", "summary": SUMMARY, "extra": "unexpected"},
                ]:
                    with self.assertRaises(HTTPError) as failure:
                        self.call(f"http://127.0.0.1:{server.server_port}/create-work-order", invalid)
                    self.assertEqual(failure.exception.code, 400)
                    failure.exception.close()
                self.assertEqual(ledger.counts("test-1")["workOrderExecutions"], 0)
            finally:
                server.shutdown()
                server.server_close()
                thread.join()

    @staticmethod
    def call(url, body=None):
        request = Request(url, data=None if body is None else json.dumps(body).encode(),
                          headers={"Content-Type": "application/json"})
        with urlopen(request, timeout=5) as response:
            return json.load(response)


if __name__ == "__main__":
    unittest.main()
