#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 MANAGER_YAML AI_WORKER_IMAGE GENERAL_WORKER_IMAGE" >&2
  exit 2
fi

manager_yaml="$1"
ai_image="$2"
general_image="$3"
[[ -f "${manager_yaml}" && ! -L "${manager_yaml}" ]] || {
  echo "manager manifest must be a regular file: ${manager_yaml}" >&2
  exit 1
}
[[ -n "${ai_image}" && -n "${general_image}" ]] || {
  echo "worker image references must not be empty" >&2
  exit 1
}

python3 - "${manager_yaml}" "${ai_image}" "${general_image}" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
text = path.read_text()
for flag, value in (("--ai-worker-image", sys.argv[2]), ("--general-worker-image", sys.argv[3])):
    prefix = f"          - {flag}="
    matches = [line for line in text.splitlines() if line.startswith(prefix)]
    if len(matches) != 1:
        raise SystemExit(f"expected exactly one {flag} entry in {path}, found {len(matches)}")
    text = text.replace(matches[0], prefix + value, 1)
temporary = path.with_name(f".{path.name}.worker-images.tmp")
temporary.write_text(text)
temporary.replace(path)
PY
