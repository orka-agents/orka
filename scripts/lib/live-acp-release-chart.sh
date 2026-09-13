#!/usr/bin/env bash
# shellcheck disable=SC2016 # acp_report_update arguments are jq programs.
# Source-only acceptance of the packaged candidate in the bootstrap's own Kind
# cluster. The caller verifies candidate provenance before invoking this file.

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  echo "error: source scripts/lib/live-acp-release-chart.sh; do not execute it directly" >&2
  exit 2
fi

live_acp_release_chart_die() {
  printf 'error: %s\n' "$*" >&2
  return 1
}

live_acp_release_chart_kubectl() {
  kubectl --context "${LIVE_ACP_CONTEXT}" "$@"
}

# Values contain references to operator-created Secrets, never key material.
live_acp_release_chart_values() {
  python3 - "$1" "$2" "$3" <<'PY'
import base64
import json
from pathlib import Path
import re
import sys

manifest, ca_file, output = map(Path, sys.argv[1:])
candidate = json.loads(manifest.read_text())
images = candidate["images"]

def image(name):
    ref = images.get(name, "")
    repository = "ghcr.io/orka-agents/orka" + ("" if name == "controller" else "/" + name)
    if not isinstance(ref, str) or not re.fullmatch(re.escape(repository) + r"@sha256:[0-9a-f]{64}", ref):
        raise SystemExit("candidate is missing an immutable release image for " + name)
    repository, digest = ref.split("@", 1)
    return {"repository": repository, "digest": digest, "pullPolicy": "IfNotPresent"}

values = {
    "controller": {
        "mode": "harness-v2", "watchNamespace": "orka-system",
        "image": image("controller"),
        "agentExecutionSnapshot": {"existingSecret": "live-acp-chart-snapshot", "key": "key"},
        "acpArtifact": {"existingSecret": "live-acp-chart-artifact"},
        "acpRuntime": {"namespace": "orka-runtimes"},
    },
    "publisher": {"enabled": True, "image": image("workspace-publisher"),
                  "auth": {"existingSecret": "live-acp-chart-publisher"}},
    "providerProxy": {"enabled": True, "auth": {"existingSecret": "live-acp-chart-provider"}},
    "scmEgressProxy": {"enabled": True, "auth": {"existingSecret": "live-acp-chart-scm"}},
    "workers": {"ai": {"image": image("ai-worker")}, "general": {"image": image("general-worker")}},
    "webhooks": {"tls": {"existingSecret": "live-acp-chart-webhook"},
                 "caBundle": base64.b64encode(ca_file.read_bytes()).decode("ascii")},
    "store": {"persistence": {"enabled": True}},
}
for provider in ("codex", "claude", "copilot", "opencode"):
    role = "acp-" + provider + "-runtime"
    image(role)
    values["controller"]["acpRuntime"][provider + "Image"] = images[role]
# Supply valid v1 render inputs so the opposite-mode check tests the live mode
# identity guard, rather than failing because v1 values were omitted.
values["harnessV1"] = {
    "image": image("agent-harness-wrapper"),
    "auth": {"existingSecret": "live-acp-chart-v1-auth"},
    "tls": {"existingSecret": "live-acp-chart-v1-tls"},
}
output.write_text(json.dumps(values) + "\n")
PY
}

