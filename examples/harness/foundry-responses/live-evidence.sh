#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: examples/harness/foundry-responses/live-evidence.sh [--namespace NAME] [--runtime NAME] [--task NAME] [--out DIR] [--logs-since DURATION]

Capture a credentials-safe evidence bundle for the live Foundry hosted
Responses/Fibey gate after the task has run. The bundle stores Kubernetes
AgentRuntime metadata, the complete paginated Orka task event stream, approvals,
verifier output, and a summary-only adapter log scan. It intentionally does not
store raw adapter logs.

Defaults:
  --namespace  default
  --runtime    fibey-agentkit-foundry-responses
  --task       fibey-foundry-responses-quincy-north-alert
  --out        ./foundry-responses-live-evidence-<timestamp>
  --logs-since empty, meaning scan all available deployment logs. Set a kubectl
               duration such as 30m only when the run start time is known.

Required commands: kubectl, orka, python3.
USAGE
}

namespace="default"
runtime="fibey-agentkit-foundry-responses"
task="fibey-foundry-responses-quincy-north-alert"
out_dir=""
logs_since=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace)
      [[ $# -ge 2 ]] || { echo "--namespace requires a value" >&2; exit 2; }
      namespace="$2"
      shift 2
      ;;
    --runtime)
      [[ $# -ge 2 ]] || { echo "--runtime requires a value" >&2; exit 2; }
      runtime="$2"
      shift 2
      ;;
    --task)
      [[ $# -ge 2 ]] || { echo "--task requires a value" >&2; exit 2; }
      task="$2"
      shift 2
      ;;
    --out)
      [[ $# -ge 2 ]] || { echo "--out requires a value" >&2; exit 2; }
      out_dir="$2"
      shift 2
      ;;
    --logs-since)
      [[ $# -ge 2 ]] || { echo "--logs-since requires a value" >&2; exit 2; }
      logs_since="$2"
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

require_cmd kubectl
require_cmd orka
require_cmd python3

if [[ -z "$out_dir" ]]; then
  out_dir="foundry-responses-live-evidence-$(date -u +%Y%m%dT%H%M%SZ)"
fi

umask 077
mkdir -p "$out_dir"
runtime_tmp=""
events_tmp=""
approvals_tmp=""
pods_tmp=""
cleanup() {
  [[ -z "${runtime_tmp:-}" ]] || rm -f "$runtime_tmp"
  [[ -z "${events_tmp:-}" ]] || rm -f "$events_tmp"
  [[ -z "${approvals_tmp:-}" ]] || rm -f "$approvals_tmp"
  [[ -z "${pods_tmp:-}" ]] || rm -f "$pods_tmp"
}
trap cleanup EXIT

report="$out_dir/README.md"
agentruntime_json="$out_dir/agentruntime.json"
events_json="$out_dir/task-events.json"
approvals_json="$out_dir/task-approvals.json"
verifier_out="$out_dir/fibey-verifier.txt"
log_scan="$out_dir/adapter-log-scan.txt"
artifact_forbidden_pattern="(api[-_]?key|authorization|bearer|secret|password|credential|txn-token|ORKA_FOUNDRY_RESPONSES_|https?://[^[:space:]\"<>]*(fibey-telemetry|fibey-incidents|fibey-dispatch|support-tool))"
log_forbidden_pattern="(api[-_]?key|authorization|bearer|secret|password|credential|txn-token|ORKA_FOUNDRY_RESPONSES_|https?://[^[:space:]\"<>]*(fibey-telemetry|fibey-incidents|fibey-dispatch|support-tool))"

{
  echo "# Foundry Responses live evidence"
  echo
  echo "- Captured at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "- Namespace: ${namespace}"
  echo "- AgentRuntime: ${runtime}"
  echo "- Task: ${task}"
  echo
  echo "Raw adapter logs are not stored by this script."
} >"$report"

scan_saved_artifact() {
  local file="$1"
  local label="$2"
  if [[ ! -s "$file" ]]; then
    return
  fi
  if grep -Eiq "$artifact_forbidden_pattern" "$file"; then
    rm -f "$file"
    echo "error: forbidden credential/tool-url pattern detected in ${label}; removed ${file}" >&2
    exit 1
  fi
}

runtime_tmp="$(mktemp)"
kubectl -n "$namespace" get agentruntime "$runtime" -o json >"$runtime_tmp"
python3 - "$runtime_tmp" "$agentruntime_json" <<'PY'
import json
import sys
from pathlib import Path

runtime = json.loads(Path(sys.argv[1]).read_text())
status = runtime.get("status") or {}
metadata = runtime.get("metadata") or {}
metadata_generation = metadata.get("generation")
observed_generation = status.get("observedGeneration")
if metadata_generation is not None and observed_generation != metadata_generation:
    raise SystemExit("AgentRuntime status.observedGeneration does not match metadata.generation")
conditions = status.get("conditions") or []
ready = [c for c in conditions if c.get("type") == "Ready"]
if not ready or str(ready[-1].get("status", "")).lower() != "true":
    raise SystemExit("AgentRuntime Ready=True was not observed")
condition_generation = ready[-1].get("observedGeneration")
if condition_generation is not None and metadata_generation is not None and condition_generation != metadata_generation:
    raise SystemExit("AgentRuntime Ready condition observedGeneration does not match metadata.generation")
observed = status.get("observedCapabilities") or status.get("observedCapabilitiesRaw") or {}
if isinstance(observed, dict):
    modes = observed.get("toolExecutionModes") or []
    classes = observed.get("brokeredToolClasses") or []
    continuation = observed.get("supportsContinuation")
else:
    modes, classes, continuation = [], [], None
if "brokered" not in modes:
    raise SystemExit("AgentRuntime observed capabilities do not include brokered mode")
if not {"read", "write"}.issubset(set(classes)):
    raise SystemExit("AgentRuntime observed capabilities do not include both read and write brokered classes")
if continuation is not True:
    raise SystemExit("AgentRuntime observed capabilities do not include supportsContinuation=true")
summary = {
    "kind": runtime.get("kind", "AgentRuntime"),
    "metadata": {
        "name": (runtime.get("metadata") or {}).get("name"),
        "namespace": (runtime.get("metadata") or {}).get("namespace"),
        "generation": (runtime.get("metadata") or {}).get("generation"),
    },
    "status": {
        "ready": ready[-1],
        "observedCapabilities": observed,
        "observedGeneration": status.get("observedGeneration"),
    },
}
Path(sys.argv[2]).write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n")
PY
rm -f "$runtime_tmp"
scan_saved_artifact "$agentruntime_json" "AgentRuntime summary"

events_tmp="$(mktemp)"
approvals_tmp="$(mktemp)"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
python3 "${script_dir}/fetch_task_events.py" \
  --task "$task" \
  --namespace "$namespace" \
  --output "$events_tmp"
orka task approvals "$task" --namespace "$namespace" --output json >"$approvals_tmp"
python3 - "$events_tmp" "$events_json" <<'PY'
import json
import sys
from pathlib import Path

payload = json.loads(Path(sys.argv[1]).read_text())
if not isinstance(payload, dict) or not isinstance(payload.get("events"), list):
    raise SystemExit("task event capture must be a paginated event object")
events = payload["events"]
latest_seq = int(payload.get("latestSeq", 0))
after_seq = int(payload.get("afterSeq", 0))
sequence_values = [
    int(event.get("seq", 0))
    for event in events
    if isinstance(event, dict) and event.get("seq") is not None
]
if after_seq != 0:
    raise SystemExit(f"task event capture is incomplete: afterSeq must be 0, got {after_seq}")
complete_sequence = len(sequence_values) == latest_seq and all(
    seq == expected for expected, seq in enumerate(sequence_values, start=1)
)
if not complete_sequence:
    raise SystemExit(
        "task event capture is incomplete: "
        f"sequences do not cover 1 through latestSeq {latest_seq}"
    )

safe_top_level = {
    "seq",
    "type",
    "eventType",
    "severity",
    "toolName",
    "tool",
    "name",
    "toolCallID",
    "toolCallId",
    "approvalID",
    "approvalId",
    "targetTool",
    "brokeredClass",
    "executionState",
    "idempotencyKey",
    "executionIdempotencyKey",
    "Idempotency-Key",
    "createdAt",
}
safe_scalar_content_keys = {
    "approvalID",
    "approvalId",
    "targetTool",
    "toolCallID",
    "toolCallId",
    "toolName",
    "tool",
    "name",
    "brokeredClass",
    "executionState",
    "decision",
}


def scalar(value):
    return value if isinstance(value, (str, int, float, bool)) and not isinstance(value, type(None)) else None


def error_marker_is_set(value):
    return value not in (None, "", False, 0, {}, [])


def has_direct_error(value):
    if not isinstance(value, dict):
        return False
    return (
        error_marker_is_set(value.get("error"))
        or error_marker_is_set(value.get("errorCode"))
        or error_marker_is_set(value.get("toolError"))
    )


def safe_content(value):
    if not isinstance(value, dict):
        return {}
    safe = {}
    for key in safe_scalar_content_keys:
        found = scalar(value.get(key))
        if found not in (None, ""):
            safe[key] = found
    for key in ("executionIdempotencyKey", "Execution-Idempotency-Key"):
        idempotency = scalar(value.get(key))
        if isinstance(idempotency, str) and idempotency.strip():
            safe["executionIdempotencyKey"] = idempotency.strip()
            break
    for key in ("idempotencyKey", "Idempotency-Key"):
        idempotency = scalar(value.get(key))
        if isinstance(idempotency, str) and idempotency.strip():
            safe["idempotencyKey"] = idempotency.strip()
            break
    harness = value.get("harness")
    if isinstance(harness, dict):
        frame_type = scalar(harness.get("frameType"))
        if isinstance(frame_type, str) and frame_type:
            safe["harness"] = {"frameType": frame_type}
    return safe


safe_events = []
for event in events:
    if not isinstance(event, dict):
        raise SystemExit("task event capture contains a non-object event")
    safe_event = {}
    for key in safe_top_level:
        if key in event:
            value = scalar(event[key])
            if value not in (None, ""):
                safe_event[key] = value
    content = event.get("content")
    if isinstance(content, str):
        try:
            content = json.loads(content)
        except Exception:  # noqa: BLE001 - unsafe content is omitted, not stored
            content = {}
    redacted_content = safe_content(content)
    if redacted_content:
        safe_event["content"] = redacted_content
    safe_event["hasError"] = has_direct_error(event) or has_direct_error(content)
    safe_events.append(safe_event)

safe_payload = {
    "namespace": scalar(payload.get("namespace")),
    "streamType": scalar(payload.get("streamType")),
    "streamID": scalar(payload.get("streamID")),
    "afterSeq": 0,
    "latestSeq": latest_seq,
    "events": safe_events,
}
Path(sys.argv[2]).write_text(json.dumps(safe_payload, indent=2, sort_keys=True) + "\n")
PY
scan_saved_artifact "$events_json" "redacted task events"
python3 - "$approvals_tmp" "$approvals_json" <<'PY'
import json
import sys
from pathlib import Path

payload = json.loads(Path(sys.argv[1]).read_text())
if isinstance(payload, dict):
    items = payload.get("approvals") or payload.get("items") or payload.get("events") or []
    if not isinstance(items, list):
        items = [payload]
elif isinstance(payload, list):
    items = payload
else:
    items = []
summary = []
for index, item in enumerate(items, start=1):
    item = item if isinstance(item, dict) else {}
    summary.append({
        "index": index,
        "id": item.get("id"),
        "status": item.get("status"),
        "targetTool": item.get("targetTool"),
        "toolCallID": item.get("toolCallID"),
        "decisionTime": item.get("decisionTime"),
    })
Path(sys.argv[2]).write_text(json.dumps({"approvalCount": len(items), "approvals": summary}, indent=2, sort_keys=True) + "\n")
PY
scan_saved_artifact "$approvals_json" "task approvals summary"

fibey_verifier="${script_dir}/../fibey-custom-agent-demo/verify-foundry-responses.sh"
if [[ ! -x "$fibey_verifier" ]]; then
  fibey_verifier="${script_dir}/../../fibey-custom-agent-demo/verify-foundry-responses.sh"
fi
"$fibey_verifier" --json "$events_tmp" >/dev/null
"$fibey_verifier" --json "$events_json" >"$verifier_out"
scan_saved_artifact "$verifier_out" "Fibey verifier output"

pods_tmp="$(mktemp)"
kubectl -n "$namespace" get pods -l "app.kubernetes.io/name=${runtime}" -o json >"$pods_tmp"
python3 - "$pods_tmp" <<'PY'
import json
import sys
from pathlib import Path

pods = json.loads(Path(sys.argv[1]).read_text()).get("items") or []
if not pods:
    raise SystemExit("no adapter pods found for log evidence")
restarted = []
for pod in pods:
    pod_name = (pod.get("metadata") or {}).get("name", "<unknown>")
    statuses = (pod.get("status") or {}).get("containerStatuses") or []
    for status in statuses:
        if int(status.get("restartCount") or 0) > 0:
            restarted.append(f"{pod_name}/{status.get('name', '<container>')}")
if restarted:
    raise SystemExit("adapter pod restarts observed; previous logs must be inspected before evidence can pass: " + ", ".join(restarted))
PY
rm -f "$pods_tmp"

log_err="$out_dir/adapter-log-error.txt"
pods="$(kubectl -n "$namespace" get pods \
  -l "app.kubernetes.io/name=${runtime}" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>"$log_err")"
if [[ -z "$pods" ]]; then
  {
    echo "adapter log scan: FAILED"
    echo "No adapter pods were found for app.kubernetes.io/name=${runtime}."
    echo "kubectl error: $(tr '\n' ' ' <"$log_err")"
  } >"$log_scan"
  rm -f "$log_err"
  cat "$log_scan" >&2
  exit 1
fi
log_text=""
log_pods=0
while IFS= read -r pod; do
  [[ -n "$pod" ]] || continue
  log_pods=$((log_pods + 1))
  log_args=(logs "pod/${pod}" --all-containers)
  if [[ -n "$logs_since" ]]; then
    log_args+=(--since "$logs_since")
  fi
  if ! pod_logs="$(kubectl -n "$namespace" "${log_args[@]}" 2>>"$log_err")"; then
    {
      echo "adapter log scan: FAILED"
      echo "Could not retrieve adapter logs from pod/${pod}. Raw logs were not stored."
      echo "kubectl error: $(tr '\n' ' ' <"$log_err")"
    } >"$log_scan"
    rm -f "$log_err"
    cat "$log_scan" >&2
    exit 1
  fi
  log_text+=$'\n'"${pod_logs}"
done <<<"$pods"
rm -f "$log_err"
if [[ -z "${log_text//[[:space:]]/}" ]]; then
  {
    echo "adapter log scan: FAILED"
    echo "No adapter logs were returned from pods for deployment/${runtime}; evidence is indeterminate."
  } >"$log_scan"
  cat "$log_scan" >&2
  exit 1
fi
if grep -Eiq "$log_forbidden_pattern" <<<"$log_text"; then
  {
    echo "adapter log scan: FAILED"
    echo "A forbidden credential/tool-url pattern was detected in adapter logs. Raw logs were not stored."
  } >"$log_scan"
  cat "$log_scan" >&2
  exit 1
fi
{
  echo "adapter log scan: passed"
  echo "scanned pods: ${log_pods}"
  echo "scanned tail lines: $(wc -l <<<"$log_text" | tr -d ' ')"
} >"$log_scan"

{
  echo
  echo "## Evidence files"
  echo
  echo "- AgentRuntime JSON: $(basename "$agentruntime_json")"
  echo "- Task events JSON: $(basename "$events_json")"
  echo "- Task approvals JSON: $(basename "$approvals_json")"
  echo "- Fibey verifier output: $(basename "$verifier_out")"
  echo "- Adapter log scan summary: $(basename "$log_scan")"
  echo
  echo "## Verifier output"
  echo
  sed 's/^/> /' "$verifier_out"
  echo
  echo "## Adapter log scan"
  echo
  sed 's/^/> /' "$log_scan"
} >>"$report"

echo "Foundry Responses live evidence captured in: $out_dir" >&2
