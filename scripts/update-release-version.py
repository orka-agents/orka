#!/usr/bin/env python3
"""Update Gatekeeper-style Helm generator inputs for an Orka release."""

from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
VERSION_RE = re.compile(r"^v\d+\.\d+\.\d+(?:-(?:beta|rc)\.\d+)?$")
MEMORY_RELEASE_STAGE = "foundation"
PUBLISHED_IMAGES = (
    ("controller", ""),
    ("ai-worker", "/ai-worker"),
    ("general-worker", "/general-worker"),
    ("agent-harness-wrapper", "/agent-harness-wrapper"),
)
HELM_IMAGE_REPOSITORIES = (
    "ghcr.io/orka-agents/orka",
    "ghcr.io/orka-agents/orka/ai-worker",
    "ghcr.io/orka-agents/orka/general-worker",
    "ghcr.io/orka-agents/orka/agent-harness-wrapper",
)
HELM_IMAGE_TAG_REPLACEMENTS = len(PUBLISHED_IMAGES)


def replace_exact(
    updates: dict[pathlib.Path, str],
    path: pathlib.Path,
    pattern: str,
    replacement: str,
    expected: int = 1,
) -> None:
    text = updates.get(path)
    if text is None:
        text = path.read_text()
    updated, count = re.subn(pattern, replacement, text, flags=re.MULTILINE)
    if count != expected:
        raise RuntimeError(f"expected {expected} replacements in {path}, found {count}")
    updates[path] = updated


def parse_image_suffixes(block: str) -> list[tuple[str, str]]:
    return re.findall(
        r'^\s+- image:\s*([^\s]+)\n\s+image_suffix:\s*"([^"]*)"',
        block,
        flags=re.MULTILINE,
    )


def validate_release_image_contract(release_workflow: str) -> None:
    release_stage_match = re.search(
        r"^\s+MEMORY_RELEASE_STAGE:\s*([^\s]+)\s*$",
        release_workflow,
        flags=re.MULTILINE,
    )
    if release_stage_match is None or release_stage_match.group(1) != MEMORY_RELEASE_STAGE:
        raise RuntimeError(
            f"release workflow memory stage must be {MEMORY_RELEASE_STAGE}"
        )

    build_matrix = release_workflow.split("  build-and-push:", 1)[1].split("    steps:", 1)[0]
    scan_matrix = release_workflow.split("  scan:", 1)[1].split("    steps:", 1)[0]
    sign_matrix = release_workflow.split("  sign-and-attest:", 1)[1].split("    steps:", 1)[0]
    promote_block = release_workflow.split("      - name: Promote release tags", 1)[1].split(
        "  publish-helm-chart:", 1
    )[0]

    for label, actual in {
        "build-and-push image matrix": parse_image_suffixes(build_matrix),
        "sign-and-attest image matrix": parse_image_suffixes(sign_matrix),
    }.items():
        if actual != list(PUBLISHED_IMAGES):
            raise RuntimeError(f"{label} = {actual!r}, expected {list(PUBLISHED_IMAGES)!r}")

    scan_entries = re.findall(
        r'^\s+- image:\s*([^\s]+)\n'
        r'\s+image_suffix:\s*"([^"]*)"\n'
        r'\s+platform:\s*([^\s]+)',
        scan_matrix,
        flags=re.MULTILINE,
    )
    expected_scan_entries = [
        (image, suffix, platform)
        for image, suffix in PUBLISHED_IMAGES
        for platform in ("linux/amd64", "linux/arm64")
    ]
    if scan_entries != expected_scan_entries:
        raise RuntimeError(
            f"scan platform matrix = {scan_entries!r}, expected {expected_scan_entries!r}"
        )

    promote_entries = re.findall(
        r'^\s+promote_image\s+([^\s]+)\s+"([^"]*)"$',
        promote_block,
        flags=re.MULTILINE,
    )
    if promote_entries != list(PUBLISHED_IMAGES):
        raise RuntimeError(
            f"promote-release-tags image list = {promote_entries!r}, "
            f"expected {list(PUBLISHED_IMAGES)!r}"
        )


