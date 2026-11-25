#!/usr/bin/env bash
# Demo 80: Orka oversight, "hit play".
#
#   hack/demos/80-oversight.sh up      # seed the dashboard: charter, producers, history
#   hack/demos/80-oversight.sh play    # fan out the drill agents, stream their decisions
#   hack/demos/80-oversight.sh reset   # back to the seeded baseline (< 1 minute)
#   hack/demos/80-oversight.sh status  # posture and counts
#
# Oversight is read-only in this phase: decisions are recorded and shown,
# nothing is held. Every scenario-specific file comes from a scenario pack
# (OVERSIGHT_PACK), so this script names no sensor, model or registry:
#
#   $OVERSIGHT_PACK/declarations.yaml   ConfigMap orka-oversight-producers
#                                       (charters.json, producers.json, ...)
#   $OVERSIGHT_PACK/history/*.jsonl     seeded history, shifted to end now
#   $OVERSIGHT_PACK/take/*.jsonl        a recorded live take, replayed with
#                                       its timing (the default play mode)
#   $OVERSIGHT_PACK/tasks/wave-*/*.yaml scripted drill Tasks, applied in waves
#   $OVERSIGHT_PACK/live-bridge         optional: streams live decisions for
#                                       OVERSIGHT_MODE=live (see below)
#
# Environment:
#   KUBECONFIG         a scoped kubeconfig for the demo cluster (required)
#   OVERSIGHT_PACK     the scenario pack directory (required)
#   DEMO_NAMESPACE     the controller's watched namespace (default orka-system)
#   ORKA_API_BASE      the API, port-forwarded (default http://127.0.0.1:18080)
#   ORKA_CLI           the orka CLI (default: bin/orka, built if missing)
#   OVERSIGHT_MODE     replay (default) or live
#   OVERSIGHT_SPEED    replay speed-up of the recorded take (default 1)
#   WAVE_DELAY         seconds between Task waves (default 6)
#   OVERSIGHT_MAX_SCORER_CALLS  live mode soft ceiling passed to the bridge (default 500)
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
: "${KUBECONFIG:?set KUBECONFIG to a scoped kubeconfig for the demo cluster}"
: "${OVERSIGHT_PACK:?set OVERSIGHT_PACK to a scenario pack directory}"
NS="${DEMO_NAMESPACE:-orka-system}"
API="${ORKA_API_BASE:-http://127.0.0.1:18080}"
MODE="${OVERSIGHT_MODE:-replay}"
ORKA="${ORKA_CLI:-${repo_root}/bin/orka}"
DEMO_LABEL="orka.ai/demo=oversight"
export KUBECONFIG

log() { printf '\033[1m==> %s\033[0m\n' "$*"; }
k() { kubectl -n "${NS}" "$@"; }

ensure_cli() {
  if [[ ! -x "${ORKA}" ]]; then
    (cd "${repo_root}" && go build -o "${ORKA}" ./cmd/cli)
  fi
}

ensure_api() {
  if ! curl -fsS "${API}/healthz" >/dev/null 2>&1; then
    local port="${API##*:}"
    log "Port-forwarding the Orka API to ${API}"
    nohup kubectl -n orka-system port-forward svc/orka-api "${port}:8080" >/dev/null 2>&1 &
    for _ in $(seq 30); do curl -fsS "${API}/healthz" >/dev/null 2>&1 && return 0; sleep 1; done
    echo "the Orka API did not answer at ${API}" >&2
    return 1
  fi
}

ensure_identities() {
  # An ingest identity that can only create decisions, and a viewer that can
  # read them and label outcomes. Worker ServiceAccounts are always refused.
  kubectl apply -n "${NS}" -f - >/dev/null <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata: {name: oversight-ingest, labels: {orka.ai/demo: oversight}}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: oversight-viewer, labels: {orka.ai/demo: oversight}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: oversight-ingest, labels: {orka.ai/demo: oversight}}
rules:
  - {apiGroups: [core.orka.ai], resources: [oversightdecisions], verbs: [create]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: oversight-ingest, labels: {orka.ai/demo: oversight}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: oversight-ingest}
subjects: [{kind: ServiceAccount, name: oversight-ingest}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: oversight-viewer, labels: {orka.ai/demo: oversight}}
rules:
  - {apiGroups: [core.orka.ai], resources: [oversightdecisions], verbs: [get, list]}
  - {apiGroups: [core.orka.ai], resources: [oversightdecisions/feedback], verbs: [create]}
  - {apiGroups: [core.orka.ai], resources: [chats/config], verbs: [get]}
  - {apiGroups: [core.orka.ai], resources: [tasks, agents, sessions, runtimepools, agentruntimes, providers, tools], verbs: [get, list, watch]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: oversight-viewer, labels: {orka.ai/demo: oversight}}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: oversight-viewer}
subjects: [{kind: ServiceAccount, name: oversight-viewer}]
EOF
}

ingest_token() { k create token oversight-ingest --duration=2h; }
viewer_token() { k create token oversight-viewer --duration=8h; }

replay() {
  "${ORKA}" oversight replay --server "${API}" --token "$(ingest_token)" --into "${NS}" "$@"
}

decision_count() {
  "${ORKA}" oversight decisions --server "${API}" --token "$(viewer_token)" --namespace "${NS}" --limit 1 -o json |
    python3 -I -c 'import json, sys; print(len(json.load(sys.stdin)["items"]))'
}

files_in() {
  local dir="$1"
  [[ -d "${dir}" ]] || return 0
  find "${dir}" -maxdepth 1 -name '*.jsonl' | sort
}

