#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: examples/harness/foundry-responses/live-smoke.sh [--apply] [--wait] [--namespace NAME]

Credentials-safe preflight/deploy helper for the live Foundry hosted AgentKit
Responses smoke gate. By default this performs preflight only. With --apply it
creates/updates the namespace-local adapter Deployment, Service, Secrets, and
AgentRuntime facade, then optionally waits for readiness with --wait.

Required environment for preflight/apply:
  ORKA_FOUNDRY_RESPONSES_ENDPOINT
    Full hosted Responses URL, OR set both:
      ORKA_FOUNDRY_RESPONSES_PROJECT_ENDPOINT
      ORKA_FOUNDRY_RESPONSES_AGENT_NAME
  Exactly one Foundry auth value:
    ORKA_FOUNDRY_RESPONSES_API_KEY
    ORKA_FOUNDRY_RESPONSES_AUTH_BEARER

Optional environment:
  ORKA_FOUNDRY_RESPONSES_NAMESPACE            default: foundry-responses-smoke
  ORKA_FOUNDRY_RESPONSES_RUNTIME_NAME         default: sample-foundry-responses-runtime
  ORKA_FOUNDRY_RESPONSES_ADAPTER_IMAGE        default: ghcr.io/orka-agents/orka/foundry-responses-harness-adapter:latest
  ORKA_FOUNDRY_RESPONSES_ADAPTER_BEARER_TOKEN generated if absent for this run
  ORKA_FOUNDRY_RESPONSES_API_VERSION          default: v1
  ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES default: read
  ORKA_FOUNDRY_RESPONSES_BROKERED_CONTINUATION_PROOF optional

The script never prints secret values. Do not run with shell tracing (set -x).
USAGE
}

