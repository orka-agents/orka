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

# The shared redactor knows nothing of the fixture's own token format, and
# the literal OIDC/model credentials this run minted must never reach a
# log either: every diagnostic below goes through redact_all.
ORKA_REDACT_SECRET_VARS=(token other_token model_credential client_secret client_basic completion)
redact_all() {
  redact | sed -E 's/fx-(access|refresh)-[A-Za-z0-9._~+\/=-]+/fx-\1-[REDACTED]/g'
}

# controller_pod_names prints the controller Deployment's live pods, sorted,
# one per line; pods already terminating are left out.
controller_pod_names() {
  local selector
  selector="$(kubectl -n "${namespace}" get deployment/"${deployment}" -o json \
    | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')" || return 1
  kubectl -n "${namespace}" get pods -l "${selector}" -o json \
    | jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name' | sort
}

. "${script_dir}/lib/kind-local-registry.sh"
. "${script_dir}/lib/e2e-admission-tls.sh"

cluster="${KIND_CLUSTER:-orka-live-connectors-e2e}"
namespace="${ORKA_NAMESPACE:-orka-system}"
deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:live-connectors-e2e}"
publisher_image="${ORKA_WORKSPACE_PUBLISHER_IMAGE:-orka-workspace-publisher:live-connectors-e2e}"
worker_image="${ORKA_AI_WORKER_IMAGE:-orka-ai-worker:live-connectors-e2e}"
fixture_image="${ORKA_CONNECTORS_FIXTURE_IMAGE:-orka-connectors-fixture:live-connectors-e2e}"
api_port="${ORKA_API_LOCAL_PORT:-18080}"
fixture_port="${ORKA_FIXTURE_LOCAL_PORT:-18081}"
fixture_tls_port="${ORKA_FIXTURE_LOCAL_TLS_PORT:-18443}"
# Whole seconds only (an optional trailing "s"): the script sleeps past it.
# The credential source refreshes a token with 60s or less left, so the
# fixture TTL leaves the first read a full minute to happen on the token
# consent issued, before the lane waits that token out.
access_ttl_seconds="${ORKA_CONNECTORS_E2E_ACCESS_TTL_SECONDS:-120}"
access_ttl_seconds="${access_ttl_seconds%s}"
[[ "${access_ttl_seconds}" =~ ^[1-9][0-9]*$ ]] || { printf 'error: ORKA_CONNECTORS_E2E_ACCESS_TTL_SECONDS must be whole seconds, got %q\n' "${access_ttl_seconds}" >&2; exit 1; }
# The credential source refreshes a token with 60s or less left, and the
# first read must run on the consent-issued token (refreshes == 0), so the
# TTL needs the refresh horizon plus room for Task startup.
(( access_ttl_seconds >= 90 )) || { printf 'error: ORKA_CONNECTORS_E2E_ACCESS_TTL_SECONDS must be at least 90 (60s refresh horizon plus Task startup), got %s\n' "${access_ttl_seconds}" >&2; exit 1; }
access_ttl="${access_ttl_seconds}s"
subject="alice"
other_subject="bob"
audience="orka-live-connectors-e2e"
fixture_host="connectors-fixture.${namespace}.svc"
issuer="http://${fixture_host}:8080/oidc"
client_id="orka-e2e-client"
client_secret="$(openssl rand -hex 24)"
# The provider defaults to HTTP Basic client authentication, so the wire
# form of the client credential is this value, not the raw secret.
client_basic="$(printf '%s:%s' "${client_id}" "${client_secret}" | base64 | tr -d '\n')"
model_credential="fixture-$(openssl rand -hex 12)"
callback_base="http://localhost:${api_port}"
workdir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/orka-connectors-e2e.XXXXXX")"
chmod 700 "${workdir}"
# The lane's cluster lives in its own kubeconfig: kind writes it there and
# every kubectl below reads it, so the developer's current-context is never
# switched to a cluster this script later deletes.
export KUBECONFIG="${workdir}/kubeconfig"
kustomization="${repo_root}/config/manager/kustomization.yaml"
backup="${workdir}/kustomization.yaml"
api_pf_pid=""; fixture_pf_pid=""; fixture_tls_pf_pid=""
created_kind_cluster="0"
created_kind_registry="0"
pf_log="${workdir}/port-forward.log"

