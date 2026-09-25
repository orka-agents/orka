"""Exercise the local gateway against AgentKit's hosted streaming implementation.

Run with the pinned AgentKit common package installed.
All listeners are disposable loopback endpoints; no cluster or provider is used.
"""

import asyncio
import http.client
import importlib.util
import io
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from contextlib import redirect_stderr
from http.server import ThreadingHTTPServer
from unittest.mock import patch

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
import uvicorn

from agentkit_serve_common.foundry_streaming import brokered_stream_response
from prepare import prepare_gateway_tls


fixture_path = Path(__file__).with_name("runtime_fixture.py")
fixture_spec = importlib.util.spec_from_file_location("runtime_fixture", fixture_path)
fixture = importlib.util.module_from_spec(fixture_spec)
fixture_spec.loader.exec_module(fixture)


class GatewayTests(unittest.TestCase):
    tls = False

    @classmethod
    def setUpClass(cls):
        cls.tls_directory = None
        if cls.tls:
            cls.tls_directory = tempfile.TemporaryDirectory()
            cls.addClassCleanup(cls.tls_directory.cleanup)
            cls.tls_path = Path(cls.tls_directory.name) / "gateway-tls"
            prepare_gateway_tls(cls.tls_path)

    def setUp(self):
        self.release = threading.Event()
        self.started = threading.Event()
        self.cancelled = threading.Event()
        self.before_headers = threading.Event()
        self.disconnected_before_headers = threading.Event()
        self.requests = 0
        app = FastAPI()

        @app.post("/responses")
        async def responses(request: Request):
            payload = await request.json()
            self.requests += 1
            if payload.get("reject"):
                return JSONResponse({"error": "fixture rejection"}, status_code=409)
            if payload.get("wait_before_headers"):
                self.before_headers.set()
                while not self.release.is_set():
                    if await request.is_disconnected():
                        self.disconnected_before_headers.set()
                        return JSONResponse({"cancelled": True})
                    await asyncio.sleep(0.01)

            async def operation(stream):
                stream.session_id = payload["agent_session_id"]
                await stream.accept("resp_fixture_gateway")
                self.started.set()
                try:
                    while not self.release.is_set():
                        await asyncio.sleep(0.01)
                except asyncio.CancelledError:
                    self.cancelled.set()
                    raise
                return JSONResponse({**stream.created.result(), "status": "completed"})

            return await brokered_stream_response("gateway-fixture", operation)

        self.listener = socket.socket()
        self.listener.bind(("127.0.0.1", 0))
        self.listener.listen()
        self.hosted = uvicorn.Server(uvicorn.Config(app, log_level="error", access_log=False))
        self.hosted_thread = threading.Thread(
            target=self.hosted.run, kwargs={"sockets": [self.listener]}, daemon=True
        )
        self.hosted_thread.start()
        deadline = time.monotonic() + 3
        while not self.hosted.started and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(self.hosted.started)
        self.gateway = ThreadingHTTPServer(("127.0.0.1", 0), fixture.Handler)
        if self.tls:
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.minimum_version = ssl.TLSVersion.TLSv1_2
            context.load_cert_chain(self.tls_path / "tls.crt", self.tls_path / "tls.key")
            self.gateway.socket = context.wrap_socket(self.gateway.socket, server_side=True)
        self.gateway.mode = "gateway"
        self.gateway.hosted_url = f"http://127.0.0.1:{self.listener.getsockname()[1]}"
        self.gateway.lock = threading.Lock()
        self.gateway.sessions = {"gateway-test-session": "active"}
        self.gateway.identity_header = secrets.token_urlsafe(32)
        self.gateway.access_token = secrets.token_urlsafe(32)
        self.gateway_thread = threading.Thread(target=self.gateway.serve_forever, daemon=True)
        self.gateway_thread.start()
        self.connections = []
        self.responses = []

    def tearDown(self):
        self.release.set()
        for response in self.responses:
            response.close()
        for connection in self.connections:
            connection.close()
        self.gateway.shutdown()
        self.gateway.server_close()
        self.gateway_thread.join(timeout=3)
        self.hosted.should_exit = True
        self.hosted_thread.join(timeout=3)
        self.listener.close()
        self.assertFalse(self.gateway_thread.is_alive())
        self.assertFalse(self.hosted_thread.is_alive())

    def request(self, **values):
        connection = self.connection()
        self.connections.append(connection)
        connection.request(
            "POST",
            "/api/projects/fixture/agents/human-approval-hosted/endpoint/protocols/openai/responses?api-version=v1",
            body=json.dumps({"agent_session_id": "gateway-test-session", "stream": True, **values}),
            headers={
                "Authorization": "Bearer " + self.gateway.access_token,
                "Content-Type": "application/json",
                "Accept": "text/event-stream",
            },
        )
        return connection

    def connection(self):
        if self.tls:
            context = ssl.create_default_context(cafile=self.tls_path / "ca.crt")
            return http.client.HTTPSConnection("127.0.0.1", self.gateway.server_port, timeout=3, context=context)
        return http.client.HTTPConnection("127.0.0.1", self.gateway.server_port, timeout=3)

    def response(self, connection):
        response = connection.getresponse()
        self.responses.append(response)
        return response

    def frame(self, response):
        data = bytearray()
        while line := response.readline(16 * 1024):
            data.extend(line)
            self.assertLess(len(data), 16 * 1024)
            if line == b"\n":
                break
        frames = [line[6:] for line in data.splitlines() if line.startswith(b"data: ")]
        self.assertEqual(len(frames), 1)
        return json.loads(frames[0])

    def test_headers_and_created_arrive_before_terminal_is_released(self):
        started = time.monotonic()
        response = self.response(self.request())
        self.assertEqual(response.status, 200)
        self.assertEqual(response.getheader("Content-Type"), "text/event-stream; charset=utf-8")
        self.assertIsNone(response.getheader("Content-Length"))
        self.assertEqual(self.frame(response)["type"], "response.created")
        self.assertTrue(self.started.wait(1))
        self.assertFalse(self.release.is_set())
        elapsed = time.monotonic() - started
        self.release.set()
        self.assertEqual(self.frame(response)["type"], "response.completed")
        self.assertEqual(response.read(), b"")
        self.assertEqual(self.requests, 1)
        print(json.dumps({"test": "early-created", "seconds": round(elapsed, 3), "hostedRequests": self.requests}))

    def test_disconnect_cancels_agentkit_operation_while_terminal_is_held(self):
        connection = self.request()
        response = self.response(connection)
        self.assertEqual(self.frame(response)["type"], "response.created")
        self.assertTrue(self.started.wait(1))
        started = time.monotonic()
        response.close()
        connection.close()
        self.assertTrue(self.cancelled.wait(2), "hosted AgentKit did not observe cancellation")
        self.assertFalse(self.release.is_set())
        self.assertEqual(self.requests, 1)
        print(json.dumps({"test": "disconnect", "seconds": round(time.monotonic() - started, 3), "hostedRequests": self.requests}))

    def test_disconnect_closes_hosted_request_before_response_headers(self):
        connection = self.request(wait_before_headers=True)
        self.assertTrue(self.before_headers.wait(1))
        connection.close()
        self.assertTrue(self.disconnected_before_headers.wait(2))
        self.assertFalse(self.release.is_set())
        self.assertEqual(self.requests, 1)

    def test_complete_http_rejection_is_forwarded_without_retry(self):
        response = self.response(self.request(reject=True))
        self.assertEqual(response.status, 409)
        self.assertEqual(json.loads(response.read()), {"error": "fixture rejection"})
        self.assertEqual(self.requests, 1)
        self.assertFalse(self.started.is_set())