# The packaged chart serves admission through orka-webhook, not the Kustomize
# installation's orka-admission Service.
live_acp_release_chart_tls() (
  set -Eeuo pipefail
  umask 077
  local directory="$1"
  cat >"${directory}/ca.conf" <<'CA'
[req]
prompt = no
distinguished_name = ca_name
x509_extensions = ca_extensions
[ca_name]
CN = Orka release chart test CA
[ca_extensions]
basicConstraints = critical, CA:true
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always
CA
  cat >"${directory}/serving.conf" <<'SERVING'
[req]
prompt = no
distinguished_name = serving_name
req_extensions = serving_extensions
[serving_name]
CN = orka-webhook.orka-system.svc
[serving_extensions]
basicConstraints = critical, CA:false
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:orka-webhook.orka-system.svc,DNS:orka-webhook.orka-system.svc.cluster.local
subjectKeyIdentifier = hash
SERVING
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 7 \
    -config "${directory}/ca.conf" -keyout "${directory}/ca.key" \
    -out "${directory}/ca.crt" >/dev/null 2>&1 || return 1
  openssl req -new -newkey rsa:2048 -nodes -sha256 \
    -config "${directory}/serving.conf" -keyout "${directory}/tls.key" \
    -out "${directory}/tls.csr" >/dev/null 2>&1 || return 1
  openssl x509 -req -sha256 -days 7 -in "${directory}/tls.csr" \
    -CA "${directory}/ca.crt" -CAkey "${directory}/ca.key" -CAcreateserial \
    -extfile "${directory}/serving.conf" -extensions serving_extensions \
    -out "${directory}/tls.crt" >/dev/null 2>&1 || return 1
  openssl verify -CAfile "${directory}/ca.crt" -verify_hostname orka-webhook.orka-system.svc \
    "${directory}/tls.crt" >/dev/null 2>&1
)

live_acp_release_chart_assert_recovery() {
  jq -e -s '
    def present: type == "string" and length > 0;
    .[0] as $before | .[1] as $after
    | ($before.pvcs | length) == 2
      and ($before.pvcs | map(.name) | sort) == ["orka-store", "orka-workspace-publisher"]
      and all($before.pvcs[]; (.uid | present) and (.volumeName | present) and .phase == "Bound")
      and $before.pvcs == $after.pvcs
      and $before.controller.claim == "orka-store" and $after.controller.claim == "orka-store"
      and $before.publisher.claim == "orka-workspace-publisher" and $after.publisher.claim == "orka-workspace-publisher"
      and ($before.controller.podUID | present) and ($after.controller.podUID | present)
      and $before.controller.podUID != $after.controller.podUID
      and ($before.task.uid | present) and $before.task.phase == "Succeeded"
      and $before.task.attempts == 1 and $before.task.resultAvailable == true
      and ($before.task.jobName | present) and ($before.task.jobUID | present)
      and $before.task == $after.task
      and ($before.jobs | length) == 1 and $before.jobs[0].uid == $before.task.jobUID
      and $before.jobs[0].taskUID == $before.task.uid
      and ($before.workerPods | length) == 1 and ($before.workerPods[0].uid | present)
      and $before.workerPods[0].jobUID == $before.task.jobUID
      and $before.workerPods[0].phase == "Succeeded" and $before.workerPods[0].restarts == 0
      and $after.jobs == [] and $after.workerPods == []
  ' "$1" "$2" >/dev/null
}

live_acp_release_chart_assert_mode_rejection() {
  # A transport error or a missing Secret is not an immutable-mode rejection.
  [[ "$4" != "0" ]] || return 1
  grep -Eq 'controller mode identity is missing or incompatible|controller\.mode is immutable' "$3" || return 1
  jq -e -s 'length == 2 and .[0] == .[1]
    and (.[0].release.revision | type == "number" and . >= 1)
    and .[0].release.status == "deployed" and .[0].namespace.mode == "harness-v2"
    and (.[0].objects | length > 0) and (.[0].secrets | length == 6)
    and all(.[0].secrets[]; (.uid | type == "string" and length > 0)
      and (.resourceVersion | type == "string" and length > 0))' "$1" "$2" >/dev/null
}

_live_acp_release_chart_ready_pod() {
  live_acp_release_chart_kubectl -n orka-system get pods \
    -l "app.kubernetes.io/instance=orka,app.kubernetes.io/component=$1" -o json |
    jq -e '.items | map(select(.metadata.deletionTimestamp == null and .status.phase == "Running"
      and any(.status.conditions[]?; .type == "Ready" and .status == "True")))
      | if length == 1 then .[0] else error("expected one ready chart Pod") end'
}

