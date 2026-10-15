"""Verify disposable gateway identity and private build/evidence boundaries."""

import argparse
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import urlsplit

import e2e
import prepare


class PrepareTests(unittest.TestCase):
    def test_generated_endpoint_and_certificate_match_without_baking_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "agentkit"
            for folder, package in (("common", "agentkit_serve_common"),
                                    ("microsoft-agent-framework", "agentkit_serve")):
                runtime = source / "runtimes" / folder
                (runtime / package).mkdir(parents=True)
                for name in ("pyproject.toml", "README.md", "Dockerfile"):
                    (runtime / name).write_text("fixture\n")
            output = root / "context"
            with patch("sys.argv", ["prepare.py", "--agentkit", str(source), "--output", str(output)]):
                prepare.main()
            config = json.loads((output / "foundry.json").read_text())
            endpoint = urlsplit(config["hostedTarget"]["projectEndpoint"])
            self.assertEqual(endpoint.scheme, "https")
            self.assertEqual(endpoint.hostname, "human-approval-hosted")
            tls = root / "private" / "gateway-tls"
            for option, identity in (("-verify_hostname", endpoint.hostname), ("-verify_ip", "127.0.0.1")):
                result = subprocess.run(["openssl", "verify", option, identity, "-CAfile", str(tls / "ca.crt"),
                                         str(tls / "tls.crt")], capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 0)
            wrong = subprocess.run(["openssl", "verify", "-verify_hostname", "unrelated.invalid",
                                    "-CAfile", str(tls / "ca.crt"), str(tls / "tls.crt")],
                                   capture_output=True, timeout=10)
            self.assertNotEqual(wrong.returncode, 0)
            self.assertEqual(stat.S_IMODE(tls.stat().st_mode), 0o700)
            for name in ("ca.key", "tls.key"):
                self.assertEqual(stat.S_IMODE((tls / name).stat().st_mode), 0o600)
            self.assertFalse(list(output.rglob("*.key")))
            self.assertFalse(list(output.rglob("*.crt")))

            class Cluster:
                args = argparse.Namespace(work=root)

                def __init__(self):
                    self.tls_fields = None

                def apply(self, _):
                    pass

                def call(self, *_, body):
                    if body["metadata"]["name"] == "human-approval-gateway-tls":
                        self.tls_fields = set(body["stringData"])

            cluster = Cluster()
            previous_umask = os.umask(0o077)
            try:
                e2e.prepare_secrets(cluster)
            finally:
                os.umask(previous_umask)
            self.assertEqual(cluster.tls_fields, {"ca.crt", "tls.crt", "tls.key"})


if __name__ == "__main__":
    unittest.main()
