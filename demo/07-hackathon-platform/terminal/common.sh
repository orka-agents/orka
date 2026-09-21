#!/usr/bin/env bash
set -eu -o pipefail

terminal_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
capture_repo=$(CDPATH='' cd -- "$terminal_root/../../.." && pwd)
cd "$capture_repo"

export ORKA_DEMO_ENV=/dev/null
export ORKA_CONFIG_DIR="$capture_repo/bin/hackathon-platform/terminal/orka-config"
export DEMO_AUTO=1
export TYPE_SPEED=${TYPE_SPEED:-72}
source "$capture_repo/demo/lib/demo.sh"
# Select the existing private CLI configuration without changing HOME.
orka() { python3 "$capture_repo/demo/06-hackathon/cli.py" "$@"; }
export -f orka
verify_scene() { python3 "$capture_repo/demo/06-hackathon/terminal/view.py" ready "$1"; }
fix_task=hackathon-fix-0afb30da8140
printf '\033[2J\033[H\033[?25l'