_live_acp_release_chart_observe() {
  local task="$1" output="$2"
  live_acp_release_chart_kubectl -n orka-system get task "${task}" -o json >"${chart_work_dir}/task.json" || return 1
  live_acp_release_chart_kubectl -n orka-system get jobs -l "orka.ai/task=${task}" -o json >"${chart_work_dir}/jobs.json" || return 1
  live_acp_release_chart_kubectl -n orka-system get pods -l "orka.ai/task=${task}" -o json >"${chart_work_dir}/worker-pods.json" || return 1
  live_acp_release_chart_kubectl -n orka-system get pvc orka-store orka-workspace-publisher -o json >"${chart_work_dir}/pvcs.json" || return 1
  _live_acp_release_chart_ready_pod controller >"${chart_work_dir}/controller-pod.json" || return 1
  _live_acp_release_chart_ready_pod workspace-publisher >"${chart_work_dir}/publisher-pod.json" || return 1
  jq -n \
    --slurpfile task "${chart_work_dir}/task.json" --slurpfile jobs "${chart_work_dir}/jobs.json" \
    --slurpfile pods "${chart_work_dir}/worker-pods.json" --slurpfile pvcs "${chart_work_dir}/pvcs.json" \
    --slurpfile controller "${chart_work_dir}/controller-pod.json" --slurpfile publisher "${chart_work_dir}/publisher-pod.json" '
    def durable_pod($container):
      . as $pod | [.spec.containers[] | select(.name == $container) | .volumeMounts[] | select(.mountPath == "/data") | .name] as $mounts
      | {podUID: .metadata.uid, claim: ([.spec.volumes[] | select(.name == $mounts[0]) | .persistentVolumeClaim.claimName][0])};
    {
      task: ($task[0] | {namespace:.metadata.namespace, name:.metadata.name, uid:.metadata.uid,
        phase:.status.phase, attempts:.status.attempts, jobName:.status.jobName, jobUID:.status.jobUID,
        completionTime:.status.completionTime, resultAvailable:.status.resultRef.available}),
      pvcs: ([$pvcs[0].items[] | {name:.metadata.name, uid:.metadata.uid, volumeName:.spec.volumeName, phase:.status.phase}] | sort_by(.name)),
      controller: ($controller[0] | durable_pod("controller")), publisher: ($publisher[0] | durable_pod("publisher")),
      jobs: [$jobs[0].items[] | {name:.metadata.name, uid:.metadata.uid,
        taskUID: ([.metadata.ownerReferences[]? | select(.kind == "Task" and .controller == true) | .uid][0])}],
      workerPods: [$pods[0].items[] | {uid:.metadata.uid, phase:.status.phase,
        jobUID: ([.metadata.ownerReferences[]? | select(.kind == "Job" and .controller == true) | .uid][0]),
        image: ([.spec.containers[] | select(.name == "worker") | .image][0]),
        restarts: ([.status.containerStatuses[]? | select(.name == "worker") | .restartCount][0])}]
    }' >"${output}"
}

_live_acp_release_chart_stop_api() {
  if [[ -n "${chart_api_pid:-}" ]]; then
    kill "${chart_api_pid}" >/dev/null 2>&1 || true
    wait "${chart_api_pid}" >/dev/null 2>&1 || true
    chart_api_pid=""
  fi
}

_live_acp_release_chart_start_api() {
  local deadline=$((SECONDS + 30))
  _live_acp_release_chart_stop_api
  : >"${chart_work_dir}/api-forward.log"
  # Keep the PID attached to kubectl itself so cleanup cannot orphan a child
  # port-forward process behind a background shell function.
  (exec kubectl --context "${LIVE_ACP_CONTEXT}" -n orka-system port-forward --address=127.0.0.1 \
    deployment/orka-controller :8080) >"${chart_work_dir}/api-forward.log" 2>&1 &
  chart_api_pid=$!
  while (( SECONDS < deadline )); do
    kill -0 "${chart_api_pid}" >/dev/null 2>&1 || break
    chart_api_port="$(awk '$1 == "Forwarding" && $3 ~ /^127\.0\.0\.1:[0-9]+$/ {split($3, address, ":"); print address[2]; exit}' "${chart_work_dir}/api-forward.log")"
    if [[ "${chart_api_port}" =~ ^[0-9]+$ ]] && curl --fail --silent --noproxy '*' --max-time 2 \
      "http://127.0.0.1:${chart_api_port}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  live_acp_release_chart_die "packaged controller API port-forward did not become ready"
}

