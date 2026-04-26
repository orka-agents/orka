# Demo Magic Scenarios

This directory contains a small `demo-magic` kit for showing Orka in four ways:

- `10-chat-pr.sh`: chat endpoint -> coordinator -> coder -> reviewers -> PR
- `20-manual-workflow.sh`: explicit coordinator task with the same PR workflow
- `30-cron-workflow.sh`: scheduled runtime task with recurring child runs
- `40-security-scanning.sh`: repository scan -> findings -> patch -> PR

There is also:

- `00-preflight.sh`: quick readiness check before the live demo
- `reset.sh`: cleanup helper for the named demo resources

## Prerequisites

- A running Orka controller reachable at `ORKA_API_BASE`
- `kubectl`, `curl`, `jq`, and a built `bin/orka`
- A local `demo-magic.sh`
- A demo namespace that is not the controller namespace
- A Provider CRD and runtime credential Secret in that demo namespace
- A git credential Secret in that demo namespace for clone, push, and PR creation

Set `DEMO_MAGIC_PATH` to your local checkout of `demo-magic.sh`. The scripts also look in a few common locations, but the environment variable is the most reliable option.

## Required Environment

Set these before running the scenarios:

```bash
export DEMO_MAGIC_PATH="$HOME/src/demo-magic/demo-magic.sh"
export ORKA_API_BASE="http://127.0.0.1:8080"
export DEMO_NAMESPACE="demo-magic"

export DEMO_PROVIDER_REF="copilot-proxy-openai"
export DEMO_AI_MODEL="gpt-5.4"

export DEMO_RUNTIME_TYPE="codex"
export DEMO_RUNTIME_MODEL="gpt-5.4"
export DEMO_RUNTIME_SECRET_REF="codex-proxy-token"

export DEMO_GIT_REPO="https://github.com/your-org/your-demo-repo.git"
export DEMO_GIT_BRANCH="main"
export DEMO_GIT_SECRET_REF="git-credentials"
```

Optional tuning:

```bash
export DEMO_GIT_FORK_REPO="https://github.com/your-org/your-demo-fork.git"
export DEMO_PR_BASE_BRANCH="main"
export DEMO_GIT_SUB_PATH="services/api"

export DEMO_CHAT_REQUEST="..."
export DEMO_MANUAL_REQUEST="..."
export DEMO_CRON_REQUEST="..."
export DEMO_CRON_SCHEDULE="*/1 * * * *"
export DEMO_SECURITY_SCAN_NAME="demo-security-repository"
export DEMO_SECURITY_SCHEDULE="0 */6 * * *"
```

## Suggested Run Order

```bash
hack/demos/reset.sh
hack/demos/00-preflight.sh
hack/demos/10-chat-pr.sh
hack/demos/20-manual-workflow.sh
hack/demos/30-cron-workflow.sh
hack/demos/40-security-scanning.sh
```

## Notes

- The scripts render working files into `DEMO_WORKDIR`, which defaults to `/tmp/orka-demo`.
- `30-cron-workflow.sh` quietly waits for at least one scheduled child run before it starts presenting commands.
- `40-security-scanning.sh` quietly seeds the repository scan if needed. The very first run can take a while.
- The security demo is one-shot by default. Set `DEMO_SECURITY_SCHEDULE` if you want the `RepositoryScan` to recur.
- Security scan history is stored in SQLite. Deleting the `RepositoryScan` CR cleans up Kubernetes resources, but historical findings and scan runs may still exist. If you want a visually clean slate for that demo, use a fresh `DEMO_SECURITY_SCAN_NAME`.
- If `bin/orka` is not built in this repo, the scripts automatically fall back to a sibling checkout at `../orka/bin/orka` when present.
