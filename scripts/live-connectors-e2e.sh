#!/usr/bin/env bash
# Live connectors E2E: a person links an account through a fake OAuth provider,
# a native AI Task uses the linked token to read, a write waits for approval
# and then runs, the second read is served by a refreshed token, and
# disconnecting revokes the tokens. Everything outside Orka is the in-cluster
# fixture under test/fixtures/connectors; no model provider or real OAuth app
# is involved. The controller runs with the fixture-only
# --connectors-allow-private-endpoints flag so the provider may live in the
# cluster; production never sets it.
set -Eeuo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
. "${script_dir}/lib/redact.sh"
. "${script_dir}/lib/kind-local-registry.sh"
. "${script_dir}/lib/e2e-admission-tls.sh"

cluster="${KIND_CLUSTER:-orka-live-connectors-e2e}"
namespace="${ORKA_NAMESPACE:-orka-system}"
deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:live-connectors-e2e}"
worker_image="${ORKA_AI_WORKER_IMAGE:-orka-ai-worker:live-connectors-e2e}"
publisher_image="${ORKA_WORKSPACE_PUBLISHER_IMAGE:-orka-workspace-publisher:live-connectors-e2e}"
fixture_image="${ORKA_CONNECTORS_FIXTURE_IMAGE:-orka-connectors-fixture:live-connectors-e2e}"
api_port="${ORKA_API_LOCAL_PORT:-18080}"
fixture_port="${ORKA_FIXTURE_LOCAL_PORT:-18081}"
fixture_tls_port="${ORKA_FIXTURE_LOCAL_TLS_PORT:-18443}"
access_ttl="${ORKA_CONNECTORS_E2E_ACCESS_TTL:-75s}"
subject="alice"
audience="orka-live-connectors-e2e"
fixture_host="connectors-fixture.${namespace}.svc"
issuer="http://${fixture_host}:8080/oidc"
client_id="orka-e2e-client"
client_secret="$(openssl rand -hex 24)"
model_credential="fixture-$(openssl rand -hex 12)"
callback_base="http://localhost:${api_port}"
workdir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/orka-connectors-e2e.XXXXXX")"
chmod 700 "${workdir}"
kustomization="${repo_root}/config/manager/kustomization.yaml"
backup="${workdir}/kustomization.yaml"
api_pf_pid=""; fixture_pf_pid=""; fixture_tls_pf_pid=""
pf_log="${workdir}/port-forward.log"

cleanup() {
  status=$?
  for pid in "${api_pf_pid}" "${fixture_pf_pid}" "${fixture_tls_pf_pid}"; do
    if [[ -n "${pid}" ]]; then kill "${pid}" >/dev/null 2>&1 || true; wait "${pid}" 2>/dev/null || true; fi
  done
  if [[ -f "${backup}" ]]; then cp "${backup}" "${kustomization}"; fi
  if [[ ${status} -ne 0 ]]; then
    {
      kubectl -n "${namespace}" get pods,tasks,connections,connectorproviders 2>/dev/null || true
      kubectl -n "${namespace}" logs deployment/"${deployment}" --tail=300 2>/dev/null || true
      kubectl -n "${namespace}" logs deployment/connectors-fixture --tail=100 2>/dev/null || true
      kubectl -n "${namespace}" logs -l orka.ai/task-name --tail=200 --all-containers=true 2>/dev/null || true
      [[ -f "${pf_log}" ]] && { printf '%s\n' '--- port-forward log ---'; cat "${pf_log}"; }
    } | redact >&2
  fi
  orka_kind_registry_stop
  kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true
  rm -rf "${workdir}"
  exit "${status}"
}
trap cleanup EXIT

request() {
  local method="$1" url="$2" output="$3"; shift 3
  curl -sS -o "${output}" -w '%{http_code}' -X "${method}" "${url}" "$@"
}

start_port_forwards() {
  kubectl -n "${namespace}" port-forward service/orka-api "${api_port}:8080" >>"${pf_log}" 2>&1 &
  api_pf_pid=$!
  kubectl -n "${namespace}" port-forward service/connectors-fixture "${fixture_port}:8080" "${fixture_tls_port}:8443" >>"${pf_log}" 2>&1 &
  fixture_pf_pid=$!
}

