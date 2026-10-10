#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
# shellcheck source=scripts/lib/e2e-common.sh
. "${script_dir}/lib/e2e-common.sh"
# shellcheck source=scripts/lib/redact.sh
. "${script_dir}/lib/redact.sh"
# shellcheck source=scripts/lib/e2e-cleanup.sh
. "${script_dir}/lib/e2e-cleanup.sh"

kind_cluster="${KIND_CLUSTER:-orka-aikit-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
aikit_namespace="aikit-system"
aikit_service="aikit"
aikit_port=8080
aikit_image="${AIKIT_IMAGE:-ghcr.io/kaito-project/aikit/qwen3.5:4b@sha256:525dfb8b5ccc1c180f0eab633bcf459d2a574c14bfab01c03535191c810d121d}"
aikit_model="${AIKIT_MODEL:-qwen-3.5-4b}"
aikit_base_url="http://${aikit_service}.${aikit_namespace}.svc:${aikit_port}"
proxy_pf_pid=""
monitor_pid=""
work_dir=""
cleanup_report_dir=""
e2e_started=false

is_expected_kind_context() {
  local current
  current="$(kubectl config current-context 2>/dev/null)" || return 1
  [[ "${current}" == "kind-${kind_cluster}" ]]
}

require_kind_context() {
  is_expected_kind_context || die "AIKit E2E requires the exact kind-${kind_cluster} context"
}

cleanup_port_forward() {
  local pid="$1"
  if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
}

# Kubelet counters work with AIKit's distroless container. Project only this
# Pod's CPU/memory measurements; do not expose node-wide workload metadata.
sample_model_resources() {
  require_kind_context
  local pod identity node uid
  pod="$(kubectl --request-timeout=5s get pods -n "${aikit_namespace}"     -l "app.kubernetes.io/name=${aikit_service}" -o json)" || return 1
  identity="$(printf '%s' "${pod}" | jq -cer '
    [.items[] | select(.status.phase == "Running")] |
    if length == 1 then .[0] | {node:.spec.nodeName,uid:.metadata.uid}
    else error("expected one running model Pod") end')" || return 1
  node="$(printf '%s' "${identity}" | jq -r .node)"
  uid="$(printf '%s' "${identity}" | jq -r .uid)"
  [[ "${node}" =~ ^[a-z0-9][a-z0-9.-]*$ && -n "${uid}" ]] || return 1
  kubectl --request-timeout=10s get --raw "/api/v1/nodes/${node}/proxy/stats/summary" |
    jq -ce --arg uid "${uid}" --arg namespace "${aikit_namespace}" '
      [.pods[] | select(.podRef.uid == $uid and .podRef.namespace == $namespace) |
        {cpu:{time:.cpu.time,usageNanoCores:.cpu.usageNanoCores,usageCoreNanoSeconds:.cpu.usageCoreNanoSeconds},
         memory:{time:.memory.time,usageBytes:.memory.usageBytes,workingSetBytes:.memory.workingSetBytes,
                 rssBytes:.memory.rssBytes,pageFaults:.memory.pageFaults,majorPageFaults:.memory.majorPageFaults}}] |
      if length == 1 then .[0] else error("model Pod stats unavailable") end'
}

start_model_monitor() {
  (
    while is_expected_kind_context; do
      date -u '+%Y-%m-%dT%H:%M:%SZ'
      sample_model_resources 2>&1 | redact || true
      sleep 15
    done
  ) >"${cleanup_report_dir}/model-resources.log" &
  monitor_pid=$!
}

dump_diagnostics() {
  {
    kubectl get pods,svc,deploy -n "${aikit_namespace}" -o wide || true
    kubectl get events -n "${aikit_namespace}" --sort-by=.lastTimestamp || true
    kubectl logs -n "${aikit_namespace}" deployment/"${aikit_service}" --tail=100 || true
    kubectl logs -n "${orka_namespace}" deployment/orka-controller-manager --tail=100 || true
    kubectl logs -n "${orka_namespace}" deployment/orka-provider-auth-proxy --tail=100 || true
  } 2>&1 | redact >&2
}

