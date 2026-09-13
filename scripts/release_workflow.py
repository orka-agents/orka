#!/usr/bin/env python3
"""Prepare, qualify, and publish one immutable Orka release candidate."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
from urllib.parse import quote
from urllib.error import URLError
from urllib.request import Request, urlopen


REPOSITORY = "orka-agents/orka"
ROOT = Path(__file__).resolve().parent.parent
VERSION = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(beta|rc)\.(0|[1-9][0-9]*))?")
SHA = re.compile(r"[0-9a-f]{40}")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
IMAGES = (
    "controller", "ai-worker", "general-worker", "agent-harness-wrapper",
    "acp-codex-runtime", "acp-claude-runtime", "acp-copilot-runtime",
    "acp-opencode-runtime", "workspace-publisher",
)
ROLES = {
    "controller": "controller", "publisher": "workspace-publisher",
    "codex": "acp-codex-runtime", "claude": "acp-claude-runtime",
    "copilot": "acp-copilot-runtime", "opencode": "acp-opencode-runtime",
}
GIT_AUTH = ("git", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential")


def require(condition: object, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def command(*args: str, cwd: Path = ROOT, input_text: str | None = None) -> str:
    result = subprocess.run(args, cwd=cwd, input=input_text, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    # Do not echo subprocess output: some commands handle credentialed requests.
    require(result.returncode == 0, f"{args[0]} {args[1] if len(args) > 1 else ''} failed")
    return result.stdout.strip()


def api(path: str, payload: dict | None = None, method: str = "POST") -> object:
    args = ["gh", "api", path]
    if payload is not None:
        args += ["--method", method, "--input", "-"]
    try:
        output = command(*args, input_text=json.dumps(payload) if payload is not None else None)
    except RuntimeError:
        raise RuntimeError(f"GitHub API {method if payload is not None else 'GET'} {path} failed; "
                           "check job token permissions and repository configuration") from None
    return json.loads(output) if output else None


def paginated(path: str, key: str) -> list:
    pages = json.loads(command("gh", "api", "--paginate", "--slurp", path))
    return [item for page in pages for item in (page[key] if key else page)]


def branch_for(version: str) -> str:
    require(isinstance(version, str), "version must be a string")
    match = VERSION.fullmatch(version)
    require(match is not None, "version must be vX.Y.Z[-beta.N|-rc.N] without leading zeros")
    return f"release-{match[1]}.{match[2]}"


def image_repository(name: str) -> str:
    return f"ghcr.io/{REPOSITORY}" + (f"/{name}" if name != "controller" else "")


def file_hash(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_json(path: Path, value: object) -> None:
    path.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n")


def output(name: str, value: str) -> None:
    require("\n" not in value and "\r" not in value, "invalid workflow output")
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
            stream.write(f"{name}={value}\n")
    print(f"{name}={value}")


def summary(text: str) -> None:
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as stream:
            stream.write(text + "\n")


def branch_head(branch: str) -> str:
    return api(f"repos/{REPOSITORY}/git/ref/heads/{quote(branch, safe='')}")["object"]["sha"]


def check_environment(name: str, branch: str, approval: bool = False) -> None:
    environment = api(f"repos/{REPOSITORY}/environments/{name}")
    policy = environment.get("deployment_branch_policy") or {}
    require(policy.get("custom_branch_policies") is True,
            f"{name} must restrict deployments to explicitly selected branches")
    policies = paginated(f"repos/{REPOSITORY}/environments/{name}/deployment-branch-policies?per_page=100", "branch_policies")
    require(any(p.get("type") == "branch" and p.get("name") == branch for p in policies),
            f"Add the exact {branch} branch to the {name} environment; wildcard rules are insufficient")
    if approval or name in {"release", "live-acp-release-gate"}:
        require(any(rule.get("type") == "required_reviewers" and rule.get("reviewers")
                    for rule in environment.get("protection_rules", [])),
                f"The {name} environment must have a required reviewer before starting a release")
        require(environment.get("can_admins_bypass") is False,
                f"Disable administrator bypass for the {name} environment")


def check_preparation_automation(trusted: str, candidate: str) -> None:
    """Check executable release tooling before checking out an existing line."""
    require(SHA.fullmatch(trusted) and SHA.fullmatch(candidate), "invalid preparation source identity")
    paths = (".github", "scripts", "cmd/build", ".agents/skills/kindctl", "bin", "vendor",
             "go.mod", "go.sum", "go.work", "go.work.sum", "Makefile", "GNUmakefile", "makefile",
             ":(exclude)cmd/build/helmify/static")
    changed = command("git", "diff", "--name-only", "--no-renames", trusted, candidate, "--", *paths).splitlines()
    if "Makefile" in changed:
        # A previous preparation changes only this literal version assignment.
        # Do not normalize arbitrary Make expressions, additions, or file modes.
        def makefile_identity(commit: str) -> tuple[str, str]:
            entry = command("git", "ls-tree", commit, "--", "Makefile").split()
            require(len(entry) == 4 and entry[1] == "blob", "release Makefile is missing")
            content = command("git", "show", f"{commit}:Makefile")
            content = re.sub(rf"(?m)^VERSION := {VERSION.pattern}$", "VERSION := RELEASE_VERSION", content)
            return entry[0], content

        if makefile_identity(trusted) == makefile_identity(candidate):
            changed.remove("Makefile")
    require(not changed,
            "Release automation differs from the dispatched default-branch commit; "
            "backport the reviewed workflows, scripts, generator, and toolchain before preparation")


def check_context(branch: str, candidate: str) -> None:
    require(SHA.fullmatch(candidate), "candidate must be a full lowercase commit SHA")
    require(os.environ.get("GITHUB_REPOSITORY") == REPOSITORY, "release must run in orka-agents/orka")
    require(os.environ.get("GITHUB_EVENT_NAME") == "workflow_dispatch", "release requires workflow_dispatch")
    require(os.environ.get("GITHUB_REF") == f"refs/heads/{branch}", "workflow must run from the candidate branch")
    require(os.environ.get("GITHUB_SHA") == candidate, "candidate must equal the dispatched workflow SHA")
    require(command("git", "rev-parse", "HEAD") == candidate, "checkout differs from candidate")
    require(branch_head(branch) == candidate, "candidate branch moved; prepare and qualify its new head")


def validate_run(run: dict, workflow: str, branch: str, candidate: str) -> None:
    require(run.get("event") == "workflow_dispatch" and run.get("path") == f".github/workflows/{workflow}"
            and run.get("head_branch") == branch and run.get("head_sha") == candidate
            and run.get("repository", {}).get("full_name") == REPOSITORY
            and run.get("head_repository", {}).get("full_name") == REPOSITORY,
            "workflow run does not match the trusted candidate")


def dispatch(workflow: str, branch: str, candidate: str, inputs: dict, wait: bool = False) -> dict:
    require(branch_head(branch) == candidate, "branch moved before dispatch")
    endpoint = f"repos/{REPOSITORY}/actions/workflows/{workflow}/runs?event=workflow_dispatch&branch={quote(branch, safe='')}&per_page=100"
    previous = {run["id"] for run in api(endpoint)["workflow_runs"]}
    api(f"repos/{REPOSITORY}/actions/workflows/{workflow}/dispatches", {"ref": branch, "inputs": inputs})
    deadline = time.monotonic() + 120
    while True:
        matches = [run for run in api(endpoint)["workflow_runs"]
                   if run["id"] not in previous and run["head_sha"] == candidate
                   and inputs["dispatch_id"] in run.get("display_title", "")]
        require(len(matches) <= 1, "dispatch matched multiple workflow runs")
        if matches:
            run = matches[0]
            validate_run(run, workflow, branch, candidate)
            break
        require(time.monotonic() < deadline, "dispatched workflow was not observed; inspect Actions before retrying")
        time.sleep(5)
    print(f"Started https://github.com/{REPOSITORY}/actions/runs/{run['id']}")
    if wait:
        # The child has a four-hour execution limit. Leave time for its
        # environment approval within the parent's six-hour runner limit.
        deadline = time.monotonic() + 5 * 60 * 60 + 40 * 60
        while run["status"] != "completed":
            require(time.monotonic() < deadline, "qualification timed out; publication is blocked")
            time.sleep(15)
            run = api(f"repos/{REPOSITORY}/actions/runs/{run['id']}")
            validate_run(run, workflow, branch, candidate)
        require(run["conclusion"] == "success", "qualification failed; inspect the linked run")
    return run


def tag_ref(version: str) -> dict | None:
    # Listing refs distinguishes an absent tag from an API/authentication failure.
    refs = api(f"repos/{REPOSITORY}/git/matching-refs/tags/{version}")
    matches = [ref for ref in refs if ref["ref"] == f"refs/tags/{version}"]
    require(len(matches) <= 1, "ambiguous release tag")
    return matches[0] if matches else None


def prepare(version: str) -> None:
    branch = branch_for(version)
    default = api(f"repos/{REPOSITORY}")["default_branch"]
    trusted = os.environ.get("GITHUB_SHA", "")
    check_context(default, trusted)
    require(not command("git", "status", "--porcelain"), "preparation requires a clean checkout")
    check_environment("release", branch, approval=True)
    check_environment("live-acp-release-gate", branch)
    require(tag_ref(version) is None, "version is already tagged; retry failed publication jobs in the original run")
    runs = paginated(f"repos/{REPOSITORY}/actions/workflows/release.yml/runs?branch={branch}&per_page=100", "workflow_runs")
    require(not any(run["status"] != "completed" for run in runs),
            "another release is still running for this branch; finish or cancel it first")
    refs = api(f"repos/{REPOSITORY}/git/matching-refs/heads/{branch}")
    existing = next((ref for ref in refs if ref["ref"] == f"refs/heads/{branch}"), None)
    if existing:
        command("git", "fetch", "origin", f"refs/heads/{branch}")
        base = existing["object"]["sha"]
        check_preparation_automation(trusted, base)
        command("git", "checkout", "--detach", base)
    else:
        base = trusted
    require((ROOT / "scripts/release_workflow.py").is_file(), "backport release automation to this branch first")
    for args in (("make", "release-manifest", f"NEWVERSION={version}"),
                 ("make", "promote-staging-manifest"),
                 ("make", "verify-release-manifest", f"NEWVERSION={version}"),
                 ("git", "diff", "--check")):
        # Build output is useful; no credential-bearing arguments are passed.
        subprocess.run(args, cwd=ROOT, check=True)
    paths = (command("git", "diff", "--name-only").splitlines()
             + command("git", "ls-files", "--others", "--exclude-standard").splitlines())
    allowed = {"Makefile", "cmd/build/helmify/static/Chart.yaml", "cmd/build/helmify/static/values.yaml",
               "config/manager/manager.yaml", "config/manager/kustomization.yaml"}
    require(all(path in allowed or path.startswith(("manifest_staging/", "deploy/", "charts/orka/")) for path in paths),
            "release generation changed an unexpected source file")
    # Include new generated files, but never unrelated untracked paths.
    command("git", "add", "--", *sorted(allowed), "manifest_staging", "deploy", "charts/orka")
    if command("git", "diff", "--cached", "--name-only"):
        command("git", "-c", "user.name=github-actions[bot]", "-c",
                "user.email=41898282+github-actions[bot]@users.noreply.github.com",
                "commit", "-s", "-m", f"chore(release): prepare {version}")
    candidate = command("git", "rev-parse", "HEAD")
    # The lease is an exact compare-and-swap, including the initial absent ref.
    lease = existing["object"]["sha"] if existing else ""
    try:
        command(*GIT_AUTH, "push", f"--force-with-lease=refs/heads/{branch}:{lease}",
                "origin", f"{candidate}:refs/heads/{branch}")
    except RuntimeError:
        raise RuntimeError(f"Push to {branch} failed. Check branch rules and the native token's Contents write "
                           "permission; the expected branch head may also have changed.") from None
    summary(f"Prepared `{version}` at `{candidate}` on `{branch}`.\n\n"
            f"[Review generated changes](https://github.com/{REPOSITORY}/compare/{base}...{candidate}).")
    if base != trusted:
        summary(f"[Review release-line source changes](https://github.com/{REPOSITORY}/compare/{trusted}...{base}) "
                "before approving qualification. Release tooling matches the dispatched default-branch commit.")
    run = dispatch("release.yml", branch, candidate, {
        "release_version": version, "candidate_sha": candidate,
        "dispatch_id": f"prepare-{os.environ['GITHUB_RUN_ID']}-{os.environ['GITHUB_RUN_ATTEMPT']}",
    })
    summary(f"[Follow validation and release approval]({run['html_url']}).")


def validate_candidate(version: str, candidate: str) -> None:
    branch = branch_for(version)
    check_context(branch, candidate)
    check_environment("release", branch, approval=True)
    check_environment("live-acp-release-gate", branch)
    require(tag_ref(version) is None, "tag already exists; use Re-run failed jobs to retain the original artifacts")
    output("artifact_attempt", os.environ["GITHUB_RUN_ATTEMPT"])


def bundle(directory: Path, version: str, candidate: str, attempt: str) -> None:
    branch = branch_for(version)
    require(SHA.fullmatch(candidate), "invalid candidate SHA")
    require(attempt.isdigit() and int(attempt) > 0, "invalid build attempt")
    images = {}
    for name in IMAGES:
        digest = (directory / "digests" / f"digest-{name}.txt").read_text().strip()
        require(DIGEST.fullmatch(digest), f"missing or invalid {name} digest")
        images[name] = f"{image_repository(name)}@{digest}"
    archive = directory / f"orka-{version[1:]}.tgz"
    require(archive.is_file(), "packaged candidate chart is missing")
    write_json(directory / "candidate.json", {
        "schemaVersion": 1, "repository": REPOSITORY, "version": version,
        "candidateSHA": candidate, "branch": branch,
        "buildRunID": os.environ["GITHUB_RUN_ID"], "buildRunAttempt": attempt,
        "images": images, "chart": {"file": archive.name, "sha256": file_hash(archive)},
    })


def load_bundle(directory: Path) -> dict:
    data = json.loads((directory / "candidate.json").read_text())
    require(data.get("schemaVersion") == 1 and data.get("repository") == REPOSITORY, "invalid release bundle")
    require(SHA.fullmatch(data.get("candidateSHA", "")), "invalid bundle candidate")
    require(data.get("branch") == branch_for(data.get("version", "")), "invalid bundle branch")
    require(all(isinstance(data.get(key), str) and data[key].isdigit() and int(data[key]) > 0
                for key in ("buildRunID", "buildRunAttempt")), "invalid bundle run identity")
    require(set(data.get("images", {})) == set(IMAGES), "release bundle must contain every release image")
    for name, ref in data["images"].items():
        require(re.fullmatch(re.escape(image_repository(name)) + r"@sha256:[a-f0-9]{64}", ref),
                f"invalid release image reference for {name}")
    chart = data.get("chart", {})
    require(chart.get("file") == f"orka-{data['version'][1:]}.tgz", "invalid chart archive path")
    require(file_hash(directory / chart["file"]) == chart.get("sha256"), "candidate chart bytes changed")
    return data


def download_bundle(run_id: str, attempt: str, branch: str, candidate: str, directory: Path) -> None:
    require(run_id.isdigit() and attempt.isdigit() and int(run_id) > 0 and int(attempt) > 0, "invalid build run")
    run = api(f"repos/{REPOSITORY}/actions/runs/{run_id}")
    validate_run(run, "release.yml", branch, candidate)
    require(run["status"] == "in_progress" and run["run_attempt"] >= int(attempt),
            "release build must belong to the active candidate run")
    name = f"release-candidate-{run_id}-{attempt}"
    artifacts = paginated(f"repos/{REPOSITORY}/actions/runs/{run_id}/artifacts?per_page=100", "artifacts")
    require(sum(a["name"] == name and not a["expired"] for a in artifacts) == 1, "missing or ambiguous candidate artifact")
    directory.mkdir(parents=True, exist_ok=False)
    command("gh", "run", "download", run_id, "--repo", REPOSITORY, "--name", name, "--dir", str(directory))
    data = load_bundle(directory)
    require((data["buildRunID"], data["buildRunAttempt"], data["candidateSHA"], data["branch"]) ==
            (run_id, attempt, candidate, branch), "candidate artifact does not match build run")
    after = api(f"repos/{REPOSITORY}/actions/runs/{run_id}")
    validate_run(after, "release.yml", branch, candidate)
    require(after["run_attempt"] == run["run_attempt"] and after["status"] == "in_progress",
            "candidate run changed while downloading artifacts")


def verify_report(directory: Path, report: dict) -> None:
    data = load_bundle(directory)
    require(report.get("result") == "qualified" and report.get("candidateSHA") == data["candidateSHA"],
            "report does not qualify this candidate")
    release = report.get("release", {})
    require(release == {
        "buildRunID": data["buildRunID"], "buildRunAttempt": data["buildRunAttempt"],
        "bundleSHA256": file_hash(directory / "candidate.json"), "version": data["version"],
    }, "qualification used different release artifacts")
    require(all(report.get("builtImages", {}).get(role) == data["images"][name]
                and report.get("images", {}).get(role, {}).get("requestedImage") == data["images"][name]
                for role, name in ROLES.items()), "qualification images differ from publication images")
    chart = report.get("chart", {})
    require(chart.get("packageSHA256") == data["chart"]["sha256"]
            and all(chart.get(check) is True for check in
                    ("install", "containerTask", "recovery", "noReplay", "oppositeModeRejected")),
            "release chart installation and recovery are not qualified")


def qualify(directory: Path) -> None:
    data = load_bundle(directory)
    check_context(data["branch"], data["candidateSHA"])
    require(data["buildRunID"] == os.environ.get("GITHUB_RUN_ID"),
            "qualification must use this workflow's own candidate bundle")
    runs = paginated(f"repos/{REPOSITORY}/actions/workflows/live-acp-release-gate.yml/runs?per_page=100", "workflow_runs")
    require(not any(run["status"] != "completed" for run in runs),
            "Another live ACP release gate is active or awaiting approval; finish or cancel it before retrying qualification")
    run = dispatch("live-acp-release-gate.yml", data["branch"], data["candidateSHA"], {
        "source_repository": f"https://github.com/{REPOSITORY}.git",
        "source_ref": data["candidateSHA"], "pr_base": data["branch"],
        "release_run_id": data["buildRunID"], "release_run_attempt": data["buildRunAttempt"],
        "dispatch_id": f"release-{os.environ['GITHUB_RUN_ID']}-{os.environ['GITHUB_RUN_ATTEMPT']}",
    }, wait=True)
    command("bash", "scripts/verify-acp-release-qualification.sh", data["candidateSHA"], str(run["id"]), data["branch"])
    source = ROOT / f"bin/acp-release-qualification-{run['id']}-{run['run_attempt']}/acceptance.json"
    report = json.loads(source.read_text())
    verify_report(directory, report)
    shutil.copyfile(source, directory / "acceptance.json")
    write_json(directory / "qualification.json", {
        "runID": str(run["id"]), "runAttempt": str(run["run_attempt"]),
        "acceptanceSHA256": file_hash(source), "candidateSHA256": file_hash(directory / "candidate.json"),
    })
    summary(f"Qualified `{data['candidateSHA']}` with the packaged chart and published image digests.\n\n"
            f"[ACP acceptance and cleanup evidence]({run['html_url']}).\n\n"
            "Review the candidate artifact and generated commit before approving the release job.")


def verify_publication(directory: Path) -> dict:
    data = load_bundle(directory)
    check_context(data["branch"], data["candidateSHA"])
    require(data["buildRunID"] == os.environ.get("GITHUB_RUN_ID"),
            "publication must run in the workflow that built this candidate")
    check_environment("release", data["branch"], approval=True)
    proof = json.loads((directory / "qualification.json").read_text())
    require(proof.get("candidateSHA256") == file_hash(directory / "candidate.json")
            and proof.get("acceptanceSHA256") == file_hash(directory / "acceptance.json"),
            "approved qualification artifacts changed")
    run_id, attempt = proof.get("runID", ""), proof.get("runAttempt", "")
    require(run_id.isdigit() and attempt.isdigit(), "invalid qualification run identity")
    command("bash", "scripts/verify-acp-release-qualification.sh", data["candidateSHA"], run_id, data["branch"])
    current = api(f"repos/{REPOSITORY}/actions/runs/{run_id}")
    require(str(current["run_attempt"]) == attempt, "qualification was rerun after approval evidence was prepared")
    report_path = ROOT / f"bin/acp-release-qualification-{run_id}-{attempt}/acceptance.json"
    require(file_hash(report_path) == proof["acceptanceSHA256"], "qualified report changed")
    verify_report(directory, json.loads(report_path.read_text()))
    # Revalidate the original bundle too. A rerun must not replace the images
    # or chart while an older qualification artifact awaits approval.
    with tempfile.TemporaryDirectory(prefix="orka-release-candidate-") as temporary:
        original = Path(temporary) / "candidate"
        download_bundle(data["buildRunID"], data["buildRunAttempt"], data["branch"], data["candidateSHA"], original)
        require(file_hash(original / "candidate.json") == file_hash(directory / "candidate.json"),
                "build artifact changed after qualification")
    return data


def create_tag(data: dict, directory: Path) -> None:
    message = json.dumps({"candidateSHA256": file_hash(directory / "candidate.json"),
                          "qualificationSHA256": file_hash(directory / "qualification.json")}, sort_keys=True)
    existing = tag_ref(data["version"])
    if existing:
        require(existing["object"]["type"] == "tag", "existing release tag has no qualification binding")
        tag = api(f"repos/{REPOSITORY}/git/tags/{existing['object']['sha']}")
        require(tag["object"]["type"] == "commit" and tag["object"]["sha"] == data["candidateSHA"]
                and tag["message"].strip() == message, "existing tag identifies different release artifacts")
        return
    tag = api(f"repos/{REPOSITORY}/git/tags", {"tag": data["version"], "message": message,
              "object": data["candidateSHA"], "type": "commit"})
    api(f"repos/{REPOSITORY}/git/refs", {"ref": f"refs/tags/{data['version']}", "sha": tag["sha"]})


def registry_digest(ref: str) -> str | None:
    result = subprocess.run(("docker", "buildx", "imagetools", "inspect", ref,
                             "--format", "{{json .Manifest}}"), cwd=ROOT, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        # Only a registry's missing-manifest response establishes absence.
        # Authentication, network, and rate-limit failures must stop publication.
        require(re.search(r"manifest unknown|manifest_unknown|: not found", result.stderr, re.I)
                and not re.search(r"unauthorized|denied|forbidden|429|401|403", result.stderr, re.I),
                f"could not inspect release image {ref}")
        return None
    digest = json.loads(result.stdout).get("digest", "")
    require(DIGEST.fullmatch(digest), f"registry returned an invalid digest for {ref}")
    return digest


def check_image_versions(data: dict) -> None:
    for name, ref in data["images"].items():
        require(registry_digest(ref) == ref.split("@", 1)[1], f"qualified {name} image is unavailable")
        for tag in (data["version"][1:], f"sha-{data['candidateSHA'][:7]}"):
            existing = registry_digest(f"{image_repository(name)}:{tag}")
            require(existing is None or existing == ref.split("@", 1)[1],
                    f"immutable {name}:{tag} already points to different bytes")


def stable_versions() -> list[tuple[int, int, int]]:
    refs = api(f"repos/{REPOSITORY}/git/matching-refs/tags/v")
    return [tuple(map(int, match.groups()[:3])) for ref in refs
            if (match := VERSION.fullmatch(ref["ref"].removeprefix("refs/tags/"))) and match[4] is None]


def release_aliases(data: dict) -> list[str]:
    version = data["version"][1:]
    tags = [version, f"sha-{data['candidateSHA'][:7]}"]
    if "-" not in version:
        current = tuple(map(int, version.split(".")))
        versions = stable_versions() + [current]
        if current == max(v for v in versions if v[:2] == current[:2]):
            tags.append(".".join(version.split(".")[:2]))
        if current == max(versions):
            tags.append("latest")
    return tags


def promote_images(data: dict) -> None:
    tags = release_aliases(data)
    for name, ref in data["images"].items():
        for tag in tags:
            target = f"{image_repository(name)}:{tag}"
            digest = ref.split("@", 1)[1]
            if registry_digest(target) == digest:
                continue
            command("docker", "buildx", "imagetools", "create", "--tag", target, ref)
            require(registry_digest(target) == digest, f"published {target} does not match qualified bytes")


def publish_chart(data: dict, directory: Path) -> tuple[str, str]:
    archive = directory / data["chart"]["file"]
    with tempfile.TemporaryDirectory(prefix="orka-release-pages-") as temporary:
        pages = Path(temporary)
        command("git", "init", "--quiet", cwd=pages)
        command("git", "remote", "add", "origin", f"https://github.com/{REPOSITORY}.git", cwd=pages)
        refs = api(f"repos/{REPOSITORY}/git/matching-refs/heads/gh-pages")
        if any(ref["ref"] == "refs/heads/gh-pages" for ref in refs):
            command("git", "fetch", "--depth=1", "origin", "gh-pages", cwd=pages)
            command("git", "checkout", "-B", "gh-pages", "FETCH_HEAD", cwd=pages)
        else:
            command("git", "checkout", "--orphan", "gh-pages", cwd=pages)
        charts = pages / "charts"
        charts.mkdir(exist_ok=True)
        destination = charts / archive.name
        if destination.exists():
            require(file_hash(destination) == data["chart"]["sha256"], "published chart has different bytes")
        shutil.copyfile(archive, destination)
        index = charts / "index.yaml"
        previous = index.read_text() if index.exists() else ""
        command("helm", "repo", "index", str(charts), "--url", "https://orka-agents.github.io/orka/charts")
        # Helm refreshes timestamps on every invocation. Preserve the old bytes
        # when all entries already match; still repair an absent/incomplete index.
        def without_timestamps(value: str) -> str:
            return re.sub(r"(?m)^(?:    created:|generated:).*$", "", value)
        if previous and without_timestamps(previous) == without_timestamps(index.read_text()):
            index.write_text(previous)
        command("git", "add", "--", f"charts/{archive.name}", "charts/index.yaml", cwd=pages)
        if command("git", "diff", "--cached", "--name-only", cwd=pages):
            command("git", "-c", "user.name=github-actions[bot]", "-c",
                    "user.email=41898282+github-actions[bot]@users.noreply.github.com", "commit", "-s", "-m",
                    f"chore(release): publish {data['version']} chart", cwd=pages)
            command(*GIT_AUTH, "push", "origin", "HEAD:refs/heads/gh-pages", cwd=pages)
        return command("git", "rev-parse", "HEAD", cwd=pages), file_hash(index)


def check_pages() -> None:
    pages = api(f"repos/{REPOSITORY}/pages")
    require(pages.get("build_type") == "legacy"
            and pages.get("source") == {"branch": "gh-pages", "path": "/"},
            "release chart publication requires Pages to serve the gh-pages branch root")


def build_pages(commit: str) -> None:
    # Native-token branch pushes do not trigger Pages builds. The Pages REST
    # endpoint works with the publication job's pages: write permission.
    api(f"repos/{REPOSITORY}/pages/builds", {})
    deadline = time.monotonic() + 10 * 60
    while True:
        build = api(f"repos/{REPOSITORY}/pages/builds/latest")
        if build.get("commit") == commit:
            require(build.get("status") != "errored", "GitHub Pages build failed; retry publication")
            if build.get("status") == "built":
                return
        require(time.monotonic() < deadline, "GitHub Pages did not serve the published chart commit in time")
        time.sleep(10)


def verify_served_chart(data: dict, index_hash: str) -> None:
    expected = {data["chart"]["file"]: data["chart"]["sha256"], "index.yaml": index_hash}
    deadline = time.monotonic() + 120
    while True:
        matched = True
        for name, digest in expected.items():
            request = Request(f"https://orka-agents.github.io/orka/charts/{name}", headers={"Cache-Control": "no-cache"})
            try:
                with urlopen(request, timeout=20) as response:
                    matched = matched and hashlib.sha256(response.read()).hexdigest() == digest
            except (URLError, TimeoutError):
                matched = False
        if matched:
            return
        require(time.monotonic() < deadline, "Pages is not serving the qualified chart and index; retry publication")
        time.sleep(10)


def archive_release(data: dict, directory: Path) -> None:
    releases = paginated(f"repos/{REPOSITORY}/releases?per_page=100", "")
    matches = [release for release in releases if release["tag_name"] == data["version"]]
    require(len(matches) <= 1, "ambiguous GitHub Release")
    if matches:
        release = matches[0]
    else:
        proof = json.loads((directory / "qualification.json").read_text())
        release = api(f"repos/{REPOSITORY}/releases", {
            "tag_name": data["version"], "target_commitish": data["candidateSHA"], "name": data["version"],
            "draft": True, "prerelease": "-" in data["version"],
            "body": f"Release candidate `{data['candidateSHA']}`.\n\n"
                    f"[Build and approval](https://github.com/{REPOSITORY}/actions/runs/{data['buildRunID']}).\n"
                    f"[Live ACP and chart acceptance](https://github.com/{REPOSITORY}/actions/runs/{proof['runID']}).\n\n"
                    "Attached manifests bind the exact chart, images, and qualification evidence.",
        })
    files = [directory / name for name in ("candidate.json", "qualification.json", "acceptance.json", data["chart"]["file"])]
    for path in files:
        current = api(f"repos/{REPOSITORY}/releases/{release['id']}")
        assets = [asset for asset in current["assets"] if asset["name"] == path.name]
        require(len(assets) <= 1, f"ambiguous release asset {path.name}")
        if not assets:
            command("gh", "release", "upload", data["version"], str(path), "--repo", REPOSITORY)
        # Downloads also verify a completed upload after an interrupted attempt.
        with tempfile.TemporaryDirectory(prefix="orka-release-asset-") as temporary:
            command("gh", "release", "download", data["version"], "--repo", REPOSITORY,
                    "--pattern", path.name, "--dir", temporary)
            require(file_hash(Path(temporary) / path.name) == file_hash(path),
                    f"release asset {path.name} has different bytes; refusing to overwrite it")
    if release["draft"]:
        api(f"repos/{REPOSITORY}/releases/{release['id']}", {
            "draft": False, "make_latest": "true" if "latest" in release_aliases(data) else "false",
        }, method="PATCH")


def publish(directory: Path) -> None:
    data = verify_publication(directory)
    check_pages()
    check_image_versions(data)
    create_tag(data, directory)
    promote_images(data)
    pages_commit, index_hash = publish_chart(data, directory)
    build_pages(pages_commit)
    verify_served_chart(data, index_hash)
    archive_release(data, directory)
    summary(f"Published `{data['version']}` from `{data['candidateSHA']}`. "
            "The tag binds the exact candidate and qualification artifact hashes.")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("prepare").add_argument("version")
    candidate = commands.add_parser("candidate")
    candidate.add_argument("version")
    candidate.add_argument("sha")
    make_bundle = commands.add_parser("bundle")
    make_bundle.add_argument("directory", type=Path)
    make_bundle.add_argument("version")
    make_bundle.add_argument("sha")
    make_bundle.add_argument("attempt")
    download = commands.add_parser("download")
    for name in ("run_id", "attempt", "branch", "sha"):
        download.add_argument(name)
    download.add_argument("directory", type=Path)
    for name in ("check-bundle", "qualify", "publish"):
        commands.add_parser(name).add_argument("directory", type=Path)
    environment = commands.add_parser("check-environment")
    environment.add_argument("name")
    environment.add_argument("branch")
    args = parser.parse_args()
    if args.command == "prepare":
        prepare(args.version)
    elif args.command == "candidate":
        validate_candidate(args.version, args.sha)
    elif args.command == "bundle":
        bundle(args.directory.resolve(), args.version, args.sha, args.attempt)
    elif args.command == "download":
        download_bundle(args.run_id, args.attempt, args.branch, args.sha, args.directory.resolve())
    elif args.command == "check-bundle":
        load_bundle(args.directory.resolve())
    elif args.command == "check-environment":
        check_environment(args.name, args.branch, args.name == "release")
    elif args.command == "qualify":
        qualify(args.directory.resolve())
    elif args.command == "publish":
        publish(args.directory.resolve())


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print(f"Release stopped: {error}", file=sys.stderr)
        raise SystemExit(1) from None
