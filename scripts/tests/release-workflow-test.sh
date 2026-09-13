#!/usr/bin/env bash
set -Eeuo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
command -v helm >/dev/null
python3 -m unittest discover -s "${root}/scripts/tests" -p test_release_workflow.py