on_exit() {
  local status="$1"
  trap - EXIT
  cleanup_port_forward "${monitor_pid:-}"
  cleanup_port_forward "${proxy_pf_pid}"
  if ! is_expected_kind_context; then
    log "Refusing diagnostics or teardown against an unverified cluster context"
    rm -rf "${work_dir}" >/dev/null 2>&1 || true
    exit 90
  fi
  if [[ "${e2e_started}" == true ]] && ! e2e_cleanup_evidence_passed "${cleanup_report_dir}"; then
    log "Normal resource cleanup was not proved; preserving evidence"
    status=90
  fi
  if [[ "${status}" -ne 0 ]]; then
    dump_diagnostics
    log "AIKit Qwen e2e failed"
  fi
  if ! e2e_cleanup_kind "${kind_cluster}" "${cleanup_report_dir}"; then
    log "Kind teardown was not proved; preserving evidence"
    status=90
  fi
  rm -rf "${work_dir}" >/dev/null 2>&1 || true
  exit "${status}"
}

# Called by BeforeSuite after the normal production installation. Only this
# isolated E2E cluster gets a different upstream. Authentication, token reload,
# runtime ingress, and DNS/model NetworkPolicy rules remain unchanged.
# Kind's default CNI does not enforce NetworkPolicy; this lane checks the
# configured rules, not a denied-egress security boundary.
configure_provider_proxy() {
  require_kind_context
  [[ "${E2E_LOCAL_MODEL:-}" == "${aikit_model}" ]] || die "local model configuration does not match AIKIT_MODEL"
  local args patch egress
  args="$(kubectl get deployment orka-provider-auth-proxy -n "${orka_namespace}" -o json |
    jq -ce --arg url "${aikit_base_url}" '
      .spec.template.spec.containers[] | select(.name == "proxy") | .args
      | if any(.[]; startswith("--upstream-base-url=")) then
          map(if startswith("--upstream-base-url=") then "--upstream-base-url=" + $url else . end)
        else error("provider proxy upstream flag is missing") end')" || die "could not resolve provider proxy upstream arguments"
  patch="$(jq -cn --argjson args "${args}" '{spec:{template:{spec:{containers:[{name:"proxy",args:$args}]}}}}')"
  kubectl patch deployment orka-provider-auth-proxy -n "${orka_namespace}" --type=strategic -p "${patch}" || die "could not configure provider proxy upstream"

  egress="$(kubectl get networkpolicy orka-provider-auth-proxy -n "${orka_namespace}" -o json |
    jq -ce --arg namespace "${aikit_namespace}" --arg service "${aikit_service}" --argjson port "${aikit_port}" '
      .spec.egress
      | if any(.[]; any(.to[]?; .namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "vekil-system" or
                       .namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == $namespace)) then .
        else error("provider proxy model egress is missing") end
      | map(
        if any(.to[]?; .namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "vekil-system" or
                          .namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == $namespace) then
          .to = [{namespaceSelector:{matchLabels:{"kubernetes.io/metadata.name":$namespace}},
                  podSelector:{matchLabels:{"app.kubernetes.io/name":$service}}}]
          | .ports = [{protocol:"TCP",port:$port}]
        else . end)')" || die "could not resolve provider proxy model egress"
  patch="$(jq -cn --argjson egress "${egress}" '{spec:{egress:$egress}}')"
  kubectl patch networkpolicy orka-provider-auth-proxy -n "${orka_namespace}" --type=merge -p "${patch}" || die "could not configure provider proxy model egress"
  kubectl rollout status deployment/orka-provider-auth-proxy -n "${orka_namespace}" --timeout=2m

  # Clients retain their 180s assertion budget. Bound CI-only server work just
  # below it, instead of letting abandoned requests occupy the model for 30m.
  # Production defaults and native-runtime Task deadlines are unchanged.
  args="$(kubectl get deployment orka-controller-manager -n "${orka_namespace}" -o json |
    jq -ce '.spec.template.spec.containers[] | select(.name == "manager") | .args
      | map(select(startswith("--chat-max-duration=") | not)) + ["--chat-max-duration=170s"]')" || die "could not resolve controller arguments"
  patch="$(jq -cn --argjson args "${args}" '{spec:{template:{spec:{containers:[{name:"manager",args:$args}]}}}}')"
  kubectl patch deployment orka-controller-manager -n "${orka_namespace}" --type=strategic -p "${patch}" || die "could not bound CI chat requests"
  kubectl rollout status deployment/orka-controller-manager -n "${orka_namespace}" --timeout=2m
  # The base installation creates this namespace for its default upstream.
  # No Vekil workload is installed in this lane.
  kubectl delete namespace vekil-system --ignore-not-found --wait=true --timeout=1m
}