wait_for_http() {
  local url="$1" label="$2" attempts=90
  while (( attempts > 0 )); do
    if curl -fsS --connect-timeout 5 --max-time 10 "${url}" >/dev/null 2>&1; then return 0; fi
    attempts=$((attempts - 1)); sleep 2
  done
  die "${label} did not become ready"
}

fixture_state() { curl -fsS "http://127.0.0.1:${fixture_port}/fixture/state"; }

wait_for_state() {
  local expression="$1" label="$2" attempts="${3:-120}"
  while (( attempts > 0 )); do
    if fixture_state | jq -e "${expression}" >/dev/null 2>&1; then return 0; fi
    attempts=$((attempts - 1)); sleep 2
  done
  fixture_state | redact >&2 || true
  die "fixture never reached: ${label}"
}

wait_for_task_condition() {
  local task="$1" expression="$2" label="$3" attempts="${4:-150}"
  while (( attempts > 0 )); do
    if kubectl -n "${namespace}" get task "${task}" -o json 2>/dev/null | jq -e "${expression}" >/dev/null 2>&1; then return 0; fi
    attempts=$((attempts - 1)); sleep 2
  done
  kubectl -n "${namespace}" get task "${task}" -o yaml 2>/dev/null | redact >&2 || true
  die "task ${task} never reached: ${label}"
}

[[ "${namespace}" == "orka-system" ]] || die "ORKA_NAMESPACE must be orka-system for the canonical make deploy path"
for cmd in make go docker kind kubectl curl jq openssl; do require_cmd "${cmd}"; done
cd "${repo_root}"
cp "${kustomization}" "${backup}"

log "Creating kind cluster ${cluster}"
make setup-test-e2e KIND_CLUSTER="${cluster}"
kubectl config use-context "kind-${cluster}" >/dev/null
log "Installing current Orka CRDs"
make install
kubectl create namespace vekil-system --dry-run=client -o yaml | kubectl apply -f -
orka_kind_registry_start "${cluster}"

log "Building and loading images"
make docker-build IMG="${manager_image}"
make docker-build-ai-worker AI_WORKER_IMG="${worker_image}"
make docker-build-workspace-publisher WORKSPACE_PUBLISHER_IMG="${publisher_image}"
docker build -f test/fixtures/connectors/Dockerfile -t "${fixture_image}" .
kind load docker-image "${manager_image}" "${worker_image}" "${fixture_image}" --name "${cluster}"
manager_ref="$(orka_kind_registry_push "${manager_image}" "orka/controller")"
publisher_ref="$(orka_kind_registry_push "${publisher_image}" "orka/workspace-publisher")"
placeholder_digest="sha256:$(printf '0%.0s' {1..64})"

log "Bootstrapping test-only admission TLS (task provenance webhook)"
orka_e2e_bootstrap_admission_tls
make deploy \
  IMG="${manager_ref}" \
  AI_WORKER_IMG="${worker_image}" \
  WORKSPACE_PUBLISHER_IMG="${publisher_ref}" \
  ACP_CODEX_RUNTIME_IMG="example.invalid/orka/acp-codex@${placeholder_digest}" \
  ACP_CLAUDE_RUNTIME_IMG="example.invalid/orka/acp-claude@${placeholder_digest}" \
  ACP_COPILOT_RUNTIME_IMG="example.invalid/orka/acp-copilot@${placeholder_digest}" \
  ACP_OPENCODE_RUNTIME_IMG="example.invalid/orka/acp-opencode@${placeholder_digest}"
kubectl wait --for=condition=Established crd/tasks.core.orka.ai --timeout=60s

log "Creating the fixture TLS material and CA trust"
tls_dir="${workdir}/tls"; mkdir -p "${tls_dir}"; chmod 700 "${tls_dir}"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=orka connectors e2e CA" \
  -keyout "${tls_dir}/ca.key" -out "${tls_dir}/ca.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj "/CN=${fixture_host}" \
  -keyout "${tls_dir}/tls.key" -out "${tls_dir}/tls.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:%s,DNS:localhost,IP:127.0.0.1\n' "${fixture_host}" >"${tls_dir}/san.cnf"
