#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: examples/fibey-custom-agent-demo/verify-foundry-responses.sh [--task NAME] [--namespace NAME] [--json EVENTS.json]

Checks the live Fibey Foundry hosted AgentKit Responses scenario evidence from
Orka task events. By default it paginates `orka task events --output json`
through `latestSeq`. Use --json to verify a previously captured event payload
without contacting Orka; payloads with `latestSeq` must be complete.

Expected evidence:
  - read brokered tool request for check-network-telemetry or get-active-incidents
  - write brokered tool request for dispatch-work-order or escalate-incident
  - matching ApprovalRequested and ApprovalApproved events precede write execution
  - an idempotency key is present in the write execution ledger event
  - terminal TaskSucceeded/AgentRuntimeCompleted/TurnCompleted-style event exists

This verifier does not approve tasks and never reads Foundry credentials.
USAGE
}

task="fibey-foundry-responses-quincy-north-alert"
namespace="default"
json_file=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --task)
      [[ $# -ge 2 ]] || { echo "--task requires a value" >&2; exit 2; }
      task="$2"
      shift 2
      ;;
    --namespace)
      [[ $# -ge 2 ]] || { echo "--namespace requires a value" >&2; exit 2; }
      namespace="$2"
      shift 2
      ;;
    --json)
      [[ $# -ge 2 ]] || { echo "--json requires a path" >&2; exit 2; }
      json_file="$2"
      shift 2
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

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "error: $1 is required" >&2; exit 2; }
}

require_cmd python3

json_tmp=""
cleanup() {
  [[ -z "$json_tmp" ]] || rm -f "$json_tmp"
}
trap cleanup EXIT

if [[ -z "$json_file" ]]; then
  require_cmd orka
  json_tmp="$(mktemp)"
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  event_fetcher="${script_dir}/../harness/foundry-responses/fetch_task_events.py"
  python3 "$event_fetcher" --task "$task" --namespace "$namespace" --output "$json_tmp"
  json_file="$json_tmp"
fi

python3 - "$json_file" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
try:
    payload = json.loads(path.read_text())
except Exception as exc:  # noqa: BLE001 - user-facing validation script
    raise SystemExit(f"error: read event JSON: {exc}")

if isinstance(payload, dict):
    if isinstance(payload.get("events"), list):
        events = payload["events"]
    elif isinstance(payload.get("items"), list):
        events = payload["items"]
    else:
        events = [payload]
elif isinstance(payload, list):
    events = payload
else:
    raise SystemExit("error: event JSON must be an object or list")

if isinstance(payload, dict) and payload.get("latestSeq") is not None:
    try:
        after_seq = int(payload.get("afterSeq", 0))
        latest_seq = int(payload["latestSeq"])
        sequences = [int(event.get("seq", 0)) for event in events if isinstance(event, dict)]
    except (TypeError, ValueError) as exc:
        raise SystemExit(f"error: invalid event sequence metadata: {exc}") from exc
    if after_seq != 0:
        raise SystemExit(f"error: event JSON is incomplete: afterSeq must be 0, got {after_seq}")
    complete_sequence = len(sequences) == latest_seq and all(
        seq == expected for expected, seq in enumerate(sequences, start=1)
    )
    if not complete_sequence:
        captured_seq = max(sequences, default=0)
        raise SystemExit(
            "error: event JSON is incomplete: "
            f"captured sequences do not cover 1 through latestSeq {latest_seq} "
            f"(highest captured sequence {captured_seq})"
        )

READ_TOOLS = {"check-network-telemetry", "get-active-incidents"}
WRITE_TOOLS = {"dispatch-work-order", "escalate-incident"}
TERMINAL_TYPES = {
    "TaskSucceeded",
    "AgentRuntimeCompleted",
    "TurnCompleted",
    "TaskCompleted",
}
TASK_TERMINAL_TYPES = {"TaskSucceeded", "TaskFailed", "TaskCancelled"}
def field(event, name):
    if not isinstance(event, dict):
        return None
    if name in event:
        return event[name]
    content = event.get("content")
    if isinstance(content, dict) and name in content:
        return content[name]
    if isinstance(content, str):
        try:
            decoded = json.loads(content)
        except Exception:  # noqa: BLE001
            decoded = None
        if isinstance(decoded, dict) and name in decoded:
            return decoded[name]
    return None


def content_field(event, name):
    if not isinstance(event, dict):
        return None
    content = event.get("content")
    if isinstance(content, dict):
        return content.get(name)
    if isinstance(content, str):
        try:
            decoded = json.loads(content)
        except Exception:  # noqa: BLE001
            return None
        if isinstance(decoded, dict):
            return decoded.get(name)
    return None


def event_type(event):
    if not isinstance(event, dict):
        return ""
    return str(event.get("type") or event.get("eventType") or "")


def tool_name(event):
    for key in ("toolName", "tool", "name"):
        value = field(event, key)
        if isinstance(value, str) and value:
            return value
    return ""


def seq(event):
    if not isinstance(event, dict):
        return None
    for name in ("seq", "sequence", "_verifyOrder", "id"):
        value = event.get(name)
        if value is None:
            continue
        try:
            return int(value)
        except Exception:  # noqa: BLE001
            continue
    return None


def idempotency_value(value):
    if isinstance(value, dict):
        for key, nested in value.items():
            if key in {"idempotencyKey", "Idempotency-Key"}:
                if isinstance(nested, str) and nested.strip():
                    return nested.strip()
            found = idempotency_value(nested)
            if found:
                return found
    elif isinstance(value, list):
        for item in value:
            found = idempotency_value(item)
            if found:
                return found
    elif isinstance(value, str):
        try:
            decoded = json.loads(value)
        except Exception:  # noqa: BLE001
            return ""
        return idempotency_value(decoded)
    return ""


def approval_id(event):
    for key in ("approvalID", "approvalId"):
        value = field(event, key)
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def tool_call_id(event):
    for key in ("toolCallID", "toolCallId"):
        value = field(event, key)
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def is_write_execution_start(event):
    if event_type(event) != "ToolCallStarted":
        return False
    if is_harness_tool_request(event):
        return False
    return field(event, "executionState") == "started" and field(event, "brokeredClass") == "write"


def is_harness_tool_request(event):
    if event_type(event) == "ToolCallRequested":
        return True
    harness_identity = field(event, "harness")
    return (
        event_type(event) == "ToolCallStarted"
        and isinstance(harness_identity, dict)
        and harness_identity.get("frameType") == "ToolCallRequested"
    )


ordered_events = []
for index, event in enumerate(events, start=1):
    if isinstance(event, dict) and seq(event) is None:
        copied = dict(event)
        copied["_verifyOrder"] = index
        ordered_events.append(copied)
    else:
        ordered_events.append(event)

events = ordered_events
read_events = [e for e in events if tool_name(e) in READ_TOOLS and is_harness_tool_request(e)]
write_events = [e for e in events if tool_name(e) in WRITE_TOOLS]
write_request_events = [e for e in write_events if is_harness_tool_request(e)]
approval_request_events = [e for e in events if event_type(e) == "ApprovalRequested"]
approval_approved_events = [e for e in events if event_type(e) == "ApprovalApproved"]
approval_declined_events = [e for e in events if event_type(e) == "ApprovalDeclined"]
write_exec_events = [e for e in write_events if is_write_execution_start(e)]
write_start_events = write_exec_events
terminal_events = [e for e in events if event_type(e) in TERMINAL_TYPES]
task_terminal_events = [e for e in events if event_type(e) in TASK_TERMINAL_TYPES]
idempotency_events = [e for e in write_exec_events if idempotency_value(e)]

failures = []
if not read_events:
    failures.append("missing read brokered tool event for check-network-telemetry/get-active-incidents")
if not write_request_events:
    failures.append("missing write brokered tool event for dispatch-work-order/escalate-incident")
if not approval_request_events:
    failures.append("missing ApprovalRequested event")
if not approval_approved_events:
    failures.append("missing ApprovalApproved event")
if not write_exec_events:
    failures.append("missing write ToolCallStarted event after approval")
if write_exec_events:
    for event in write_exec_events:
        write_tool = tool_name(event)
        write_order = seq(event)
        if not idempotency_value(event):
            failures.append(f"write execution for {write_tool} is missing idempotency key evidence")
        write_tool_call_id = tool_call_id(event)
        if not write_tool_call_id:
            failures.append(f"write execution for {write_tool} is missing toolCallID")
            continue
        matching_write_requests = [
            request for request in write_request_events
            if tool_name(request) == write_tool
            and tool_call_id(request) == write_tool_call_id
            and seq(request) < write_order
        ]
        if not matching_write_requests:
            failures.append(f"write execution for {write_tool} has no matching preceding mapped request")
            continue
        write_approval_id = approval_id(event)
        if not write_approval_id:
            failures.append(f"write execution for {write_tool} is missing approvalID")
            continue
        matching_approval_requests = [
            approval for approval in approval_request_events
            if approval_id(approval) == write_approval_id
            and tool_name(approval) == write_tool
            and content_field(approval, "toolCallID") == write_tool_call_id
            and seq(approval) < write_order
            and any(seq(request) < seq(approval) for request in matching_write_requests)
        ]
        if not matching_approval_requests:
            failures.append(f"write execution for {write_tool} has no matching preceding ApprovalRequested")
        matching_approved = [
            approval for approval in approval_approved_events
            if approval_id(approval) == write_approval_id
            and seq(approval) < write_order
            and any(seq(request) < seq(approval) for request in matching_approval_requests)
        ]
        if not matching_approved:
            failures.append(f"write execution for {write_tool} has no matching preceding ApprovalApproved")
        matching_declined = [
            approval for approval in approval_declined_events
            if approval_id(approval) == write_approval_id and seq(approval) < write_order
        ]
        if matching_declined:
            failures.append(f"write execution for {write_tool} follows ApprovalDeclined")
if not idempotency_events:
    failures.append("missing write ToolCallStarted idempotency key evidence")

missing_idempotency_tools = sorted(
    tool for tool in {tool_name(event) for event in write_exec_events}
    if tool not in {tool_name(event) for event in idempotency_events}
)
if missing_idempotency_tools:
    failures.append(
        "missing write execution idempotency key evidence for: " + ", ".join(missing_idempotency_tools)
    )

starts_by_tool = {}
for event in write_start_events:
    starts_by_tool.setdefault(tool_name(event), 0)
    starts_by_tool[tool_name(event)] += 1
for write_tool, count in starts_by_tool.items():
    if count > 1:
        failures.append(f"duplicate write execution starts for {write_tool}")

idempotency_by_tool = {}
for event in idempotency_events:
    idempotency_by_tool.setdefault(tool_name(event), set()).add(idempotency_value(event))
for write_tool, keys in idempotency_by_tool.items():
    if len(keys) > 1:
        failures.append(f"multiple write idempotency keys for {write_tool}")
if not terminal_events:
    failures.append("missing terminal completion event")
elif write_exec_events:
    latest_write_seq = max(seq(event) for event in write_exec_events)
    earliest_terminal_seq = min(seq(event) for event in terminal_events)
    if earliest_terminal_seq <= latest_write_seq:
        failures.append("terminal completion event does not follow all write executions")
if not task_terminal_events:
    failures.append("missing final TaskSucceeded lifecycle event")
else:
    final_task_terminal = max(task_terminal_events, key=seq)
    final_task_type = event_type(final_task_terminal)
    if final_task_type != "TaskSucceeded":
        failures.append(f"final Task lifecycle outcome is {final_task_type}, want TaskSucceeded")
    final_event = max(events, key=seq) if events else None
    final_event_type = event_type(final_event) if final_event else ""
    if final_event_type != "TaskSucceeded":
        failures.append(f"final execution event is {final_event_type}, want TaskSucceeded")

if failures:
    print("Fibey Foundry Responses verification failed:", file=sys.stderr)
    for failure in failures:
        print(f"- {failure}", file=sys.stderr)
    raise SystemExit(1)

print("Fibey Foundry Responses verification passed:")
print(f"- read events: {len(read_events)}")
print(f"- write requests: {len(write_request_events)}")
print(f"- approval requests: {len(approval_request_events)}")
print(f"- approval decisions: {len(approval_approved_events)}")
print(f"- idempotency evidence events: {len(idempotency_events)}")
print(f"- terminal events: {len(terminal_events)}")
PY