cleanup() {
  status=$?
  # Teardown must run to the end whatever a diagnostic below returns:
  # errexit stays active inside an EXIT trap, so it is switched off here.
  set +e
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
      if [[ -f "${pf_log}" ]]; then printf '%s\n' '--- port-forward log ---'; cat "${pf_log}"; fi
    } | redact_all >&2
  fi
  # Only a registry and a cluster this run created are torn down.
  if [[ "${created_kind_registry}" == "1" ]]; then orka_kind_registry_stop; fi
  if [[ "${created_kind_cluster}" == "1" ]]; then kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true; fi
  rm -rf "${workdir}"
  exit "${status}"
}
trap cleanup EXIT

# local_curl reaches the port-forwards on loopback directly: a proxy from
# the environment must never see the bearer tokens, OAuth code and state,
# or completion token these calls carry.
local_curl() { curl --noproxy '*' "$@"; }

request() {
  local method="$1" url="$2" output="$3"; shift 3
  local_curl -sS -o "${output}" -w '%{http_code}' -X "${method}" "${url}" "$@"
}

# url_shape prints a URL without its query values and fragment: the
# authorization code, state, and completion token never reach the log.
url_shape() {
  local raw="$1" base query keys=""
  base="${raw%%\?*}"
  base="${base%%#*}" # a fragment without a query would otherwise ride along in base
  query="${raw#*\?}"; query="${query%%#*}"
  if [[ "${raw}" == *"?"* ]]; then
    keys="$(printf '%s' "${query}" | tr '&' '\n' | sed 's/=.*//' | paste -sd, -)"
  fi
  printf '%s?{%s}%s' "${base}" "${keys}" "$([[ "${raw}" == *"#"* ]] && printf '#<fragment>')"
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
    if local_curl -fsS --connect-timeout 5 --max-time 10 "${url}" >/dev/null 2>&1; then return 0; fi
    attempts=$((attempts - 1)); sleep 2
  done
  die "${label} did not become ready"
}

fixture_state() { local_curl -fsS "http://127.0.0.1:${fixture_port}/fixture/state"; }

wait_for_state() {
  local expression="$1" label="$2" attempts="${3:-120}"
  while (( attempts > 0 )); do
    if fixture_state | jq -e "${expression}" >/dev/null 2>&1; then return 0; fi
    attempts=$((attempts - 1)); sleep 2
  done
  fixture_state | redact_all >&2 || true
  die "fixture never reached: ${label}"
}

wait_for_task_condition_on() {
  local kind="$1" name="$2" expression="$3" label="$4" attempts="${5:-150}"
  while (( attempts > 0 )); do
    if kubectl -n "${namespace}" get "${kind}" "${name}" -o json 2>/dev/null | jq -e "${expression}" >/dev/null 2>&1; then return 0; fi
    attempts=$((attempts - 1)); sleep 2
  done
  kubectl -n "${namespace}" get "${kind}" "${name}" -o yaml 2>/dev/null | redact_all >&2 || true
  die "${kind} ${name} never reached: ${label}"
}

wait_for_task_condition() {
  wait_for_task_condition_on task "$@"
}

[[ "${namespace}" == "orka-system" ]] || die "ORKA_NAMESPACE must be orka-system for the canonical make deploy path"
for cmd in make go docker kind kubectl curl jq openssl; do require_cmd "${cmd}"; done
cd "${repo_root}"
cp "${kustomization}" "${backup}"

# A cluster of this name that already exists belongs to somebody else:
# it would not be in this run's kubeconfig, and it is never deleted here.
if kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
  die "kind cluster ${cluster} already exists; delete it or set KIND_CLUSTER to an unused name"
fi
log "Creating kind cluster ${cluster}"
# Created directly with the e2e Kind config: the Makefile's setup target
# treats any cluster whose name merely contains this one as "already
# exists", which would leave this run's kubeconfig without a context.
# Ownership is recorded before the fallible create: the preflight proved
# the name free, so a cluster that fails mid-bootstrap is still this run's
# to delete.
created_kind_cluster="1"
kind create cluster --name "${cluster}" --config "${repo_root}/test/e2e/kind-config.yaml"
kubectl config use-context "kind-${cluster}" >/dev/null
log "Installing current Orka CRDs"
make install
kubectl create namespace vekil-system --dry-run=client -o yaml | kubectl apply -f -
# The registry helper reuses a running container of this name; one that
# exists already belongs to another invocation and is neither reused nor
# removed here.
if [[ -n "$(docker container ls --all --filter "name=^/$(orka_kind_registry_name "${cluster}")$" --format '{{.ID}}')" ]]; then
  die "kind registry $(orka_kind_registry_name "${cluster}") already exists; remove it or set KIND_CLUSTER to an unused name"