deploy_aikit() {
  require_kind_context
  kubectl apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: ${aikit_namespace}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${aikit_service}
  namespace: ${aikit_namespace}
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: ${aikit_service}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ${aikit_service}
    spec:
      automountServiceAccountToken: false
      volumes:
        - name: configuration
          configMap:
            name: aikit-configuration
      containers:
        - name: model
          image: ${aikit_image}
          imagePullPolicy: IfNotPresent
          args: ["--config-file=/etc/orka-aikit/config.yaml"]
          volumeMounts:
            - name: configuration
              mountPath: /etc/orka-aikit
              readOnly: true
          ports:
            - name: http
              containerPort: ${aikit_port}
          env:
            - name: LOCALAI_THREADS
              value: "4"
            - name: LOCALAI_CONTEXT_SIZE
              value: "32768"
            - name: LOCALAI_LOAD_TO_MEMORY
              value: ${aikit_model}
          resources:
            requests:
              cpu: "2"
              memory: 2Gi
            limits:
              cpu: "4"
              memory: 6Gi
          startupProbe:
            httpGet:
              path: /readyz
              port: http
            periodSeconds: 5
            failureThreshold: 120
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
            periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: ${aikit_service}
  namespace: ${aikit_namespace}
spec:
  selector:
    app.kubernetes.io/name: ${aikit_service}
  ports:
    - port: ${aikit_port}
      targetPort: http
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: orka-model-clients-only
  namespace: ${aikit_namespace}
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: ${aikit_service}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ${orka_namespace}
      ports:
        - protocol: TCP
          port: ${aikit_port}
YAML
  kubectl create configmap aikit-configuration -n "${aikit_namespace}" \
    --from-file=config.yaml="${script_dir}/fixtures/aikit/config.yaml" \
    --from-file=chat-template.jinja="${script_dir}/fixtures/aikit/qwen3.5-chat-template.jinja" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl rollout status deployment/"${aikit_service}" -n "${aikit_namespace}" --timeout=10m
}