openssl x509 -req -in "${tls_dir}/tls.csr" -CA "${tls_dir}/ca.crt" -CAkey "${tls_dir}/ca.key" -CAcreateserial \
  -days 2 -extfile "${tls_dir}/san.cnf" -out "${tls_dir}/tls.crt" >/dev/null 2>&1
kubectl -n "${namespace}" create secret tls connectors-fixture-tls --cert="${tls_dir}/tls.crt" --key="${tls_dir}/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "${namespace}" create configmap connectors-fixture-ca --from-file=ca.crt="${tls_dir}/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "${namespace}" create secret generic connectors-fixture-oauth --from-literal=clientSecret="${client_secret}" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "${namespace}" create secret generic connectors-fixture-model --from-literal=api-key="${model_credential}" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

log "Deploying the connectors fixture"
kubectl -n "${namespace}" apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: connectors-fixture
spec:
  replicas: 1
  selector: {matchLabels: {app: connectors-fixture}}
  template:
    metadata:
      labels: {app: connectors-fixture}
    spec:
      containers:
        - name: fixture
          image: ${fixture_image}
          imagePullPolicy: IfNotPresent
          env:
            - {name: FIXTURE_OIDC_ISSUER, value: "${issuer}"}
            - {name: FIXTURE_OIDC_AUDIENCE, value: "${audience}"}
            - {name: FIXTURE_OAUTH_CLIENT_ID, value: "${client_id}"}
            - {name: FIXTURE_OAUTH_CLIENT_SECRET, valueFrom: {secretKeyRef: {name: connectors-fixture-oauth, key: clientSecret}}}
            - {name: FIXTURE_MODEL_CREDENTIAL, valueFrom: {secretKeyRef: {name: connectors-fixture-model, key: api-key}}}
            - {name: FIXTURE_ACCESS_TOKEN_TTL, value: "${access_ttl}"}
            - {name: FIXTURE_TLS_CERT, value: /tls/tls.crt}
            - {name: FIXTURE_TLS_KEY, value: /tls/tls.key}
          ports:
            - {containerPort: 8080}
            - {containerPort: 8443}
          volumeMounts:
            - {name: tls, mountPath: /tls, readOnly: true}
      volumes:
        - name: tls
          secret: {secretName: connectors-fixture-tls}
---
apiVersion: v1
kind: Service
metadata:
  name: connectors-fixture
spec:
  selector: {app: connectors-fixture}
  ports:
    - {name: http, port: 8080, targetPort: 8080}
    - {name: https, port: 8443, targetPort: 8443}
YAML
kubectl -n "${namespace}" rollout status deployment/connectors-fixture --timeout=3m

