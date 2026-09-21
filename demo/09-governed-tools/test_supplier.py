#!/usr/bin/env python3
"""Exercise the real HTTP service, including the order endpoint the demo blocks."""

import http.client
import json
import secrets
import tempfile
import threading
import unittest
from pathlib import Path

from supplier import Supplier


class SupplierTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.credential = secrets.token_hex(32)
        credential_file = Path(self.directory.name) / "credential"
        credential_file.write_text(self.credential)
        self.server = Supplier(("127.0.0.1", 0), credential_file)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.directory.cleanup()

    def request(self, method, path, body=None, authenticated=False):
        connection = http.client.HTTPConnection(*self.server.server_address, timeout=5)
        headers = {"Content-Type": "application/json"}
        if authenticated:
            headers["Authorization"] = "Bearer " + self.credential
        connection.request(method, path, body=json.dumps(body) if body is not None else None, headers=headers)
        response = connection.getresponse()
        payload = response.read().decode()
        connection.close()
        self.assertTrue(self.credential not in payload, "response exposed a credential")
        return response.status, json.loads(payload)

    def test_stock_is_authenticated_and_receipted(self):
        path = "/v1/stock?run=lookup-test&item=replacement-filter"
        self.assertEqual(self.request("GET", path)[0], 401)
        status, stock = self.request("GET", path, authenticated=True)
        self.assertEqual(status, 200)
        self.assertEqual(stock["available"], 32)
        _, evidence = self.request("GET", "/receipts?run=lookup-test")
        self.assertEqual([r["credentialAccepted"] for r in evidence["receipts"]], [False, True])
        self.assertEqual(evidence["ordersCreated"], 0)

    def test_order_endpoint_really_can_create_an_order(self):
        payload = {"item": "replacement-filter", "quantity": 20}
        status, result = self.request("POST", "/v1/orders?run=order-test", payload, authenticated=True)
        self.assertEqual(status, 201)
        self.assertTrue(result["orderCreated"])
        _, evidence = self.request("GET", "/receipts?run=order-test")
        self.assertEqual(evidence["orderRequests"], 1)
        self.assertEqual(evidence["ordersCreated"], 1)

    def test_failed_auth_still_records_that_an_order_reached_the_backend(self):
        self.assertEqual(self.request("POST", "/v1/orders?run=denied-test", {"item": "replacement-filter", "quantity": 20})[0], 401)
        _, evidence = self.request("GET", "/receipts?run=denied-test")
        self.assertEqual(evidence["orderRequests"], 1)
        self.assertEqual(evidence["ordersCreated"], 0)
        self.assertFalse(evidence["receipts"][0]["credentialAccepted"])

    def test_other_runs_do_not_disappear_from_global_order_counts(self):
        self.request("POST", "/v1/orders?run=other-run", {"item": "replacement-filter", "quantity": 20}, authenticated=True)
        _, evidence = self.request("GET", "/receipts?run=current-run")
        self.assertEqual(evidence["receipts"], [])
        self.assertEqual(evidence["totalOrderRequests"], 1)
        self.assertEqual(evidence["totalOrdersCreated"], 1)

    def test_invalid_quantity_does_not_create_order(self):
        for value in (True, 0, 101, "20"):
            self.assertEqual(self.request("POST", "/v1/orders?run=bad-quantity", {"item": "replacement-filter", "quantity": value}, authenticated=True)[0], 400)
        _, evidence = self.request("GET", "/receipts?run=bad-quantity")
        self.assertEqual(evidence["orderRequests"], 4)
        self.assertEqual(evidence["ordersCreated"], 0)


if __name__ == "__main__":
    unittest.main()