_live_acp_release_chart_result() {
  local task="$1" marker="$2"
  curl --fail --silent --noproxy '*' --max-time 30 \
    --header "@${chart_work_dir}/api-header" \
    "http://127.0.0.1:${chart_api_port}/api/v1/tasks/${task}/result?namespace=orka-system" \
    --output "${chart_work_dir}/result.json" || return 1
  jq -e --arg expected "${marker}" '.result == $expected' "${chart_work_dir}/result.json" >/dev/null
}

_live_acp_release_chart_stable_state() {
  local output="$1"
  helm --kube-context "${LIVE_ACP_CONTEXT}" -n orka-system status orka -o json |
    jq '{revision:.version, status:.info.status}' >"${chart_work_dir}/release-state.json" || return 1
  helm --kube-context "${LIVE_ACP_CONTEXT}" -n orka-system get manifest orka >"${chart_work_dir}/release-manifest.yaml" || return 1
  live_acp_release_chart_kubectl -n orka-system get -f "${chart_work_dir}/release-manifest.yaml" -o json |
    jq -s '[.[] | if .kind == "List" then .items[] else . end | {
      kind, namespace:.metadata.namespace, name:.metadata.name, uid:.metadata.uid, generation:.metadata.generation,
      labels:.metadata.labels, annotations:.metadata.annotations, spec, rules, roleRef, subjects, webhooks
    }] | sort_by(.kind, .namespace, .name)' >"${chart_work_dir}/release-objects.json" || return 1
  live_acp_release_chart_kubectl get namespace orka-system -o json |
    jq '{uid:.metadata.uid, mode:.metadata.labels["orka.ai/controller-mode"]}' >"${chart_work_dir}/namespace-state.json" || return 1
  live_acp_release_chart_kubectl -n orka-system get secret \
    live-acp-chart-snapshot live-acp-chart-webhook live-acp-chart-publisher \
    live-acp-chart-provider live-acp-chart-scm live-acp-chart-artifact -o json |
    jq '[.items[] | {name:.metadata.name, uid:.metadata.uid, resourceVersion:.metadata.resourceVersion}] | sort_by(.name)' \
      >"${chart_work_dir}/secret-identities.json" || return 1
  jq -n --slurpfile release "${chart_work_dir}/release-state.json" \
    --slurpfile objects "${chart_work_dir}/release-objects.json" --rawfile manifest "${chart_work_dir}/release-manifest.yaml" \
    --slurpfile namespace "${chart_work_dir}/namespace-state.json" --slurpfile secrets "${chart_work_dir}/secret-identities.json" \
    '{release:$release[0], objects:$objects[0], manifest:$manifest, namespace:$namespace[0], secrets:$secrets[0]}' >"${output}"
}