class TLSGatewayTests(GatewayTests):
    tls = True

    def test_unknown_ca_is_rejected(self):
        connection = http.client.HTTPSConnection("127.0.0.1", self.gateway.server_port, timeout=3)
        self.connections.append(connection)
        with self.assertRaises(ssl.SSLCertVerificationError):
            connection.request("GET", "/health")

    def test_authenticated_identity_and_session_requests_require_credentials(self):
        for path, header, expected in (
            ("/metadata/identity/oauth2/token", {}, 403),
            ("/metadata/identity/oauth2/token", {"X-IDENTITY-HEADER": self.gateway.identity_header}, 200),
            ("/api/projects/fixture/agents/human-approval-hosted", {}, 401),
            ("/api/projects/fixture/agents/human-approval-hosted",
             {"Authorization": "Bearer " + self.gateway.access_token}, 200),
        ):
            connection = self.connection()
            self.connections.append(connection)
            connection.request("GET", path, headers=header)
            response = self.response(connection)
            self.assertEqual(response.status, expected)
            response.read()


class GatewayStartupTests(unittest.TestCase):
    def test_unsafe_gateway_bind_fails_before_opening_a_listener(self):
        for arguments, identity in (
            (["--host", "0.0.0.0"], secrets.token_urlsafe(32)),
            (["--host", "0.0.0.0", "--tls-cert", "fixture.crt"], secrets.token_urlsafe(32)),
            (["--host", "0.0.0.0", "--tls-cert", "fixture.crt", "--tls-key", "fixture.key"], ""),
        ):
            with self.subTest(arguments=arguments), patch("sys.argv", ["fixture", "gateway", "--port", "0", *arguments]), \
                    patch.dict(os.environ, {"IDENTITY_HEADER": identity}), \
                    patch.object(fixture, "ThreadingHTTPServer") as server, redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as result:
                    fixture.main()
                self.assertEqual(result.exception.code, 2)
                server.assert_not_called()

    def test_real_cli_serves_tls_with_verified_health_and_authentication(self):
        with tempfile.TemporaryDirectory() as directory:
            tls = Path(directory) / "gateway-tls"
            prepare_gateway_tls(tls)
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            environment = dict(os.environ, IDENTITY_HEADER=secrets.token_urlsafe(32))
            command = [sys.executable, str(fixture_path), "gateway", "--host", "127.0.0.1", "--port", str(port),
                       "--tls-cert", str(tls / "tls.crt"), "--tls-key", str(tls / "tls.key")]
            process = subprocess.Popen(command, env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                context = ssl.create_default_context(cafile=tls / "ca.crt")
                deadline = time.monotonic() + 5
                while True:
                    self.assertIsNone(process.poll(), "gateway exited before readiness")
                    connection = http.client.HTTPSConnection("127.0.0.1", port, timeout=1, context=context)
                    try:
                        connection.request("GET", "/health")
                        response = connection.getresponse()
                        self.assertEqual(response.status, 200)
                        response.read()
                        break
                    except ConnectionRefusedError:
                        self.assertLess(time.monotonic(), deadline, "gateway never became ready")
                        time.sleep(0.01)
                    finally:
                        connection.close()
                for headers, expected in (({}, 403), ({"X-IDENTITY-HEADER": environment["IDENTITY_HEADER"]}, 200)):
                    connection = http.client.HTTPSConnection("127.0.0.1", port, timeout=1, context=context)
                    try:
                        connection.request("GET", "/metadata/identity/oauth2/token", headers=headers)
                        response = connection.getresponse()
                        self.assertEqual(response.status, expected)
                        response.read()
                    finally:
                        connection.close()
            finally:
                process.terminate()
                process.wait(timeout=5)


if __name__ == "__main__":
    unittest.main(verbosity=2)
