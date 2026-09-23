#!/usr/bin/env python3
"""Count harmless tool calls in SQLite so a retry cannot hide duplicate execution."""

import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import re
import sqlite3
from urllib.parse import parse_qs, urlsplit


RUN_ID = re.compile(r"[a-z0-9-]{1,63}\Z")
SUMMARY = "Inspect the pressure transmitter."


class Ledger:
    def __init__(self, path):
        self.path = str(path)
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        with sqlite3.connect(self.path) as connection:
            connection.execute(
                "CREATE TABLE IF NOT EXISTS calls "
                "(id INTEGER PRIMARY KEY, run_id TEXT NOT NULL, tool TEXT NOT NULL)"
            )

    def record(self, run_id, tool):
        with sqlite3.connect(self.path, timeout=10) as connection:
            connection.execute("BEGIN IMMEDIATE")
            connection.execute("INSERT INTO calls (run_id, tool) VALUES (?, ?)", (run_id, tool))
            return connection.execute(
                "SELECT COUNT(*) FROM calls WHERE run_id = ? AND tool = ?", (run_id, tool)
            ).fetchone()[0]

    def counts(self, run_id):
        with sqlite3.connect(self.path, timeout=10) as connection:
            rows = dict(connection.execute(
                "SELECT tool, COUNT(*) FROM calls WHERE run_id = ? GROUP BY tool", (run_id,)
            ))
        executions = rows.get("create-work-order", 0)
        return {
            "simulation": True,
            "runID": run_id,
            "inventoryReads": rows.get("read-inventory", 0),
            "workOrderExecutions": executions,
            "workOrderIDs": [f"simulated-{run_id}-{index}" for index in range(1, executions + 1)],
        }


def make_server(address, ledger):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, _format, *_args):
            pass

        def respond(self, status, value):
            data = json.dumps(value, separators=(",", ":")).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            path = urlsplit(self.path)
            if path.path == "/health":
                self.respond(200, {"simulation": True, "status": "ok"})
                return
            run_id = parse_qs(path.query).get("runID", [""])[0]
            if path.path != "/counts" or not RUN_ID.fullmatch(run_id):
                self.respond(400, {"error": "a valid runID is required at /counts"})
                return
            self.respond(200, ledger.counts(run_id))

        def do_POST(self):
            tool = urlsplit(self.path).path.removeprefix("/")
            if tool not in {"read-inventory", "create-work-order"}:
                self.respond(404, {"error": "unknown simulated tool"})
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= 16384:
                    raise ValueError("invalid body length")
                arguments = json.loads(self.rfile.read(length))
                required = {"runID", "asset"}
                if tool == "create-work-order":
                    required.add("summary")
                if not isinstance(arguments, dict) or set(arguments) != required:
                    raise ValueError("invalid fields")
                run_id = arguments["runID"]
                if not isinstance(run_id, str) or not RUN_ID.fullmatch(run_id):
                    raise ValueError("invalid run ID")
                if arguments["asset"] != "pump-1":
                    raise ValueError("invalid asset")
                if tool == "create-work-order" and arguments["summary"] != SUMMARY:
                    raise ValueError("invalid summary")
            except (KeyError, TypeError, ValueError):
                self.respond(400, {"error": "arguments do not match the simulated tool schema"})
                return
            count = ledger.record(run_id, tool)
            if tool == "read-inventory":
                result = {"simulation": True, "asset": "pump-1", "available": True, "inventoryReads": count}
            else:
                result = {"simulation": True, "workOrderID": f"simulated-{run_id}-{count}", "executionCount": count}
            self.respond(200, result)

    return ThreadingHTTPServer(address, Handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8099)
    parser.add_argument("--state", type=Path, required=True)
    args = parser.parse_args()
    server = make_server((args.host, args.port), Ledger(args.state))
    print(f"Simulated tools listening on {args.host}:{server.server_port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
