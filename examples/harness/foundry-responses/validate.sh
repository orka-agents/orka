#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: examples/harness/foundry-responses/validate.sh [--agentkit PATH] [--full]

Runs deterministic local validation for the Orka Foundry hosted Responses adapter.
No live Foundry endpoint, token, API key, or Kubernetes cluster is required.

Options:
  --agentkit PATH  Also run AgentKit's deterministic Foundry brokered protocol
                   tests from PATH/runtimes/common. This is explicit because a
                   sibling AgentKit checkout may contain unrelated local changes.
  --full           Also run `make test` for the full non-e2e Orka suite.
  -h, --help       Show this help.
USAGE
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
agentkit_root=""
run_full=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --agentkit)
      [[ $# -ge 2 ]] || { echo "--agentkit requires a path" >&2; exit 2; }
      agentkit_root="$2"
      shift 2
      ;;
    --full)
      run_full=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

run() {
  printf '\n==> %s\n' "$*" >&2
  "$@"
}

cd "$repo_root"

grep -qx '!examples/harness/foundry-responses/\*.go' .dockerignore || {
  echo "Foundry Responses Go sources are not included in the Docker build context" >&2
  exit 1
}

run go test ./examples/harness/foundry-responses -count=1
run go test \
  ./examples/harness/foundry \
  ./examples/harness/foundry-responses \
  ./examples/harness/echo \
  ./internal/harness \
  ./internal/harness/conformance
run go test ./internal/controller -run 'Test.*(AgentRuntime|Harness|Brokered|Runtime)'
run python3 -m unittest examples/harness/foundry-responses/test_fetch_task_events.py
while IFS= read -r -d '' script; do
  run bash -n "$script"
done < <(find examples -type f -name '*.sh' -print0 | sort -z)

expect_verifier_failure() {
  local fixture="$1"
  local expected="$2"
  local label="$3"
  local out_file err_file code
  out_file="$(mktemp)"
  err_file="$(mktemp)"
  set +e
  examples/fibey-custom-agent-demo/verify-foundry-responses.sh --json "$fixture" >"$out_file" 2>"$err_file"
  code=$?
  set -e
  if [[ "$code" == "0" ]]; then
    cat "$out_file" >&2
    rm -f "$out_file" "$err_file"
    echo "expected ${label} verifier fixture to fail" >&2
    exit 1
  fi
  if ! grep -q "$expected" "$err_file"; then
    cat "$err_file" >&2
    rm -f "$out_file" "$err_file"
    echo "${label} fixture failed for the wrong reason" >&2
    exit 1
  fi
  rm -f "$out_file" "$err_file"
}

live_smoke_err="$(mktemp)"
set +e
ORKA_FOUNDRY_RESPONSES_ENDPOINT="https://:443/agents/test/endpoint/protocols/openai/responses" \
  ORKA_FOUNDRY_RESPONSES_PROJECT_ENDPOINT="" \
  ORKA_FOUNDRY_RESPONSES_AGENT_NAME="" \
  ORKA_FOUNDRY_RESPONSES_API_KEY="placeholder" \
  ORKA_FOUNDRY_RESPONSES_AUTH_BEARER="" \
  examples/harness/foundry-responses/live-smoke.sh >/dev/null 2>"$live_smoke_err"
live_smoke_code=$?
set -e
if [[ "$live_smoke_code" == "0" ]] || ! grep -q "safe /responses URL" "$live_smoke_err"; then
  cat "$live_smoke_err" >&2
  rm -f "$live_smoke_err"
  echo "expected empty-hostname live smoke preflight to fail" >&2
  exit 1
fi
rm -f "$live_smoke_err"

missing_image_err="$(mktemp)"
set +e
ORKA_FOUNDRY_RESPONSES_ENDPOINT="http://127.0.0.1/agents/test/endpoint/protocols/openai/responses" \
  ORKA_FOUNDRY_RESPONSES_API_KEY="placeholder" \
  ORKA_FOUNDRY_RESPONSES_AUTH_BEARER="" \
  ORKA_FOUNDRY_RESPONSES_ADAPTER_IMAGE="" \
  ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES="" \
  examples/harness/foundry-responses/live-smoke.sh --apply >/dev/null 2>"$missing_image_err"
missing_image_code=$?
set -e
if [[ "$missing_image_code" == "0" ]] || ! grep -q "ADAPTER_IMAGE is required" "$missing_image_err"; then
  cat "$missing_image_err" >&2
  rm -f "$missing_image_err"
  echo "expected live smoke apply without an explicit image to fail" >&2
  exit 1
fi
rm -f "$missing_image_err"

missing_proof_err="$(mktemp)"
set +e
ORKA_FOUNDRY_RESPONSES_ENDPOINT="http://127.0.0.1/agents/test/endpoint/protocols/openai/responses" \
  ORKA_FOUNDRY_RESPONSES_API_KEY="placeholder" \
  ORKA_FOUNDRY_RESPONSES_AUTH_BEARER="" \
  ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES="read" \
  ORKA_FOUNDRY_RESPONSES_BROKERED_CONTINUATION_PROOF="" \
  examples/harness/foundry-responses/live-smoke.sh >/dev/null 2>"$missing_proof_err"
missing_proof_code=$?
set -e
if [[ "$missing_proof_code" == "0" ]] || ! grep -q "CONTINUATION_PROOF is required" "$missing_proof_err"; then
  cat "$missing_proof_err" >&2
  rm -f "$missing_proof_err"
  echo "expected brokered live smoke without continuation proof to fail" >&2
  exit 1
fi
rm -f "$missing_proof_err"

whitespace_proof_err="$(mktemp)"
set +e
ORKA_FOUNDRY_RESPONSES_ENDPOINT="http://127.0.0.1/agents/test/endpoint/protocols/openai/responses" \
  ORKA_FOUNDRY_RESPONSES_API_KEY="placeholder" \
  ORKA_FOUNDRY_RESPONSES_AUTH_BEARER="" \
  ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES="read" \
  ORKA_FOUNDRY_RESPONSES_BROKERED_CONTINUATION_PROOF="   " \
  examples/harness/foundry-responses/live-smoke.sh >/dev/null 2>"$whitespace_proof_err"
whitespace_proof_code=$?
set -e
if [[ "$whitespace_proof_code" == "0" ]] || ! grep -q "CONTINUATION_PROOF is required" "$whitespace_proof_err"; then
  cat "$whitespace_proof_err" >&2
  rm -f "$whitespace_proof_err"
  echo "expected whitespace-only brokered continuation proof to fail" >&2
  exit 1
fi
rm -f "$whitespace_proof_err"

invalid_class_err="$(mktemp)"
set +e
ORKA_FOUNDRY_RESPONSES_ENDPOINT="http://127.0.0.1/agents/test/endpoint/protocols/openai/responses" \
  ORKA_FOUNDRY_RESPONSES_API_KEY="placeholder" \
  ORKA_FOUNDRY_RESPONSES_AUTH_BEARER="" \
  ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES="r ead" \
  examples/harness/foundry-responses/live-smoke.sh >/dev/null 2>"$invalid_class_err"
invalid_class_code=$?
set -e
if [[ "$invalid_class_code" == "0" ]] || ! grep -q "unsupported brokered class 'r ead'" "$invalid_class_err"; then
  cat "$invalid_class_err" >&2
  rm -f "$invalid_class_err"
  echo "expected internally spaced brokered class to fail preflight" >&2
  exit 1
fi
rm -f "$invalid_class_err"

smoke_tmp="$(mktemp -d)"
smoke_capture="$smoke_tmp/rendered.yaml"
cat >"$smoke_tmp/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ " $* " == *" apply "* ]]; then
  cat >>"$CAPTURE_FILE"
  printf '\n---\n' >>"$CAPTURE_FILE"
  exit 0
fi
if [[ "${1:-}" == "get" && "${2:-}" == "namespace" ]]; then
  exit 0
fi
if [[ " $* " == *" get deployment/"* ]]; then
  exit 1
fi
exit 0
SH
chmod +x "$smoke_tmp/kubectl"
PATH="$smoke_tmp:$PATH" \
  CAPTURE_FILE="$smoke_capture" \
  ORKA_FOUNDRY_RESPONSES_ENDPOINT="http://127.0.0.1/agents/test/endpoint/protocols/openai/responses" \
  ORKA_FOUNDRY_RESPONSES_API_KEY="placeholder" \
  ORKA_FOUNDRY_RESPONSES_AUTH_BEARER="" \
  ORKA_FOUNDRY_RESPONSES_ADAPTER_BEARER_TOKEN="adapter-placeholder" \
  ORKA_FOUNDRY_RESPONSES_ADAPTER_IMAGE="example.invalid/foundry-adapter:test" \
  ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES="" \
  examples/harness/foundry-responses/live-smoke.sh --apply >/dev/null 2>"$smoke_tmp/stderr"
if grep -q '^    - brokered$' "$smoke_capture" || \
   grep -q '^    brokeredToolClasses:' "$smoke_capture" || \
   ! grep -q '^    supportsContinuation: false$' "$smoke_capture"; then
  cat "$smoke_capture" >&2
  rm -rf "$smoke_tmp"
  echo "observed-only live smoke rendered incompatible brokered capabilities" >&2
  exit 1
fi
rm -rf "$smoke_tmp"

run examples/fibey-custom-agent-demo/verify-foundry-responses.sh \
  --json examples/fibey-custom-agent-demo/testdata/foundry-responses-events-pass.json
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-missing-write-exec.json \
  "missing write ToolCallStarted event after approval" \
  "missing-write"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-duplicate-write.json \
  "duplicate write execution starts for dispatch-work-order" \
  "duplicate-write"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-missing-approval-decision.json \
  "missing ApprovalApproved event" \
  "missing-approval-decision"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-declined-write.json \
  "follows ApprovalDeclined" \
  "declined-write"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-missing-approval-id.json \
  "is missing approvalID" \
  "missing-approval-id"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-decision-before-request.json \
  "has no matching preceding ApprovalApproved" \
  "decision-before-request"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-overlapping-write-marker.json \
  "missing write ToolCallStarted event after approval" \
  "overlapping-write-marker"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-mismatched-write-request.json \
  "has no matching preceding mapped request" \
  "mismatched-write-request"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-partial-idempotency.json \
  "write execution for escalate-incident is missing execution idempotency key evidence" \
  "partial-idempotency"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-generic-idempotency-only.json \
  "write execution for dispatch-work-order is missing execution idempotency key evidence" \
  "generic-idempotency-only"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-failure-after-start.json \
  "write execution for dispatch-work-order has matching ToolCallFailed" \
  "failure-after-start"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-completion-after-terminal.json \
  "write execution for dispatch-work-order is missing ToolCallCompleted" \
  "completion-after-terminal"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-truncated-page.json \
  "event JSON is incomplete" \
  "truncated-event-page"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-tail-page.json \
  "afterSeq must be 0" \
  "tail-event-page"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-write-after-terminal.json \
  "terminal completion event does not follow all write executions" \
  "write-after-terminal"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-terminal-write-terminal.json \
  "terminal completion event does not follow all write executions" \
  "terminal-write-terminal"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-task-failed.json \
  "final Task lifecycle outcome is TaskFailed" \
  "task-failed-after-runtime-completion"
expect_verifier_failure \
  examples/fibey-custom-agent-demo/testdata/foundry-responses-events-success-before-runtime-completion.json \
  "final execution event is AgentRuntimeCompleted" \
  "task-success-before-runtime-completion"

if [[ "$run_full" == "1" ]]; then
  run make test
fi

if [[ -n "$agentkit_root" ]]; then
  common_dir="$agentkit_root/runtimes/common"
  if [[ ! -d "$common_dir" ]]; then
    echo "AgentKit common runtime directory not found: $common_dir" >&2
    exit 2
  fi
  if ! command -v uv >/dev/null 2>&1; then
    echo "uv is required for AgentKit validation but was not found in PATH" >&2
    exit 2
  fi
  (
    cd "$common_dir"
    run uv run --extra dev pytest -q \
      tests/test_foundry_brokered_protocol.py \
      tests/test_brokered_schema.py \
      tests/test_foundry_protocol.py
  )
fi

printf '\nFoundry hosted Responses local validation passed.\n' >&2
