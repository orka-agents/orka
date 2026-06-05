#!/usr/bin/env bash

set -Eeuo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"

kind_cluster="${KIND_CLUSTER:-orka-live-github-oidc-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
orka_controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
orka_api_service="${ORKA_API_SERVICE:-orka-api}"
orka_api_service_port="${ORKA_API_SERVICE_PORT:-8080}"
orka_api_local_port="${ORKA_API_LOCAL_PORT:-18080}"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:live-github-oidc-e2e}"
live_kontxt_image="${ORKA_LIVE_KONTXT_IMAGE:-orka-live-kontxt-e2e:live-github-oidc-e2e}"
github_oidc_audience="${ORKA_GITHUB_OIDC_AUDIENCE:-orka-live-github-oidc-e2e}"
github_oidc_issuer="${ORKA_GITHUB_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"
github_oidc_token="${ORKA_GITHUB_OIDC_TOKEN:-}"
kontxt_issuer="${ORKA_KONTXT_ISSUER:-https://kontxt-live.test}"
kontxt_audience="${ORKA_KONTXT_AUDIENCE:-orka-live-kontxt-e2e}"
kontxt_subject="${ORKA_KONTXT_SUBJECT:-kontxt-workload-subject}"
kontxt_requesting_workload="${ORKA_KONTXT_REQUESTING_WORKLOAD:-spiffe://example.test/ns/default/sa/live-kontxt-client}"
kontxt_jwks_name="${ORKA_KONTXT_JWKS_NAME:-kontxt-jwks}"
kontxt_jwks_port="${ORKA_KONTXT_JWKS_PORT:-8080}"
kontxt_jwks_url="http://${kontxt_jwks_name}.default.svc.cluster.local:${kontxt_jwks_port}/.well-known/jwks.json"
kontxt_tts_name="${ORKA_KONTXT_TTS_NAME:-kontxt-tts}"
kontxt_tts_port="${ORKA_KONTXT_TTS_PORT:-8080}"
kontxt_tts_local_port="${ORKA_KONTXT_TTS_LOCAL_PORT:-18082}"
kontxt_tts_url="http://${kontxt_tts_name}.default.svc.cluster.local:${kontxt_tts_port}"
kontxt_tts_jwks_url="${kontxt_tts_url}/.well-known/jwks.json"
kontxt_downstream_name="${ORKA_KONTXT_DOWNSTREAM_NAME:-kontxt-downstream-verifier}"
kontxt_downstream_port="${ORKA_KONTXT_DOWNSTREAM_PORT:-8081}"
kontxt_downstream_local_port="${ORKA_KONTXT_DOWNSTREAM_LOCAL_PORT:-18083}"
kontxt_tts_parent_scope="${ORKA_KONTXT_TTS_PARENT_SCOPE:-write orka:agents:run orka:tools:http}"
kontxt_tts_child_scope="${ORKA_KONTXT_TTS_CHILD_SCOPE:-orka:agents:run}"
kontxt_tts_tool_scope="${ORKA_KONTXT_TTS_TOOL_SCOPE:-orka:tools:http}"
kontxt_token=""
kontxt_tts_parent_token=""
kontxt_child_token=""
api_pf_pid=""
kontxt_tts_pf_pid=""
kontxt_downstream_pf_pid=""
task_name=""
kontxt_task_name=""
kontxt_tts_parent_task_name=""
kontxt_tts_child_task_name=""
kontxt_tts_coordinator_agent="live-tts-coordinator"
kontxt_tts_worker_agent="live-tts-worker"
work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/live-github-oidc-e2e.XXXXXX")"
api_pf_log="${work_dir}/api-port-forward.log"
kontxt_tts_pf_log="${work_dir}/kontxt-tts-port-forward.log"
kontxt_downstream_pf_log="${work_dir}/kontxt-downstream-port-forward.log"
kontxt_jwks_file="${work_dir}/kontxt-jwks.json"
kontxt_token_file="${work_dir}/kontxt-token.txt"
kontxt_key_file="${work_dir}/kontxt-key.pem"
kontxt_kid_file="${work_dir}/kontxt-kid.txt"
kontxt_fixture_generator="${work_dir}/generate-kontxt-fixture.go"
kontxt_tts_parent_token_file="${work_dir}/kontxt-tts-parent-token.txt"
kontxt_child_token_file="${work_dir}/kontxt-child-token.txt"
manager_kustomization="${repo_root}/config/manager/kustomization.yaml"
manager_kustomization_backup="${work_dir}/manager-kustomization.yaml.bak"

redact() {
  local text
  text="$(cat)"
  if [[ -n "${github_oidc_token}" ]]; then
    text="${text//${github_oidc_token}/[REDACTED_GITHUB_OIDC_JWT]}"
  fi
  if [[ -n "${ORKA_GITHUB_OIDC_TOKEN:-}" ]]; then
    text="${text//${ORKA_GITHUB_OIDC_TOKEN}/[REDACTED_GITHUB_OIDC_JWT]}"
  fi
  if [[ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]]; then
    text="${text//${ACTIONS_ID_TOKEN_REQUEST_TOKEN}/[REDACTED_ACTIONS_ID_TOKEN_REQUEST_TOKEN]}"
  fi
  if [[ -n "${kontxt_token}" ]]; then
    text="${text//${kontxt_token}/[REDACTED_KONTXT_TXTOKEN]}"
  fi
  if [[ -n "${kontxt_tts_parent_token}" ]]; then
    text="${text//${kontxt_tts_parent_token}/[REDACTED_KONTXT_TTS_PARENT_TXTOKEN]}"
  fi
  if [[ -n "${kontxt_child_token}" ]]; then
    text="${text//${kontxt_child_token}/[REDACTED_KONTXT_CHILD_TXTOKEN]}"
  fi
  printf '%s' "${text}" | sed -E \
    -e 's/(Authorization: *([Bb]earer|token) +)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/(ACTIONS_ID_TOKEN_REQUEST_TOKEN=)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/(ORKA_GITHUB_OIDC_TOKEN=)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/[REDACTED_JWT]/g'
}

cleanup_one_port_forward() {
  local pid="$1"
  if [[ -n "${pid}" ]]; then
    if kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || true
    fi
    wait "${pid}" 2>/dev/null || true
  fi
}

cleanup_port_forward() {
  cleanup_one_port_forward "${api_pf_pid}"
  cleanup_one_port_forward "${kontxt_tts_pf_pid}"
  cleanup_one_port_forward "${kontxt_downstream_pf_pid}"
  api_pf_pid=""
  kontxt_tts_pf_pid=""
  kontxt_downstream_pf_pid=""
}

restore_manager_kustomization() {
  if [[ -f "${manager_kustomization_backup}" ]]; then
    cp "${manager_kustomization_backup}" "${manager_kustomization}" || true
  fi
}