# Exercise the streamed tool-call shape used by Codex before building Orka.
# A successful text-only response does not qualify this backend path.
qualify_responses_tools() {
  local url="$1"
  jq -n --arg model "${aikit_model}" '{model:$model,stream:true,max_output_tokens:128,
    input:[{role:"user",content:"Call ci_echo with text ORKA_TOOL_READY. Do not answer directly."}],
    tools:[{type:"function",name:"ci_echo",description:"Echo the supplied text.",
      parameters:{type:"object",properties:{text:{type:"string"}},required:["text"],additionalProperties:false}}],
    tool_choice:{type:"function",name:"ci_echo"}}' >"${work_dir}/tools-warmup.json"
  if ! curl -fsS -N --max-time 180 -H 'Content-Type: application/json' \
    --data-binary @"${work_dir}/tools-warmup.json" "${url}/v1/responses" >"${work_dir}/tools-warmup.sse"; then
    die "Qwen streamed Responses tool preflight failed or timed out"
  fi
  sed -n 's/^data: //p' "${work_dir}/tools-warmup.sse" | sed '/^\[DONE\]\r*$/d' |
    jq -se 'map(select(.type == "response.completed")) |
      if length == 1 then .[0].response else error("missing unique response.completed event") end' \
      >"${work_dir}/tools-warmup-response.json"
  jq -e '.status == "completed" and any(.output[]?;
    .type == "function_call" and .name == "ci_echo" and (.call_id | length > 0) and
    (.arguments | fromjson | .text == "ORKA_TOOL_READY"))' \
    "${work_dir}/tools-warmup-response.json" >/dev/null || die "Qwen preflight did not emit the requested tool call"
  # Only the tool output exposes this fresh value; echoing the prompt cannot pass.
  local tool_output
  tool_output="ORKA_TOOL_OUTPUT_$(od -An -N16 -tx1 /dev/urandom | tr -d '[:space:]')"
  jq -n --arg model "${aikit_model}" --arg tool_output "${tool_output}" \
    --slurpfile response "${work_dir}/tools-warmup-response.json" \
    '{model:$model,max_output_tokens:128,input:(
      [{role:"user",content:"Call ci_echo with text ORKA_TOOL_READY. Do not answer directly."}] +
      $response[0].output +
      [$response[0].output[] | select(.type == "function_call") |
        {type:"function_call_output",call_id:.call_id,output:$tool_output}] +
      [{role:"developer",content:"Reply with exactly the tool output text and nothing else."}])}' \
      >"${work_dir}/tool-result-warmup.json"
  curl -fsS --max-time 180 -H 'Content-Type: application/json' \
    --data-binary @"${work_dir}/tool-result-warmup.json" "${url}/v1/responses" >"${work_dir}/tool-result-warmup-response.json"
  jq -e --arg tool_output "${tool_output}" '[.output[]?.content[]?.text // empty] | join("") == $tool_output' \
    "${work_dir}/tool-result-warmup-response.json" >/dev/null || die "Qwen preflight did not consume its tool result"
  log "Qwen streamed Responses tool call and tool-result preflight passed"
}

warm_model() {
  local port=18190 url="http://127.0.0.1:18190" ready=false
  kubectl port-forward -n "${aikit_namespace}" service/"${aikit_service}" "${port}:${aikit_port}" \
    --address=127.0.0.1 >"${work_dir}/model-port-forward.log" 2>&1 &
  proxy_pf_pid=$!
  for _ in $(seq 1 30); do
    kill -0 "${proxy_pf_pid}" 2>/dev/null || die "AIKit port-forward stopped"
    if curl -fsS --max-time 2 "${url}/readyz" >/dev/null 2>&1; then
      ready=true
      break
    fi
    sleep 1
  done
  [[ "${ready}" == true ]] || die "AIKit readiness probe timed out"
  curl -fsS --max-time 10 "${url}/v1/models" >"${work_dir}/models.json"
  jq -e --arg model "${aikit_model}" 'any(.data[]; .id == $model)' "${work_dir}/models.json" >/dev/null
  jq -n --arg model "${aikit_model}" \
    '{model:$model,messages:[{role:"user",content:"Reply with OK."}],temperature:0,max_tokens:16}' >"${work_dir}/warmup.json"
  curl -fsS --max-time 300 -H 'Content-Type: application/json' \
    --data-binary @"${work_dir}/warmup.json" "${url}/v1/chat/completions" >"${work_dir}/warmup-response.json"
  jq -e '.choices[0].message.content | type == "string" and length > 0' "${work_dir}/warmup-response.json" >/dev/null
  # Qualify the multi-turn Responses shape used by both Orka and Codex before
  # building the full stack. The baked Qwen template rejects late control roles.
  jq -n --arg model "${aikit_model}" '{model:$model,max_output_tokens:128,input:[
    {role:"system",content:"This is an Orka connectivity check."},
    {role:"user",content:"Please perform the check."},
    {role:"assistant",content:"I will perform it."},
    {role:"developer",content:"Reply with exactly ORKA_RESPONSES_READY."},
    {role:"user",content:"Proceed."}]}' >"${work_dir}/responses-warmup.json"
  curl -fsS --max-time 180 -H 'Content-Type: application/json' \
    --data-binary @"${work_dir}/responses-warmup.json" "${url}/v1/responses" >"${work_dir}/responses-warmup-response.json"
  if ! jq -e '[.output[]?.content[]?.text // empty] | join("") | contains("ORKA_RESPONSES_READY")' \
    "${work_dir}/responses-warmup-response.json" >/dev/null; then
    # Only this synthetic connectivity response is projected; never dump caller
    # requests, headers, environment, or credentials into diagnostics.
    jq '{status,usage,output:[.output[]? | {type,content}]}' \
      "${work_dir}/responses-warmup-response.json" | head -c 4096 | redact >&2
    die "Qwen multi-turn Responses preflight did not return its connectivity marker"
  fi
  log "Qwen chat and multi-turn Responses preflight passed"
  qualify_responses_tools "${url}"
  make ensure-ui-embed
  # Prefix preparation is startup work, not an extension of live request
  # budgets. Qualification immediately afterward still has a 170s deadline.
  AIKIT_PROBE_URL="${url}" AIKIT_PROBE_MODEL="${aikit_model}" \
    AIKIT_PROBE_REPORT="${cleanup_report_dir}/full-prompt-probe.json" \
    AIKIT_PROBE_PRELOAD_PREFIXES=true \
    go test -tags=e2e ./internal/api -run '^TestAIKitFullPromptProbe$' -v -count=1 -timeout=40m
  cleanup_port_forward "${proxy_pf_pid}"
  proxy_pf_pid=""
}

