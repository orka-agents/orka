#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: examples/fibey-custom-agent-demo/verify-foundry-responses.sh [--task NAME] [--namespace NAME] [--json EVENTS.json]

Checks the live Fibey Foundry hosted AgentKit Responses scenario evidence from
Orka task events. By default it calls `orka task events --output json`. Use
--json to verify a previously captured event payload without contacting Orka.

Expected evidence:
  - read brokered tool request for check-network-telemetry or get-active-incidents
  - write brokered tool request for dispatch-work-order or escalate-incident
  - ApprovalRequested is present before write ToolCallStarted
  - an idempotency key is present in write ToolCallStarted content
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
  orka task events "$task" --namespace "$namespace" --output json >"$json_tmp"
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

READ_TOOLS = {"check-network-telemetry", "get-active-incidents"}
WRITE_TOOLS = {"dispatch-work-order", "escalate-incident"}
TERMINAL_TYPES = {
    "TaskSucceeded",
    "AgentRuntimeCompleted",
    "TurnCompleted",
    "TaskCompleted",
}
WRITE_EXEC_TYPES = {"ToolCallStarted"}


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


def contains_idempotency(event):
    return bool(idempotency_value(event))


ordered_events = []
for index, event in enumerate(events, start=1):
    if isinstance(event, dict) and seq(event) is None:
        copied = dict(event)
        copied["_verifyOrder"] = index
        ordered_events.append(copied)
    else:
        ordered_events.append(event)

events = ordered_events
read_events = [e for e in events if tool_name(e) in READ_TOOLS]
write_events = [e for e in events if tool_name(e) in WRITE_TOOLS]
approval_events = [e for e in events if event_type(e) == "ApprovalRequested"]
write_exec_events = [e for e in write_events if event_type(e) in WRITE_EXEC_TYPES]
write_start_events = [e for e in write_events if event_type(e) == "ToolCallStarted"]
terminal_events = [e for e in events if event_type(e) in TERMINAL_TYPES]
idempotency_events = [e for e in write_exec_events if idempotency_value(e)]

failures = []
if not read_events:
    failures.append("missing read brokered tool event for check-network-telemetry/get-active-incidents")
if not write_events:
    failures.append("missing write brokered tool event for dispatch-work-order/escalate-incident")
if not approval_events:
    failures.append("missing ApprovalRequested event")
if not write_exec_events:
    failures.append("missing write ToolCallStarted event after approval")
if write_exec_events and approval_events:
    for event in write_exec_events:
        write_tool = tool_name(event)
        write_order = seq(event)
        matching_approvals = [
            approval for approval in approval_events
            if tool_name(approval) == write_tool and seq(approval) < write_order
        ]
        if not matching_approvals:
            failures.append(f"write execution for {write_tool} has no preceding approval")
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

if failures:
    print("Fibey Foundry Responses verification failed:", file=sys.stderr)
    for failure in failures:
        print(f"- {failure}", file=sys.stderr)
    raise SystemExit(1)

print("Fibey Foundry Responses verification passed:")
print(f"- read events: {len(read_events)}")
print(f"- write events: {len(write_events)}")
print(f"- approvals: {len(approval_events)}")
print(f"- idempotency evidence events: {len(idempotency_events)}")
print(f"- terminal events: {len(terminal_events)}")
PY