dump_diagnostics() {
  log "Collecting redacted diagnostics"

  {
    echo "=== Current Kubernetes Context ==="
    kubectl config current-context 2>/dev/null || true
    echo
    echo "=== Orka Namespace Resources ==="
    kubectl get pods,svc,deploy,tasks -n "${orka_namespace}" -o wide 2>/dev/null || true
    echo
    echo "=== Default Namespace Resources ==="
    kubectl get pods,svc,deploy,tasks,agents,providers,secrets -n default -o wide 2>/dev/null || true
    echo
    echo "=== Orka Namespace Events ==="
    kubectl get events -n "${orka_namespace}" --sort-by=.lastTimestamp 2>/dev/null || true
    echo
    echo "=== Default Namespace Events ==="
    kubectl get events -n default --sort-by=.lastTimestamp 2>/dev/null || true
    echo
    echo "=== Controller Logs ==="
    local controller_pod
    controller_pod="$(kubectl get pods -l control-plane=controller-manager -n "${orka_namespace}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [[ -n "${controller_pod}" ]]; then
      kubectl logs "${controller_pod}" -n "${orka_namespace}" -c manager --tail=200 2>/dev/null || true
    else
      kubectl logs deployment/"${orka_controller_deployment}" -n "${orka_namespace}" -c manager --tail=200 2>/dev/null || true
    fi
    echo
    echo "=== kontxt TTS Logs ==="
    kubectl logs deployment/"${kontxt_tts_name}" -n default --tail=200 2>/dev/null || true
    echo
    echo "=== kontxt Downstream Verifier Logs ==="
    kubectl logs deployment/"${kontxt_downstream_name}" -n default --tail=200 2>/dev/null || true
    echo
    echo "=== API Port-forward Log ==="
    if [[ -f "${api_pf_log}" ]]; then
      cat "${api_pf_log}" 2>/dev/null || true
    fi
    echo
    echo "=== kontxt TTS Port-forward Log ==="
    if [[ -f "${kontxt_tts_pf_log}" ]]; then
      cat "${kontxt_tts_pf_log}" 2>/dev/null || true
    fi
    echo
    echo "=== kontxt Downstream Port-forward Log ==="
    if [[ -f "${kontxt_downstream_pf_log}" ]]; then
      cat "${kontxt_downstream_pf_log}" 2>/dev/null || true
    fi
  } | redact >&2
}

