"""Exercise the cancellation case through the disposable model HTTP endpoint."""

import http.client
import importlib.util
import json
from pathlib import Path
import select
import socket
import threading
import unittest
from http.server import ThreadingHTTPServer
from unittest.mock import patch


fixture_spec = importlib.util.spec_from_file_location(
    "runtime_fixture", Path(__file__).with_name("runtime_fixture.py"))
fixture = importlib.util.module_from_spec(fixture_spec)
fixture_spec.loader.exec_module(fixture)


def continuation(run_id="ci-foundry-cancel", code="approval_cancelled"):
    return {"messages": [
        {"role": "user", "content": "Propose an action with runID " + run_id},
        {"role": "tool", "content": json.dumps({
            "approved": False, "error": {"code": code, "message": "Denied."}})},
    ]}


class ModelTests(unittest.TestCase):
    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), fixture.Handler)
        self.server.daemon_threads = True
        self.server.mode = "model"
        self.server.lock, self.server.counts = threading.Lock(), {}
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.close_server)

    def close_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.assertFalse(self.thread.is_alive())

    def post(self, payload):
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=2)
        self.addCleanup(connection.close)
        connection.request("POST", "/v1/chat/completions", body=json.dumps(payload),
                           headers={"Content-Type": "application/json"})
        response = connection.getresponse()
        return response.status, json.loads(response.read())

    def test_hold_requires_current_structured_cancellation_denial(self):
        self.assertTrue(fixture.holds_cancellation_continuation(
            continuation(), "ci-foundry-cancel"))
        cases = [
            (continuation("ci-foundry-decline"), "ci-foundry-decline"),
            (continuation(code="approval_declined"), "ci-foundry-cancel"),
            ({"messages": [{"role": "user", "content": "runID ci-foundry-cancel"},
                           {"role": "tool", "content": "approval_cancelled"}]}, "ci-foundry-cancel"),
            ({"messages": [{"role": "user", "content": "runID ci-foundry-cancel"},
                           {"role": "tool", "content": json.dumps({"approved": True,
                               "error": {"code": "approval_cancelled"}})}]}, "ci-foundry-cancel"),
            ({"messages": [*continuation()["messages"],
                           {"role": "user", "content": "runID ci-foundry-cancel"}]}, "ci-foundry-cancel"),
            ({"messages": [*continuation()["messages"],
                           {"role": "tool", "content": json.dumps({"approved": True})}]}, "ci-foundry-cancel"),
        ]
        for request, run_id in cases:
            with self.subTest(request=request):
                self.assertFalse(fixture.holds_cancellation_continuation(request, run_id))

    def test_held_continuation_waits_for_actual_client_disconnect(self):
        entered, disconnected = threading.Event(), threading.Event()
        observed = []
        original = fixture.wait_for_model_disconnect

        def observe(connection, timeout):
            entered.set()
            result = original(connection, timeout)
            observed.append(result)
            disconnected.set()
            return result

        connection = socket.create_connection(self.server.server_address, timeout=2)
        self.addCleanup(connection.close)
        payload = json.dumps(continuation()).encode()
        headers = ("POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\n"
                   "Content-Type: application/json\r\nContent-Length: " +
                   str(len(payload)) + "\r\nConnection: close\r\n\r\n").encode()
        with patch.object(fixture, "wait_for_model_disconnect", observe):
            connection.sendall(headers + payload)
            self.assertTrue(entered.wait(2), "cancellation continuation was not held")
            self.assertFalse(disconnected.is_set(), "hold ended before the client cancelled")
            self.assertEqual(select.select([connection], [], [], 0)[0], [],
                             "fixture emitted a model result while cancellation was pending")
            connection.close()
            self.assertTrue(disconnected.wait(2), "client disconnect did not release the hold")
        self.assertEqual(observed, [True])
        self.assertEqual(self.server.counts, {"ci-foundry-cancel": 1})

    def test_missing_cancellation_fails_at_the_safety_deadline(self):
        with patch.object(fixture, "CANCELLATION_CONTINUATION_TIMEOUT_SECONDS", 0.05):
            status, result = self.post(continuation())
        self.assertEqual(status, 504)
        self.assertEqual(result, {"error": "cancellation continuation did not disconnect"})
        self.assertNotIn("choices", result)

    def test_other_denials_retain_normal_completed_model_response(self):
        for run_id, code in [("ci-foundry-decline", "approval_declined"),
                             ("ci-foundry-expire", "approval_expired"),
                             ("other-denial", "approval_cancelled")]:
            with self.subTest(run_id=run_id):
                status, result = self.post(continuation(run_id, code))
                self.assertEqual(status, 200)
                choice = result["choices"][0]
                self.assertEqual(choice["finish_reason"], "stop")
                self.assertIn(code, choice["message"]["content"])


if __name__ == "__main__":
    unittest.main()
