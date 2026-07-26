#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

log() { printf '==> %s\n' "$*" >&2; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || { printf 'missing required command: %s\n' "$1" >&2; exit 1; }; }

require_cmd go
require_cmd bash
require_cmd kubectl
require_cmd yarn

log "Running fake-GitHub RepositoryMonitor E2E suite"
bash scripts/repository-monitor-fake-e2e.sh

log "Checking generated CLI docs"
make docs-cli-check

log "Validating RepositoryMonitor example manifests"
for example in \
  examples/github-label-triggered-issue-loop \
  examples/repository-monitor-issue-plan-only \
  examples/repository-monitor-pr-review-repair; do
  log "kubectl kustomize ${example}"
  kubectl kustomize "${example}" >/dev/null
done

log "Building website docs"
(
  cd website
  yarn build
)

log "Validating RepositoryMonitor smoke workflow syntax"
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/repository-monitor-smoke.yml

log "RepositoryMonitor validation passed"