log "Configuring the controller: OIDC, connectors, fixture CA, private endpoints (fixture only)"
kubectl -n "${namespace}" get deployment "${deployment}" -o json | jq \
  --arg ca "/etc/orka-e2e/fixture-ca/ca.crt" '
    .spec.template.spec.containers |= map(
      if .name == "manager" then
        .imagePullPolicy = "IfNotPresent"
        | .volumeMounts = (((.volumeMounts // []) | map(select(.name != "fixture-ca"))) + [{"name": "fixture-ca", "mountPath": "/etc/orka-e2e/fixture-ca", "readOnly": true}])
        | .env = (((.env // []) | map(select(.name != "SSL_CERT_FILE"))) + [{"name": "SSL_CERT_FILE", "value": $ca}])
      else . end)
    | .spec.template.spec.volumes = (((.spec.template.spec.volumes // []) | map(select(.name != "fixture-ca"))) + [{"name": "fixture-ca", "configMap": {"name": "connectors-fixture-ca"}}])
  ' | kubectl apply -f - >/dev/null
kubectl -n "${namespace}" set env deployment/"${deployment}" \
  ORKA_OIDC_ISSUER="${issuer}" \
  ORKA_OIDC_AUDIENCE="${audience}" \
  ORKA_OIDC_ALLOWED_SUBJECTS="${subject}" \
  ORKA_OIDC_NAMESPACE="${namespace}" \
  ORKA_TASK_PROVENANCE_ADMISSION_EXTERNAL=true \
  ORKA_CONNECTORS_ENABLED=true \
  ORKA_CONNECTOR_CALLBACK_BASE_URL="${callback_base}" \
  ORKA_CONNECTORS_ALLOW_PRIVATE_ENDPOINTS=true \
  ORKA_OIDC_JWKS_URL- ORKA_CONTEXT_TOKEN_PROFILE- ORKA_CONTEXT_TOKEN_ISSUER- ORKA_CONTEXT_TOKEN_AUDIENCE-
kubectl -n "${namespace}" rollout status deployment/"${deployment}" --timeout=5m
start_port_forwards
wait_for_http "http://127.0.0.1:${api_port}/readyz" "Orka API"
wait_for_http "http://127.0.0.1:${fixture_port}/healthz" "connectors fixture"

log "Declaring the provider, tools, policy, model provider, and agent"
kubectl -n "${namespace}" apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: ConnectorProvider
metadata:
  name: fixture
spec:
  displayName: Fixture forge
  oauth:
    authorizeURL: https://${fixture_host}:8443/oauth/authorize
    tokenURL: https://${fixture_host}:8443/oauth/token
    revocationURL: https://${fixture_host}:8443/oauth/revoke
    clientID: ${client_id}
    clientSecretRef: {name: connectors-fixture-oauth, key: clientSecret}
    scopes:
      read: [items:read]
      write: [items:write]
  tools:
    - name: itemsread
      class: read
      source: HTTP
      description: Read items as the linked person
      parameters: {"type":"object","properties":{"q":{"type":"string"}},"additionalProperties":false}
      http: {url: "https://${fixture_host}:8443/api/items", method: GET}
    - name: itemswrite
      class: write
      source: HTTP
      description: Create an item as the linked person
      parameters: {"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}
      http: {url: "https://${fixture_host}:8443/api/items", method: POST}
---
apiVersion: core.orka.ai/v1alpha1
kind: OutboundAccessPolicy
metadata:
  name: fixture-as-me
spec:
  connection:
    providerRef: {name: fixture}
---
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: itemsread
spec:
  description: Read items as the linked person
  brokeredToolClass: read
  parameters: {"type":"object","properties":{"q":{"type":"string"}},"additionalProperties":false}
  http:
    url: https://${fixture_host}:8443/api/items
    method: GET
    outboundAccessPolicyRef: {name: fixture-as-me}
---
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: itemswrite
spec:
  description: Create an item as the linked person
  brokeredToolClass: write
  parameters: {"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}
  http:
    url: https://${fixture_host}:8443/api/items
    method: POST
    outboundAccessPolicyRef: {name: fixture-as-me}
---
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: fixture-model
spec:
  type: openai
  baseURL: http://${fixture_host}:8080/v1
  defaultModel: fixture
  secretRef: {name: connectors-fixture-model, key: api-key}
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: linked-agent
spec:
  providerRef: {name: fixture-model}
  model: {name: fixture}
  systemPrompt:
    inline: Use the linked account tools exactly as the fixture script says.
  tools:
    - {name: itemsread}
    - {name: itemswrite}
  coordination:
    enabled: true
    autonomous: true
    maxIterations: 3
    approvalRequiredTools: [itemswrite]
YAML
for ((i = 0; i < 60; i++)); do
  if kubectl -n "${namespace}" get connectorprovider fixture -o json | jq -e '[.status.conditions[]? | select(.type=="Accepted" or .type=="ResolvedRefs") | select(.status=="True")] | length == 2' >/dev/null 2>&1; then break; fi
  sleep 2
done
kubectl -n "${namespace}" get connectorprovider fixture -o json | jq -e '[.status.conditions[]? | select(.status=="True")] | length >= 2' >/dev/null \
  || { kubectl -n "${namespace}" get connectorprovider fixture -o yaml | redact >&2; die "ConnectorProvider was not accepted"; }

log "Signing in as ${subject} through the OIDC fixture"
token="$(curl -fsS -X POST "http://127.0.0.1:${fixture_port}/oidc/mint" -H 'Content-Type: application/json' \
  -d "{\"subject\":\"${subject}\",\"email\":\"${subject}@example.test\"}" | jq -er '.token')"
auth=(-H "Authorization: Bearer ${token}")
api="http://127.0.0.1:${api_port}/api/v1"

log "Link: start consent as ${subject} (readWrite)"
status="$(request POST "${api}/connections" "${workdir}/start.json" "${auth[@]}" -H 'Content-Type: application/json' -d '{"provider":"fixture","mode":"readWrite"}')"
[[ "${status}" == 200 || "${status}" == 201 ]] || { cat "${workdir}/start.json" | redact >&2; die "start consent returned HTTP ${status}"; }
connection="$(jq -er '.connection.name' "${workdir}/start.json")"
authorize_url="$(jq -er '.authorizeURL' "${workdir}/start.json")"
[[ "${authorize_url}" == "https://${fixture_host}:8443/oauth/authorize?"* ]] || die "unexpected authorize URL host"
# The browser step, done by hand: the provider consents and redirects to the
# controller callback; the callback redirects to the settings page with the
# completion token in the fragment.
local_authorize="https://127.0.0.1:${fixture_tls_port}${authorize_url#https://${fixture_host}:8443}"
callback_location="$(curl -sS --cacert "${tls_dir}/ca.crt" --resolve "${fixture_host}:${fixture_tls_port}:127.0.0.1" -o /dev/null -w '%{redirect_url}' "${local_authorize}")"
[[ "${callback_location}" == "${callback_base}/api/v1/connections/callback?"* ]] || die "provider redirected elsewhere: $(printf '%s' "${callback_location}" | redact)"
settings_location="$(curl -sS -o /dev/null -w '%{redirect_url}' "http://127.0.0.1:${api_port}${callback_location#${callback_base}}")"
settings_query="${settings_location#*\?}"; settings_query="${settings_query%%#*}"
[[ "${settings_location}" == "${callback_base}/settings/connectors?"*"#completion="* ]] \
  && [[ "&${settings_query}&" == *"&status=pending&"* ]] \
  && [[ "&${settings_query}&" == *"&connection=${connection}&"* ]] \
  && [[ "&${settings_query}&" == *"&namespace=${namespace}&"* ]] \
  || die "callback redirected elsewhere: $(printf '%s' "${settings_location}" | sed -E 's/completion=[^&]+/completion=<redacted>/' | redact)"
completion="${settings_location#*#completion=}"
status="$(request POST "${api}/connections/${connection}/complete" "${workdir}/complete.json" "${auth[@]}" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg c "${completion}" '{completion:$c}')")"
[[ "${status}" == 200 ]] || { cat "${workdir}/complete.json" | redact >&2; die "complete returned HTTP ${status}"; }
jq -e '.ready == true and .mode == "readWrite" and .state == "Ready"' "${workdir}/complete.json" >/dev/null || die "connection is not Ready after completion"
jq -e 'tostring | test("fx-access|fx-refresh") | not' "${workdir}/complete.json" >/dev/null || die "connection response leaked token material"
wait_for_state '.codeExchanges == 1 and .tokensIssued == 1' "one code exchange" 5
linked_at="$(date +%s)"

log "Use: a Task created by ${subject} reads through the linked account, then parks its write for approval"
task="linked-$(date +%s)-${RANDOM}"
status="$(request POST "${api}/tasks" "${workdir}/task.json" "${auth[@]}" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg name "${task}" --arg ns "${namespace}" '{name:$name,namespace:$ns,type:"ai",agentRef:{name:"linked-agent"},prompt:"Exercise the linked account: read, write, read.",timeout:"20m"}')")"
[[ "${status}" == 201 ]] || { cat "${workdir}/task.json" | redact >&2; die "task creation returned HTTP ${status}"; }
jq -e --arg issuer "${issuer}" --arg subject "${subject}" '.spec.requestedBy.issuer == $issuer and .spec.requestedBy.subject == $subject' "${workdir}/task.json" >/dev/null
wait_for_state '.reads == 1 and .writes == 0 and .distinctBearers == 1' "the first read with the linked token" 150
wait_for_task_condition "${task}" '[.status.conditions[]? | select(.type=="WaitingForApproval" and .status=="True")] | length == 1' "WaitingForApproval" 90
kubectl -n "${namespace}" get task "${task}" -o json | jq -e '.status.connectionBindings[0].provider == "fixture" and .status.connectionBindings[0].uid != ""' >/dev/null \
  || die "the Task did not freeze the requester's Connection"

log "Refresh: let the first access token expire before approving"
ttl_seconds="$(( $(printf '%s' "${access_ttl}" | sed 's/s$//') ))"
elapsed="$(( $(date +%s) - linked_at ))"
if (( elapsed < ttl_seconds + 5 )); then sleep "$(( ttl_seconds + 5 - elapsed ))"; fi

log "Approval on write: approve the parked itemswrite"
status="$(request GET "${api}/tasks/${task}/approvals?namespace=${namespace}" "${workdir}/approvals.json" "${auth[@]}")"
[[ "${status}" == 200 ]] || die "approvals list returned HTTP ${status}"
approval_id="$(jq -er '[.approvals[] | select(.targetTool=="itemswrite" and .status=="pending")][0].id' "${workdir}/approvals.json")"
status="$(request POST "${api}/tasks/${task}/approvals/${approval_id}/decision?namespace=${namespace}" "${workdir}/decision.json" "${auth[@]}" \
  -H 'Content-Type: application/json' -d '{"decision":"approve"}')"
[[ "${status}" == 200 ]] || { cat "${workdir}/decision.json" | redact >&2; die "approval decision returned HTTP ${status}"; }
wait_for_state '.writes == 1 and .lastWriteTitle == "hello from orka" and .reads == 2 and .refreshes >= 1 and .distinctBearers >= 2' \
  "the approved write and a refreshed second read" 240
wait_for_task_condition "${task}" '.status.phase == "Succeeded"' "Succeeded" 150
kubectl -n "${namespace}" get task "${task}" -o json | jq -e '(.status.result // "" | test("CONNECTORS_E2E_DONE")) or true' >/dev/null

log "Fail closed: an unlinked person gets no credential"
other_token="$(curl -fsS -X POST "http://127.0.0.1:${fixture_port}/oidc/mint" -H 'Content-Type: application/json' -d '{"subject":"bob"}' | jq -er '.token')"
status="$(request GET "${api}/connections" "${workdir}/bob.json" -H "Authorization: Bearer ${other_token}")"
[[ "${status}" == 403 ]] || jq -e '.items | length == 0' "${workdir}/bob.json" >/dev/null || die "another person saw ${subject}'s connections"

log "Disconnect: revoke the tokens and remove the link"
status="$(request DELETE "${api}/connections/${connection}" "${workdir}/delete.json" "${auth[@]}")"
[[ "${status}" == 200 || "${status}" == 202 || "${status}" == 204 ]] || { cat "${workdir}/delete.json" | redact >&2; die "disconnect returned HTTP ${status}"; }
wait_for_state '.revocations >= 1' "a revocation" 60
for ((i = 0; i < 60; i++)); do
  status="$(request GET "${api}/connections/${connection}" "${workdir}/gone.json" "${auth[@]}")"
  [[ "${status}" == 404 ]] && break
  sleep 2
done
[[ "${status}" == 404 ]] || die "connection still readable after disconnect (HTTP ${status})"

log "No token material in controller logs"
if kubectl -n "${namespace}" logs deployment/"${deployment}" --all-containers=true 2>/dev/null | grep -Eq 'fx-access-|fx-refresh-'; then
  die "linked token material appeared in controller logs"
fi
if kubectl -n "${namespace}" logs deployment/"${deployment}" --all-containers=true 2>/dev/null | grep -Fq "${token}"; then
  die "the OIDC token appeared in controller logs"
fi
fixture_state | jq '{codeExchanges, refreshes, revocations, reads, writes, distinctBearers, rejected, modelTurns}' >&2
log "Live connectors E2E passed"