fi
# Ownership is recorded before the helper runs: the check above proved the
# name free, so whatever the helper creates (even partially) is this run's
# to remove.
created_kind_registry="1"
orka_kind_registry_start "${cluster}"

log "Building and loading images"
make docker-build IMG="${manager_image}"
make docker-build-ai-worker AI_WORKER_IMG="${worker_image}"
docker build -f test/fixtures/connectors/Dockerfile -t "${fixture_image}" .
kind load docker-image "${manager_image}" "${worker_image}" "${fixture_image}" --name "${cluster}"
manager_ref="$(orka_kind_registry_push "${manager_image}" "orka/controller")"
# The canonical deploy path waits for the workspace publisher to roll out
# (apply-acp-production.sh wait_for_workload_dependencies), so an inert
# reference cannot stand in for it: the real image is built like the manager's.
make docker-build-workspace-publisher WORKSPACE_PUBLISHER_IMG="${publisher_image}"
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
log "Deploying fail-closed Orka admission (task provenance webhook) with the controller image under test"
orka_e2e_deploy_admission "${manager_ref}" kubectl "${namespace}"
kubectl get validatingwebhookconfiguration orka-admission -o jsonpath='{.webhooks[*].name}' 2>/dev/null | grep -q . \
  || die "the Orka admission webhook configuration is not installed; external provenance protection would be a lie"

kubectl -n "${namespace}" set env deployment/"${deployment}" \
  ORKA_OIDC_ISSUER="${issuer}" \
  ORKA_OIDC_AUDIENCE="${audience}" \
  ORKA_OIDC_ALLOWED_SUBJECTS="${subject},${other_subject}" \
  ORKA_OIDC_NAMESPACE="${namespace}" \
  ORKA_TASK_PROVENANCE_ADMISSION_EXTERNAL=true \
  ORKA_CONNECTORS_ENABLED=true \
  ORKA_CONNECTOR_CALLBACK_BASE_URL="${callback_base}" \
  ORKA_CONNECTORS_ALLOW_PRIVATE_ENDPOINTS=i-understand-tokens-may-leave-the-cluster \
  ORKA_OIDC_JWKS_URL- ORKA_CONTEXT_TOKEN_PROFILE- ORKA_CONTEXT_TOKEN_ISSUER- ORKA_CONTEXT_TOKEN_AUDIENCE-
kubectl -n "${namespace}" rollout status deployment/"${deployment}" --timeout=5m
# The leak check at the end reads these pods' logs. A pod replaced mid-lane
# would take its logs with it, so the set is recorded now and must not change.
controller_pods="$(controller_pod_names)"
[[ -n "${controller_pods}" ]] || die "no running controller pods after the rollout"
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
provider_ready='[.status.conditions[]? | select(.type=="Accepted" or .type=="ResolvedRefs") | select(.status=="True")] | length == 2'
for ((i = 0; i < 60; i++)); do
  if kubectl -n "${namespace}" get connectorprovider fixture -o json | jq -e "${provider_ready}" >/dev/null 2>&1; then break; fi
  sleep 2
done
kubectl -n "${namespace}" get connectorprovider fixture -o json | jq -e "${provider_ready}" >/dev/null \
  || { kubectl -n "${namespace}" get connectorprovider fixture -o yaml | redact_all >&2; die "ConnectorProvider was not accepted"; }

log "Signing in as ${subject} through the OIDC fixture"
token="$(local_curl -fsS -X POST "http://127.0.0.1:${fixture_port}/oidc/mint" -H 'Content-Type: application/json' \
  -d "{\"subject\":\"${subject}\",\"email\":\"${subject}@example.test\"}" | jq -er '.token')"
auth=(-H "Authorization: Bearer ${token}")
api="http://127.0.0.1:${api_port}/api/v1"

