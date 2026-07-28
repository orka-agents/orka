#!/usr/bin/env python3
"""Fetch a complete Orka task event stream through the paginated CLI."""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable

PAGE_LIMIT = 1000
MAX_PAGES = 10000

Runner = Callable[..., subprocess.CompletedProcess[str]]


def _int_field(payload: dict[str, Any], name: str) -> int:
    value = payload.get(name, 0)
    if isinstance(value, bool):
        raise ValueError(f"{name} must be an integer")
    try:
        parsed = int(value)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{name} must be an integer") from exc
    if parsed < 0:
        raise ValueError(f"{name} must be non-negative")
    return parsed


def fetch_task_events(
    task: str,
    namespace: str,
    *,
    runner: Runner = subprocess.run,
) -> dict[str, Any]:
    after = 0
    all_events: list[dict[str, Any]] = []
    first_payload: dict[str, Any] | None = None

    for _ in range(MAX_PAGES):
        command = [
            "orka",
            "task",
            "events",
            task,
            "--namespace",
            namespace,
            "--after",
            str(after),
            "--limit",
            str(PAGE_LIMIT),
            "--output",
            "json",
        ]
        result = runner(command, capture_output=True, text=True, check=False)
        if result.returncode != 0:
            detail = result.stderr.strip() or result.stdout.strip() or "unknown error"
            raise RuntimeError(f"orka task events failed: {detail}")

        try:
            payload = json.loads(result.stdout)
        except json.JSONDecodeError as exc:
            raise ValueError(f"orka task events returned invalid JSON: {exc}") from exc
        if not isinstance(payload, dict):
            raise ValueError("orka task events response must be a JSON object")
        if first_payload is None:
            first_payload = payload

        response_after = _int_field(payload, "afterSeq")
        latest = _int_field(payload, "latestSeq")
        if response_after != after:
            raise ValueError(
                f"orka task events returned afterSeq {response_after}, expected {after}"
            )
        events = payload.get("events")
        if not isinstance(events, list):
            raise ValueError("orka task events response is missing an events array")

        page_events: list[dict[str, Any]] = []
        last_seq = after
        expected_seq = after + 1
        for event in events:
            if not isinstance(event, dict):
                raise ValueError("orka task events returned a non-object event")
            seq = _int_field(event, "seq")
            if seq != expected_seq:
                raise ValueError(
                    f"orka task events returned sequence {seq}, expected {expected_seq}"
                )
            last_seq = seq
            expected_seq += 1
            page_events.append(event)
        all_events.extend(page_events)

        if last_seq >= latest:
            base = dict(first_payload)
            base["afterSeq"] = 0
            base["latestSeq"] = latest
            base["events"] = all_events
            return base
        if not page_events:
            raise RuntimeError(
                f"orka task events stopped at sequence {after} before latestSeq {latest}"
            )
        after = last_seq

    raise RuntimeError(f"orka task events exceeded {MAX_PAGES} pages")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--task", required=True)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    try:
        payload = fetch_task_events(args.task, args.namespace)
        args.output.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
