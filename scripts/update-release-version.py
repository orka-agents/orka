#!/usr/bin/env python3
"""Update Gatekeeper-style Helm generator inputs for an Orka release."""

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

    chart = ROOT / "third_party/open-policy-agent/gatekeeper/helmify/static/Chart.yaml"
    replace_exact(updates, chart, r"^version: .*$", f"version: {version}")
    replace_exact(updates, chart, r"^appVersion: .*$", f'appVersion: "{release_tag}"')

    values = ROOT / "third_party/open-policy-agent/gatekeeper/helmify/static/values.yaml"
    replace_exact(updates, values, r"^(\s+tag:)\s*.*$", rf'\1 "{version}"', expected=4)

    substitutions = {
        ROOT / "config/manager/manager.yaml": [
            (r"(ghcr\.io/orka-agents/orka/ai-worker:)[^\s]+", rf"\g<1>{version}"),
            (r"(ghcr\.io/orka-agents/orka/general-worker:)[^\s]+", rf"\g<1>{version}"),
            (r"(^\s*image:\s*)controller:[^\s]+$", rf"\g<1>controller:{version}"),
        ],
        ROOT / "config/harness-wrapper/deployment.yaml": [
            (r"(ghcr\.io/orka-agents/orka/agent-harness-wrapper:)[^\s]+", rf"\g<1>{version}"),
        ],
        ROOT / "config/manager/kustomization.yaml": [
            (r"^(\s*newTag:)\s*.*$", rf"\g<1> {version}"),
        ],
    }
    for path, replacements in substitutions.items():
        for pattern, replacement in replacements:
            replace_exact(updates, path, pattern, replacement)

    # Validate every expected replacement before changing any file. Each file is
    # then replaced atomically so a failed write cannot leave truncated YAML.
    for path, contents in updates.items():
        temporary = path.with_name(f".{path.name}.release-version.tmp")
        temporary.write_text(contents)
        temporary.replace(path)

    print(f"Updated release inputs for {release_tag}.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
