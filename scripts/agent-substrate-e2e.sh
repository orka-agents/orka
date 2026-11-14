#!/usr/bin/env bash
# Native conformance against the unmodified official Substrate v1 pin: direct
# exec with data-only checkpoint restore, pooled MCP Tools, egress-gateway
# credential injection, and trace propagation through the MCP proxy.
set -Eeuo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
ORKA_NAMESPACE=orka-system
KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
TMP_ROOT="${SUBSTRATE_E2E_RUN_DIR:-${ROOT_DIR}/bin/substrate-e2e-${KIND_CLUSTER}}"
SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME=orka-substrate-bootstrap
SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY=token
# Egress-gateway credential injection: the upstream only ever sees, and reports,
# a digest of what the gateway injected.
UPSTREAM_NAMESPACE=ate-demo
UPSTREAM_NAME=credential-echo
UPSTREAM_URL="http://${UPSTREAM_NAME}.${UPSTREAM_NAMESPACE}.svc.cluster.local/"
UPSTREAM_TOKEN_SECRET=native-mcp-upstream-token
OTEL_COLLECTOR_NAME=substrate-e2e-otel
PORT_FORWARD_PIDS=()
source "${ROOT_DIR}/scripts/lib/substrate-upstream.sh"
source "${ROOT_DIR}/scripts/lib/substrate-orka-local.sh"
source "${ROOT_DIR}/scripts/lib/e2e-admission-tls.sh"
source "${ROOT_DIR}/scripts/lib/redact.sh"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
kubectl_ate() { "${TMP_ROOT}/kubectl-ate" --context "kind-${KIND_CLUSTER}" "$@"; }
cleanup() {
  local rc=$?
  for pid in "${PORT_FORWARD_PIDS[@]}"; do kill "${pid}" 2>/dev/null || true; done
  if (( rc )) && [[ "${CLUSTER_PREPARED:-0}" == 1 ]]; then
    runtime_diagnostics
    kubectl get pods -A 2>/dev/null || true
    local job
    for job in substrate-direct-conformance $(kubectl -n orka-system get jobs -l orka.ai/e2e=native-mcp-client -o name 2>/dev/null); do
      job_diagnostics "${job#job.batch/}"
    done
    workload_logs ate-system -l app=ate-api-server
    workload_logs ate-system -l app=atenet-egress
    workload_logs ate-system -l app=k8s-credential-provider
    workload_logs "${UPSTREAM_NAMESPACE}" -l app="${UPSTREAM_NAME}"
    workload_logs ate-demo -l ate.dev/worker-pool=orka-native
    workload_logs orka-system -l orka.ai/network-role=provider-auth-proxy
    workload_logs orka-system -l control-plane=controller-manager
  fi
  if [[ "${KEEP_CLUSTER}" != 1 && "${CLUSTER_PREPARED:-0}" == 1 ]]; then kind delete cluster --name "${KIND_CLUSTER}"; fi
  exit "${rc}"
}
# Diagnostics redact every credential this run generated.
load_redaction_secrets() {
  if [[ -f "${TMP_ROOT}/bootstrap-token" ]]; then bootstrap_secret="$(<"${TMP_ROOT}/bootstrap-token")"; fi
  if [[ -f "${TMP_ROOT}/upstream-token" ]]; then upstream_secret="$(<"${TMP_ROOT}/upstream-token")"; fi
}
runtime_diagnostics() {
  local bootstrap_secret="" upstream_secret=""
  local ORKA_REDACT_SECRET_VARS=(bootstrap_secret upstream_secret)
  load_redaction_secrets
  # Select diagnostic status fields; omit specs and native identity records.
  kubectl -n orka-system --request-timeout=15s get tools,substrateactorpools -o json 2>/dev/null |
    jq '[.items[] | {kind, name: .metadata.name, phase: .status.phase, state: .status.state,
      lifecycle: .status.lifecycle, message: .status.message,
      execution: (.status.execution | if . == null then null else {state, outcome, reason} end),
      conditions: [.status.conditions[]? | {type, status, reason, message}]}]' | redact >&2 || true
}
workload_logs() {
  local namespace="$1" bootstrap_secret="" upstream_secret=""
  shift
  local ORKA_REDACT_SECRET_VARS=(bootstrap_secret upstream_secret)
  load_redaction_secrets
  kubectl -n "${namespace}" --request-timeout=15s logs "$@" --all-containers=true \
    --prefix=true --tail=200 --pod-running-timeout=5s 2>&1 | redact >&2 || true
}
job_diagnostics() {
  local name="$1" bootstrap_secret="" upstream_secret=""
  local ORKA_REDACT_SECRET_VARS=(bootstrap_secret upstream_secret)
  load_redaction_secrets
  log "Conformance diagnostics for job/${name}"
  # Restrict metadata to status; Pod specs can contain projected credentials.
  kubectl -n orka-system --request-timeout=15s get pods -l "job-name=${name}" -o json 2>/dev/null |
    jq '[.items[] | {name: .metadata.name, phase: .status.phase, conditions: .status.conditions,
      containers: [.status.containerStatuses[]? | {name, state}]}]' | redact >&2 || true
  workload_logs orka-system "job/${name}"
}
wait_job() {
  local name="$1" seconds="$2" start status
  start=$(date +%s)
  while true; do
    status="$(kubectl -n orka-system --request-timeout=15s get job "${name}" -o json)" || return 1
    if jq -e 'any(.status.conditions[]?; (.type == "Failed" or .type == "FailureTarget") and .status == "True")' <<<"${status}" >/dev/null; then
      printf 'Conformance job/%s failed\n' "${name}" >&2
      return 1
    fi
    if jq -e 'any(.status.conditions[]?; .type == "Complete" and .status == "True")' <<<"${status}" >/dev/null; then return 0; fi
    if (( $(date +%s) - start >= seconds )); then
      printf 'Timed out waiting for conformance job/%s\n' "${name}" >&2
      return 1
    fi
    sleep 2
  done
}
wait_field() {
  local resource="$1" name="$2" expression="$3" expected="$4" seconds="${5:-600}" start now next_diagnostics object
  start=$(date +%s)
  next_diagnostics=$((start + 30))
  while true; do
    object="$(kubectl -n orka-system --request-timeout=15s get "${resource}" "${name}" -o json 2>/dev/null)" || return 1
    if [[ "$(jq -r "${expression}" <<<"${object}")" == "${expected}" ]]; then return 0; fi
    now=$(date +%s)
    if (( now - start >= seconds )); then
      printf 'Timed out waiting for %s/%s %s = %s\n' "${resource}" "${name}" "${expression}" "${expected}" >&2
      runtime_diagnostics
      return 1
    fi
    if (( now >= next_diagnostics )); then
      runtime_diagnostics
      next_diagnostics=$((now + 30))
    fi
    sleep 2
  done
}
wait_absent() {
  local resource="$1" name="$2"
  local present
  present="$(kubectl -n orka-system get "${resource}" "${name}" --ignore-not-found -o name)" || return 1
  if [[ -n "${present}" ]]; then
    kubectl -n orka-system wait --for=delete "${resource}/${name}" --timeout=600s
  fi
}
mount_substrate_identity() {
  local resource="$1" name="$2" container="$3"
  kubectl -n orka-system patch "${resource}" "${name}" --type=strategic -p "$(jq -cn --arg container "${container}" '{spec:{template:{spec:{securityContext:{fsGroup:65532},containers:[{name:$container,volumeMounts:[{name:"substrate-client",mountPath:"/run/substrate-client",readOnly:true},{name:"substrate-server",mountPath:"/run/substrate-server",readOnly:true}]}],volumes:[{name:"substrate-client",projected:{sources:[{podCertificate:{signerName:"podidentity.podcert.ate.dev/identity",keyType:"ECDSAP256",credentialBundlePath:"credential-bundle.pem"}}]}},{name:"substrate-server",projected:{sources:[{clusterTrustBundle:{signerName:"servicedns.podcert.ate.dev/identity",labelSelector:{matchLabels:{"podcert.ate.dev/canarying":"live"}},path:"trust-bundle.pem"}}]}}]}}}}')"
}
publish_worker_image() {
  (cd "${SUBSTRATE_DIR}" && KO_DOCKER_REPO="localhost:${KIND_REGISTRY_PORT}" ko build --platform="linux/$(go env GOARCH)" ./cmd/ateworker-gvisor)
}
build_image() {
  local name="$1" dockerfile="$2" ref
  ref="localhost:${KIND_REGISTRY_PORT}/orka/${name}:native-conformance"
  docker build -t "${ref}" -f "${ROOT_DIR}/${dockerfile}" "${ROOT_DIR}" >&2
  docker push "${ref}" >&2
  docker inspect --format '{{index .RepoDigests 0}}' "${ref}"
}
# One template-sized Actor: worker Pod limits leave room for exactly one
# (gVisor reserves 64Mi of the worker's memory limit).
NATIVE_ACTOR_CPU_MILLICORES=1000
NATIVE_ACTOR_MEMORY_BYTES=1073741824
# orka-direct runs the workspace agent behind the sealed bootstrap, with a
# DurableDir workspace and the native identity projection the agent binds its
# credential to. orka-mcp runs the MCP conformance server, which reaches the
# credential echo upstream only through the egress gateway.
native_template_manifest() {
  local name="$1" image="$2" public_key="${3:-}" upstream_url="${4:-}"
  jq -n --arg name "${name}" --arg image "${image}" --arg publicKey "${public_key}" --arg upstream "${upstream_url}" \
    --argjson cpu "${NATIVE_ACTOR_CPU_MILLICORES}" --arg memory "${NATIVE_ACTOR_MEMORY_BYTES}" '
    ($name == "orka-direct") as $direct |
    {metadata:{atespace:"orka-system",name:$name},workerSelector:{matchLabels:{"orka.ai/native-pool":"conformance"}},
     containers:[({name:"server",image:$image,
       env:([{name:"ORKA_WORKSPACE_AGENT_LISTEN_ADDR",value:":80"}]
         + (if $direct then [{name:"ORKA_WORKSPACE_BOOTSTRAP_PUBLIC_KEY",value:$publicKey}] else [] end)
         + (if ($direct | not) and $upstream != "" then [{name:"ORKA_MCP_E2E_UPSTREAM_URL",value:$upstream}] else [] end)),
       wakeupProbe:{httpGet:{path:(if $direct then "/v1/health" else "/healthz" end),port:80}},
       securityContext:{capabilities:{drop:["ALL"],add:["NET_BIND_SERVICE","SETUID","SETGID","CHOWN","KILL"]}}}
       # Native overlay roots and DurableDir mounts start as 0700. Restore root
       # traversal and workspace writes before commands drop to UID 1000.
       + (if $direct then {command:["/bin/sh","-ec"],
           args:["chmod 0755 /; chmod 1777 /workspace; exec /orka-workspace-agent"],
           volumeMounts:[{name:"identity",mountPath:"/run/orka-substrate-identity"},{name:"workspace",mountPath:"/workspace"}]}
         else {} end))],
     resources:{limits:{cpuMillicores:$cpu,memoryBytes:$memory}},
     snapshotConfig:{preferredFidelity:"SNAPSHOT_FIDELITY_VOLUMES",goldenSnapshotConfig:{mode:"GOLDEN_SNAPSHOT_MODE_DISABLED"}}}
    + (if $direct then {volumes:[{name:"workspace",durableDir:{}},
        {name:"identity",systemInfo:{dataSources:[{actorMetadata:{items:[
          {field:"ACTOR_METADATA_FIELD_ATESPACE",path:"atespace"},
          {field:"ACTOR_METADATA_FIELD_NAME",path:"name"},
          {field:"ACTOR_METADATA_FIELD_UID",path:"uid"}]}}]}}]}
      else {} end)
  '
}
# Orka's controller authenticates with its pod certificate. Grant it editor on
# its Atespace and global viewer for worker placement reads; it is never a
# bootstrap owner. "get access-policy" exits 0 with no output when a scope has
# no policy, and "create" refuses an existing one, so read the policy as JSON
# and either create it or merge the binding into it under its uid/version
# preconditions.
ensure_access_binding() {
  local role="$1" principal="$2" manifest="$3"
  shift 3
  local current
  current="$(kubectl_ate get access-policy "$@" -o json)"
  if [[ -z "${current}" ]]; then
    jq -n --arg role "${role}" --arg principal "${principal}" \
      '{metadata:{name:"default"},bindings:[{role:$role,members:[$principal]}]}' >"${manifest}"
    kubectl_ate create access-policy "$@" -f "${manifest}" >/dev/null
    return
  fi
  if jq -e --arg role "${role}" --arg principal "${principal}" \
    'any(.bindings[]?; .role == $role and any(.members[]?; . == $principal))' <<<"${current}" >/dev/null; then
    return
  fi
  jq --arg role "${role}" --arg principal "${principal}" '
    .bindings = ((.bindings // []) as $bindings |
      if any($bindings[]; .role == $role)
      then $bindings | map(if .role == $role then .members = ((.members // []) + [$principal]) else . end)
      else $bindings + [{role:$role,members:[$principal]}] end)' <<<"${current}" >"${manifest}"
  kubectl_ate update access-policy "$@" -f "${manifest}" >/dev/null
}
grant_orka_native_access() {
  local principal="user:spiffe://cluster.local/ns/${ORKA_NAMESPACE}/sa/orka-controller-manager"
  ensure_access_binding editor "${principal}" "${TMP_ROOT}/orka-atespace-policy.json" -a orka-system
  ensure_access_binding viewer "${principal}" "${TMP_ROOT}/orka-global-policy.json" --global
}
create_native_resources() {
  local worker_image="$1" direct_image="$2" mcp_image="$3" public_key="$4"
  kubectl create namespace ate-demo --dry-run=client -o yaml | kubectl apply -f -
  bash "${ROOT_DIR}/scripts/lib/ensure-static-mode-namespace.sh" kubectl "${ORKA_NAMESPACE}" harness-v2
  # Substrate's system NetworkPolicy admits API clients only from labeled namespaces.
  kubectl label namespace "${ORKA_NAMESPACE}" ate.dev/api-access=true --overwrite
  kubectl_ate get atespace orka-system >/dev/null 2>&1 || kubectl_ate create atespace orka-system
  grant_orka_native_access
  kubectl -n ate-demo apply -f - <<YAML
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  name: orka-native
  labels: {orka.ai/native-pool: conformance}
spec:
  replicas: 3
  workerImage: ${worker_image}
  sandboxClasses: [{name: gvisor, configRef: {name: gvisor-default}}]
  template:
    resources:
      requests: {cpu: 250m, memory: 512Mi}
      limits: {cpu: "1", memory: 1088Mi}
YAML
  native_template_manifest orka-direct "${direct_image}" "${public_key}" >"${TMP_ROOT}/orka-direct.json"
  kubectl_ate create actor-template -f "${TMP_ROOT}/orka-direct.json"
  native_template_manifest orka-mcp "${mcp_image}" "" "${UPSTREAM_URL}" >"${TMP_ROOT}/orka-mcp.json"
  kubectl_ate create actor-template -f "${TMP_ROOT}/orka-mcp.json"
  kubectl -n ate-demo wait --for=jsonpath='{.status.readyReplicas}'=3 workerpool/orka-native --timeout=5m
}
# The upstream is an ordinary Pod outside the sandbox. Actors reach it only
# through the egress gateway, which injects the credential an EgressPolicy names.
deploy_credential_upstream() {
  local image="$1"
  kubectl -n "${UPSTREAM_NAMESPACE}" apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: ${UPSTREAM_NAME}}
spec:
  replicas: 1
  selector: {matchLabels: {app: ${UPSTREAM_NAME}}}
  template:
    metadata: {labels: {app: ${UPSTREAM_NAME}}}
    spec:
      automountServiceAccountToken: false
      containers:
      - name: upstream
        image: ${image}
        env:
        - {name: ORKA_MCP_E2E_UPSTREAM_MODE, value: credential-digest}
        - {name: ORKA_WORKSPACE_AGENT_LISTEN_ADDR, value: ":8080"}
        ports: [{containerPort: 8080}]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}}
        # The image binary carries a file capability for port 80; keep it in
        # the bounding set or exec fails.
        securityContext:
          allowPrivilegeEscalation: false
          capabilities: {drop: [ALL], add: [NET_BIND_SERVICE]}
          runAsNonRoot: true
---
apiVersion: v1
kind: Service
metadata: {name: ${UPSTREAM_NAME}}
spec:
  selector: {app: ${UPSTREAM_NAME}}
  ports: [{port: 80, targetPort: 8080}]
YAML
  kubectl -n "${UPSTREAM_NAMESPACE}" rollout status "deployment/${UPSTREAM_NAME}" --timeout=3m
}
# The k8s.io credential provider is default-deny per Atespace and reads its
# policy only at startup. Let the orka-system Atespace resolve Secrets in the
# Tool namespace only, then restart the provider.
grant_credential_provider_namespace() {
  kubectl -n ate-system apply -f - <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: k8s-credential-provider-namespace-policy
  namespace: ate-system
data:
  namespace-policy.yaml: |
    policies:
    - atespace: orka-system
      allowedNamespaces:
      - orka-system
YAML
  kubectl -n ate-system rollout restart deployment/k8s-credential-provider
  kubectl -n ate-system rollout status deployment/k8s-credential-provider --timeout=3m
}
create_upstream_token() {
  openssl rand -hex 32 | tr -d '\n' >"${TMP_ROOT}/upstream-token"
  chmod 600 "${TMP_ROOT}/upstream-token"
  kubectl -n orka-system create secret generic "${UPSTREAM_TOKEN_SECRET}" \
    --from-file="token=${TMP_ROOT}/upstream-token" --dry-run=client -o yaml | kubectl apply -f -
}
# The upstream reports sha256 of the Authorization it received; the Tool's
# credential rule injects "Bearer <token>".
expected_injected_digest() {
  printf 'Bearer %s' "$(<"${TMP_ROOT}/upstream-token")" | sha256sum | cut -d' ' -f1
}
deploy_otel_collector() {
  kubectl -n orka-system apply -f - <<YAML
apiVersion: v1
kind: ConfigMap
metadata: {name: ${OTEL_COLLECTOR_NAME}-config}
data:
  collector.yaml: |
    receivers:
      otlp:
        protocols:
          grpc: {endpoint: 0.0.0.0:4317}
    processors:
      batch: {timeout: 1s}
    exporters:
      debug: {verbosity: detailed}
      debug/metrics: {verbosity: basic}
    service:
      pipelines:
        traces: {receivers: [otlp], processors: [batch], exporters: [debug]}
        metrics: {receivers: [otlp], processors: [batch], exporters: [debug/metrics]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: ${OTEL_COLLECTOR_NAME}}
spec:
  replicas: 1
  selector: {matchLabels: {app: ${OTEL_COLLECTOR_NAME}}}
  template:
    metadata: {labels: {app: ${OTEL_COLLECTOR_NAME}}}
    spec:
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: collector
        image: otel/opentelemetry-collector:0.111.0
        args: [--config=/etc/otelcol/collector.yaml]
        ports: [{containerPort: 4317}]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
        volumeMounts: [{name: config, mountPath: /etc/otelcol, readOnly: true}]
      volumes: [{name: config, configMap: {name: ${OTEL_COLLECTOR_NAME}-config}}]
---
apiVersion: v1
kind: Service
metadata: {name: ${OTEL_COLLECTOR_NAME}}
spec:
  selector: {app: ${OTEL_COLLECTOR_NAME}}
  ports: [{name: otlp-grpc, port: 4317}]
YAML
  kubectl -n orka-system rollout status "deployment/${OTEL_COLLECTOR_NAME}" --timeout=3m
}
exercise_direct() {
  local image="$1"
  kubectl -n orka-system apply -f - <<YAML
apiVersion: batch/v1
kind: Job
metadata: {name: substrate-direct-conformance}
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 600
  template:
    spec:
      restartPolicy: Never
      # The fixture drives the controller's direct executor. v1 authorizes the
      # native API per identity and the router admits only the controller's
      # SPIFFE ID, so it runs with that pod-certificate identity; it never
      # needs the Kubernetes token.
      serviceAccountName: orka-controller-manager
      automountServiceAccountToken: false
      securityContext: {fsGroup: 65532}
      containers:
      - name: conformance
        image: ${image}
        volumeMounts:
        - {name: bootstrap, mountPath: /run/orka-bootstrap, readOnly: true}
        - {name: substrate-client, mountPath: /run/substrate-client, readOnly: true}
        - {name: substrate-server, mountPath: /run/substrate-server, readOnly: true}
      volumes:
      - name: bootstrap
        secret: {secretName: ${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME}}
      - name: substrate-client
        projected:
          sources:
          - podCertificate:
              signerName: podidentity.podcert.ate.dev/identity
              keyType: ECDSAP256
              credentialBundlePath: credential-bundle.pem
      - name: substrate-server
        projected:
          sources:
          - clusterTrustBundle:
              signerName: servicedns.podcert.ate.dev/identity
              labelSelector: {matchLabels: {podcert.ate.dev/canarying: live}}
              path: trust-bundle.pem
YAML
  wait_job substrate-direct-conformance 600
  kubectl -n orka-system logs job/substrate-direct-conformance | grep -Fxq 'native direct workspace conformance passed'
  kubectl -n orka-system delete job substrate-direct-conformance
}
# run_tool_client <job> <tool> <message> <expected result> [traceparent]
run_tool_client() {
  local job="$1" tool="$2" message="$3" expected="$4" traceparent="${5:-}"
  jq -n --arg job "${job}" --arg tool "${tool}" --arg image "${TOOL_CLIENT_IMAGE}" \
    --arg args "$(jq -cn --arg message "${message}" '{message:$message}')" \
    --arg expected "${expected}" --arg traceparent "${traceparent}" '
    {apiVersion:"batch/v1",kind:"Job",
     metadata:{name:$job,labels:{"orka.ai/e2e":"native-mcp-client"}},
     spec:{backoffLimit:0,activeDeadlineSeconds:180,template:{spec:{
       restartPolicy:"Never",serviceAccountName:"native-mcp-client",
       containers:[{name:"tool-client",image:$image,env:([
         # Substrate MCP Tools are reached only through the authenticated
         # orka-api proxy; this client never talks to the router.
         {name:"ORKA_CONTROLLER_URL",value:"http://orka-api.orka-system.svc:8080"},
         {name:"ORKA_TOOL_NAMESPACE",value:"orka-system"},
         {name:"ORKA_TOOL_NAME",value:$tool},
         {name:"ORKA_TOOL_ARGS",value:$args},
         {name:"ORKA_TOOL_EXPECT_RESULT",value:$expected}]
         + (if $traceparent == "" then [] else [{name:"ORKA_TOOL_TRACEPARENT",value:$traceparent}] end))}]}}}}' |
    kubectl -n orka-system apply -f -
  wait_job "${job}" 180
  kubectl -n orka-system delete job "${job}"
}
# Print "<name>\t<trace>\t<span>\t<parent>" for every span the debug exporter logged.
collector_spans() {
  kubectl -n orka-system logs "deployment/${OTEL_COLLECTOR_NAME}" --tail=-1 | python3 -c '
import sys
spans, current = [], None
def finish():
    global current
    if current and current.get("name") and current.get("trace"):
        spans.append(current)
    current = None
for raw in sys.stdin:
    line = raw.strip()
    if line.startswith("Span #"):
        finish(); current = {}; continue
    if line.startswith(("ResourceSpans #", "ScopeSpans #")):
        finish(); continue
    if current is None or ":" not in line:
        continue
    key, value = (part.strip() for part in line.split(":", 1))
    field = {"Trace ID": "trace", "Parent ID": "parent", "ID": "span", "Span ID": "span", "Name": "name"}.get(key)
    if field and field not in current:
        current[field] = value
finish()
for span in spans:
    print("\t".join(span.get(k, "") for k in ("name", "trace", "span", "parent")))
'
}
# The orka-api server span must continue the caller's trace, and the router
# client span must be its child in the same trace.
verify_mcp_trace() {
  local trace_id="$1" deadline spans server router
  deadline=$(( $(date +%s) + 90 ))
  while true; do
    spans="$(collector_spans | awk -F'\t' -v t="${trace_id}" '$2 == t')" || spans=""
    server="$(awk -F'\t' '$1 == "POST /api/v1/tools/:name/mcp" {print $3; exit}' <<<"${spans}")"
    router="$(awk -F'\t' '$1 == "substrate.router POST" {print $4; exit}' <<<"${spans}")"
    if [[ -n "${server}" && -n "${router}" ]]; then
      [[ "${router}" == "${server}" ]] || {
        printf 'substrate.router span parent %s is not the orka-api span %s\n' "${router}" "${server}" >&2
        return 1
      }
      log "MCP proxy and router spans joined caller trace ${trace_id}"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      printf 'trace %s is missing the orka-api or substrate.router span; spans seen:\n%s\n' "${trace_id}" "${spans}" >&2
      return 1
    fi
    sleep 3
  done
}
exercise_mcp() {
  TOOL_CLIENT_IMAGE="$1"
  kubectl -n orka-system apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: SubstrateActorPool
metadata: {name: native-mcp}
spec:
  templateRef: {name: orka-mcp, namespace: orka-system}
  targetActors: 1
  precreateActors: true
---
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata: {name: native-mcp}
spec:
  description: Native Substrate MCP conformance
  parameters: {type: object, properties: {message: {type: string}}, required: [message]}
  mcp:
    path: /mcp
    substrateActor:
      templateRef: {name: orka-mcp, namespace: orka-system}
      poolRef: {name: native-mcp}
      boot: true
---
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata: {name: native-mcp-egress}
spec:
  description: Native Substrate MCP egress credential injection conformance
  parameters: {type: object, properties: {message: {type: string}}, required: [message]}
  mcp:
    path: /mcp
    substrateActor:
      templateRef: {name: orka-mcp, namespace: orka-system}
      boot: true
      egress:
      - scheme: http
        host: ${UPSTREAM_NAME}.${UPSTREAM_NAMESPACE}.svc.cluster.local
        ports: [80]
        credential:
          header: Authorization
          prefix: "Bearer "
          secretRef: {name: ${UPSTREAM_TOKEN_SECRET}, key: token}
YAML
  wait_field tool native-mcp '.status.available' true
  wait_field tool native-mcp-egress '.status.available' true
  wait_field tool native-mcp-egress '[.status.conditions[]? | select(.type == "EgressPolicyReady")][0].status' True
  kubectl -n orka-system apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: native-mcp-client}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: native-mcp-client}