main() {
  for command in make go kubectl kind curl jq; do require_cmd "${command}"; done
  [[ "${aikit_image}" =~ ^[a-z0-9][a-z0-9./:_-]+@sha256:[a-f0-9]{64}$ ]] || die "AIKIT_IMAGE must be digest-pinned"
  [[ "${aikit_model}" =~ ^[a-zA-Z0-9._-]+$ ]] || die "AIKIT_MODEL must be a model identifier"
  work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/aikit-e2e.XXXXXX")"
  local report_root="${E2E_CLEANUP_REPORT_DIR:-${repo_root}/bin/e2e-cleanup}"
  mkdir -p "${report_root}"
  cleanup_report_dir="$(mktemp -d "${report_root}/aikit-attempt.XXXXXX")"
  export E2E_CLEANUP_REPORT_DIR="${cleanup_report_dir}"
  trap 'on_exit $?' EXIT

  # This lane never loads local provider credentials or calls a cloud model.
  unset COPILOT_GITHUB_TOKEN E2E_GITHUB_TOKEN E2E_OPENAI_API_KEY E2E_ANTHROPIC_API_KEY
  export AIKIT_MODEL="${aikit_model}" E2E_LOCAL_MODEL="${aikit_model}"
  log "Creating Kind cluster ${kind_cluster}"
  make setup-test-e2e KIND_CLUSTER="${kind_cluster}"
  log "Deploying digest-pinned AIKit Qwen CPU model"
  deploy_aikit
  start_model_monitor
  warm_model

  log "Running real model and runtime E2E specs without Vekil"
  e2e_started=true
  KIND_CLUSTER="${kind_cluster}" \
  E2E_LIVE_COPILOT_PROXY_BASE_URL="${aikit_base_url}/v1" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_NAMESPACE="${aikit_namespace}" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_NAME="${aikit_service}" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_PORT="${aikit_port}" \
  E2E_LIVE_ACP_PROVIDER_PROXY_SERVICE_NAMESPACE="${aikit_namespace}" \
  E2E_LIVE_ACP_PROVIDER_PROXY_SERVICE_NAME="${aikit_service}" \
  E2E_LIVE_ACP_PROVIDER_PROXY_SERVICE_PORT="${aikit_port}" \
  go test -tags=e2e ./test/e2e/ -timeout 45m -v -ginkgo.v \
    -ginkgo.focus="Live Copilot Proxy Provider|Live Chat API|Live Anthropic Compat API|Live Agent Runtime Matrix"
  log "AIKit Qwen e2e passed"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  case "${1:-}" in
    configure-provider-proxy) configure_provider_proxy ;;
    "") main ;;
    *) die "usage: $0 [configure-provider-proxy]" ;;
  esac
fi