on_exit() {
  local status="$1"
  set +e

  if [[ "${status}" -ne 0 ]]; then
    dump_diagnostics
  fi

  cleanup_port_forward

  if [[ -n "${task_name}" ]]; then
    kubectl delete task "${task_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  fi
  if [[ -n "${kontxt_task_name}" ]]; then
    kubectl delete task "${kontxt_task_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  fi
  if [[ -n "${kontxt_tts_parent_task_name}" ]]; then
    kubectl delete task "${kontxt_tts_parent_task_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  fi
  if [[ -n "${kontxt_tts_child_task_name}" ]]; then
    kubectl delete task "${kontxt_tts_child_task_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  fi
  kubectl delete tasks -l "orka.ai/parent-task=${kontxt_tts_parent_task_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  kubectl delete agent "${kontxt_tts_coordinator_agent}" "${kontxt_tts_worker_agent}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  kubectl delete provider live-tts-provider -n default --ignore-not-found=true >/dev/null 2>&1 || true
  kubectl delete secret live-tts-provider-secret -n default --ignore-not-found=true >/dev/null 2>&1 || true
  kubectl delete deployment "${kontxt_jwks_name}" "${kontxt_tts_name}" "${kontxt_downstream_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  kubectl delete service "${kontxt_jwks_name}" "${kontxt_tts_name}" "${kontxt_downstream_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  kubectl delete configmap "${kontxt_jwks_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true

  restore_manager_kustomization
  make cleanup-test-e2e KIND_CLUSTER="${kind_cluster}" >/dev/null 2>&1 || true
  rm -rf "${work_dir}" >/dev/null 2>&1 || true

  if [[ "${status}" -ne 0 ]]; then
    log "Live GitHub OIDC e2e failed"
  fi
}

run() {
  printf '+ ' >&2
  printf '%q ' "$@" >&2
  printf '\n' >&2
  "$@"
}

wait_for_http() {
  local url="$1"
  local description="$2"
  local attempts_remaining=90

  while (( attempts_remaining > 0 )); do
    if curl -fsS --connect-timeout 5 --max-time 10 "${url}" >/dev/null 2>&1; then
      return 0
    fi
    if [[ -n "${api_pf_pid}" ]] && ! kill -0 "${api_pf_pid}" 2>/dev/null; then
      warn "API port-forward exited while waiting for ${description}; restarting"
      wait "${api_pf_pid}" 2>/dev/null || true
      api_pf_pid="$(start_api_port_forward)"
    fi
    attempts_remaining=$((attempts_remaining - 1))
    sleep 2
  done

  die "${description} never became available at ${url}"
}

wait_for_labeled_resource() {
  local resource="$1"
  local namespace="$2"
  local selector="$3"
  local output_file="$4"
  local description="$5"
  local attempts_remaining=90

  while (( attempts_remaining > 0 )); do
    if kubectl get "${resource}" -n "${namespace}" -l "${selector}" -o json >"${output_file}" 2>/dev/null && \
      jq -e '(.items // []) | length > 0' "${output_file}" >/dev/null; then
      return 0
    fi
    attempts_remaining=$((attempts_remaining - 1))
    sleep 2
  done

  {
    echo "Timed out waiting for ${description} with selector ${selector}"
    kubectl get "${resource}" -n "${namespace}" -l "${selector}" -o wide 2>/dev/null || true
  } | redact >&2
  die "${description} was not created"
}

start_port_forward() {
  local namespace_arg="$1"
  local resource="$2"
  local local_port="$3"
  local remote_port="$4"
  local logfile="$5"

  kubectl -n "${namespace_arg}" port-forward "${resource}" "${local_port}:${remote_port}" >>"${logfile}" 2>&1 &
  echo $!
}

start_api_port_forward() {
  start_port_forward "${orka_namespace}" "svc/${orka_api_service}" "${orka_api_local_port}" "${orka_api_service_port}" "${api_pf_log}"
}

start_kontxt_tts_port_forward() {
  start_port_forward default "svc/${kontxt_tts_name}" "${kontxt_tts_local_port}" "${kontxt_tts_port}" "${kontxt_tts_pf_log}"
}

start_kontxt_downstream_port_forward() {
  start_port_forward default "svc/${kontxt_downstream_name}" "${kontxt_downstream_local_port}" "${kontxt_downstream_port}" "${kontxt_downstream_pf_log}"
}

require_github_oidc_token_source() {
  if [[ -n "${github_oidc_token}" ]]; then
    return 0
  fi
  if [[ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" && -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]]; then
    return 0
  fi

  rm -rf "${work_dir}" >/dev/null 2>&1 || true
  die "GitHub OIDC token source is required: run in GitHub Actions with id-token: write or set ORKA_GITHUB_OIDC_TOKEN"
}

write_kontxt_fixture_generator() {
  cat >"${kontxt_fixture_generator}" <<'GO'
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
)

func mustEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		panic(fmt.Sprintf("%s is required", name))
	}
	return value
}

func main() {
	switch mode := mustEnv("KONTXT_FIXTURE_MODE"); mode {
	case "jwks":
		generateJWKSFixture()
	case "token":
		mintTokenFixture()
	default:
		panic(fmt.Sprintf("unsupported KONTXT_FIXTURE_MODE %q", mode))
	}
}

func generateJWKSFixture() {
	jwksPath := mustEnv("KONTXT_JWKS_FILE")
	keyPath := mustEnv("KONTXT_KEY_FILE")
	kidPath := mustEnv("KONTXT_KID_FILE")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	kid := fmt.Sprintf("kontxt-live-%d", time.Now().UnixNano())

	jwk := map[string]any{
		"kty": "RSA",
		"use": "sig",
		"kid": kid,
		"alg": kontxttoken.SigningAlgorithm,
		"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}
	jwks := map[string]any{"keys": []any{jwk}}

	jwksBytes, err := json.MarshalIndent(jwks, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(jwksPath, jwksBytes, 0o600); err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		panic(err)
	}
	if err := os.WriteFile(kidPath, []byte(kid), 0o600); err != nil {
		panic(err)
	}
}

func mintTokenFixture() {
	tokenPath := mustEnv("KONTXT_TOKEN_FILE")
	keyPath := mustEnv("KONTXT_KEY_FILE")
	kidPath := mustEnv("KONTXT_KID_FILE")
	issuer := mustEnv("KONTXT_ISSUER")
	audience := mustEnv("KONTXT_AUDIENCE")
	subject := mustEnv("KONTXT_SUBJECT")
	requestingWorkload := mustEnv("KONTXT_REQUESTING_WORKLOAD")

	key := readPrivateKey(keyPath)
	kidBytes, err := os.ReadFile(kidPath)
	if err != nil {
		panic(err)
	}
	kid := strings.TrimSpace(string(kidBytes))
	if kid == "" {
		panic("empty kontxt key id")
	}

	token, err := kontxttoken.New(kontxttoken.Claims{
		Issuer:             issuer,
		Audience:           audience,
		Subject:            subject,
		Scope:              "read write",
		RequestingWorkload: requestingWorkload,
		TransactionContext: map[string]any{
			"e2e": "live-kind",
		},
		RequesterContext: map[string]any{
			"ci": "github-actions",
		},
	}, key, kid, 4*time.Hour)
	if err != nil {
		panic(err)
	}

	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		panic(err)
	}
}

func readPrivateKey(path string) *rsa.PrivateKey {
	keyPEM, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		panic("failed to decode RSA private key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	return key
}
GO
}

generate_kontxt_jwks_fixture() {
  write_kontxt_fixture_generator
  log "Generating kontxt JWKS fixture"
  KONTXT_FIXTURE_MODE="jwks" \
    KONTXT_JWKS_FILE="${kontxt_jwks_file}" \
    KONTXT_KEY_FILE="${kontxt_key_file}" \
    KONTXT_KID_FILE="${kontxt_kid_file}" \
    go run "${kontxt_fixture_generator}"
  [[ -s "${kontxt_jwks_file}" ]] || die "generated kontxt JWKS was empty"
  [[ -s "${kontxt_key_file}" ]] || die "generated kontxt private key was empty"
  [[ -s "${kontxt_kid_file}" ]] || die "generated kontxt key id was empty"
}

mint_kontxt_token() {
  log "Minting kontxt TxToken fixture"
  [[ -s "${kontxt_key_file}" ]] || die "kontxt private key fixture is missing"
  [[ -s "${kontxt_kid_file}" ]] || die "kontxt key id fixture is missing"
  KONTXT_FIXTURE_MODE="token" \
    KONTXT_TOKEN_FILE="${kontxt_token_file}" \
    KONTXT_KEY_FILE="${kontxt_key_file}" \
    KONTXT_KID_FILE="${kontxt_kid_file}" \
    KONTXT_ISSUER="${kontxt_issuer}" \
    KONTXT_AUDIENCE="${kontxt_audience}" \
    KONTXT_SUBJECT="${kontxt_subject}" \
    KONTXT_REQUESTING_WORKLOAD="${kontxt_requesting_workload}" \
    go run "${kontxt_fixture_generator}"
  kontxt_token="$(<"${kontxt_token_file}")"
  [[ -n "${kontxt_token}" ]] || die "generated kontxt token was empty"
}

deploy_kontxt_jwks() {
  log "Deploying in-cluster kontxt JWKS endpoint"
  kubectl create configmap "${kontxt_jwks_name}" \
    -n default \
    --from-file=jwks.json="${kontxt_jwks_file}" \
    --dry-run=client \
    -o yaml | kubectl apply -f -

  kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${kontxt_jwks_name}
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${kontxt_jwks_name}
  template:
    metadata:
      labels:
        app: ${kontxt_jwks_name}
    spec:
      containers:
        - name: jwks
          image: busybox:1.36
          imagePullPolicy: IfNotPresent
          command: ["/bin/sh", "-c"]
          args: ["httpd -f -p ${kontxt_jwks_port} -h /www"]
          ports:
            - containerPort: ${kontxt_jwks_port}
          volumeMounts:
            - name: jwks
              mountPath: /www
              readOnly: true
      volumes:
        - name: jwks
          configMap:
            name: ${kontxt_jwks_name}
            items:
              - key: jwks.json
                path: .well-known/jwks.json
YAML

  kubectl apply -f - <<YAML
apiVersion: v1
kind: Service
metadata:
  name: ${kontxt_jwks_name}
  namespace: default
spec:
  selector:
    app: ${kontxt_jwks_name}
  ports:
    - port: ${kontxt_jwks_port}
      targetPort: ${kontxt_jwks_port}
YAML

  kubectl rollout status deployment/"${kontxt_jwks_name}" -n default --timeout=3m
}

deploy_live_kontxt_tts() {
  log "Deploying real kontxt TTS for live exchange/replacement"
  kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${kontxt_tts_name}
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${kontxt_tts_name}
  template:
    metadata:
      labels:
        app: ${kontxt_tts_name}
    spec:
      containers:
        - name: tts
          image: ${live_kontxt_image}
          imagePullPolicy: IfNotPresent
          args:
            - tts-server
            - --addr=:${kontxt_tts_port}
            - --issuer=${kontxt_issuer}
            - --trust-domain=${kontxt_audience}
            - --subject-issuer=${github_oidc_issuer}
            - --subject-audience=${github_oidc_audience}
            - --replacement-jwks-url=${kontxt_tts_jwks_url}
            - --token-lifetime=15m
          ports:
            - containerPort: ${kontxt_tts_port}
YAML

  kubectl apply -f - <<YAML
apiVersion: v1
kind: Service
metadata:
  name: ${kontxt_tts_name}
  namespace: default
spec:
  selector:
    app: ${kontxt_tts_name}
  ports:
    - port: ${kontxt_tts_port}
      targetPort: ${kontxt_tts_port}
YAML

  kubectl rollout status deployment/"${kontxt_tts_name}" -n default --timeout=5m
}

deploy_kontxt_downstream_verifier() {
  local expected_txn="$1"

  log "Deploying downstream mock verifier for child TxTokens"
  kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${kontxt_downstream_name}
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${kontxt_downstream_name}
  template:
    metadata:
      labels:
        app: ${kontxt_downstream_name}
    spec:
      containers:
        - name: verifier
          image: ${live_kontxt_image}
          imagePullPolicy: IfNotPresent
          args:
            - downstream-verifier
            - --addr=:${kontxt_downstream_port}
            - --jwks-url=${kontxt_tts_jwks_url}
            - --audience=${kontxt_audience}
            - --expect-txn=${expected_txn}
            - --expect-scope=${kontxt_tts_child_scope}
          ports:
            - containerPort: ${kontxt_downstream_port}
YAML

  kubectl apply -f - <<YAML
apiVersion: v1
kind: Service
metadata:
  name: ${kontxt_downstream_name}
  namespace: default
spec:
  selector:
    app: ${kontxt_downstream_name}
  ports:
    - port: ${kontxt_downstream_port}
      targetPort: ${kontxt_downstream_port}
YAML

  kubectl rollout status deployment/"${kontxt_downstream_name}" -n default --timeout=5m
}

exchange_github_oidc_for_kontxt_tts_token() {
  local response_file claims_file status request_details request_context tts_local_base
  response_file="${work_dir}/kontxt-tts-parent-response.json"
  claims_file="${work_dir}/kontxt-tts-parent-claims.json"
  tts_local_base="http://127.0.0.1:${kontxt_tts_local_port}"

  request_details="$(jq -cn \
    --arg coordinator "${kontxt_tts_coordinator_agent}" \
    --arg worker "${kontxt_tts_worker_agent}" \
    '{e2e:"live-tts",namespace:"default",taskType:"ai",agent:$coordinator,allowedAgents:[$coordinator,$worker],maxDepth:2}')"
  request_context="$(jq -cn '{ci:"github-actions",path:"live-tts"}')"

  log "Exchanging GitHub OIDC token for parent TxToken through real kontxt TTS"
  status="$(curl -sS --connect-timeout 10 --max-time 60 \
    -o "${response_file}" \
    -w '%{http_code}' \
    -X POST "${tts_local_base}/token_endpoint" \
    -H "X-Kontxt-Workload: ${kontxt_requesting_workload}" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
    --data-urlencode "subject_token=${github_oidc_token}" \
    --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:id_token' \
    --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:txn_token' \
    --data-urlencode "scope=${kontxt_tts_parent_scope}" \
    --data-urlencode "request_details=${request_details}" \
    --data-urlencode "request_context=${request_context}")"
  expect_http_status "${status}" "200" "${response_file}" "kontxt TTS parent token exchange"

  kontxt_tts_parent_token="$(jq -er '.access_token // empty' "${response_file}")" || die "kontxt TTS response did not contain access_token"
  [[ -n "${kontxt_tts_parent_token}" ]] || die "kontxt TTS returned an empty access_token"
  printf '%s' "${kontxt_tts_parent_token}" >"${kontxt_tts_parent_token_file}"

  go run ./scripts/live-kontxt-e2e verify-token \
    --token-file "${kontxt_tts_parent_token_file}" \
    --jwks-url "${tts_local_base}/.well-known/jwks.json" \
    --audience "${kontxt_audience}" >"${claims_file}"
}

