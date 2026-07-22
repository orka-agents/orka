#!/usr/bin/env python3
"""Compare Helm chart archives by normalized member contents."""

from __future__ import annotations

import pathlib
import sys
import tarfile
from typing import TypeAlias

Entry: TypeAlias = tuple[str, str, int, bytes]


def snapshot(path: pathlib.Path) -> list[Entry]:
    entries: list[Entry] = []
    names: set[str] = set()
    with tarfile.open(path, mode="r:gz") as archive:
        for member in archive.getmembers():
            pure_name = pathlib.PurePosixPath(member.name)
            if pure_name.is_absolute() or ".." in pure_name.parts:
                raise ValueError(f"unsafe archive member: {member.name}")
            if member.name in names:
                raise ValueError(f"duplicate archive member: {member.name}")
            names.add(member.name)

            if member.isfile():
                extracted = archive.extractfile(member)
                if extracted is None:
                    raise ValueError(f"could not read archive member: {member.name}")
                kind = "file"
                payload = extracted.read()
            elif member.isdir():
                kind = "directory"
                payload = b""
            elif member.issym():
                kind = "symlink"
                payload = member.linkname.encode()
            elif member.islnk():
                kind = "hardlink"
                payload = member.linkname.encode()
            else:
                raise ValueError(f"unsupported archive member: {member.name}")

            entries.append((member.name, kind, member.mode & 0o7777, payload))
    return sorted(entries)


def main() -> int:
    if len(sys.argv) != 3:
        print(f"usage: {pathlib.Path(sys.argv[0]).name} ARCHIVE_A ARCHIVE_B", file=sys.stderr)
        return 2

    try:
        identical = snapshot(pathlib.Path(sys.argv[1])) == snapshot(pathlib.Path(sys.argv[2]))
    except (OSError, tarfile.TarError, ValueError) as error:
        print(f"could not compare chart contents: {error}", file=sys.stderr)
        return 2
    return 0 if identical else 1


if __name__ == "__main__":
    raise SystemExit(main())
