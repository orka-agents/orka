"""Exercise release provenance, failed dispatches, and publication retries offline."""

from copy import deepcopy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("release_workflow", ROOT / "scripts/release_workflow.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
SHA = "a" * 40
DIGEST = "sha256:" + "b" * 64
BRANCH = "release-0.2"
VERSION = "v0.2.0"


def run(workflow="release.yml", **overrides):
    return {
        "id": 123, "run_attempt": 1, "status": "in_progress", "conclusion": None,
        "event": "workflow_dispatch", "path": f".github/workflows/{workflow}",
        "head_sha": SHA, "head_branch": BRANCH,
        "repository": {"full_name": release.REPOSITORY},
        "head_repository": {"full_name": release.REPOSITORY},
        "display_title": "Release v0.2.0 (prepare-10-1)", "html_url": "https://github.com/orka-agents/orka/actions/runs/123",
        **overrides,
    }


class ReleaseFixture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="orka-release-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.bundle = self.root / "bundle"
        self.bundle.mkdir()
        (self.bundle / "digests").mkdir()
        for name in release.IMAGES:
            (self.bundle / "digests" / f"digest-{name}.txt").write_text(DIGEST)
        (self.bundle / "orka-0.2.0.tgz").write_bytes(b"packaged chart bytes")
        environment = patch.dict(os.environ, {
            "GITHUB_REPOSITORY": release.REPOSITORY, "GITHUB_EVENT_NAME": "workflow_dispatch",
            "GITHUB_REF": f"refs/heads/{BRANCH}", "GITHUB_SHA": SHA,
            "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1",
        })
        environment.start()
        self.addCleanup(environment.stop)
        release.bundle(self.bundle, VERSION, SHA, "1")
        self.data = release.load_bundle(self.bundle)

    def report(self):
        return {
            "result": "qualified", "candidateSHA": SHA,
            "release": {"version": VERSION, "buildRunID": "123", "buildRunAttempt": "1",
                        "bundleSHA256": release.file_hash(self.bundle / "candidate.json")},
            "builtImages": {role: self.data["images"][name] for role, name in release.ROLES.items()},
            "images": {role: {"requestedImage": self.data["images"][name]} for role, name in release.ROLES.items()},
            "chart": {"packageSHA256": self.data["chart"]["sha256"], "install": True,
                      "containerTask": True, "recovery": True, "noReplay": True, "oppositeModeRejected": True},
        }

    def qualification(self):
        release.write_json(self.bundle / "acceptance.json", self.report())
        release.write_json(self.bundle / "qualification.json", {
            "runID": "456", "runAttempt": "1",
            "acceptanceSHA256": release.file_hash(self.bundle / "acceptance.json"),
            "candidateSHA256": release.file_hash(self.bundle / "candidate.json"),
        })

    def copy_bundle(self, target):
        shutil.copytree(self.bundle, target, dirs_exist_ok=True)


class ReleaseTest(ReleaseFixture):
    def test_versions_always_target_release_lines(self):
        for version in ("v0.2.0", "v0.2.0-beta.1", "v0.2.0-rc.1"):
            self.assertEqual(release.branch_for(version), BRANCH)
        for version in ("0.2.0", "v00.2.0", "v0.02.0", "v0.2.00", "v0.2.0-dev", "v0.2.0-rc.01", "v0.2.0\n"):
            with self.subTest(version=version), self.assertRaises(RuntimeError):
                release.branch_for(version)

    def test_environment_requires_explicit_branch_and_real_approval(self):
        environment = {"deployment_branch_policy": {"custom_branch_policies": True},
                       "can_admins_bypass": False,
                       "protection_rules": [{"type": "required_reviewers", "reviewers": [{"id": 42}]}]}
        policies = [{"type": "branch", "name": BRANCH}]

        def api_result(environment, default="main"):
            return lambda path: {"default_branch": default} if path == f"repos/{release.REPOSITORY}" else environment

        with patch.object(release, "api", side_effect=api_result(environment)), \
                patch.object(release, "paginated", return_value=policies):
            for name in ("release", "live-acp-release-gate"):
                release.check_environment(name, BRANCH)
        release_lines = policies + [{"type": "branch", "name": "release-1.0"}]
        for default in ("main", "trunk"):
            with patch.object(release, "api", side_effect=api_result(environment, default)), \
                    patch.object(release, "paginated", return_value=release_lines):
                release.check_environment("release", BRANCH)
            with patch.object(release, "api", side_effect=api_result(environment, default)), \
                    patch.object(release, "paginated", return_value=release_lines + [{"type": "branch", "name": default}]):
                release.check_environment("live-acp-release-gate", BRANCH)
                release.check_environment("live-acp-release-gate", default)
                with self.assertRaises(RuntimeError):
                    release.check_environment("release", BRANCH)
        mutations = [
            ({**environment, "protection_rules": []}, policies),
            ({**environment, "protection_rules": [{"type": "required_reviewers", "reviewers": []}]}, policies),
            ({**environment, "can_admins_bypass": True}, policies),
            ({**environment, "deployment_branch_policy": None}, policies),
            (environment, [{"type": "branch", "name": "release-*"}]),
            (environment, [{"type": "tag", "name": BRANCH}]),
            (environment, [{"type": "branch", "name": "main"}]),
        ]
        mutations += [(environment, policies + [extra]) for extra in (
            {"type": "branch", "name": "release-*"},
            {"type": "tag", "name": BRANCH},
            {"type": "branch", "name": "feature/unreviewed"},
            {"type": "branch", "name": "release-00.2"},
        )]
        for name in ("release", "live-acp-release-gate"):
            for env, rules in mutations:
                with self.subTest(name=name, env=env, rules=rules), patch.object(release, "api", side_effect=api_result(env)), \
                        patch.object(release, "paginated", return_value=rules), self.assertRaises(RuntimeError):
                    release.check_environment(name, BRANCH)

    def test_missing_environment_stops_preparation_before_push_or_dispatch(self):
        with patch.object(release, "api", return_value={"default_branch": "main"}), \
                patch.object(release, "check_context"), patch.object(release, "command", return_value="") as command, \
                patch.object(release, "check_environment", side_effect=RuntimeError("environment absent")), \
                patch.object(release, "dispatch") as dispatch, self.assertRaises(RuntimeError):
            release.prepare(VERSION)
        dispatch.assert_not_called()
        self.assertFalse(any("push" in call.args for call in command.call_args_list))

    def test_candidate_must_be_the_checked_out_dispatch_and_current_head(self):
        with patch.object(release, "command", return_value=SHA), patch.object(release, "branch_head", return_value=SHA):
            release.check_context(BRANCH, SHA)
            for key, value in {"GITHUB_REF": "refs/heads/main", "GITHUB_SHA": "c" * 40,
                               "GITHUB_EVENT_NAME": "push", "GITHUB_REPOSITORY": "external/orka"}.items():
                with self.subTest(key=key), patch.dict(os.environ, {key: value}), self.assertRaises(RuntimeError):
                    release.check_context(BRANCH, SHA)
        with patch.object(release, "command", return_value=SHA), \
                patch.object(release, "branch_head", return_value="c" * 40), self.assertRaises(RuntimeError):
            release.check_context(BRANCH, SHA)

    def test_bundle_rejects_changed_chart_images_and_run_identity(self):
        for key, value in (("candidateSHA", "not-a-sha"), ("buildRunAttempt", "0"),
                           ("repository", "external/orka"), ("branch", "main"),
                           ("images", {"controller": "ghcr.io/orka-agents/orka:latest"}),
                           ("chart", {"file": "../outside", "sha256": "a" * 64})):
            data = {**self.data, key: value}
            release.write_json(self.bundle / "candidate.json", data)
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                release.load_bundle(self.bundle)
        release.write_json(self.bundle / "candidate.json", self.data)
        (self.bundle / "orka-0.2.0.tgz").write_bytes(b"different package")
        with self.assertRaisesRegex(RuntimeError, "chart bytes changed"):
            release.load_bundle(self.bundle)

    def test_qualification_binds_packaged_chart_and_observed_images(self):
        report = self.report()
        release.verify_report(self.bundle, report)
        mutations = []
        for field in ("buildRunID", "buildRunAttempt", "version", "bundleSHA256"):
            changed = deepcopy(report)
            changed["release"][field] = "different"
            mutations.append(changed)
        for field in ("install", "containerTask", "recovery", "noReplay", "oppositeModeRejected"):
            changed = deepcopy(report)
            changed["chart"][field] = False
            mutations.append(changed)
        changed = deepcopy(report)
        changed["images"]["codex"]["requestedImage"] = "image@sha256:" + "c" * 64
        mutations.append(changed)
        for changed in mutations:
            with self.subTest(report=changed), self.assertRaises(RuntimeError):
                release.verify_report(self.bundle, changed)

    def test_bundle_download_rechecks_run_attempt_and_artifact_identity(self):
        def download(*args, **kwargs):
            self.copy_bundle(Path(args[-1]))
            return ""
        artifact = {"name": "release-candidate-123-1", "expired": False}
        variants = [
            (run(), run(), [artifact], True),
            (run(head_sha="c" * 40), run(), [artifact], False),
            (run(head_repository={"full_name": "external/orka"}), run(), [artifact], False),
            (run(status="completed", conclusion="success"), run(), [artifact], False),
            (run(), run(run_attempt=2), [artifact], False),
            (run(), run(status="completed"), [artifact], False),
            (run(), run(), [artifact, artifact], False),
            (run(), run(), [{**artifact, "expired": True}], False),
            (run(), run(), [{**artifact, "name": "release-candidate-123-2"}], False),
        ]
        for index, (before, after, artifacts, valid) in enumerate(variants):
            with self.subTest(index=index), patch.object(release, "api", side_effect=[before, after]), \
                    patch.object(release, "paginated", return_value=artifacts), patch.object(release, "command", side_effect=download):
                if valid:
                    release.download_bundle("123", "1", BRANCH, SHA, self.root / f"download-{index}")
                else:
                    with self.assertRaises(RuntimeError):
                        release.download_bundle("123", "1", BRANCH, SHA, self.root / f"download-{index}")

    def test_dispatch_correlates_the_new_run_instead_of_reusing_old_success(self):
        previous = run(id=120, status="completed", conclusion="success")
        unrelated = run(id=121, display_title="Release (someone-else)")
        expected = run()
        with patch.object(release, "branch_head", return_value=SHA), \
                patch.object(release, "api", side_effect=[{"workflow_runs": [previous]}, None,
                                                        {"workflow_runs": [previous, unrelated]},
                                                        {"workflow_runs": [previous, unrelated, expected]}]), \
                patch.object(release.time, "sleep"):
            self.assertEqual(release.dispatch("release.yml", BRANCH, SHA, {"dispatch_id": "prepare-10-1"})["id"], 123)

    def test_failed_unobserved_ambiguous_and_moved_dispatches_stop(self):
        for responses in (
            [{"workflow_runs": []}, RuntimeError("dispatch rejected")],
            [{"workflow_runs": []}, None, {"workflow_runs": []}],
            [{"workflow_runs": []}, None, {"workflow_runs": [run(), run(id=124)]}],
        ):
            with self.subTest(responses=responses), patch.object(release, "branch_head", return_value=SHA), \
                    patch.object(release, "api", side_effect=responses), patch.object(release.time, "monotonic", side_effect=[0, 121]), \
                    self.assertRaises(RuntimeError):
                release.dispatch("release.yml", BRANCH, SHA, {"dispatch_id": "prepare-10-1"})
        with patch.object(release, "branch_head", return_value="c" * 40), patch.object(release, "api") as api, \
                self.assertRaises(RuntimeError):
            release.dispatch("release.yml", BRANCH, SHA, {"dispatch_id": "prepare-10-1"})
        api.assert_not_called()

    def test_failed_live_gate_never_prepares_approval_evidence(self):
        with patch.object(release, "check_context"), patch.object(release, "dispatch", side_effect=RuntimeError("gate failed")), \
                patch.object(release, "paginated", return_value=[]), \
                self.assertRaises(RuntimeError):
            release.qualify(self.bundle)
        self.assertFalse((self.bundle / "qualification.json").exists())

    def test_busy_live_gate_stops_before_dispatch_or_approval_evidence(self):
        for status in ("queued", "in_progress", "waiting", "requested", "pending"):
            with self.subTest(status=status), patch.object(release, "check_context"), \
                    patch.object(release, "paginated", return_value=[run(status=status)]), \
                    patch.object(release, "dispatch") as dispatch, \
                    self.assertRaisesRegex(RuntimeError, "Another live ACP release gate"):
                release.qualify(self.bundle)
            dispatch.assert_not_called()
        self.assertFalse((self.bundle / "qualification.json").exists())

    def test_registry_absence_is_distinct_from_auth_or_transport_failure(self):
        def result(code, out="", err=""):
            return subprocess.CompletedProcess([], code, out, err)
        for response, expected in ((result(0, json.dumps({"digest": DIGEST})), DIGEST),
                                   (result(1, err="ERROR: ghcr.io/orka-agents/orka:0.2.0: not found"), None)):
            with patch.object(release.subprocess, "run", return_value=response):
                self.assertEqual(release.registry_digest("ghcr.io/orka-agents/orka:0.2.0"), expected)
        for error in ("unauthorized: not found", "403 forbidden", "429 too many requests", "i/o timeout"):
            with self.subTest(error=error), patch.object(release.subprocess, "run", return_value=result(1, err=error)), \
                    self.assertRaises(RuntimeError):
                release.registry_digest("ghcr.io/orka-agents/orka:0.2.0")

    def test_immutable_tags_are_checked_before_any_publication(self):
        with patch.object(release, "registry_digest", return_value=DIGEST):
            release.check_image_versions(self.data)
        with patch.object(release, "registry_digest", side_effect=[DIGEST, "sha256:" + "c" * 64]), \
                self.assertRaisesRegex(RuntimeError, "already points to different bytes"):
            release.check_image_versions(self.data)

    def test_older_releases_and_prereleases_do_not_roll_aliases_back(self):
        with patch.object(release, "stable_versions", return_value=[(0, 2, 0), (0, 3, 0)]):
            self.assertEqual(release.release_aliases(self.data), ["0.2.0", f"sha-{SHA[:7]}", "0.2"])
        with patch.object(release, "stable_versions", return_value=[(0, 2, 1)]):
            self.assertEqual(release.release_aliases(self.data), ["0.2.0", f"sha-{SHA[:7]}"])
        with patch.object(release, "stable_versions", side_effect=AssertionError("prerelease queried stable tags")):
            self.assertEqual(release.release_aliases({**self.data, "version": "v0.2.0-rc.1"}), ["0.2.0-rc.1", f"sha-{SHA[:7]}"])

    def test_existing_tag_only_allows_identical_candidate_and_qualification(self):
        self.qualification()
        message = json.dumps({"candidateSHA256": release.file_hash(self.bundle / "candidate.json"),
                              "qualificationSHA256": release.file_hash(self.bundle / "qualification.json")}, sort_keys=True)
        existing = {"object": {"type": "tag", "sha": "d" * 40}}
        tag = {"object": {"type": "commit", "sha": SHA}, "message": message}
        with patch.object(release, "tag_ref", return_value=existing), patch.object(release, "api", return_value=tag) as api:
            release.create_tag(self.data, self.bundle)
            self.assertEqual(len(api.call_args_list), 1)
        for changed in ({**tag, "message": "unqualified"}, {**tag, "object": {"type": "commit", "sha": "c" * 40}}):
            with patch.object(release, "tag_ref", return_value=existing), patch.object(release, "api", return_value=changed), \
                    self.assertRaises(RuntimeError):
                release.create_tag(self.data, self.bundle)

    def test_publication_requires_the_original_run_and_unchanged_qualified_artifacts(self):
        self.qualification()
        report_dir = self.root / "bin/acp-release-qualification-456-1"
        report_dir.mkdir(parents=True)
        shutil.copyfile(self.bundle / "acceptance.json", report_dir / "acceptance.json")
        def download(*args):
            self.copy_bundle(args[-1])
        with patch.object(release, "ROOT", self.root), patch.object(release, "check_context"), \
                patch.object(release, "check_environment"), patch.object(release, "command"), \
                patch.object(release, "api", return_value={"run_attempt": 1}), \
                patch.object(release, "download_bundle", side_effect=download):
            release.verify_publication(self.bundle)
            with patch.dict(os.environ, {"GITHUB_RUN_ID": "999"}), self.assertRaises(RuntimeError):
                release.verify_publication(self.bundle)
            with patch.object(release, "api", return_value={"run_attempt": 2}), self.assertRaises(RuntimeError):
                release.verify_publication(self.bundle)
            (self.bundle / "acceptance.json").write_text("{}")
            with self.assertRaisesRegex(RuntimeError, "artifacts changed"):
                release.verify_publication(self.bundle)

    def test_publication_failure_cannot_create_tags_or_publish_assets(self):
        with patch.object(release, "verify_publication", side_effect=RuntimeError("no valid evidence")), \
                patch.object(release, "create_tag") as tag, patch.object(release, "promote_images") as images, \
                patch.object(release, "publish_chart") as chart, self.assertRaises(RuntimeError):
            release.publish(self.bundle)
        for operation in (tag, images, chart):
            operation.assert_not_called()

    def test_pages_requires_the_exact_published_commit_and_success(self):
        with patch.object(release, "api", side_effect=[None, {"commit": "old", "status": "built"},
                                                      {"commit": SHA, "status": "built"}]), patch.object(release.time, "sleep"):
            release.build_pages(SHA)
        with patch.object(release, "api", side_effect=[None, {"commit": SHA, "status": "errored"}]), self.assertRaises(RuntimeError):
            release.build_pages(SHA)
        with patch.object(release, "api", side_effect=[None, {"commit": "old", "status": "built"}]), \
                patch.object(release.time, "monotonic", side_effect=[0, 601]), self.assertRaises(RuntimeError):
            release.build_pages(SHA)

    def test_served_chart_and_index_must_match_after_pages_build(self):
        archive = (self.bundle / self.data["chart"]["file"]).read_bytes()
        index = b"index containing the qualified version"
        digest = hashlib.sha256(index).hexdigest()
        with patch.object(release, "urlopen", side_effect=[io.BytesIO(b"stale chart"), io.BytesIO(index),
                                                          io.BytesIO(archive), io.BytesIO(index)]), \
                patch.object(release.time, "sleep") as sleep:
            release.verify_served_chart(self.data, digest)
            sleep.assert_called_once()
        with patch.object(release, "urlopen", side_effect=[io.BytesIO(archive), io.BytesIO(b"stale index")]), \
                patch.object(release.time, "monotonic", side_effect=[0, 121]), self.assertRaises(RuntimeError):
            release.verify_served_chart(self.data, digest)

    def test_archive_retries_preserve_existing_assets_and_reject_changed_bytes(self):
        self.qualification()
        files = {name: (self.bundle / name).read_bytes() for name in
                 ("candidate.json", "qualification.json", "acceptance.json", "orka-0.2.0.tgz")}
        record = {"id": 789, "tag_name": VERSION, "draft": False,
                  "assets": [{"name": name} for name in files]}
        def download(*args, **kwargs):
            self.assertEqual(args[:3], ("gh", "release", "download"))
            name = args[args.index("--pattern") + 1]
            (Path(args[args.index("--dir") + 1]) / name).write_bytes(files[name])
            return ""
        with patch.object(release, "paginated", return_value=[record]), patch.object(release, "api", return_value=record), \
                patch.object(release, "command", side_effect=download):
            release.archive_release(self.data, self.bundle)
            files["acceptance.json"] = b"changed published evidence"
            with self.assertRaisesRegex(RuntimeError, "refusing to overwrite"):
                release.archive_release(self.data, self.bundle)


@unittest.skipUnless(shutil.which("helm"), "Helm is required for the chart publication integration test")
class ChartPublicationTest(ReleaseFixture):
    def test_exact_chart_publication_preserves_site_repairs_index_and_retries(self):
        source = self.root / "chart"
        source.mkdir()
        (source / "Chart.yaml").write_text("apiVersion: v2\nname: orka\nversion: 0.2.0\n")
        subprocess.run(["helm", "package", str(source), "--destination", str(self.bundle)], check=True, capture_output=True)
        release.bundle(self.bundle, VERSION, SHA, "1")
        self.data = release.load_bundle(self.bundle)
        remote = self.root / "pages.git"
        checkout = self.root / "pages"
        original_command = release.command
        original_command("git", "init", "--bare", str(remote))
        original_command("git", "clone", str(remote), str(checkout))
        original_command("git", "checkout", "--orphan", "gh-pages", cwd=checkout)
        (checkout / "index.html").write_text("preserved website")
        (checkout / "charts").mkdir()
        shutil.copyfile(self.bundle / "orka-0.2.0.tgz", checkout / "charts/orka-0.2.0.tgz")
        original_command("git", "add", ".", cwd=checkout)
        original_command("git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                         "commit", "-m", "existing chart, missing index", cwd=checkout)
        original_command("git", "push", "origin", "gh-pages", cwd=checkout)
        def command(*args, **kwargs):
            if args[:4] == ("git", "remote", "add", "origin"):
                args = (*args[:4], str(remote))
            return original_command(*args, **kwargs)
        with patch.object(release, "command", side_effect=command), \
                patch.object(release, "api", return_value=[{"ref": "refs/heads/gh-pages"}]):
            first = release.publish_chart(self.data, self.bundle)
            second = release.publish_chart(self.data, self.bundle)
            self.assertEqual(first, second, "an identical publication retry created another commit")
        self.assertEqual(original_command("git", "--git-dir", str(remote), "show", "gh-pages:index.html"), "preserved website")
        index = original_command("git", "--git-dir", str(remote), "show", "gh-pages:charts/index.yaml")
        self.assertIn(self.data["chart"]["sha256"], index)
        archived = subprocess.check_output(["git", "--git-dir", str(remote), "show", "gh-pages:charts/orka-0.2.0.tgz"])
        self.assertEqual(hashlib.sha256(archived).hexdigest(), self.data["chart"]["sha256"])


class PreparationTest(unittest.TestCase):
    """Use real local Git refs to prove branch and compare-and-swap behavior."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="orka-prepare-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.remote = self.root / "origin.git"
        self.checkout = self.root / "checkout"
        self.original_command = release.command
        self.original_command("git", "init", "--bare", str(self.remote))
        self.original_command("git", "clone", str(self.remote), str(self.checkout))
        self.git("checkout", "--orphan", "main")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.invalid")
        files = {
            "cmd/build/helmify/static/Chart.yaml": "initial\n",
            "cmd/build/helmify/static/values.yaml": "initial\n",
            "config/manager/manager.yaml": "initial\n",
            "config/manager/kustomization.yaml": "initial\n",
            "scripts/release_workflow.py": "# automation is present on this release line\n",
            "manifest_staging/generated.txt": "initial\n", "deploy/generated.txt": "initial\n",
            "charts/orka/generated.txt": "initial\n",
            "Makefile": "VERSION := v0.1.1\n"
                        "release-manifest:\n\t@printf '%s\\n' '$(NEWVERSION)' > manifest_staging/generated.txt\n"
                        "promote-staging-manifest:\n\t@cp manifest_staging/generated.txt deploy/generated.txt\n"
                        "\t@cp manifest_staging/generated.txt charts/orka/generated.txt\n"
                        "verify-release-manifest:\n\t@test \"$$(cat deploy/generated.txt)\" = '$(NEWVERSION)'\n",
        }
        for name, value in files.items():
            path = self.checkout / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(value)
        self.git("add", ".")
        self.git("commit", "-m", "initial")
        self.main = self.git("rev-parse", "HEAD")
        self.git("push", "origin", "main")
        for mock in (
            patch.object(release, "ROOT", self.checkout),
            patch.object(release, "command", side_effect=self.command),
            patch.object(release, "api", side_effect=self.api),
            patch.object(release, "paginated", return_value=[]),
            patch.object(release, "check_environment"),
            patch.object(release, "summary"),
            patch.dict(os.environ, {"GITHUB_REPOSITORY": release.REPOSITORY, "GITHUB_EVENT_NAME": "workflow_dispatch",
                                    "GITHUB_REF": "refs/heads/main", "GITHUB_SHA": self.main,
                                    "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1"}),
        ):
            mock.start()
            self.addCleanup(mock.stop)

    def git(self, *args):
        return self.original_command("git", *args, cwd=self.checkout)

    def command(self, *args, **kwargs):
        kwargs.setdefault("cwd", self.checkout)
        return self.original_command(*args, **kwargs)

    def ref(self, name):
        return self.original_command("git", "--git-dir", str(self.remote), "rev-parse", name)

    def api(self, endpoint, *args, **kwargs):
        self.assertFalse(args or kwargs, "preparation made an unexpected GitHub API mutation")
        if endpoint == f"repos/{release.REPOSITORY}":
            return {"default_branch": "main"}
        if endpoint.endswith("/git/ref/heads/main"):
            return {"object": {"sha": self.ref("refs/heads/main")}}
        if "/git/matching-refs/tags/" in endpoint:
            return []
        if endpoint.endswith(f"/git/matching-refs/heads/{BRANCH}"):
            refs = self.original_command("git", "--git-dir", str(self.remote), "for-each-ref",
                                         "--format=%(refname) %(objectname)", f"refs/heads/{BRANCH}")
            return [{"ref": line.split()[0], "object": {"sha": line.split()[1]}} for line in refs.splitlines()]
        self.fail(f"unexpected API read: {endpoint}")

    def test_preparation_creates_release_branch_and_leaves_main_unchanged(self):
        with patch.object(release, "dispatch", return_value=run()) as dispatch:
            release.prepare(VERSION)
        candidate = self.ref(f"refs/heads/{BRANCH}")
        self.assertEqual(self.ref("refs/heads/main"), self.main)
        self.assertNotEqual(candidate, self.main)
        self.assertEqual(self.git("rev-parse", f"{candidate}^"), self.main)
        self.assertIn("Signed-off-by: github-actions[bot]", self.git("log", "-1", "--format=%B"))
        self.assertEqual((self.checkout / "deploy/generated.txt").read_text().strip(), VERSION)
        self.assertEqual(dispatch.call_args.args[:3], ("release.yml", BRANCH, candidate))
        self.assertEqual(dispatch.call_args.args[3]["candidate_sha"], candidate)

    def test_existing_release_line_is_the_parent_and_concurrent_push_is_preserved(self):
        self.git("checkout", "-b", BRANCH)
        (self.checkout / "backport.txt").write_text("existing release backport\n")
        self.git("add", "backport.txt")
        self.git("commit", "-m", "backport")
        base = self.git("rev-parse", "HEAD")
        self.git("push", "origin", BRANCH)
        self.git("checkout", "main")
        with patch.object(release, "dispatch", return_value=run()):
            release.prepare(VERSION)
        self.assertEqual(self.git("rev-parse", "HEAD^"), base)
        self.assertTrue((self.checkout / "backport.txt").exists())
        self.assertEqual(self.ref("refs/heads/main"), self.main)

        # Restore the fixture's remote release head, then simulate another
        # writer after generation but before this preparation's leased push.
        self.original_command("git", "--git-dir", str(self.remote), "update-ref", f"refs/heads/{BRANCH}", base)
        self.git("checkout", "main")
        def raced_command(*args, **kwargs):
            if "push" in args and any(arg.startswith("--force-with-lease=") for arg in args):
                self.original_command("git", "--git-dir", str(self.remote), "update-ref", f"refs/heads/{BRANCH}", self.main)
            return self.command(*args, **kwargs)
        with patch.object(release, "command", side_effect=raced_command), patch.object(release, "dispatch") as dispatch, \
                self.assertRaises(RuntimeError):
            release.prepare(VERSION)
        dispatch.assert_not_called()
        self.assertEqual(self.ref(f"refs/heads/{BRANCH}"), self.main)

    def test_release_tooling_changes_stop_before_checkout_generation_or_dispatch(self):
        changes = {
            "Makefile": "VERSION := $(shell touch untrusted-command-ran)\n",
            "GNUmakefile": "release-manifest:\n\t@touch untrusted-command-ran\n",
            ".github/workflows/release.yml": "name: altered release workflow\n",
            "scripts/release_workflow.py": "# altered release automation\n",
            "cmd/build/helmify/untrusted.go": "package main\nfunc init() {}\n",
            "bin/controller-gen": "#!/bin/sh\ntouch untrusted-command-ran\n",
            "vendor/modules.txt": "# unreviewed vendored toolchain\n",
            "go.mod": "module untrusted.invalid/release\n",
            "go.work": "go 1.27\n",
            ".agents/skills/kindctl/bin/kindctl": "#!/bin/sh\ntouch untrusted-command-ran\n",
        }
        for name, contents in changes.items():
            with self.subTest(name=name):
                self.git("checkout", "-B", BRANCH, "main")
                path = self.checkout / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(contents)
                self.git("add", name)
                self.git("commit", "-m", "unreviewed release tooling")
                base = self.git("rev-parse", "HEAD")
                self.git("push", "--force", "origin", BRANCH)
                self.git("checkout", "main")
                with patch.object(release, "dispatch") as dispatch, \
                        self.assertRaisesRegex(RuntimeError, "Release automation differs"):
                    release.prepare(VERSION)
                dispatch.assert_not_called()
                self.assertEqual(self.git("rev-parse", "HEAD"), self.main)
                self.assertEqual(self.ref(f"refs/heads/{BRANCH}"), base)
                self.assertEqual((self.checkout / "deploy/generated.txt").read_text(), "initial\n")
                self.assertFalse((self.checkout / "untrusted-command-ran").exists())

    def test_previous_release_version_does_not_change_tooling_identity(self):
        self.git("checkout", "-b", BRANCH)
        makefile = self.checkout / "Makefile"
        makefile.write_text(makefile.read_text().replace("VERSION := v0.1.1", "VERSION := v0.2.0-rc.1"))
        self.git("add", "Makefile")
        self.git("commit", "-m", "previous release version")
        base = self.git("rev-parse", "HEAD")
        self.git("push", "origin", BRANCH)
        self.git("checkout", "main")
        with patch.object(release, "dispatch", return_value=run()):
            release.prepare(VERSION)
        self.assertEqual(self.git("rev-parse", "HEAD^"), base)
        self.assertEqual(self.ref("refs/heads/main"), self.main)

    def test_unexpected_untracked_generation_output_stops_before_push(self):
        makefile = self.checkout / "Makefile"
        makefile.write_text(makefile.read_text().replace("release-manifest:\n", "release-manifest:\n\t@touch unexpected.txt\n", 1))
        self.git("add", "Makefile")
        self.git("commit", "-m", "fixture generator writes an unexpected path")
        self.main = self.git("rev-parse", "HEAD")
        self.git("push", "origin", "main")
        with patch.dict(os.environ, {"GITHUB_SHA": self.main}), patch.object(release, "dispatch") as dispatch, \
                self.assertRaisesRegex(RuntimeError, "unexpected source file"):
            release.prepare(VERSION)
        dispatch.assert_not_called()
        self.assertEqual(self.api(f"repos/{release.REPOSITORY}/git/matching-refs/heads/{BRANCH}"), [])


if __name__ == "__main__":
    unittest.main()