create_live_tts_agents() {
  log "Creating live TTS provider and coordination agents"
  kubectl create secret generic live-tts-provider-secret \
    -n default \
    --from-literal=api-key=dummy-live-tts-key \
    --dry-run=client \
    -o yaml | kubectl apply -f -

  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: live-tts-provider
  namespace: default
spec:
  type: openai
  baseURL: http://127.0.0.1:9/v1
  defaultModel: live-tts-model
  secretRef:
    name: live-tts-provider-secret
    key: api-key
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${kontxt_tts_worker_agent}
  namespace: default
spec:
  providerRef:
    name: live-tts-provider
  model:
    name: live-tts-model
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${kontxt_tts_coordinator_agent}
  namespace: default
spec:
  providerRef:
    name: live-tts-provider
  model:
    name: live-tts-model
  coordination:
    enabled: true
    maxDepth: 2
    maxConcurrentChildren: 2
    allowedAgents:
      - name: ${kontxt_tts_worker_agent}
YAML
}

configure_orka_for_live_tts() {
  log "Reconfiguring Orka for real kontxt TTS-backed TxTokens"
  run kubectl -n "${orka_namespace}" set env deployment/"${orka_controller_deployment}" \
    ORKA_CONTEXT_TOKEN_JWKS_URL="${kontxt_tts_jwks_url}" \
    ORKA_CONTEXT_TOKEN_TTS_URL="${kontxt_tts_url}" \
    ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE=incoming \
    ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE=urn:ietf:params:oauth:token-type:txn_token \
    ORKA_CONTEXT_TOKEN_CHILD_SCOPE="${kontxt_tts_child_scope}" \
    ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE="${kontxt_tts_tool_scope}" \
    ORKA_CONTEXT_TOKEN_TASK_CREATE_SCOPES=write
  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m
}

create_tts_parent_task() {
  local api_base="$1"
  local expected_txn="$2"
  local payload response status

  kontxt_tts_parent_task_name="kontxt-tts-parent-$(date +%s)-${RANDOM}"
  payload="${work_dir}/create-kontxt-tts-parent-task.json"
  response="${work_dir}/create-kontxt-tts-parent-task-response.json"

  jq -n --arg name "${kontxt_tts_parent_task_name}" --arg agent "${kontxt_tts_coordinator_agent}" '{
    name: $name,
    namespace: "default",
    type: "ai",
    agentRef: {name: $agent},
    schedule: "0 0 1 1 *",
    ai: {prompt: "live kontxt TTS parent task"}
  }' >"${payload}"

  status="$(api_request POST "${api_base}/api/v1/tasks" "${response}" \
    -H "Txn-Token: ${kontxt_tts_parent_token}" \
    -H 'Content-Type: application/json' \
    --data @"${payload}")"
  expect_http_status "${status}" "201" "${response}" "kontxt TTS parent task creation"

  jq -e --arg issuer "${kontxt_issuer}" --arg txn "${expected_txn}" --arg scope "${kontxt_tts_parent_scope}" '
    .spec.transaction.profile == "kontxt"
    and .spec.transaction.issuer == $issuer
    and .spec.transaction.id == $txn
    and .spec.transaction.scope == $scope
    and (.spec.transaction.context.e2e == "live-tts")
    and (.spec.transaction.context.namespace == "default")
    and (.spec.transaction.context.taskType == "ai")
    and .metadata.labels["orka.ai/transaction-id"] == .spec.transaction.id
    and .metadata.annotations["orka.ai/transaction-id"] == .spec.transaction.id
  ' "${response}" >/dev/null || {
    {
      echo "created kontxt TTS parent Task response did not contain expected transaction metadata"
      cat "${response}"
    } | redact >&2
    die "missing or invalid kontxt TTS parent transaction metadata"
  }
}