_live_acp_release_chart_acceptance() (
  set -Eeuo pipefail
  umask 077
  local manifest="$1" archive="$2" package_sha="$3"
  local chart_work_dir chart_api_pid="" chart_api_port="" key deployment expected_image
  local task="release-chart-acceptance" marker deadline phase job rc=0 attempt existing_namespace
  chart_work_dir="$(mktemp -d "${LIVE_ACP_SECRET_DIR}/release-chart.XXXXXX")" || return 1
  trap '_live_acp_release_chart_stop_api; rm -rf -- "${chart_work_dir}"' EXIT

  acp_report_update '.chart = {install:false, containerTask:false, recovery:false, noReplay:false,
    oppositeModeRejected:false, packageSHA256:$sha, namespace:"orka-system", release:"orka"}' --arg sha "${package_sha}" || return 1
  existing_namespace="$(live_acp_release_chart_kubectl get namespace orka-system --ignore-not-found -o name)" || return 1
  [[ -z "${existing_namespace}" ]] || {
    live_acp_release_chart_die "release chart acceptance requires a fresh orka-system namespace"
    return 1
  }
  live_acp_release_chart_tls "${chart_work_dir}" || return 1
  live_acp_release_chart_values "${manifest}" "${chart_work_dir}/ca.crt" "${chart_work_dir}/values.json" || return 1
  openssl rand 32 >"${chart_work_dir}/snapshot-key" || return 1
  for key in publisher-token publisher-capability provider-token scm-token artifact-capability; do
    openssl rand -hex 32 | tr -d '\n' >"${chart_work_dir}/${key}" || return 1
  done
  jq -n '{apiVersion:"v1",kind:"Namespace",metadata:{name:"orka-system",labels:{"orka.ai/controller-mode":"harness-v2"}}}' |
    live_acp_release_chart_kubectl create -f - >/dev/null || return 1
  live_acp_release_chart_kubectl -n orka-system create secret generic live-acp-chart-snapshot \
    --from-file="key=${chart_work_dir}/snapshot-key" >/dev/null || return 1
  live_acp_release_chart_kubectl -n orka-system create secret generic live-acp-chart-webhook --type=kubernetes.io/tls \
    --from-file="tls.crt=${chart_work_dir}/tls.crt" --from-file="tls.key=${chart_work_dir}/tls.key" \
    --from-file="ca.crt=${chart_work_dir}/ca.crt" >/dev/null || return 1
  live_acp_release_chart_kubectl -n orka-system create secret generic live-acp-chart-publisher \
    --from-file="controller-token=${chart_work_dir}/publisher-token" \
    --from-file="operation-capability-secret=${chart_work_dir}/publisher-capability" >/dev/null || return 1
  live_acp_release_chart_kubectl -n orka-system create secret generic live-acp-chart-provider \
    --from-file="token=${chart_work_dir}/provider-token" >/dev/null || return 1
  live_acp_release_chart_kubectl -n orka-system create secret generic live-acp-chart-scm \
    --from-file="token=${chart_work_dir}/scm-token" >/dev/null || return 1
  live_acp_release_chart_kubectl -n orka-system create secret generic live-acp-chart-artifact \
    --from-file="capability-secret=${chart_work_dir}/artifact-capability" >/dev/null || return 1

  live_acp_kind_log "Installing the candidate chart and its archived CRDs"
  bash "${LIVE_ACP_REPO_ROOT}/scripts/apply-helm-crds.sh" "${archive}" "${LIVE_ACP_CONTEXT}" || return 1
  if ! helm --kube-context "${LIVE_ACP_CONTEXT}" -n orka-system install orka "${archive}" \
    --skip-crds --values "${chart_work_dir}/values.json" --wait --timeout "${LIVE_ACP_ROLLOUT_TIMEOUT}" \
    >"${chart_work_dir}/helm-install.log" 2>&1; then
    live_acp_release_chart_die "candidate chart installation did not become ready"
    return 1
  fi
  for deployment in orka-controller orka-workspace-publisher orka-provider-auth-proxy orka-scm-egress-proxy; do
    live_acp_release_chart_kubectl -n orka-system rollout status "deployment/${deployment}" \
      --timeout="${LIVE_ACP_ROLLOUT_TIMEOUT}" || return 1
    expected_image="${LIVE_ACP_CONTROLLER_REF}"
    if [[ "${deployment}" == "orka-workspace-publisher" ]]; then expected_image="${LIVE_ACP_PUBLISHER_REF}"; fi
    live_acp_release_chart_kubectl -n orka-system get "deployment/${deployment}" -o json |
      jq -e --arg image "${expected_image}" '.spec.template.spec.containers | length == 1 and .[0].image == $image' >/dev/null || return 1
  done
  acp_report_update '.chart.install = true' || return 1

  # Exercise the chart-configured default worker and the authenticated result
  # API. No explicit Task image can hide a broken worker image value.
  marker="orka-release-chart-$(openssl rand -hex 16)"
  jq -n --arg task "${task}" --arg marker "${marker}" '{apiVersion:"core.orka.ai/v1alpha1",kind:"Task",
    metadata:{name:$task,namespace:"orka-system"},spec:{type:"container",command:["/bin/sh","-c","printf %s \"$1\"","--",$marker],
    timeout:"3m",retryPolicy:{maxRetries:0}}}' >"${chart_work_dir}/task-manifest.json" || return 1
  live_acp_release_chart_kubectl create -f "${chart_work_dir}/task-manifest.json" >/dev/null || return 1
  deadline=$((SECONDS + 300))
  while (( SECONDS < deadline )); do
    phase="$(live_acp_release_chart_kubectl -n orka-system get task "${task}" -o jsonpath='{.status.phase}')" || return 1
    case "${phase}" in
      Succeeded) break ;;
      Failed|Cancelled) live_acp_release_chart_die "candidate chart container Task failed"; return 1 ;;
    esac
    sleep 2
  done
  [[ "${phase}" == "Succeeded" ]] || { live_acp_release_chart_die "candidate chart container Task timed out"; return 1; }
  live_acp_release_chart_kubectl -n orka-system create token orka-client --duration=1h >"${chart_work_dir}/api-token" || return 1
  { printf 'Authorization: Bearer '; cat "${chart_work_dir}/api-token"; printf '\n'; } >"${chart_work_dir}/api-header"
  _live_acp_release_chart_start_api || return 1
  _live_acp_release_chart_result "${task}" "${marker}" || return 1
  _live_acp_release_chart_observe "${task}" "${chart_work_dir}/before.json" || return 1
  jq -e --arg image "${LIVE_ACP_GENERAL_WORKER_REF}" '.workerPods | length == 1 and .[0].image == $image' \
    "${chart_work_dir}/before.json" >/dev/null || return 1
  acp_report_update '.chart.containerTask = true' || return 1

  # Remove the completed worker and its logs before replacement. A missing
  # SQLite result can no longer be reconstructed from the old Pod output.
  job="$(jq -er '.task.jobName' "${chart_work_dir}/before.json")" || return 1
  live_acp_release_chart_kubectl -n orka-system delete job "${job}" --cascade=foreground --wait=true --timeout=60s >/dev/null || return 1
  live_acp_release_chart_kubectl -n orka-system get pods -l "orka.ai/task=${task}" -o json |
    jq -e '.items | length == 0' >/dev/null || return 1
  _live_acp_release_chart_stop_api
  live_acp_kind_log "Checking retained chart state after controller replacement"
  live_acp_release_chart_kubectl -n orka-system rollout restart deployment/orka-controller >/dev/null || return 1
  live_acp_release_chart_kubectl -n orka-system rollout status deployment/orka-controller \
    --timeout="${LIVE_ACP_ROLLOUT_TIMEOUT}" || return 1
  _live_acp_release_chart_start_api || return 1
  _live_acp_release_chart_result "${task}" "${marker}" || return 1
  for ((attempt=0; attempt<10; attempt++)); do
    _live_acp_release_chart_observe "${task}" "${chart_work_dir}/after.json" || return 1
    live_acp_release_chart_assert_recovery "${chart_work_dir}/before.json" "${chart_work_dir}/after.json" || {
      live_acp_release_chart_die "chart state changed or the completed Task replayed after restart"
      return 1
    }
    sleep 1
  done
  _live_acp_release_chart_observe "${task}" "${chart_work_dir}/after.json" || return 1
  live_acp_release_chart_assert_recovery "${chart_work_dir}/before.json" "${chart_work_dir}/after.json" || return 1
  acp_report_update '.chart += {recovery:true, noReplay:true, jobRemovedBeforeRestart:true,
    noReplayObservationSeconds:10, pvcs:$after.pvcs, task:$before.task,
    controllerPodUIDBefore:$before.controller.podUID, controllerPodUIDAfter:$after.controller.podUID}' \
    --argjson before "$(cat "${chart_work_dir}/before.json")" --argjson after "$(cat "${chart_work_dir}/after.json")" || return 1

  live_acp_kind_log "Checking opposite-mode upgrade rejection without release changes"
  _live_acp_release_chart_stable_state "${chart_work_dir}/mode-before.json" || return 1
  helm --kube-context "${LIVE_ACP_CONTEXT}" -n orka-system upgrade orka "${archive}" \
    --values "${chart_work_dir}/values.json" --set-string controller.mode=harness-v1 \
    --set providerProxy.enabled=false --timeout "${LIVE_ACP_ROLLOUT_TIMEOUT}" \
    >"${chart_work_dir}/mode-error.log" 2>&1 || rc=$?
  _live_acp_release_chart_stable_state "${chart_work_dir}/mode-after.json" || return 1
  live_acp_release_chart_assert_mode_rejection "${chart_work_dir}/mode-before.json" \
    "${chart_work_dir}/mode-after.json" "${chart_work_dir}/mode-error.log" "${rc}" || {
    live_acp_release_chart_die "opposite-mode upgrade did not fail safely with unchanged release state"
    return 1
  }
  _live_acp_release_chart_observe "${task}" "${chart_work_dir}/after.json" || return 1
  live_acp_release_chart_assert_recovery "${chart_work_dir}/before.json" "${chart_work_dir}/after.json" || return 1
  _live_acp_release_chart_result "${task}" "${marker}" || return 1
  acp_report_update '.chart.oppositeModeRejected = true' || return 1

  # The bootstrap retains the chart for canonical ACP validation and owns its
  # cluster cleanup. Retire only this helper's deterministic Task here.
  live_acp_release_chart_kubectl -n orka-system delete task "${task}" --wait=true --timeout=60s >/dev/null
)

