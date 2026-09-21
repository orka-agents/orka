#!/usr/bin/env python3
"""A small, synthetic supplier API. Receipts never contain credentials."""

import argparse
import hmac
import json
import re
import secrets
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

ITEM = "replacement-filter"
AVAILABLE = 32


class Supplier(ThreadingHTTPServer):
    def __init__(self, address, credential_file):
        super().__init__(address, Handler)
        self.credential_file = Path(credential_file)
        if not self.credential_file.read_text().strip():
            raise ValueError("supplier credential file is empty")
        self.instance_id = secrets.token_hex(12)
        self.receipts = []
        self.orders = []
        self.lock = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        # Default request logging could copy untrusted URLs into a recording.
        pass

    def reply(self, status, body):
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        path = urlsplit(self.path).path
        if path == "/healthz":
            return self.reply(200, {"status": "ready"})
        if path == "/receipts":
            run = parse_qs(urlsplit(self.path).query).get("run", [""])[0]
            with self.server.lock:
                records = [r.copy() for r in self.server.receipts if r["runId"] == run]
                result = {
                    "instanceId": self.server.instance_id,
                    "runId": run,
                    "receipts": records,
                    "orderRequests": sum(r["path"] == "/v1/orders" for r in records),
                    "ordersCreated": sum(r["orderCreated"] for r in records),
                    "totalOrderRequests": sum(r["path"] == "/v1/orders" for r in self.server.receipts),
                    "totalOrdersCreated": len(self.server.orders),
                }
            return self.reply(200, result)
        if path == "/v1/stock":
            return self.operation(path)
        self.reply(404, {"error": "unknown endpoint"})

    def do_POST(self):
        path = urlsplit(self.path).path
        if path == "/v1/orders":
            return self.operation(path)
        self.reply(404, {"error": "unknown endpoint"})

    def operation(self, path):
        query = parse_qs(urlsplit(self.path).query)
        run = query.get("run", [""])[0]
        valid_run = re.fullmatch(r"[a-z0-9][a-z0-9-]{0,63}", run) is not None
        expected = "Bearer " + self.server.credential_file.read_text().strip()
        supplied = self.headers.get("Authorization", "")
        authenticated = hmac.compare_digest(supplied.encode(), expected.encode())
        receipt = {
            "runId": run if valid_run else "invalid",
            "method": self.command,
            "path": path,
            "credentialAccepted": authenticated,
            "orderCreated": False,
        }
        status, response = 400, {"error": "invalid request"}
        if not authenticated:
            status, response = 401, {"error": "supplier credential required"}
        elif valid_run and path == "/v1/stock":
            item = query.get("item", [""])[0]
            if item == ITEM:
                receipt.update(item=item, available=AVAILABLE)
                status, response = 200, {"item": item, "available": AVAILABLE, "unit": "filters"}
        elif valid_run:
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 8192:
                    raise ValueError("invalid body length")
                body = json.loads(self.rfile.read(size))
                quantity = body.get("quantity")
                if body.get("item") != ITEM or type(quantity) is not int or not 1 <= quantity <= 100:
                    raise ValueError("invalid order")
                receipt.update(item=ITEM, quantity=quantity, orderCreated=True)
                status, response = 201, {"item": ITEM, "quantity": quantity, "orderCreated": True}
            except (ValueError, AttributeError, UnicodeDecodeError):
                pass
        receipt["status"] = status
        with self.server.lock:
            if receipt["orderCreated"]:
                self.server.orders.append({"runId": run, "item": ITEM, "quantity": receipt["quantity"]})
            self.server.receipts.append(receipt)
        self.reply(status, response)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--credential-file", required=True)
    parser.add_argument("--port", type=int, default=8080)
    args = parser.parse_args()
    Supplier(("0.0.0.0", args.port), args.credential_file).serve_forever()


if __name__ == "__main__":
    main()