verify_tctx_mismatch_rejected() {
  local api_base="$1"
  local mismatch_payload mismatch_response mismatch_status
  mismatch_payload="${work_dir}/kontxt-tts-tctx-mismatch.json"
  mismatch_response="${work_dir}/kontxt-tts-tctx-mismatch-response.json"
  jq -n --arg name "${kontxt_tts_parent_task_name}-mismatch" '{
    name: $name,
    namespace: "default",
    type: "container",
    image: "busybox:1.36",
    command: ["/bin/sh", "-c"],
    args: ["echo should-not-run"]
  }' >"${mismatch_payload}"

  mismatch_status="$(api_request POST "${api_base}/api/v1/tasks" "${mismatch_response}" \
    -H "Txn-Token: ${kontxt_tts_parent_token}" \
    -H 'Content-Type: application/json' \
    --data @"${mismatch_payload}")"
  expect_http_status "${mismatch_status}" "403" "${mismatch_response}" "kontxt TTS tctx mismatch task creation"
}

delegate_live_tts_child() {
  local delegate_result_file delegate_err_file secret_name child_json claims_file
  delegate_result_file="${work_dir}/kontxt-tts-delegate-result.json"
  delegate_err_file="${work_dir}/kontxt-tts-delegate.err"
  child_json="${work_dir}/kontxt-tts-child-task.json"
  claims_file="${work_dir}/kontxt-tts-child-claims.json"

  log "Delegating child Task with real kontxt TTS child-token replacement"
  if ! ORKA_CONTEXT_TOKEN_TTS_URL="http://127.0.0.1:${kontxt_tts_local_port}" \
    ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE=incoming \
    ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE="${kontxt_tts_parent_token_file}" \
    ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE=urn:ietf:params:oauth:token-type:txn_token \
    ORKA_CONTEXT_TOKEN_CHILD_SCOPE="${kontxt_tts_child_scope}" \
    go run ./scripts/live-kontxt-e2e delegate-child \
      --parent "${kontxt_tts_parent_task_name}" \
      --namespace default \
      --agent "${kontxt_tts_worker_agent}" \
      --prompt "live TTS child delegation" \
      >"${delegate_result_file}" 2>"${delegate_err_file}"; then
    {
      echo "live TTS delegate-child failed"
      cat "${delegate_err_file}" 2>/dev/null || true
      cat "${delegate_result_file}" 2>/dev/null || true
    } | redact >&2
    die "live TTS child delegation failed"
  fi

  kontxt_tts_child_task_name="$(jq -er '.taskName // empty' "${delegate_result_file}")" || die "delegate-child did not return taskName"
  [[ -n "${kontxt_tts_child_task_name}" ]] || die "delegate-child returned empty taskName"

  kubectl get task "${kontxt_tts_child_task_name}" -n default -o json >"${child_json}"
  secret_name="$(jq -er '.metadata.annotations["orka.ai/transaction-token-secret"] // empty' "${child_json}")" || die "child Task did not reference a transaction-token Secret"
  jq -e --arg parent "${kontxt_tts_parent_task_name}" --arg worker "${kontxt_tts_worker_agent}" --arg txn "$(jq -er '.spec.transaction.id' "${work_dir}/create-kontxt-tts-parent-task-response.json")" '
    .metadata.annotations["orka.ai/parent-task-name"] == $parent
    and .metadata.labels["orka.ai/delegated-agent"] == $worker
    and .spec.transaction.id == $txn
    and .metadata.labels["orka.ai/transaction-id"] == .spec.transaction.id
  ' "${child_json}" >/dev/null || {
    {
      echo "child Task did not preserve expected transaction metadata"
      cat "${child_json}"
    } | redact >&2
    die "invalid child transaction metadata"
  }

  kubectl get secret "${secret_name}" -n default -o jsonpath='{.data.token}' | base64 --decode >"${kontxt_child_token_file}"
  kontxt_child_token="$(<"${kontxt_child_token_file}")"
  [[ -n "${kontxt_child_token}" ]] || die "child transaction-token Secret contained an empty token"

  go run ./scripts/live-kontxt-e2e verify-token \
    --token-file "${kontxt_child_token_file}" \
    --jwks-url "http://127.0.0.1:${kontxt_tts_local_port}/.well-known/jwks.json" \
    --audience "${kontxt_audience}" \
    --expect-txn "$(jq -er '.spec.transaction.id' "${work_dir}/create-kontxt-tts-parent-task-response.json")" \
    --expect-scope "${kontxt_tts_child_scope}" >"${claims_file}"
}

verify_downstream_accepts_child_token() {
  local response_file
  response_file="${work_dir}/kontxt-downstream-response.json"
  log "Calling downstream mock verifier with child TxToken"
  curl -fsS --connect-timeout 10 --max-time 60 \
    -X POST "http://127.0.0.1:${kontxt_downstream_local_port}/verify" \
    -H "Txn-Token: ${kontxt_child_token}" \
    -o "${response_file}"
  jq -e --arg txn "$(jq -er '.spec.transaction.id' "${work_dir}/create-kontxt-tts-parent-task-response.json")" --arg scope "${kontxt_tts_child_scope}" '
    .accepted == true and .txn == $txn and .scope == $scope
  ' "${response_file}" >/dev/null
}