live_acp_kind_deploy_release_chart() {
  local command manifest archive_name package_sha
  [[ "$-" != *x* ]] || { live_acp_release_chart_die "release chart acceptance requires shell tracing to be disabled"; return 1; }
  [[ "${LIVE_ACP_KIND_CREATED:-0}" == "1" && -n "${LIVE_ACP_KIND_CLUSTER:-}" \
    && "${LIVE_ACP_CONTEXT:-}" == "kind-${LIVE_ACP_KIND_CLUSTER}" \
    && -n "${LIVE_ACP_KUBECONFIG:-}" && "${KUBECONFIG:-}" == "${LIVE_ACP_KUBECONFIG}" ]] || {
    live_acp_release_chart_die "release chart acceptance requires the bootstrap-owned Kind context and kubeconfig"
    return 1
  }
  [[ -d "${LIVE_ACP_SECRET_DIR:-}" && -d "${LIVE_ACP_RELEASE_BUNDLE_DIR:-}" ]] || {
    live_acp_release_chart_die "release bundle and bootstrap-owned private directory are required"
    return 1
  }
  for command in helm kubectl jq python3 openssl curl; do
    command -v "${command}" >/dev/null 2>&1 || { live_acp_release_chart_die "missing required command: ${command}"; return 1; }
  done
  manifest="${LIVE_ACP_RELEASE_BUNDLE_DIR}/candidate.json"
  archive_name="$(jq -er '.chart.file' "${manifest}")" || return 1
  package_sha="$(jq -er '.chart.sha256' "${manifest}")" || return 1
  [[ "${archive_name}" =~ ^orka-[A-Za-z0-9.+-]+\.tgz$ && "${package_sha}" =~ ^[a-f0-9]{64}$ \
    && -f "${LIVE_ACP_RELEASE_BUNDLE_DIR}/${archive_name}" ]] || {
    live_acp_release_chart_die "release bundle must name a packaged chart and its SHA256"
    return 1
  }
  export ORKA_NAMESPACE=orka-system ORKA_ACP_RUNTIME_NAMESPACE=orka-runtimes
  export ORKA_CONTROLLER_DEPLOYMENT=orka-controller ORKA_CONTROLLER_CONTAINER=controller ORKA_CONTROLLER_API_PORT=8080
  export ORKA_PUBLISHER_DEPLOYMENT=orka-workspace-publisher ORKA_PUBLISHER_CONTAINER=publisher
  export ORKA_PROVIDER_PROXY_DEPLOYMENT=orka-provider-auth-proxy ORKA_PROVIDER_PROXY_CONTAINER=proxy
  export ORKA_SCM_EGRESS_PROXY_DEPLOYMENT=orka-scm-egress-proxy ORKA_SCM_EGRESS_PROXY_CONTAINER=proxy
  _live_acp_release_chart_acceptance "${manifest}" "${LIVE_ACP_RELEASE_BUNDLE_DIR}/${archive_name}" "${package_sha}"
}
