#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/generate-cli-docs.sh [--check] [output-file]

Generate the Docusaurus CLI command reference from the compiled orka binary.
Set ORKA_CLI_BIN to use a specific binary. Defaults to bin/orka and builds it
with make build-cli when it is missing.
USAGE
}

check=false
if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi
if [[ "${1:-}" == "--check" ]]; then
  check=true
  shift
fi

out="${1:-website/docs/reference/cli-commands.md}"
cli_bin="${ORKA_CLI_BIN:-bin/orka}"

if [[ ! -x "$cli_bin" ]]; then
  make build-cli >/dev/null
fi

commands=(
  ""
  "login"
  "run"
  "config" "config set-server" "config set-token" "config set-namespace" "config view"
  "auth" "auth validate" "auth whoami"
  "models" "models list"
  "status"
  "audit" "audit trace"
  "task" "task create" "task list" "task get" "task logs" "task result" "task plan" "task children" "task events" "task follow" "task trace" "task approvals" "task approve" "task decline" "task fork" "task wait" "task delete" "task artifacts" "task download"
  "workspace" "workspace status"
  "provider" "provider list" "provider get" "provider create" "provider update" "provider delete"
  "agent" "agent list" "agent get" "agent create" "agent update" "agent delete"
  "tool" "tool list" "tool get" "tool create" "tool update" "tool delete"
  "skill" "skill list" "skill get" "skill content" "skill create" "skill init" "skill validate" "skill import" "skill update" "skill delete"
  "secret" "secret list"
  "session" "session list" "session get" "session delete"
  "memory" "memory list" "memory get" "memory create" "memory update" "memory delete" "memory enable" "memory disable" "memory proposal" "memory proposal list" "memory proposal get" "memory proposal review" "memory proposal apply" "memory proposal archive"
  "security" "security repo" "security repo list" "security repo get" "security repo create" "security repo update" "security repo delete" "security scan" "security scan run" "security scan list" "security threat-model" "security threat-model get" "security threat-model update" "security finding" "security finding list" "security finding get" "security finding dismiss" "security finding reopen" "security finding validate" "security finding patch" "security finding patches" "security finding pr" "security slice" "security slice list" "security slice get" "security dropped-findings" "security dropped-findings list"
  "monitor" "monitor list" "monitor get" "monitor create" "monitor update" "monitor delete" "monitor run" "monitor runs" "monitor items"
  "monitor issues" "monitor issues list" "monitor issues get"
  "monitor commands" "monitor commands list" "monitor commands get" "monitor commands create"
  "monitor actions" "monitor actions list" "monitor actions get"
  "monitor work-actions" "monitor work-actions list" "monitor work-actions get"
  "monitor implementations" "monitor implementations list" "monitor implementations get"
  "monitor mutations" "monitor mutations list" "monitor mutations get"
  "monitor issue" "monitor issue triage" "monitor issue research" "monitor issue plan" "monitor issue approve-plan" "monitor issue implement" "monitor issue decompose" "monitor issue stop" "monitor issue resume" "monitor issue status" "monitor issue implementation" "monitor issue implementation get" "monitor issue patch" "monitor issue patch preview"
  "monitor pr" "monitor pr review" "monitor pr fix" "monitor pr fix-ci" "monitor pr update-branch" "monitor pr automerge" "monitor pr stop" "monitor pr resume" "monitor pr status" "monitor pr repairs" "monitor pr repairs list" "monitor pr ready" "monitor pr ready list" "monitor pr ready readiness"
  "monitor doctor" "monitor watch" "monitor trigger-labels" "monitor trigger-labels validate" "monitor events"
  "substrate" "substrate pool" "substrate pool list" "substrate pool get" "substrate pool create" "substrate pool update" "substrate pool delete"
  "gateway" "gateway list" "gateway get" "gateway class" "gateway class list" "gateway class get" "gateway binding" "gateway binding list" "gateway binding get" "gateway events" "gateway events list" "gateway events get" "gateway deliveries" "gateway deliveries list" "gateway deliveries get" "gateway deliveries retry"
  "completion"
)

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

{
  cat <<'HEADER'
---
slug: /cli-commands
---

# CLI Command Reference

This page is generated from `orka --help` output. Do not edit it by hand; run:

```bash
make docs-cli
```

For workflow-oriented examples and coverage notes, see [CLI Reference](./cli.md).

HEADER

  for command in "${commands[@]}"; do
    if [[ -z "$command" ]]; then
      title="orka"
      args=(--help)
    else
      title="orka $command"
      # shellcheck disable=SC2206
      parts=($command)
      args=("${parts[@]}" --help)
    fi

    printf '## `%s`\n\n' "$title"
    printf '```text\n'
    "$cli_bin" "${args[@]}"
    printf '```\n\n'
  done
} > "$tmp"

if [[ "$check" == true ]]; then
  if ! cmp -s "$tmp" "$out"; then
    echo "$out is out of date; run make docs-cli" >&2
    diff -u "$out" "$tmp" >&2 || true
    exit 1
  fi
  echo "$out is up to date"
else
  mkdir -p "$(dirname "$out")"
  mv "$tmp" "$out"
  trap - EXIT
fi