log "Link: start consent as ${subject} (readWrite)"
status="$(request POST "${api}/connections" "${workdir}/start.json" "${auth[@]}" -H 'Content-Type: application/json' -d '{"provider":"fixture","mode":"readWrite"}')"
[[ "${status}" == 200 || "${status}" == 201 ]] || {
  # Keys and the URL's shape only: the authorize URL carries the live OAuth state.
  { jq -r 'keys | join(",")' "${workdir}/start.json"; url_shape "$(jq -r '.authorizeURL // ""' "${workdir}/start.json")"; } >&2 2>/dev/null || true
  die "start consent returned HTTP ${status}"
}
connection="$(jq -er '.connection.name' "${workdir}/start.json")"
authorize_url="$(jq -er '.authorizeURL' "${workdir}/start.json")"
[[ "${authorize_url}" == "https://${fixture_host}:8443/oauth/authorize?"* ]] || die "unexpected authorize URL host"
# The browser step, done by hand: the provider consents and redirects to the
# controller callback; the callback redirects to the settings page with the
# completion token in the fragment.
local_authorize="https://127.0.0.1:${fixture_tls_port}${authorize_url#https://${fixture_host}:8443}"
callback_location="$(local_curl -sS --cacert "${tls_dir}/ca.crt" --resolve "${fixture_host}:${fixture_tls_port}:127.0.0.1" -o /dev/null -w '%{redirect_url}' "${local_authorize}")"
[[ "${callback_location}" == "${callback_base}/api/v1/connections/callback?"* ]] || die "provider redirected elsewhere: $(url_shape "${callback_location}")"
settings_location="$(local_curl -sS -o /dev/null -w '%{redirect_url}' "http://127.0.0.1:${api_port}${callback_location#${callback_base}}")"
settings_query="${settings_location#*\?}"; settings_query="${settings_query%%#*}"
[[ "${settings_location}" == "${callback_base}/settings/connectors?"*"#completion="* ]] \
  && [[ "&${settings_query}&" == *"&status=pending&"* ]] \
  && [[ "&${settings_query}&" == *"&connection=${connection}&"* ]] \
  && [[ "&${settings_query}&" == *"&namespace=${namespace}&"* ]] \
  || die "callback redirected elsewhere: $(url_shape "${settings_location}")"
completion="${settings_location#*#completion=}"
status="$(request POST "${api}/connections/${connection}/complete" "${workdir}/complete.json" "${auth[@]}" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg c "${completion}" '{completion:$c}')")"
[[ "${status}" == 200 ]] || { cat "${workdir}/complete.json" | redact_all >&2; die "complete returned HTTP ${status}"; }
jq -e '.ready == true and .mode == "readWrite" and .state == "Ready"' "${workdir}/complete.json" >/dev/null || die "connection is not Ready after completion"
jq -e 'tostring | test("fx-access|fx-refresh") | not' "${workdir}/complete.json" >/dev/null || die "connection response leaked token material"
wait_for_state '.codeExchanges == 1 and .tokensIssued == 1' "one code exchange" 5
# A readWrite consent asks for exactly the provider's read and write
# scopes; the fixture refuses unknown scopes, and this pins the set.
fixture_state | jq -e '.lastAuthorizedScope == "items:read items:write"' >/dev/null \
  || { fixture_state | jq -c '{lastAuthorizedScope}' >&2; die "the readWrite consent did not ask for exactly items:read and items:write"; }

for tool in itemsread itemswrite; do
  wait_for_task_condition_on tool "${tool}" '.status.available == true' "Tool ${tool} Available under the private-endpoint allowance" 30
done

