"""Local deterministic model and Foundry transport fixture. No Azure calls."""

import argparse
import base64
import hmac
import http.client
import json
import os
import re
import secrets
import select
import socket
import ssl
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit


def encode(value):
    return json.dumps(value, separators=(",", ":")).encode()


def user_text(message):
    content = message.get("content", "")
    if isinstance(content, list):
        return " ".join(part.get("text", "") for part in content if isinstance(part, dict))
    return content if isinstance(content, str) else ""


def model_message(request):
    messages = request.get("messages", [])
    users = [(index, user_text(message)) for index, message in enumerate(messages) if message.get("role") == "user"]
    if not users:
        return {"role": "assistant", "content": "Deterministic fixture ready."}, None
    start, prompt = users[-1]
    match = re.search(r"\brunID[ :=]+([a-z0-9-]{1,63})\b", prompt)
    if not match:
        return {"role": "assistant", "content": "Independent deterministic conversation completed."}, None
    run_id = match.group(1)
    results = [message for message in messages[start + 1:] if message.get("role") == "tool"]
    if results:
        result = user_text(results[-1])
        errors = ("approval_declined", "approval_expired", "approval_cancelled", "approval_stale", "tool_execution_failed", "tool_outcome_unknown")
        if any(code in result for code in errors) or '"isError":true' in result.replace(" ", ""):
            return {"role": "assistant", "content": "Final tool outcome: " + result}, run_id
        if len(results) >= 2 or "Do not call any other tool" in prompt:
            return {"role": "assistant", "content": "Final tool result: " + result}, run_id
    target = "read-inventory" if not results else "create-work-order"
    names = [tool.get("function", {}).get("name", "") for tool in request.get("tools", [])]
    matches = [name for name in names if name.replace("_", "-") == target or name.replace("_", "-").endswith("-" + target)]
    if len(matches) != 1:
        raise ValueError("expected exactly one requested tool schema")
    arguments = {"runID": run_id, "asset": "pump-1"}
    if target == "create-work-order":
        arguments["summary"] = "Inspect the pressure transmitter."
    call = {"id": "fixture-" + secrets.token_hex(8), "type": "function", "function": {"name": matches[0], "arguments": encode(arguments).decode()}}
    return {"role": "assistant", "content": None, "tool_calls": [call]}, run_id


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, status, body=None, content_type="application/json"):
        data = body if isinstance(body, bytes) else encode(body) if body is not None else b""
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def body(self):
        length = int(self.headers.get("Content-Length", "0"))
        if not 0 < length <= 4 * 1024 * 1024:
            raise ValueError("invalid body length")
        raw = self.rfile.read(length)
        return raw, json.loads(raw)

    def handle_request(self):
        path = urlsplit(self.path).path
        if path == "/health" and self.command == "GET":
            return self.send(200, {"simulation": True, "fixture": self.server.mode})
        if self.server.mode == "model":
            return self.model(path)
        return self.gateway(path)

    def do_GET(self):
        self.handle_request()

    def do_POST(self):
        try:
            self.handle_request()
        except (ValueError, TypeError, KeyError):
            self.send(400, {"error": "invalid fixture request"})

    def do_DELETE(self):
        self.handle_request()

    def model(self, path):
        if path == "/counts" and self.command == "GET":
            with self.server.lock:
                return self.send(200, {"simulation": True, "modelRequests": dict(self.server.counts)})
        if self.command != "POST" or path not in {"/v1/chat/completions", "/chat/completions"}:
            return self.send(404)
        _, request = self.body()
        message, run_id = model_message(request)
        with self.server.lock:
            self.server.counts[run_id or "independent"] = self.server.counts.get(run_id or "independent", 0) + 1
        result = {"id": "chatcmpl-" + secrets.token_hex(8), "object": "chat.completion", "created": int(time.time()), "model": "approval-fixture", "choices": [{"index": 0, "message": message, "finish_reason": "tool_calls" if message.get("tool_calls") else "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}
        if request.get("stream"):
            delta = dict(message)
            if delta.get("tool_calls"):
                delta["tool_calls"] = [{"index": index, **call} for index, call in enumerate(delta["tool_calls"])]
            chunk = {key: result[key] for key in ("id", "created", "model")}
            chunk["object"] = "chat.completion.chunk"
            first = {**chunk, "choices": [{"index": 0, "delta": delta, "finish_reason": None}]}
            last = {**chunk, "choices": [{"index": 0, "delta": {}, "finish_reason": result["choices"][0]["finish_reason"]}]}
            return self.send(200, b"data: " + encode(first) + b"\n\ndata: " + encode(last) + b"\n\ndata: [DONE]\n\n", "text/event-stream")
        self.send(200, result)

    def gateway(self, path):
        if path == "/metadata/identity/oauth2/token" and self.command == "GET":
            if not hmac.compare_digest(self.headers.get("X-IDENTITY-HEADER", ""), self.server.identity_header):
                return self.send(403)
            return self.send(200, {"access_token": self.server.access_token, "expires_on": str(int(time.time()) + 3600), "expires_in": "3600", "resource": "https://ai.azure.com", "token_type": "Bearer", "client_id": "local-fixture-client"})
        if not hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer " + self.server.access_token):
            return self.send(401)
        prefix = "/api/projects/fixture/agents/human-approval-hosted"
        if not path.startswith(prefix):
            return self.send(404)
        suffix = path[len(prefix):]
        if suffix == "" and self.command == "GET":
            return self.send(200, {"name": "human-approval-hosted", "agent_endpoint": {"authorization_schemes": [{"type": "entra"}]}})
        if suffix == "/versions/1" and self.command == "GET":
            return self.send(200, {"name": "human-approval-hosted", "version": "1", "status": "active", "definition": {"kind": "hosted"}})
        if suffix == "/endpoint/sessions" and self.command == "POST":
            _, request = self.body()
            session_id = request["agent_session_id"]
            if not session_id or request["version_indicator"] != {"type": "version_ref", "agent_version": "1"}:
                return self.send(400)
            with self.server.lock:
                if session_id in self.server.sessions:
                    return self.send(409)
                self.server.sessions[session_id] = "active"
            return self.send(201, self.session(session_id, "active"))
        if suffix.startswith("/endpoint/sessions/"):
            session_id = suffix.removeprefix("/endpoint/sessions/")
            stop = session_id.endswith(":stop")
            session_id = session_id.removesuffix(":stop")
            with self.server.lock:
                state = self.server.sessions.get(session_id)
                if state is None:
                    return self.send(404)
                if stop and self.command == "POST":
                    self.server.sessions[session_id] = "idle"
                    return self.send(204)
                if self.command == "DELETE":
                    del self.server.sessions[session_id]
                    return self.send(204)
                if self.command == "GET":
                    return self.send(200, self.session(session_id, state))
        if suffix == "/endpoint/protocols/openai/responses" and self.command == "POST":
            raw, request = self.body()
            with self.server.lock:
                if request.get("agent_session_id") not in self.server.sessions:
                    return self.send(404)
                self.server.sessions[request["agent_session_id"]] = "active"
            return self.forward_response(raw)
        self.send(404)

    def forward_response(self, raw):
        endpoint = urlsplit(self.server.hosted_url)
        if endpoint.scheme != "http" or endpoint.hostname not in {"127.0.0.1", "localhost", "::1"}:
            return self.send(502, {"error": "invalid local hosted endpoint"})
        upstream = http.client.HTTPConnection(endpoint.hostname, endpoint.port or 80, timeout=120)
        done = threading.Event()
        disconnected = threading.Event()
        watcher = None
        response = None
        headers_sent = False
        # HTTP/1.0 close-delimited output permits incremental forwarding of
        # decoded upstream chunks without inventing a content length.
        self.close_connection = True
        try:
            upstream.connect()
            upstream_socket = upstream.sock

            def watch_disconnect():
                # The request body is already consumed. A readable EOF now
                # means the broker cancelled, even if upstream is still quiet.
                while not done.is_set():
                    try:
                        ready, _, _ = select.select([self.connection], [], [], 0.05)
                        if not ready:
                            continue
                        # This response closes the connection, so no later
                        # request bytes need preserving. TLS sockets do not
                        # support MSG_PEEK; read also consumes close-notify.
                        if self.connection.recv(1):
                            done.wait(0.05)
                            continue
                    except OSError:
                        if done.is_set():
                            return
                    disconnected.set()
                    try:
                        upstream_socket.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass
                    return

            watcher = threading.Thread(target=watch_disconnect, daemon=True)
            watcher.start()
            upstream.request("POST", endpoint.path.rstrip("/") + "/responses", body=raw,
                             headers={"Content-Type": "application/json", "Accept": self.headers.get("Accept", "application/json"), "Connection": "close"})
            response = upstream.getresponse()
            self.send_response(response.status)
            self.send_header("Content-Type", response.getheader("Content-Type", "application/json"))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Accel-Buffering", "no")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.flush()
            headers_sent = True
            # read1 performs at most one buffered read. read(size) would wait
            # for more bytes and could hide the early response.created event.
            while chunk := response.read1(64 * 1024):
                self.wfile.write(chunk)
                self.wfile.flush()
        except (OSError, http.client.HTTPException):
            if not headers_sent and not disconnected.is_set():
                self.send(502, {"error": "local hosted transport failed"})
        finally:
            done.set()
            if response is not None:
                response.close()
            upstream.close()
            if watcher is not None:
                watcher.join(timeout=1)

    @staticmethod
    def session(session_id, state):
        return {"agent_session_id": session_id, "version_indicator": {"type": "version_ref", "agent_version": "1"}, "status": state}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("model", "gateway"))
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--hosted-url", default="http://127.0.0.1:8088")
    parser.add_argument("--tls-cert")
    parser.add_argument("--tls-key")
    args = parser.parse_args()
    if bool(args.tls_cert) != bool(args.tls_key):
        parser.error("gateway TLS requires both a serving certificate and key")
    if args.tls_cert and args.mode != "gateway":
        parser.error("TLS is only supported for the simulated identity/gateway")
    if args.mode == "gateway" and args.host != "127.0.0.1" and not args.tls_cert:
        parser.error("the simulated identity/gateway requires TLS outside loopback")
    identity_header = os.environ.get("IDENTITY_HEADER", "")
    if args.mode == "gateway" and len(identity_header) < 32:
        parser.error("gateway requires a private IDENTITY_HEADER with at least 32 characters")
    context = None
    if args.tls_cert:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(args.tls_cert, args.tls_key)
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    if context:
        server.socket = context.wrap_socket(server.socket, server_side=True)
    server.mode, server.hosted_url = args.mode, args.hosted_url
    server.lock, server.counts, server.sessions = threading.Lock(), {}, {}
    server.identity_header = identity_header
    claims = {"aud": "https://ai.azure.com", "tid": "local-fixture-tenant", "oid": "local-fixture-principal", "appid": "local-fixture-client"}
    server.access_token = "fixture." + base64.urlsafe_b64encode(encode(claims)).decode().rstrip("=") + "." + secrets.token_urlsafe(32)
    server.serve_forever()


if __name__ == "__main__":
    main()
