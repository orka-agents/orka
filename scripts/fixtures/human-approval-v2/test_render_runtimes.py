import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import e2e
import render_runtimes as render


class RendererTests(unittest.TestCase):
    def resources(self, provider):
        config = b'{"model":"fixture"}\n'
        profile = {"profile": {"digest": "sha256:" + "a" * 64,
                              "agentConfigurationDigest": "sha256:" + hashlib.sha256(config).hexdigest()},
                   "supervisorEnv": {"ORKA_ACP_PROVIDER": provider}}
        image = "example.invalid/fixture@sha256:" + "b" * 64
        images = {name: {"image": image} for name in ("fixture", "foundry-base", "agentkit-hosted")}
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "context").mkdir()
            (root / "context/foundry.json").write_bytes(config)
            with patch.object(render, "SETUP", root):
                return render.runtime_resources(provider, profile, image, images, "17", "http://controller")

    def test_both_runtime_deployments_satisfy_enrollment_topology(self):
        for provider in ("agentkit", "foundry"):
            with self.subTest(provider=provider):
                resources = self.resources(provider)
                runtime = next(item for item in resources if item["kind"] == "Deployment" and
                               item["metadata"]["name"] == "human-approval-" + provider)
                runtime["metadata"]["uid"] = "exact-deployment-uid"
                container, instance, epoch = e2e.recovery.validate_deployment(runtime, provider)
                self.assertEqual(epoch, 17)
                self.assertEqual(instance, "human-approval-" + provider + "-instance")
                pod = runtime["spec"]["template"]["spec"]
                self.assertNotIn("initContainers", pod)
                self.assertNotIn("ephemeralContainers", pod)
                supervisor = next(item for item in pod["containers"] if item["name"] == container)
                env = {item["name"]: item.get("value") for item in supervisor["env"]}
                self.assertEqual(env["ORKA_ACP_SESSION_BASE_DIR"], "/sessions")
                if provider == "foundry":
                    self.assertEqual(env["ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST"], "sha256:" + "a" * 64)
                else:
                    self.assertNotIn("ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST", env)
                self.assertNotIn("ORKA_ACP_SUPERVISOR_BOOT_ID", env)
                self.assertFalse(any(item["mountPath"] == "/agent" for item in supervisor["volumeMounts"]))

    def test_hosted_process_is_independent_of_enrolled_foundry_pod(self):
        resources = self.resources("foundry")
        deployments = {item["metadata"]["name"]: item for item in resources if item["kind"] == "Deployment"}
        foundry = deployments["human-approval-foundry"]["spec"]["template"]["spec"]
        hosted = deployments["human-approval-hosted"]["spec"]["template"]["spec"]
        self.assertEqual({item["name"] for item in foundry["containers"]}, {"supervisor", "broker"})
        self.assertEqual({item["name"] for item in hosted["containers"]}, {"hosted-agentkit", "local-hosted-transport"})
        broker = next(item for item in foundry["containers"] if item["name"] == "broker")
        environment = {item["name"]: item.get("value") for item in broker["env"]}
        self.assertEqual(environment["ORKA_FOUNDRY_BROKER_STATE_DIR"], "/broker-state/ledger")
        self.assertEqual(environment["IDENTITY_ENDPOINT"],
                         "https://human-approval-hosted:8092/metadata/identity/oauth2/token")
        self.assertNotIn("foundry-config", json.dumps(foundry))

    def test_gateway_requires_tls_and_broker_receives_only_the_public_ca(self):
        deployments = {item["metadata"]["name"]: item["spec"]["template"]["spec"]
                       for item in self.resources("foundry") if item["kind"] == "Deployment"}
        runtime = deployments["human-approval-foundry"]
        hosted = deployments["human-approval-hosted"]
        gateway = next(item for item in hosted["containers"] if item["name"] == "local-hosted-transport")
        arguments = gateway["args"]
        self.assertEqual(arguments[arguments.index("--host") + 1], "0.0.0.0")
        for flag in ("--tls-cert", "--tls-key"):
            self.assertIn("/gateway-tls/", arguments[arguments.index(flag) + 1])
        for probe in ("startupProbe", "readinessProbe", "livenessProbe"):
            code = gateway[probe]["exec"]["command"][-1]
            self.assertIn("https://127.0.0.1:8092/health", code)
            self.assertIn("ssl.create_default_context(cafile=", code)
            self.assertNotIn("_create_unverified_context", code)
        mounts = {item["name"] for item in gateway["volumeMounts"]}
        self.assertEqual(mounts, {"gateway-tls"})
        broker = next(item for item in runtime["containers"] if item["name"] == "broker")
        environment = {item["name"]: item.get("value") for item in broker["env"]}
        self.assertEqual(environment["SSL_CERT_FILE"], "/var/run/secrets/orka/gateway-ca/ca.crt")
        ca = next(item for item in runtime["volumes"] if item["name"] == "gateway-ca")
        self.assertEqual(ca["secret"]["items"], [{"key": "ca.crt", "path": "ca.crt"}])
        for container in runtime["containers"]:
            self.assertNotIn("gateway-tls", {item["name"] for item in container.get("volumeMounts", [])})
        agentkit = next(item for item in hosted["containers"] if item["name"] == "hosted-agentkit")
        self.assertNotIn("gateway-tls", {item["name"] for item in agentkit["volumeMounts"]})


if __name__ == "__main__":
    unittest.main()