log "Use: a Task created by ${subject} reads through the linked account, then parks its write for approval"
task="linked-$(date +%s)-${RANDOM}"
status="$(request POST "${api}/tasks" "${workdir}/task.json" "${auth[@]}" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg name "${task}" --arg ns "${namespace}" '{name:$name,namespace:$ns,type:"ai",agentRef:{name:"linked-agent"},prompt:"Exercise the linked account: read, write, read.",timeout:"20m"}')")"
[[ "${status}" == 201 ]] || { cat "${workdir}/task.json" | redact_all >&2; die "task creation returned HTTP ${status}"; }
jq -e --arg issuer "${issuer}" --arg subject "${subject}" '.spec.requestedBy.issuer == $issuer and .spec.requestedBy.subject == $subject' "${workdir}/task.json" >/dev/null
# The first read must run on the token consent issued: no refresh yet and a
# single issued token, so the later refresh assertions test expiry, not a
# proactive refresh that happened before the Task ever called.
wait_for_state '.reads == 1 and .writes == 0 and .distinctBearers == 1 and .refreshes == 0 and .tokensIssued == 1' "the first read with the consent-issued token" 150
first_read_at="$(date +%s)"
# The native autonomous path parks by writing the pending approval into status.message; it does not set a condition.
wait_for_task_condition "${task}" '.status.phase == "Running" and (.status.message | startswith("waiting for approval ")) and (.status.message | test(" for itemswrite "))' "parked on the itemswrite approval" 90
kubectl -n "${namespace}" get task "${task}" -o json | jq -e '.status.connectionBindings[0].provider == "fixture" and .status.connectionBindings[0].uid != ""' >/dev/null \
  || die "the Task did not freeze the requester's Connection"

log "Refresh: let the access token the first read used expire before approving"
# The first read proved it used the token consent issued (one bearer), so
# waiting a full TTL past that read leaves that token expired for certain.
elapsed="$(( $(date +%s) - first_read_at ))"
if (( elapsed < access_ttl_seconds + 5 )); then sleep "$(( access_ttl_seconds + 5 - elapsed ))"; fi

log "Approval on write: approve the parked itemswrite"
status="$(request GET "${api}/tasks/${task}/approvals?namespace=${namespace}" "${workdir}/approvals.json" "${auth[@]}")"
[[ "${status}" == 200 ]] || die "approvals list returned HTTP ${status}"
approval_id="$(jq -er '[.approvals[] | select(.targetTool=="itemswrite" and .status=="pending")][0].id' "${workdir}/approvals.json")"
status="$(request POST "${api}/tasks/${task}/approvals/${approval_id}/decision?namespace=${namespace}" "${workdir}/decision.json" "${auth[@]}" \
  -H 'Content-Type: application/json' -d '{"decision":"approve"}')"
[[ "${status}" == 200 ]] || { cat "${workdir}/decision.json" | redact_all >&2; die "approval decision returned HTTP ${status}"; }
wait_for_state '.writes == 1 and .lastWriteTitle == "hello from orka" and .reads == 2 and .refreshes >= 1 and .distinctBearers >= 2' \
  "the approved write and a refreshed second read" 240
wait_for_task_condition "${task}" '.status.phase == "Succeeded"' "Succeeded" 150
status="$(request GET "${api}/tasks/${task}/result?namespace=${namespace}" "${workdir}/result.json" "${auth[@]}")"
[[ "${status}" == 200 ]] || die "task result returned HTTP ${status}"
jq -e '(.result // "" | tostring) | test("CONNECTORS_E2E_DONE")' "${workdir}/result.json" >/dev/null \
  || { jq -c '{keys: keys}' "${workdir}/result.json" >&2; die "the scripted model never reached its final answer"; }

log "Isolation: another signed-in person sees none of ${subject}'s links"
other_token="$(local_curl -fsS -X POST "http://127.0.0.1:${fixture_port}/oidc/mint" -H 'Content-Type: application/json' \
  -d "{\"subject\":\"${other_subject}\"}" | jq -er '.token')"
status="$(request GET "${api}/connections?namespace=${namespace}" "${workdir}/other.json" -H "Authorization: Bearer ${other_token}")"
[[ "${status}" == 200 ]] || die "${other_subject}'s connection list returned HTTP ${status}"
jq -e '(.items | type) == "array" and (.items | length) == 0' "${workdir}/other.json" >/dev/null || die "${other_subject} saw ${subject}'s connections"
status="$(request GET "${api}/connections/${connection}?namespace=${namespace}" "${workdir}/other-get.json" -H "Authorization: Bearer ${other_token}")"
[[ "${status}" == 404 ]] || die "${other_subject} could read ${subject}'s connection (HTTP ${status})"