verify_scope_broadening_rejected() {
  local response_file status broad_delegate_out broad_delegate_err
  response_file="${work_dir}/kontxt-tts-broad-scope-response.json"
  broad_delegate_out="${work_dir}/kontxt-tts-broad-delegate.out"
  broad_delegate_err="${work_dir}/kontxt-tts-broad-delegate.err"

  log "Verifying real kontxt TTS rejects child scope broadening"
  status="$(curl -sS --connect-timeout 10 --max-time 60 \
    -o "${response_file}" \
    -w '%{http_code}' \
    -X POST "http://127.0.0.1:${kontxt_tts_local_port}/token_endpoint" \
    -H "X-Kontxt-Workload: ${kontxt_requesting_workload}" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
    --data-urlencode "subject_token=${kontxt_tts_parent_token}" \
    --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:txn_token' \
    --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:txn_token' \
    --data-urlencode 'scope=orka:admin')"
  expect_http_status "${status}" "403" "${response_file}" "kontxt TTS broad scope exchange"

  log "Verifying Orka child-token helper rejects scope broadening before child creation"
  if ORKA_CONTEXT_TOKEN_TTS_URL="http://127.0.0.1:${kontxt_tts_local_port}" \
    ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE=incoming \
    ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE="${kontxt_tts_parent_token_file}" \
    ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE=urn:ietf:params:oauth:token-type:txn_token \
    ORKA_CONTEXT_TOKEN_CHILD_SCOPE=orka:admin \
    go run ./scripts/live-kontxt-e2e delegate-child \
      --parent "${kontxt_tts_parent_task_name}" \
      --namespace default \
      --agent "${kontxt_tts_worker_agent}" \
      --prompt "should fail broad scope" \
      >"${broad_delegate_out}" 2>"${broad_delegate_err}"; then
    {
      echo "scope-broadening delegate unexpectedly succeeded"
      cat "${broad_delegate_out}" 2>/dev/null || true
    } | redact >&2
    die "scope-broadening delegate unexpectedly succeeded"
  fi
  if ! grep -q "not present in parent" "${broad_delegate_err}"; then
    {
      echo "scope-broadening delegate failed for an unexpected reason"
      cat "${broad_delegate_err}" 2>/dev/null || true
    } | redact >&2
    die "scope-broadening delegate failure reason was unexpected"
  fi
}

verify_live_tokens_absent_from_logs() {
  local logs_file
  logs_file="${work_dir}/kontxt-live-logs.txt"
  {
    kubectl logs deployment/"${orka_controller_deployment}" -n "${orka_namespace}" -c manager --tail=500 2>/dev/null || true
    kubectl logs deployment/"${kontxt_tts_name}" -n default --tail=500 2>/dev/null || true
    kubectl logs deployment/"${kontxt_downstream_name}" -n default --tail=500 2>/dev/null || true
    for task in "${task_name}" "${kontxt_task_name}" "${kontxt_tts_child_task_name}"; do
      if [[ -n "${task}" ]]; then
        kubectl logs -n default -l "orka.ai/task=${task}" --all-containers --tail=500 --prefix 2>/dev/null || true
      fi
    done
  } >"${logs_file}"

  for token in "${github_oidc_token}" "${kontxt_token}" "${kontxt_tts_parent_token}" "${kontxt_child_token}"; do
    if [[ -n "${token}" ]] && grep -Fq "${token}" "${logs_file}"; then
      {
        echo "raw token appeared in live kontxt logs"
        grep -Fn "${token}" "${logs_file}" || true
      } | redact >&2
      die "raw token leaked to logs"
    fi
  done
}

fetch_github_oidc_token() {
  if [[ -n "${github_oidc_token}" ]]; then
    log "Using GitHub OIDC token from ORKA_GITHUB_OIDC_TOKEN"
    return 0
  fi

  [[ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]] || die "ACTIONS_ID_TOKEN_REQUEST_TOKEN is required unless ORKA_GITHUB_OIDC_TOKEN is set"
  [[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]] || die "ACTIONS_ID_TOKEN_REQUEST_URL is required unless ORKA_GITHUB_OIDC_TOKEN is set"

  local encoded_audience
  local separator
  local response_file

  encoded_audience="$(jq -rn --arg v "${github_oidc_audience}" '$v|@uri')"
  separator="&"
  if [[ "${ACTIONS_ID_TOKEN_REQUEST_URL}" != *\?* ]]; then
    separator="?"
  fi
  response_file="${work_dir}/github-oidc-token-response.json"

  log "Fetching GitHub Actions OIDC token"
  if ! curl -fsS \
    -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
    "${ACTIONS_ID_TOKEN_REQUEST_URL}${separator}audience=${encoded_audience}" \
    >"${response_file}"; then
    {
      echo "GitHub Actions OIDC token request failed"
      cat "${response_file}" 2>/dev/null || true
    } | redact >&2
    die "failed to fetch GitHub Actions OIDC token"
  fi

  github_oidc_token="$(jq -er '.value // empty' "${response_file}")" || die "GitHub Actions OIDC token response did not contain .value"
  [[ -n "${github_oidc_token}" ]] || die "GitHub Actions OIDC token response contained an empty .value"
  rm -f "${response_file}"
}

api_request() {
  local method="$1"
  local url="$2"
  local output_file="$3"
  shift 3

  local output_basename
  local err_file
  local status

  output_basename="$(basename "${output_file}")"
  err_file="${work_dir}/curl-${method}-${output_basename}.err"
  if ! status="$(curl -sS --connect-timeout 10 --max-time 60 \
    -o "${output_file}" \
    -w '%{http_code}' \
    -X "${method}" \
    "$@" \
    "${url}" 2>"${err_file}")"; then
    {
      echo "curl ${method} ${url} failed"
      cat "${err_file}" 2>/dev/null || true
      cat "${output_file}" 2>/dev/null || true
    } | redact >&2
    return 1
  fi

  printf '%s' "${status}"
}

expect_http_status() {
  local actual="$1"
  local expected="$2"
  local response_file="$3"
  local description="$4"

  if [[ "${actual}" != "${expected}" ]]; then
    {
      echo "${description} returned HTTP ${actual}, expected ${expected}"
      echo
      cat "${response_file}" 2>/dev/null || true
    } | redact >&2
    return 1
  fi
}