cmd_up() {
  ensure_cli
  ensure_api
  log "Checking that oversight is enabled"
  if ! k get deploy orka-controller-manager -o jsonpath='{.spec.template.spec.containers[0].env}' | grep -q ORKA_OVERSIGHT_ENABLED; then
    k set env deployment/orka-controller-manager -c manager ORKA_DASHBOARD_ENABLED=true ORKA_OVERSIGHT_ENABLED=true
    k rollout status deployment/orka-controller-manager --timeout=180s
    pkill -f "port-forward svc/orka-api ${API##*:}:8080" 2>/dev/null || true
    ensure_api
  fi
  ensure_identities
  log "Declaring the charter and producers"
  k apply -f "${OVERSIGHT_PACK}/declarations.yaml"
  if [[ "$(decision_count)" != "0" ]]; then
    log "History is already seeded"
  else
    local args=()
    while IFS= read -r f; do args+=(--file "$f"); done < <(files_in "${OVERSIGHT_PACK}/history")
    if ((${#args[@]})); then
      log "Seeding history"
      replay "${args[@]}" --shift-to-now --batch-size 50
    fi
  fi
  cmd_status
}

apply_waves() {
  local first=1
  for wave in "${OVERSIGHT_PACK}"/tasks/wave-*/; do
    [[ -d "${wave}" ]] || continue
    ((first)) || sleep "${WAVE_DELAY:-6}"
    first=0
    log "Launching $(basename "${wave}")"
    for manifest in "${wave}"*.yaml; do
      kubectl apply -n "${NS}" -f "${manifest}" >/dev/null
    done
  done
}

cmd_play() {
  ensure_cli
  ensure_api
  local take_id
  take_id="-take$(date +%H%M%S)"
  case "${MODE}" in
    replay)
      local args=()
      while IFS= read -r f; do args+=(--file "$f"); done < <(files_in "${OVERSIGHT_PACK}/take")
      ((${#args[@]})) || { echo "no recorded take in ${OVERSIGHT_PACK}/take" >&2; return 1; }
      apply_waves &
      local waves=$!
      log "Streaming the recorded take (speed ${OVERSIGHT_SPEED:-1}x)"
      replay "${args[@]}" --preserve-timing --speed "${OVERSIGHT_SPEED:-1}" --id-suffix "${take_id}"
      wait "${waves}"
      ;;
    live)
      [[ -x "${OVERSIGHT_PACK}/live-bridge" ]] || { echo "live mode needs ${OVERSIGHT_PACK}/live-bridge" >&2; return 1; }
      log "Live take: scorer calls capped near ${OVERSIGHT_MAX_SCORER_CALLS:-500}; no advisor tier"
      apply_waves
      ORKA_OVERSIGHT_URL="${API}" ORKA_OVERSIGHT_NAMESPACE="${NS}" ORKA_OVERSIGHT_TOKEN="$(ingest_token)" \
        OVERSIGHT_MAX_SCORER_CALLS="${OVERSIGHT_MAX_SCORER_CALLS:-500}" OVERSIGHT_ADVISORS=off \
        OVERSIGHT_TAKE_ID="${take_id}" "${OVERSIGHT_PACK}/live-bridge"
      ;;
    *) echo "OVERSIGHT_MODE is replay or live" >&2; return 1 ;;
  esac
  cmd_status
}

cmd_reset() {
  # Decisions are immutable, so the baseline comes back by giving the
  # controller a fresh store and seeding the history again.
  log "Removing the drill Tasks"
  kubectl delete tasks.core.orka.ai -n "${NS}" -l "${DEMO_LABEL}" --ignore-not-found --wait=false >/dev/null
  log "Replacing the controller's store"
  local pvc tmp
  pvc="$(k get deploy orka-controller-manager -o jsonpath='{.spec.template.spec.volumes[?(@.name=="store")].persistentVolumeClaim.claimName}')"
  tmp="$(mktemp -d)"
  k get pvc "${pvc}" -o json | python3 -I -c '
import json, sys
pvc = json.load(sys.stdin)
meta = pvc["metadata"]
pvc["metadata"] = {k: meta[k] for k in ("name", "namespace", "labels", "annotations") if k in meta}
pvc["metadata"].get("annotations", {}).pop("pv.kubernetes.io/bind-completed", None)
pvc["metadata"].get("annotations", {}).pop("pv.kubernetes.io/bound-by-controller", None)
pvc["metadata"].get("annotations", {}).pop("volume.kubernetes.io/selected-node", None)
pvc["spec"].pop("volumeName", None)
pvc.pop("status", None)
json.dump(pvc, sys.stdout)' > "${tmp}/pvc.json"
  k scale deploy/orka-controller-manager --replicas=0
  k wait --for=delete pod -l control-plane=controller-manager --timeout=90s 2>/dev/null || true
  k delete pvc "${pvc}" --wait=true
  k apply -f "${tmp}/pvc.json"
  k scale deploy/orka-controller-manager --replicas=1
  k rollout status deploy/orka-controller-manager --timeout=180s
  pkill -f "port-forward svc/orka-api ${API##*:}:8080" 2>/dev/null || true
  cmd_up
}

cmd_status() {
  ensure_cli
  ensure_api
  "${ORKA}" oversight posture --server "${API}" --token "$(viewer_token)" --namespace "${NS}"
}

case "${1:-}" in
  up) cmd_up ;;
  play) cmd_play ;;
  reset) cmd_reset ;;
  status) cmd_status ;;
  *) sed -n '2,8p' "$0"; exit 2 ;;
esac
