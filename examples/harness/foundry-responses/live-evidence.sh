#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: examples/harness/foundry-responses/live-evidence.sh [--namespace NAME] [--runtime NAME] [--task NAME] [--out DIR] [--logs-since DURATION]

Capture a credentials-safe evidence bundle for the live Foundry hosted
Responses/Fibey gate after the task has run. The bundle stores Kubernetes
AgentRuntime metadata, Orka task events/approvals, verifier output, and a
summary-only adapter log scan. It intentionally does not store raw adapter logs.

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
orka task events "$task" --namespace "$namespace" --output json >"$events_tmp"
orka task approvals "$task" --namespace "$namespace" --output json >"$approvals_tmp"
python3 - "$events_tmp" "$events_json" <<'PY'
import json
import sys
from pathlib import Path

payload = json.loads(Path(sys.argv[1]).read_text())
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
    events = []


def content_dict(event):
    content = event.get("content") if isinstance(event, dict) else None
    if isinstance(content, dict):
        return content
    if isinstance(content, str):
        try:
            decoded = json.loads(content)
        except Exception:  # noqa: BLE001 - best-effort evidence summarizer
            return {}
        return decoded if isinstance(decoded, dict) else {}
    return {}


def event_field(event, *names):
    if not isinstance(event, dict):
        return None
    content = content_dict(event)
    for name in names:
        value = event.get(name)
        if value not in (None, ""):
            return value
        value = content.get(name)
        if value not in (None, ""):
            return value
    return None


def has_idempotency(value):
    if isinstance(value, dict):
        if any(k in value for k in ("idempotencyKey", "Idempotency-Key")):
            return True
        return any(has_idempotency(v) for v in value.values())
    if isinstance(value, list):
        return any(has_idempotency(v) for v in value)
    if isinstance(value, str):
        try:
            decoded = json.loads(value)
        except Exception:  # noqa: BLE001
            return False
        return has_idempotency(decoded)
    return False

summary = []
for index, event in enumerate(events, start=1):
    event = event if isinstance(event, dict) else {}
    summary.append({
        "index": index,
        "type": event.get("type") or event.get("eventType"),
        "toolName": event_field(event, "toolName", "tool", "name"),
        "hasIdempotencyEvidence": has_idempotency(event),
        "hasError": bool(event_field(event, "error", "errorCode")),
    })
Path(sys.argv[2]).write_text(json.dumps({"eventCount": len(events), "events": summary}, indent=2, sort_keys=True) + "\n")
PY
scan_saved_artifact "$events_json" "task events summary"
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
        "type": item.get("type") or item.get("eventType"),
        "status": item.get("status") or item.get("decision"),
        "toolName": item.get("toolName") or item.get("tool"),
    })
Path(sys.argv[2]).write_text(json.dumps({"approvalCount": len(items), "approvals": summary}, indent=2, sort_keys=True) + "\n")
PY
scan_saved_artifact "$approvals_json" "task approvals summary"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fibey_verifier="${script_dir}/../fibey-custom-agent-demo/verify-foundry-responses.sh"
if [[ ! -x "$fibey_verifier" ]]; then
  fibey_verifier="${script_dir}/../../fibey-custom-agent-demo/verify-foundry-responses.sh"
fi
"$fibey_verifier" --json "$events_tmp" >"$verifier_out"
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