log "Disconnect: revoke the tokens and remove the link"
status="$(request DELETE "${api}/connections/${connection}" "${workdir}/delete.json" "${auth[@]}")"
[[ "${status}" == 200 || "${status}" == 202 || "${status}" == 204 ]] || { cat "${workdir}/delete.json" | redact_all >&2; die "disconnect returned HTTP ${status}"; }
wait_for_state '.revocations >= 1 and .revokedRefreshes >= 1' "revocation of the issued refresh token" 60
for ((i = 0; i < 60; i++)); do
  status="$(request GET "${api}/connections/${connection}" "${workdir}/gone.json" "${auth[@]}")"
  [[ "${status}" == 404 ]] && break
  sleep 2
done
[[ "${status}" == 404 ]] || die "connection still readable after disconnect (HTTP ${status})"

log "Terminal fixture counters: exactly one read, one approved write, one more read"
# The earlier waits pass as soon as a counter is reached; only the terminal
# state proves nothing ran twice (a duplicate write after approval, say).
# The approved write refreshes the expired token once; the read right
# after it reuses the refreshed token, which has the full TTL (at least
# 90s) against the 60s refresh horizon, so exactly one refresh and two
# bearers (the consent-issued and the refreshed token) are expected, and
# refresh churn fails here.
fixture_state | jq -e '.reads == 2 and .writes == 1 and .codeExchanges == 1 and .refreshes == 1 and .distinctBearers == 2 and .revocations >= 1 and .rejected == 0' >/dev/null \
  || { fixture_state | redact_all >&2; die "terminal fixture counters do not match the single read/write/read and single refresh the lane expects"; }
fixture_state | jq '{codeExchanges, refreshes, revocations, reads, writes, distinctBearers, rejected, modelTurns, lastAuthorizedScope}' >&2

log "No token material in controller logs"
# The logs are captured first, so an unreadable log can never pass as "no match".
# Every controller container is read, including the previous instance of
# one that restarted, and the pods must be the ones the lane started with:
# a replaced or restarted container's earlier output is part of the check.
# The kubelet keeps only the immediately previous instance's logs, so a
# container that restarted more than once fails the check outright.
current_pods="$(controller_pod_names)"
[[ "${current_pods}" == "${controller_pods}" ]] \
  || die "the controller pods changed during the lane (${controller_pods} -> ${current_pods}); their earlier logs cannot be checked for leaks"
: > "${workdir}/controller.log"
for pod in ${controller_pods}; do
  kubectl -n "${namespace}" logs pod/"${pod}" --all-containers=true >> "${workdir}/controller.log" \
    || die "could not read the controller logs of ${pod} for the leak check"
  restarts="$(kubectl -n "${namespace}" get pod "${pod}" -o json | jq -c '[.status.containerStatuses[]? | {name, restartCount}]')" \
    || die "could not read the container restarts of ${pod} for the leak check"
  if jq -e 'any(.[]; .restartCount > 1)' <<<"${restarts}" >/dev/null; then
    die "a controller container of ${pod} restarted more than once; logs of instances before the previous one are gone, so the leak check cannot cover them"
  fi
  restarted="$(jq -r '.[] | select(.restartCount > 0) | .name' <<<"${restarts}")"
  for container in ${restarted}; do
    kubectl -n "${namespace}" logs pod/"${pod}" -c "${container}" --previous >> "${workdir}/controller.log" \
      || die "container ${container} of ${pod} restarted and its previous logs cannot be read for the leak check"
  done
done
[[ -s "${workdir}/controller.log" ]] || die "the controller logs are empty; the leak check proves nothing"
if grep -Eq 'fx-access-|fx-refresh-' "${workdir}/controller.log"; then
  die "linked token material appeared in controller logs"
fi
if grep -Fq -e "${token}" -e "${other_token}" "${workdir}/controller.log"; then
  die "an OIDC token appeared in controller logs"
fi
# The controller reads the provider's OAuth client secret for every code
# exchange, refresh, and revocation; it must never reach a log either, raw
# or in the Basic form it is sent in.
if grep -Fq -e "${client_secret}" -e "${client_basic}" "${workdir}/controller.log"; then
  die "the OAuth client credential appeared in controller logs"
fi
# The one-time completion token and the model credential the worker uses
# are credential material too.
if grep -Fq -e "${completion}" "${workdir}/controller.log"; then
  die "the consent completion token appeared in controller logs"
fi
if grep -Fq -e "${model_credential}" "${workdir}/controller.log"; then
  die "the model credential appeared in controller logs"
fi
log "Live connectors E2E passed"
