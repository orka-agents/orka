#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

log() { printf '==> %s\n' "$*" >&2; }

log "Running local RepositoryMonitor validation bundle"
bash scripts/repository-monitor-validate.sh

log "Running live GitHub/kind preflight"
if bash scripts/live-github-label-trigger-e2e.sh --preflight-only; then
  live_preflight="passed"
else
  live_preflight="blocked"
fi

cat <<EOF_AUDIT
RepositoryMonitor completion audit
===================================

Local/fake-GitHub validation: passed
Live GitHub/kind preflight: ${live_preflight}

Evidence covered by local validation:
- Durable command intake, replay, coalescing, guard-label blocking, closed/out-of-scope target rejection.
- Issue implementation to patch artifact, controller-owned mutation task, branch/PR creation, issue status comment link.
- Stop/resume late-task safety.
- PR review, repair, readiness, and optional head-bound automerge against fake GitHub.
- work_actions, implementation_jobs, github_mutation_records persistence/API/CLI/docs surfaces.
- Generated CLI docs, example manifests, website docs, and repository-monitor smoke workflow syntax.

Remaining live/manual evidence:
- Run scripts/live-github-label-trigger-e2e.sh without --preflight-only on a machine with Docker available to demonstrate the live kind/webhook path.
EOF_AUDIT

if [[ "${live_preflight}" != "passed" ]]; then
  exit 2
fi