main() {
  require_cmd make
  require_cmd go
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
  require_cmd curl
  require_cmd jq

  require_github_oidc_token_source

  cd "${repo_root}"
  [[ -f "${manager_kustomization}" ]] || die "missing ${manager_kustomization}"
  cp "${manager_kustomization}" "${manager_kustomization_backup}"

  trap 'status=$?; on_exit "${status}"; exit "${status}"' EXIT

  generate_kontxt_jwks_fixture

  log "Creating or reusing Kind cluster ${kind_cluster}"
  run make setup-test-e2e KIND_CLUSTER="${kind_cluster}"
  run kubectl config use-context "kind-${kind_cluster}"

  deploy_kontxt_jwks

  log "Building live kontxt helper image ${live_kontxt_image}"
  run docker build -t "${live_kontxt_image}" -f scripts/live-kontxt-e2e/Dockerfile .

  log "Loading live kontxt helper image into Kind cluster ${kind_cluster}"
  run kind load docker-image "${live_kontxt_image}" --name "${kind_cluster}"

  log "Building manager image ${manager_image}"
  run make docker-build IMG="${manager_image}"

  log "Loading manager image into Kind cluster ${kind_cluster}"
  run kind load docker-image "${manager_image}" --name "${kind_cluster}"

  log "Deploying Orka manager"
  run make deploy IMG="${manager_image}"
  run kubectl wait --for=condition=Established crd/tasks.core.orka.ai --timeout=60s

  log "Configuring local image pull policy, GitHub OIDC auth, and kontxt context-token auth"
  run kubectl -n "${orka_namespace}" patch deployment "${orka_controller_deployment}" \
    --type=strategic \
    -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"IfNotPresent"}]}}}}'
  run kubectl -n "${orka_namespace}" set env deployment/"${orka_controller_deployment}" \
    ORKA_OIDC_ISSUER="${github_oidc_issuer}" \
    ORKA_OIDC_AUDIENCE="${github_oidc_audience}" \
    ORKA_OIDC_JWKS_URL- \
    ORKA_CONTEXT_TOKEN_PROFILE=kontxt \
    ORKA_CONTEXT_TOKEN_ISSUER="${kontxt_issuer}" \
    ORKA_CONTEXT_TOKEN_AUDIENCE="${kontxt_audience}" \
    ORKA_CONTEXT_TOKEN_JWKS_URL="${kontxt_jwks_url}" \
    ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce \
    ORKA_CONTEXT_TOKEN_TASK_CREATE_SCOPES=write \
    ORKA_CONTEXT_TOKEN_HEADERS-
  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m

  log "Port-forwarding Orka API service"
  api_pf_pid="$(start_api_port_forward)"

  local api_base
  api_base="http://127.0.0.1:${orka_api_local_port}"
  wait_for_http "${api_base}/readyz" "Orka API /readyz"

  log "Verifying unauthenticated API requests are rejected"
  local unauth_response unauth_status
  unauth_response="${work_dir}/unauth-response.json"
  unauth_status="$(api_request GET "${api_base}/api/v1/tasks?namespace=default" "${unauth_response}")"
  expect_http_status "${unauth_status}" "401" "${unauth_response}" "unauthenticated task list"

  deploy_live_kontxt_tts
  log "Port-forwarding kontxt TTS service"
  kontxt_tts_pf_pid="$(start_kontxt_tts_port_forward)"
  wait_for_http "http://127.0.0.1:${kontxt_tts_local_port}/healthz" "kontxt TTS /healthz"
  wait_for_http "http://127.0.0.1:${kontxt_tts_local_port}/.well-known/jwks.json" "kontxt TTS JWKS"

  fetch_github_oidc_token

  log "Creating Task with live GitHub OIDC token"
  task_name="github-oidc-$(date +%s)-${RANDOM}"
  local create_payload create_response create_status
  create_payload="${work_dir}/create-task.json"
  create_response="${work_dir}/create-task-response.json"
  jq -n --arg name "${task_name}" '{
    name: $name,
    namespace: "default",
    type: "container",
    image: "busybox:1.36",
    command: ["/bin/sh", "-c"],
    args: ["echo github-oidc"]
  }' >"${create_payload}"

  create_status="$(api_request POST "${api_base}/api/v1/tasks" "${create_response}" \
    -H "Authorization: Bearer ${github_oidc_token}" \
    -H 'Content-Type: application/json' \
    --data @"${create_payload}")"
  expect_http_status "${create_status}" "201" "${create_response}" "OIDC task creation"

  jq -e --arg issuer "${github_oidc_issuer}" '
    .spec.requestedBy.issuer == $issuer
    and ((.spec.requestedBy.subject // "") != "")
  ' "${create_response}" >/dev/null || {
    {
      echo "created Task response did not contain expected spec.requestedBy"
      cat "${create_response}"
    } | redact >&2
    die "missing or invalid spec.requestedBy in task creation response"
  }

  log "Verifying created Task persisted requestedBy identity"
  kubectl get task "${task_name}" -n default -o json >"${work_dir}/created-task-kube.json"
  jq -e --arg issuer "${github_oidc_issuer}" '
    .spec.requestedBy.issuer == $issuer
    and ((.spec.requestedBy.subject // "") != "")
  ' "${work_dir}/created-task-kube.json" >/dev/null

  log "Verifying top-level requestedBy tampering is rejected"
  local tamper_top_payload tamper_top_response tamper_top_status
  tamper_top_payload="${work_dir}/tamper-top-requested-by.json"
  tamper_top_response="${work_dir}/tamper-top-requested-by-response.json"
  jq --arg name "${task_name}-top" '.name = $name | . + {requestedBy: {issuer: "evil", subject: "evil"}}' \
    "${create_payload}" >"${tamper_top_payload}"
  tamper_top_status="$(api_request POST "${api_base}/api/v1/tasks" "${tamper_top_response}" \
    -H "Authorization: Bearer ${github_oidc_token}" \
    -H 'Content-Type: application/json' \
    --data @"${tamper_top_payload}")"
  expect_http_status "${tamper_top_status}" "400" "${tamper_top_response}" "top-level requestedBy tampering"

  log "Verifying nested spec.requestedBy tampering is rejected"
  local tamper_spec_payload tamper_spec_response tamper_spec_status
  tamper_spec_payload="${work_dir}/tamper-spec-requested-by.json"
  tamper_spec_response="${work_dir}/tamper-spec-requested-by-response.json"
  jq --arg name "${task_name}-spec" '.name = $name | . + {spec: {requestedBy: {issuer: "evil", subject: "evil"}}}' \
    "${create_payload}" >"${tamper_spec_payload}"
  tamper_spec_status="$(api_request POST "${api_base}/api/v1/tasks" "${tamper_spec_response}" \
    -H "Authorization: Bearer ${github_oidc_token}" \
    -H 'Content-Type: application/json' \
    --data @"${tamper_spec_payload}")"
  expect_http_status "${tamper_spec_status}" "400" "${tamper_spec_response}" "nested spec.requestedBy tampering"

  exchange_github_oidc_for_kontxt_tts_token
  mint_kontxt_token

  log "Creating Task with live kontxt TxToken"
  kontxt_task_name="kontxt-$(date +%s)-${RANDOM}"
  local kontxt_payload kontxt_response kontxt_status
  kontxt_payload="${work_dir}/create-kontxt-task.json"
  kontxt_response="${work_dir}/create-kontxt-task-response.json"
  jq -n --arg name "${kontxt_task_name}" '{
    name: $name,
    namespace: "default",
    type: "container",
    image: "busybox:1.36",
    command: ["/bin/sh", "-c"],
    args: ["echo kontxt"]
  }' >"${kontxt_payload}"

  kontxt_status="$(api_request POST "${api_base}/api/v1/tasks" "${kontxt_response}" \
    -H "Txn-Token: ${kontxt_token}" \
    -H 'Content-Type: application/json' \
    --data @"${kontxt_payload}")"
  expect_http_status "${kontxt_status}" "201" "${kontxt_response}" "kontxt task creation"

  jq -e --arg issuer "${kontxt_issuer}" --arg subject "${kontxt_subject}" --arg req_wl "${kontxt_requesting_workload}" '
    .spec.requestedBy.issuer == $issuer
    and .spec.requestedBy.subject == $subject
    and .spec.requestedBy.username == $subject
    and (.spec.requestedBy.roles == ["read", "write"])
    and .spec.transaction.profile == "kontxt"
    and ((.spec.transaction.id // "") != "")
    and .spec.transaction.issuer == $issuer
    and .spec.transaction.subject == $subject
    and .spec.transaction.requestingWorkload == $req_wl
    and .spec.transaction.scope == "read write"
    and (.spec.transaction.scopes == ["read", "write"])
    and (.spec.transaction.context.e2e == "live-kind")
    and ((.spec.transaction.contextDigest // "") | startswith("sha256:"))
    and ((.spec.transaction.requesterContextDigest // "") | startswith("sha256:"))
    and .metadata.labels["orka.ai/transaction-id"] == .spec.transaction.id
    and .metadata.annotations["orka.ai/transaction-id"] == .spec.transaction.id
  ' "${kontxt_response}" >/dev/null || {
    {
      echo "created kontxt Task response did not contain expected spec.requestedBy/spec.transaction"
      cat "${kontxt_response}"
    } | redact >&2
    die "missing or invalid spec.requestedBy/spec.transaction in kontxt task creation response"
  }

  log "Verifying kontxt Task persisted requestedBy and transaction metadata"
  kubectl get task "${kontxt_task_name}" -n default -o json >"${work_dir}/created-kontxt-task-kube.json"
  jq -e --arg issuer "${kontxt_issuer}" --arg subject "${kontxt_subject}" --arg req_wl "${kontxt_requesting_workload}" '
    .spec.requestedBy.issuer == $issuer
    and .spec.requestedBy.subject == $subject
    and .spec.requestedBy.username == $subject
    and (.spec.requestedBy.roles == ["read", "write"])
    and .spec.transaction.profile == "kontxt"
    and ((.spec.transaction.id // "") != "")
    and .spec.transaction.issuer == $issuer
    and .spec.transaction.subject == $subject
    and .spec.transaction.requestingWorkload == $req_wl
    and .spec.transaction.scope == "read write"
    and (.spec.transaction.scopes == ["read", "write"])
    and (.spec.transaction.context.e2e == "live-kind")
    and ((.spec.transaction.contextDigest // "") | startswith("sha256:"))
    and ((.spec.transaction.requesterContextDigest // "") | startswith("sha256:"))
    and .metadata.labels["orka.ai/transaction-id"] == .spec.transaction.id
    and .metadata.annotations["orka.ai/transaction-id"] == .spec.transaction.id
  ' "${work_dir}/created-kontxt-task-kube.json" >/dev/null

  local kontxt_transaction_id
  kontxt_transaction_id="$(jq -er '.spec.transaction.id' "${work_dir}/created-kontxt-task-kube.json")"

  log "Verifying kontxt Job carries transaction metadata"
  local kontxt_job_json
  kontxt_job_json="${work_dir}/created-kontxt-job.json"
  wait_for_labeled_resource jobs default "orka.ai/task=${kontxt_task_name}" "${kontxt_job_json}" "kontxt Job"
  jq -e --arg txn "${kontxt_transaction_id}" '
    .items[0].metadata.labels["orka.ai/transaction-id"] == $txn
    and .items[0].metadata.annotations["orka.ai/transaction-id"] == $txn
    and .items[0].spec.template.metadata.labels["orka.ai/transaction-id"] == $txn
    and .items[0].spec.template.metadata.annotations["orka.ai/transaction-id"] == $txn
  ' "${kontxt_job_json}" >/dev/null || {
    {
      echo "kontxt Job did not carry expected transaction metadata"
      cat "${kontxt_job_json}"
    } | redact >&2
    die "missing transaction metadata on kontxt Job"
  }

  log "Verifying kontxt Pod carries transaction metadata"
  local kontxt_pod_json
  kontxt_pod_json="${work_dir}/created-kontxt-pod.json"
  wait_for_labeled_resource pods default "orka.ai/task=${kontxt_task_name}" "${kontxt_pod_json}" "kontxt Pod"
  jq -e --arg txn "${kontxt_transaction_id}" '
    .items[0].metadata.labels["orka.ai/transaction-id"] == $txn
    and .items[0].metadata.annotations["orka.ai/transaction-id"] == $txn
  ' "${kontxt_pod_json}" >/dev/null || {
    {
      echo "kontxt Pod did not carry expected transaction metadata"
      cat "${kontxt_pod_json}"
    } | redact >&2
    die "missing transaction metadata on kontxt Pod"
  }

  log "Verifying tampered kontxt TxToken is rejected"
  local tampered_kontxt_token tampered_kontxt_payload tampered_kontxt_response tampered_kontxt_status
  tampered_kontxt_token="${kontxt_token}tampered"
  tampered_kontxt_payload="${work_dir}/tampered-kontxt-task.json"
  tampered_kontxt_response="${work_dir}/tampered-kontxt-task-response.json"
  jq --arg name "${kontxt_task_name}-tampered" '.name = $name' "${kontxt_payload}" >"${tampered_kontxt_payload}"
  tampered_kontxt_status="$(api_request POST "${api_base}/api/v1/tasks" "${tampered_kontxt_response}" \
    -H "Txn-Token: ${tampered_kontxt_token}" \
    -H 'Content-Type: application/json' \
    --data @"${tampered_kontxt_payload}")"
  expect_http_status "${tampered_kontxt_status}" "401" "${tampered_kontxt_response}" "tampered kontxt task creation"

  create_live_tts_agents

  local kontxt_tts_transaction_id
  kontxt_tts_transaction_id="$(jq -er '.txn' "${work_dir}/kontxt-tts-parent-claims.json")"

  configure_orka_for_live_tts
  wait_for_http "${api_base}/readyz" "Orka API /readyz after live TTS reconfiguration"

  create_tts_parent_task "${api_base}" "${kontxt_tts_transaction_id}"
  verify_tctx_mismatch_rejected "${api_base}"
  delegate_live_tts_child

  deploy_kontxt_downstream_verifier "${kontxt_tts_transaction_id}"
  log "Port-forwarding kontxt downstream verifier service"
  kontxt_downstream_pf_pid="$(start_kontxt_downstream_port_forward)"
  wait_for_http "http://127.0.0.1:${kontxt_downstream_local_port}/healthz" "kontxt downstream verifier /healthz"
  verify_downstream_accepts_child_token
  verify_scope_broadening_rejected
  verify_live_tokens_absent_from_logs

  log "Live GitHub OIDC, kontxt, and TTS-backed delegation e2e passed"
}

main "$@"