def main() -> int:
    if len(sys.argv) != 2 or not VERSION_RE.fullmatch(sys.argv[1]):
        print("usage: update-release-version.py vX.Y.Z[-beta.N|-rc.N]", file=sys.stderr)
        return 2

    release_tag = sys.argv[1]
    # docker/metadata-action's semver {{version}} tag strips the leading v.
    # Keep appVersion aligned to the Git tag and image references aligned to
    # the published bare semver image tag.
    version = release_tag.removeprefix("v")
    updates: dict[pathlib.Path, str] = {}

    replace_exact(updates, ROOT / "Makefile", r"^VERSION := .*$", f"VERSION := {release_tag}")

    chart = ROOT / "cmd/build/helmify/static/Chart.yaml"
    replace_exact(updates, chart, r"^version: .*$", f"version: {version}")
    replace_exact(updates, chart, r"^appVersion: .*$", f'appVersion: "{release_tag}"')
    chart_stage_match = re.search(
        r"^\s+memory\.orka\.ai/release-stage:\s*([^\s]+)\s*$",
        updates[chart],
        flags=re.MULTILINE,
    )
    if chart_stage_match is None or chart_stage_match.group(1) != MEMORY_RELEASE_STAGE:
        raise RuntimeError(f"Helm chart memory stage must be {MEMORY_RELEASE_STAGE}")

    main_source = (ROOT / "cmd/main.go").read_text()
    expected_source_gate = f"memoryBuildReleaseStage = memoryReleaseStage{MEMORY_RELEASE_STAGE.title()}"
    if expected_source_gate not in main_source:
        raise RuntimeError(
            f"controller source must select the {MEMORY_RELEASE_STAGE} memory release gate"
        )

    values = ROOT / "cmd/build/helmify/static/values.yaml"
    replace_exact(
        updates,
        values,
        r"^(\s+tag:)\s*.*$",
        rf'\1 "{version}"',
        expected=HELM_IMAGE_TAG_REPLACEMENTS,
    )

    helm_repositories = [
        value.strip('"\'')
        for value in re.findall(r"^\s+repository:\s*([^\s]+)\s*$", updates[values], flags=re.MULTILINE)
    ]
    if helm_repositories != list(HELM_IMAGE_REPOSITORIES):
        raise RuntimeError(
            f"Helm image repositories = {helm_repositories!r}, "
            f"expected {list(HELM_IMAGE_REPOSITORIES)!r}"
        )

    # Keep the exact image names/suffixes in every publication stage synchronized
    # with the chart's four-image release contract.
    release_workflow = (ROOT / ".github/workflows/release.yml").read_text()
    validate_release_image_contract(release_workflow)

    substitutions = {
        ROOT / "config/manager/manager.yaml": [
            (r"(ghcr\.io/orka-agents/orka/ai-worker:)[^\s]+", rf"\g<1>{version}", 1),
            (r"(ghcr\.io/orka-agents/orka/general-worker:)[^\s]+", rf"\g<1>{version}", 1),
        ],
        ROOT / "config/harness-wrapper/deployment.yaml": [
            (r"(ghcr\.io/orka-agents/orka/agent-harness-wrapper:)[^\s]+", rf"\g<1>{version}", 1),
        ],
        ROOT / "config/manager/kustomization.yaml": [
            (r"^(\s*newTag:)\s*.*$", rf"\g<1> {version}", 2),
        ],
    }
    for path, replacements in substitutions.items():
        for pattern, replacement, expected in replacements:
            replace_exact(updates, path, pattern, replacement, expected)

    # Validate every expected replacement before changing any file, then replace
    # each file atomically so a failed write cannot leave truncated YAML.
    for path, contents in updates.items():
        temporary = path.with_name(f".{path.name}.release-version.tmp")
        temporary.write_text(contents)
        temporary.replace(path)

    print(f"Updated release inputs for {release_tag}.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
