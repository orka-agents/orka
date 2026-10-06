#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
bash scripts/repository-monitor-validate.sh
printf '%s\n' 'Local RepositoryMonitor validation passed.' 'Live model, publication, and GitHub rule enforcement must be validated separately against an explicitly selected repository.'
