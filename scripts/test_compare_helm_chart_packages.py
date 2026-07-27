#!/usr/bin/env python3
"""Focused tests for compare-helm-chart-packages.py."""

from __future__ import annotations

import io
import pathlib
import subprocess
import sys
import tarfile
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name("compare-helm-chart-packages.py")


def write_archive(
    path: pathlib.Path,
    files: list[tuple[str, bytes]],
    *,
    metadata_seed: int = 0,
) -> None:
    with tarfile.open(path, mode="w:gz") as archive:
        for index, (name, payload) in enumerate(files):
            member = tarfile.TarInfo(name)
            member.size = len(payload)
            member.mode = 0o644
            member.mtime = metadata_seed + index
            member.uid = metadata_seed + index
            member.gid = metadata_seed + index
            member.uname = f"user-{metadata_seed}"
            member.gname = f"group-{metadata_seed}"
            archive.addfile(member, io.BytesIO(payload))


class CompareHelmChartPackagesTest(unittest.TestCase):
    def compare(self, first: pathlib.Path, second: pathlib.Path) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, "-I", str(SCRIPT), str(first), str(second)],
            check=False,
            capture_output=True,
            text=True,
        )

    def test_allows_identical_normalized_contents(self) -> None:
        files = [
            ("orka/Chart.yaml", b"apiVersion: v2\nname: orka\nversion: 1.2.3\n"),
            ("orka/crds/task.yaml", b"kind: CustomResourceDefinition\n"),
        ]
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            first = root / "first.tgz"
            second = root / "second.tgz"
            write_archive(first, files, metadata_seed=1)
            write_archive(second, list(reversed(files)), metadata_seed=100)

            self.assertNotEqual(first.read_bytes(), second.read_bytes())
            self.assertEqual(self.compare(first, second).returncode, 0)

    def test_rejects_changed_contents(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            first = root / "first.tgz"
            second = root / "second.tgz"
            write_archive(first, [("orka/values.yaml", b"replicas: 1\n")])
            write_archive(second, [("orka/values.yaml", b"replicas: 2\n")])

            self.assertEqual(self.compare(first, second).returncode, 1)

    def test_fails_closed_for_unsafe_archive_member(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            safe = root / "safe.tgz"
            unsafe = root / "unsafe.tgz"
            write_archive(safe, [("orka/Chart.yaml", b"name: orka\n")])
            write_archive(unsafe, [("../Chart.yaml", b"name: orka\n")])

            result = self.compare(safe, unsafe)
            self.assertEqual(result.returncode, 2)
            self.assertIn("unsafe archive member", result.stderr)


if __name__ == "__main__":
    unittest.main()
