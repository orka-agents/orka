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
kontxt_token=""
api_pf_pid=""
task_name=""
kontxt_task_name=""
work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/live-github-oidc-e2e.XXXXXX")"
api_pf_log="${work_dir}/api-port-forward.log"
kontxt_jwks_file="${work_dir}/kontxt-jwks.json"
kontxt_token_file="${work_dir}/kontxt-token.txt"
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
  printf '%s' "${text}" | sed -E \
    -e 's/(Authorization: *([Bb]earer|token) +)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/(ACTIONS_ID_TOKEN_REQUEST_TOKEN=)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/(ORKA_GITHUB_OIDC_TOKEN=)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/[REDACTED_JWT]/g'
}

cleanup_port_forward() {
  if [[ -n "${api_pf_pid}" ]]; then
    if kill -0 "${api_pf_pid}" 2>/dev/null; then
      kill "${api_pf_pid}" 2>/dev/null || true
    fi
    wait "${api_pf_pid}" 2>/dev/null || true
    api_pf_pid=""
  fi
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
    kubectl get pods,svc,deploy,tasks -n default -o wide 2>/dev/null || true
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
    echo "=== API Port-forward Log ==="
    if [[ -f "${api_pf_log}" ]]; then
      cat "${api_pf_log}" 2>/dev/null || true
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
  kubectl delete deployment "${kontxt_jwks_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  kubectl delete service "${kontxt_jwks_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
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
    if curl -fsS "${url}" >/dev/null 2>&1; then
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

generate_kontxt_fixture() {
  local generator
  generator="${work_dir}/generate-kontxt-fixture.go"

  cat >"${generator}" <<'GO'
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
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
	jwksPath := mustEnv("KONTXT_JWKS_FILE")
	tokenPath := mustEnv("KONTXT_TOKEN_FILE")
	issuer := mustEnv("KONTXT_ISSUER")
	audience := mustEnv("KONTXT_AUDIENCE")
	subject := mustEnv("KONTXT_SUBJECT")
	requestingWorkload := mustEnv("KONTXT_REQUESTING_WORKLOAD")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	kid := fmt.Sprintf("kontxt-live-%d", time.Now().UnixNano())

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
	}, key, kid, 15*time.Minute)
	if err != nil {
		panic(err)
	}

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
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		panic(err)
	}
}
GO

  log "Generating kontxt TxToken and JWKS fixture"
  KONTXT_JWKS_FILE="${kontxt_jwks_file}" \
    KONTXT_TOKEN_FILE="${kontxt_token_file}" \
    KONTXT_ISSUER="${kontxt_issuer}" \
    KONTXT_AUDIENCE="${kontxt_audience}" \
    KONTXT_SUBJECT="${kontxt_subject}" \
    KONTXT_REQUESTING_WORKLOAD="${kontxt_requesting_workload}" \
    go run "${generator}"
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

  generate_kontxt_fixture

  log "Creating or reusing Kind cluster ${kind_cluster}"
  run make setup-test-e2e KIND_CLUSTER="${kind_cluster}"
  run kubectl config use-context "kind-${kind_cluster}"

  deploy_kontxt_jwks

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

  log "Live GitHub OIDC and kontxt e2e passed"
}

main "$@"
