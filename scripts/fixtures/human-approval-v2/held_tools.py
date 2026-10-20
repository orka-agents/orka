"""Counted tool fixture with immutable, per-run holds for crash qualification."""

import argparse
from contextlib import closing
import importlib.util
import json
from pathlib import Path
import select
import socket
import sqlite3
import sys
import time
from urllib.parse import parse_qs, urlsplit


def baseline_module():
    path = Path(__file__).with_name("simulated_tools.py")
    if not path.is_file():
        path = Path(__file__).resolve().parents[3] / "examples/human-approval-v2/simulated_tools.py"
    spec = importlib.util.spec_from_file_location("approval_simulated_tools", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class Holds:
    def __init__(self, path):
        self.path = str(path)
        with closing(sqlite3.connect(self.path)) as db, db:
            db.execute("CREATE TABLE IF NOT EXISTS holds (run_id TEXT PRIMARY KEY, configured_at REAL NOT NULL)")
            db.execute("CREATE TABLE IF NOT EXISTS attempts (run_id TEXT NOT NULL, ordinal INTEGER NOT NULL, "
                       "started_at REAL NOT NULL, ended_at REAL, completion TEXT, PRIMARY KEY (run_id, ordinal))")

    def configure(self, run_id):
        with closing(sqlite3.connect(self.path, timeout=10)) as db, db:
            db.execute("BEGIN IMMEDIATE")
            if db.execute("SELECT 1 FROM holds WHERE run_id = ?", (run_id,)).fetchone():
                return
            if db.execute("SELECT 1 FROM calls WHERE run_id = ?", (run_id,)).fetchone():
                raise ValueError("a hold must precede tool admission")
            db.execute("INSERT INTO holds VALUES (?, ?)", (run_id, time.time()))

    def started(self, run_id, ordinal):
        with closing(sqlite3.connect(self.path, timeout=10)) as db, db:
            db.execute("INSERT INTO attempts (run_id, ordinal, started_at) VALUES (?, ?, ?)",
                       (run_id, ordinal, time.time()))
            return db.execute("SELECT 1 FROM holds WHERE run_id = ?", (run_id,)).fetchone() is not None

    def finish(self, run_id, ordinal, completion):
        with closing(sqlite3.connect(self.path, timeout=10)) as db, db:
            db.execute("UPDATE attempts SET ended_at = ?, completion = ? WHERE run_id = ? AND ordinal = ? AND ended_at IS NULL",
                       (time.time(), completion, run_id, ordinal))

    def state(self, run_id):
        with closing(sqlite3.connect(self.path, timeout=10)) as db, db:
            hold = db.execute("SELECT configured_at FROM holds WHERE run_id = ?", (run_id,)).fetchone()
            rows = db.execute("SELECT ordinal, started_at, ended_at, completion FROM attempts WHERE run_id = ? ORDER BY ordinal",
                              (run_id,)).fetchall()
        return {"simulation": True, "runID": run_id, "mode": "hold" if hold else "normal",
                "configuredAt": hold[0] if hold else None, "releasedAt": None, "releaseStatus": None,
                "attempts": [{"ordinal": row[0], "startedAt": row[1], "endedAt": row[2], "completion": row[3],
                              "elapsedSeconds": round(row[2] - row[1], 3) if row[2] is not None else None} for row in rows]}


def make_server(address, state_path, max_hold_seconds=360):
    baseline = baseline_module()
    ledger = baseline.Ledger(state_path)
    holds = Holds(state_path)
    server = baseline.make_server(address, ledger)
    original = server.RequestHandlerClass

    class Handler(original):
        def plain(self, status, value):
            try:
                super().respond(status, value)
            except (BrokenPipeError, ConnectionResetError):
                pass

        def do_GET(self):
            path = urlsplit(self.path)
            if path.path == "/health":
                return self.plain(200, {"simulation": True, "status": "ok", "supportsHold": True})
            if path.path != "/admin/state":
                return super().do_GET()
            run_id = parse_qs(path.query).get("runID", [""])[0]
            if not baseline.RUN_ID.fullmatch(run_id):
                return self.plain(400, {"error": "a valid runID is required"})
            self.plain(200, holds.state(run_id))

        def do_POST(self):
            if urlsplit(self.path).path != "/admin/mode":
                return super().do_POST()
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= 2048:
                    raise ValueError("invalid body length")
                body = json.loads(self.rfile.read(length))
                if (not isinstance(body, dict) or set(body) != {"runID", "mode"} or body["mode"] != "hold" or
                    not isinstance(body["runID"], str) or not baseline.RUN_ID.fullmatch(body["runID"])):
                    raise ValueError("invalid hold request")
                holds.configure(body["runID"])
            except (ValueError, TypeError, KeyError):
                return self.plain(409, {"error": "hold requires an unused runID and cannot change after admission"})
            self.plain(200, holds.state(body["runID"]))

        def respond(self, status, value):
            if self.command != "POST" or urlsplit(self.path).path != "/create-work-order" or status != 200:
                return self.plain(status, value)
            ordinal = value["executionCount"]
            run_id = value["workOrderID"].removeprefix("simulated-").removesuffix("-" + str(ordinal))
            if not holds.started(run_id, ordinal):
                holds.finish(run_id, ordinal, "responded")
                return self.plain(status, value)
            # No release/reset API exists. A genuine client disconnect ends a
            # held execution; the safety timeout prevents an abandoned thread.
            deadline = time.monotonic() + max_hold_seconds
            while time.monotonic() < deadline:
                try:
                    ready, _, _ = select.select([self.connection], [], [], 0.1)
                    if not ready or self.connection.recv(1, socket.MSG_PEEK) != b"":
                        continue
                except OSError:
                    pass
                holds.finish(run_id, ordinal, "client_disconnected")
                self.close_connection = True
                return
            holds.finish(run_id, ordinal, "hold_safety_timeout")
            self.plain(504, {"simulation": True, "error": "held tool reached its fixture safety bound"})

    server.RequestHandlerClass = Handler
    server.daemon_threads = True
    server.handle_error = lambda *_: print("Counted simulator request failed; details suppressed.", file=sys.stderr, flush=True)
    return server


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8099)
    parser.add_argument("--state", type=Path, required=True)
    args = parser.parse_args()
    server = make_server((args.host, args.port), args.state)
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
