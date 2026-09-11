#!/usr/bin/env python3
"""Update Helm generator inputs for an Orka release."""

from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
VERSION_RE = re.compile(r"^v\d+\.\d+\.\d+(?:-(?:beta|rc)\.\d+)?$")


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


def replace_image_tag(
    updates: dict[pathlib.Path, str], path: pathlib.Path, repository: str, tag: str
) -> None:
    """Replace one image tag selected by its exact repository."""
    replace_exact(
        updates,
        path,
        rf"^([ \t]+repository:[ \t]*{re.escape(repository)}[ \t]*\n"
        rf"(?:[ \t]*(?:#.*)?\n)*[ \t]+tag:)[ \t]*.*$",
        rf'\1 "{tag}"',
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

    values = ROOT / "cmd/build/helmify/static/values.yaml"
    # Update every image published by the tagged release explicitly. Selecting
    # by exact repository keeps cardinality checks stable as new chart images
    # are added and prevents unrelated tag fields from being changed silently.
    for repository in (
        "ghcr.io/orka-agents/orka",
        "ghcr.io/orka-agents/orka/workspace-publisher",
        "ghcr.io/orka-agents/orka/agent-harness-wrapper",
        "ghcr.io/orka-agents/orka/ai-worker",
        "ghcr.io/orka-agents/orka/general-worker",
    ):
        replace_image_tag(updates, values, repository, version)

    substitutions = {
        ROOT / "config/manager/manager.yaml": [
            (r"(ghcr\.io/orka-agents/orka/ai-worker:)[^\s]+", rf"\g<1>{version}", 1),
            (r"(ghcr\.io/orka-agents/orka/general-worker:)[^\s]+", rf"\g<1>{version}", 1),
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