rules:
- apiGroups: [core.orka.ai]
  resources: [tools]
  resourceNames: [native-mcp, native-mcp-egress]
  verbs: [get]
# A caller of a credential-injecting Tool must itself be able to read the
# Secret it names; the proxy checks this on every call.
- apiGroups: [""]
  resources: [secrets]
  resourceNames: [${UPSTREAM_TOKEN_SECRET}]
  verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: native-mcp-client}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: native-mcp-client}
subjects: [{kind: ServiceAccount, name: native-mcp-client, namespace: orka-system}]
YAML
  run_tool_client native-mcp-client native-mcp native 'mcp-e2e-ok:native-mcp:native'
  # v1 Actors have no egress without an EgressPolicy rule.
  run_tool_client native-mcp-default-deny native-mcp egress-probe 'mcp-e2e-egress:blocked'
  # The gateway replaces only a header the Actor sends; it never adds one.
  run_tool_client native-mcp-egress-no-header native-mcp-egress egress-probe-unauthenticated 'mcp-e2e-egress:missing'
  run_tool_client native-mcp-egress-injected native-mcp-egress egress-probe "mcp-e2e-egress:sha256:$(expected_injected_digest)"
  log 'Egress gateway injected the Tool credential without exposing it to the Actor'
  local trace_id
  trace_id="$(openssl rand -hex 16)"
  # The Actor must receive the caller's trace still sampled; a span exporter
  # behind the router (such as the ACP supervisor's) drops unsampled parents.
  run_tool_client native-mcp-trace native-mcp trace-context "mcp-e2e-trace:${trace_id}:01" "00-${trace_id}-$(openssl rand -hex 8)-01"
  verify_mcp_trace "${trace_id}"
  local tool
  for tool in native-mcp native-mcp-egress; do
    kubectl -n orka-system delete tool "${tool}" --wait=false
    wait_absent tool "${tool}"
  done
  kubectl -n orka-system delete substrateactorpool native-mcp --wait=false
  wait_absent substrateactorpool native-mcp
  kubectl -n orka-system delete role,rolebinding,serviceaccount native-mcp-client
}
main() {
  for command in docker git go jq kind ko kubectl openssl python3 curl sha256sum; do command -v "${command}" >/dev/null || { echo "${command} is required" >&2; return 1; }; done
  mkdir -p "${TMP_ROOT}/docker-config"
  chmod 700 "${TMP_ROOT}"
  export DOCKER_CONFIG="${TMP_ROOT}/docker-config"
  printf '{"auths":{}}\n' >"${DOCKER_CONFIG}/config.json"
  trap cleanup EXIT
  substrate_prepare_upstream "${ROOT_DIR}" "${TMP_ROOT}" "${KIND_CLUSTER}"
  CLUSTER_PREPARED=1
  (cd "${SUBSTRATE_DIR}" && go build -o "${TMP_ROOT}/kubectl-ate" ./cmd/kubectl-ate)
  local controller direct mcp mcp_local client conformance worker public_key registry_ip
  controller="$(build_image controller Dockerfile)"
  direct="$(build_image workspace-agent cmd/orka-workspace-agent/Dockerfile)"
  mcp="$(build_image mcp-e2e-server cmd/orka-mcp-e2e-server/Dockerfile)"
  client="$(build_image tool-e2e-client cmd/orka-tool-e2e-client/Dockerfile)"
  conformance="$(build_image substrate-conformance scripts/fixtures/substrate-conformance/Dockerfile)"
  registry_ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' kind-registry)"
  [[ -n "${registry_ip}" ]] || { echo 'shared registry is not reachable from kind' >&2; return 1; }
  # Actor image pulls happen inside gVisor and use the registry's kind-network
  # address; ordinary Pods keep the node-mirrored localhost reference.
  mcp_local="${mcp}"
  direct="${direct/localhost:${KIND_REGISTRY_PORT}/${registry_ip}:5000}"
  mcp="${mcp/localhost:${KIND_REGISTRY_PORT}/${registry_ip}:5000}"
  openssl rand -hex 32 >"${TMP_ROOT}/bootstrap-token"
  chmod 600 "${TMP_ROOT}/bootstrap-token"
  public_key="$(cd "${ROOT_DIR}" && go run ./cmd/orka-substrate-doctor --public-key-from="${TMP_ROOT}/bootstrap-token")"
  worker="$(publish_worker_image)"
  create_native_resources "${worker}" "${direct}" "${mcp}" "${public_key}"
  grant_credential_provider_namespace
  deploy_credential_upstream "${mcp_local}"
  kubectl -n orka-system create secret generic orka-substrate-bootstrap --from-file="token=${TMP_ROOT}/bootstrap-token" --dry-run=client -o yaml | kubectl apply -f -
  create_upstream_token
  deploy_otel_collector
  deploy_orka "${controller}"
  exercise_direct "${conformance}"
  exercise_mcp "${client}"
  substrate_require_clean_upstream "${SUBSTRATE_DIR}"
  log 'Unmodified upstream Substrate conformance passed'
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