apply=0
wait_ready=0
namespace="${ORKA_FOUNDRY_RESPONSES_NAMESPACE:-foundry-responses-smoke}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply)
      apply=1
      shift
      ;;
    --wait)
      wait_ready=1
      shift
      ;;
    --namespace)
      [[ $# -ge 2 ]] || { echo "--namespace requires a value" >&2; exit 2; }
      namespace="$2"
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

if [[ "$wait_ready" == "1" && "$apply" != "1" ]]; then
  echo "error: --wait requires --apply" >&2
  exit 2
fi

runtime_name="${ORKA_FOUNDRY_RESPONSES_RUNTIME_NAME:-sample-foundry-responses-runtime}"
service_url="http://${runtime_name}.${namespace}.svc.cluster.local:8080"
adapter_image="${ORKA_FOUNDRY_RESPONSES_ADAPTER_IMAGE:-ghcr.io/orka-agents/orka/foundry-responses-harness-adapter:latest}"
api_version="${ORKA_FOUNDRY_RESPONSES_API_VERSION:-v1}"
brokered_classes="${ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES:-read}"
endpoint="${ORKA_FOUNDRY_RESPONSES_ENDPOINT:-}"
project_endpoint="${ORKA_FOUNDRY_RESPONSES_PROJECT_ENDPOINT:-}"
agent_name="${ORKA_FOUNDRY_RESPONSES_AGENT_NAME:-}"
api_key="${ORKA_FOUNDRY_RESPONSES_API_KEY:-}"
auth_bearer="${ORKA_FOUNDRY_RESPONSES_AUTH_BEARER:-}"
adapter_bearer="${ORKA_FOUNDRY_RESPONSES_ADAPTER_BEARER_TOKEN:-}"
continuation_proof="${ORKA_FOUNDRY_RESPONSES_BROKERED_CONTINUATION_PROOF:-}"
rollout_nonce="$(date -u +%Y%m%dT%H%M%SZ)"

fail() {
  echo "error: $*" >&2
  exit 2
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

is_https_or_loopback_http() {
  local value="$1"
  local rest authority
  [[ -z "$value" ]] && return 0
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* && "$value" != *$'\t'* ]] || return 1
  if [[ "$value" == https://* ]]; then
    rest="${value#https://}"
    authority="${rest%%/*}"
    authority="${authority%%\?*}"
    authority="${authority%%#*}"
    [[ -n "$authority" && "$authority" != *@* && "$authority" != *[[:space:]]* ]]
    return
  fi
  [[ "$value" == http://* ]] || return 1
  rest="${value#http://}"
  authority="${rest%%/*}"
  authority="${authority%%\?*}"
  authority="${authority%%#*}"
  [[ -n "$authority" && "$authority" != *@* ]] || return 1
  if [[ "$authority" == \[* ]]; then
    [[ "$authority" =~ ^\[::1\](:[0-9]+)?$ ]]
  else
    [[ "$authority" =~ ^(localhost|127\.0\.0\.1)(:[0-9]+)?$ ]]
  fi
}

require_nonempty_name() {
  local label="$1"
  local value="$2"
  [[ "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || \
    fail "$label must be a Kubernetes DNS label, got '$value'"
}

indent_block() {
  sed 's/^/    /'
}

preflight() {
  require_nonempty_name namespace "$namespace"
  require_nonempty_name "runtime name" "$runtime_name"

  if [[ -z "$endpoint" ]]; then
    [[ -n "$project_endpoint" && -n "$agent_name" ]] || \
      fail "set ORKA_FOUNDRY_RESPONSES_ENDPOINT or PROJECT_ENDPOINT plus AGENT_NAME"
  fi
  is_https_or_loopback_http "$endpoint" || fail "ORKA_FOUNDRY_RESPONSES_ENDPOINT must be https (or loopback http for tests)"
  is_https_or_loopback_http "$project_endpoint" || fail "ORKA_FOUNDRY_RESPONSES_PROJECT_ENDPOINT must be https (or loopback http for tests)"

  if [[ -n "$api_key" && -n "$auth_bearer" ]]; then
    fail "set exactly one of ORKA_FOUNDRY_RESPONSES_API_KEY or ORKA_FOUNDRY_RESPONSES_AUTH_BEARER"
  fi
  if [[ -z "$api_key" && -z "$auth_bearer" ]]; then
    fail "set one Foundry auth value: ORKA_FOUNDRY_RESPONSES_API_KEY or ORKA_FOUNDRY_RESPONSES_AUTH_BEARER"
  fi

  IFS=',' read -r -a classes <<<"$brokered_classes"
  for class in "${classes[@]}"; do
    class="${class//[[:space:]]/}"
    [[ "$class" == "read" || "$class" == "write" ]] || fail "unsupported brokered class '$class' (expected read/write)"
  done

  if [[ "$apply" == "1" || "$wait_ready" == "1" ]]; then
    require_cmd kubectl
  fi
}

random_bearer() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
    printf '\n'
  fi
}


decode_base64() {
  if printf '' | base64 --decode >/dev/null 2>&1; then
    base64 --decode
  else
    base64 -D
  fi
}

encode_base64() {
  printf '%s' "$1" | base64 | tr -d '[:space:]'
}

existing_adapter_bearer() {
  local encoded
  encoded="$(kubectl -n "$namespace" get secret "${runtime_name}-token" -o jsonpath='{.data.harness-bearer}' 2>/dev/null || true)"
  [[ -n "$encoded" ]] || return 1
  printf '%s' "$encoded" | decode_base64
}

emit_secret_yaml() {
  local foundry_key_name="foundry-auth"
  local foundry_value="$api_key"
  if [[ -n "$auth_bearer" ]]; then
    foundry_key_name="foundry-bearer"
    foundry_value="$auth_bearer"
  fi

  cat <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: "${runtime_name}-token"
  annotations:
    orka.ai/agent-runtime-endpoint: "${service_url}"
  labels:
    orka.ai/agent-runtime-auth: "true"
    orka.ai/agent-runtime-name: "${runtime_name}"
data:
  harness-bearer: $(encode_base64 "$adapter_bearer")
---
apiVersion: v1
kind: Secret
metadata:
  name: "${runtime_name}-adapter-config"
data:
  ${foundry_key_name}: $(encode_base64 "$foundry_value")
  continuation-proof: $(encode_base64 "$continuation_proof")
YAML
}

emit_runtime_yaml() {
  local auth_env_name="ORKA_FOUNDRY_RESPONSES_API_KEY"
  local auth_key_name="foundry-auth"
  if [[ -n "$auth_bearer" ]]; then
    auth_env_name="ORKA_FOUNDRY_RESPONSES_AUTH_BEARER"
    auth_key_name="foundry-bearer"
  fi

  cat <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: "${runtime_name}"
  labels:
    app.kubernetes.io/name: "${runtime_name}"
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: "${runtime_name}"
  template:
    metadata:
      labels:
        app.kubernetes.io/name: "${runtime_name}"
      annotations:
        orka.ai/foundry-responses-rollout: "${rollout_nonce}"
    spec:
      containers:
      - name: adapter
        image: "${adapter_image}"
        imagePullPolicy: Always
        env:
        - name: ORKA_FOUNDRY_RESPONSES_ADAPTER_ADDR
          value: ":8090"
        - name: ORKA_FOUNDRY_RESPONSES_RUNTIME_NAME
          value: "${runtime_name}"
        - name: ORKA_FOUNDRY_RESPONSES_ADAPTER_BEARER_TOKEN
          valueFrom:
            secretKeyRef:
              name: "${runtime_name}-token"
              key: harness-bearer
        - name: ORKA_FOUNDRY_RESPONSES_ENDPOINT
          value: "${endpoint}"
        - name: ORKA_FOUNDRY_RESPONSES_PROJECT_ENDPOINT
          value: "${project_endpoint}"
        - name: ORKA_FOUNDRY_RESPONSES_AGENT_NAME
          value: "${agent_name}"
        - name: ORKA_FOUNDRY_RESPONSES_API_VERSION
          value: "${api_version}"
        - name: "${auth_env_name}"
          valueFrom:
            secretKeyRef:
              name: "${runtime_name}-adapter-config"
              key: "${auth_key_name}"
        - name: ORKA_FOUNDRY_RESPONSES_BROKERED_CONTINUATION_PROOF
          valueFrom:
            secretKeyRef:
              name: "${runtime_name}-adapter-config"
              key: continuation-proof
              optional: true
        - name: ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES
          value: "${brokered_classes}"
        ports:
        - name: http
          containerPort: 8090
        readinessProbe:
          httpGet:
            path: /v1/ready
            port: http
          periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: "${runtime_name}"
spec:
  selector:
    app.kubernetes.io/name: "${runtime_name}"
  ports:
  - name: http
    port: 8080
    targetPort: http
---
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata:
  name: "${runtime_name}"
spec:
  contractVersion: orka.harness.v1
  deployment:
    mode: external-endpoint
    endpoint: "${service_url}"
  clientAuth:
    bearerTokenSecretRef:
      name: "${runtime_name}-token"
      key: harness-bearer
  capabilities:
    toolExecutionModes:
    - observed
    - brokered
    brokeredToolClasses:
$(printf '%s' "$brokered_classes" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//;s/^/    - /')
    supportsCancel: true
    supportsRuntimeSessions: true
    supportsContinuation: true
YAML
}

preflight

if [[ -z "$adapter_bearer" ]]; then
  if [[ "$apply" == "1" ]] && adapter_bearer="$(existing_adapter_bearer)"; then
    :
  else
    adapter_bearer="$(random_bearer)"
  fi
fi

if [[ "$apply" != "1" ]]; then
  echo "Foundry hosted Responses live smoke preflight passed. Re-run with --apply to deploy." >&2
  exit 0
fi

kubectl get namespace "$namespace" >/dev/null 2>&1 || kubectl create namespace "$namespace" >/dev/null
emit_secret_yaml | kubectl -n "$namespace" apply --server-side --field-manager=orka-foundry-responses-live-smoke -f - >/dev/null
kubectl -n "$namespace" annotate secret "${runtime_name}-token" kubectl.kubernetes.io/last-applied-configuration- --overwrite >/dev/null 2>&1 || true
kubectl -n "$namespace" annotate secret "${runtime_name}-adapter-config" kubectl.kubernetes.io/last-applied-configuration- --overwrite >/dev/null 2>&1 || true
emit_runtime_yaml | kubectl -n "$namespace" apply -f -

echo "Applied Foundry Responses adapter smoke resources in namespace '$namespace'." >&2

if [[ "$wait_ready" == "1" ]]; then
  kubectl -n "$namespace" rollout status "deployment/${runtime_name}" --timeout=120s
  kubectl -n "$namespace" wait --for=condition=Ready "agentruntime/${runtime_name}" --timeout=120s
fi
