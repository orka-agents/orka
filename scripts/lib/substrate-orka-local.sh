#!/usr/bin/env bash
# Orka and the local provider fixture used by native Substrate conformance.

deploy_responses_fixture() {
  local image="$1"

  log "Deploying local Responses-compatible provider fixture"
  kubectl create namespace vekil-system --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n vekil-system apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vekil
  labels:
    app.kubernetes.io/name: vekil
    app.kubernetes.io/component: responses-fixture
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: vekil
      app.kubernetes.io/component: responses-fixture
  template:
    metadata:
      labels:
        app.kubernetes.io/name: vekil
        app.kubernetes.io/component: responses-fixture
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: responses
          image: ${image}
          imagePullPolicy: Always
          ports:
            - name: http
              containerPort: 1337
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
---
apiVersion: v1
kind: Service
metadata:
  name: vekil
  labels:
    app.kubernetes.io/name: vekil
spec:
  selector:
    app.kubernetes.io/name: vekil
    app.kubernetes.io/component: responses-fixture
  ports:
    - name: http
      port: 1337
      targetPort: http
YAML
  # The fixture stores request counts in memory. Force a new Pod even when a
  # reused cluster receives the same fixed image and Pod template.
  kubectl -n vekil-system rollout restart deployment/vekil
  kubectl -n vekil-system rollout status deployment/vekil --timeout=2m
}

grant_substrate_worker_access() {
  # Match the production chart's worker namespace permissions. The generated
  # manager RoleBinding applies only in the controller's tenant namespace.
  jq -n --arg namespace "${ORKA_NAMESPACE}" '{apiVersion:"v1",kind:"List",items:[
    {apiVersion:"rbac.authorization.k8s.io/v1",kind:"Role",
      metadata:{name:"orka-substrate-worker",namespace:"ate-demo"},rules:[
        {apiGroups:[""],resources:["pods"],verbs:["get","list","delete"]},
        {apiGroups:["networking.k8s.io"],resources:["networkpolicies"],verbs:["get","list","watch","create","update","patch","delete"]}]},
    {apiVersion:"rbac.authorization.k8s.io/v1",kind:"RoleBinding",
      metadata:{name:"orka-substrate-worker",namespace:"ate-demo"},
      roleRef:{apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"orka-substrate-worker"},
      subjects:[{kind:"ServiceAccount",name:"orka-controller-manager",namespace:$namespace}]}]}' |
    kubectl -n ate-demo apply -f - || return 1

  local controller_user="system:serviceaccount:${ORKA_NAMESPACE}:orka-controller-manager" permission verb resource
  if ! kubectl auth can-i list workerpools.ate.dev --all-namespaces --as="${controller_user}" --quiet; then
    printf 'Orka lacks cluster-wide WorkerPool discovery permission\n' >&2
    return 1
  fi
  for permission in get:pods list:pods delete:pods get:networkpolicies list:networkpolicies watch:networkpolicies create:networkpolicies update:networkpolicies patch:networkpolicies delete:networkpolicies; do
    verb="${permission%%:*}"
    resource="${permission#*:}"
    if ! kubectl -n ate-demo auth can-i "${verb}" "${resource}" --as="${controller_user}" --quiet; then
      printf 'Orka lacks %s on %s in the dedicated Substrate worker namespace\n' "${verb}" "${resource}" >&2
      return 1
    fi
  done
}

deploy_orka() {
  local controller_image="$1"
  local codex_runtime_actor_ref="${2:-}"
  local tmp_config
  tmp_config="$(mktemp -d "${TMP_ROOT}/orka-config.XXXXXX")"

  log "Regenerating manifests and installing Orka CRDs"
  make -C "${ROOT_DIR}" manifests generate
  make -C "${ROOT_DIR}" install
  make -C "${ROOT_DIR}" kustomize
  if [[ "${SUBSTRATE_E2E_SUSPEND_RESUME}" == "1" ]]; then
    log "Bootstrapping test-only admission TLS"
    orka_e2e_remove_admission_webhooks
    orka_e2e_bootstrap_admission_tls kubectl "${ORKA_NAMESPACE}"
  fi

  cp -R "${ROOT_DIR}/config" "${tmp_config}/config"
  (cd "${tmp_config}/config/manager" && "${ROOT_DIR}/bin/kustomize" edit set image "controller=${controller_image}")
  (cd "${tmp_config}/config/provider-proxy" && "${ROOT_DIR}/bin/kustomize" edit set image "controller=${controller_image}")
  # Agent Substrate validation exercises the workspace provider directly. Omit
  # the unrelated clean-room publisher and SCM proxy workloads, but retain the
  # authenticated provider proxy used by the real Codex prompt smoke.
  (
    cd "${tmp_config}/config/acp-workload"
    "${ROOT_DIR}/bin/kustomize" edit remove resource ../publisher
    "${ROOT_DIR}/bin/kustomize" edit remove resource ../scm-egress-proxy
  )
  local placeholder_digest codex_runtime_image
  placeholder_digest="sha256:$(printf '0%.0s' {1..64})"
  codex_runtime_image="${codex_runtime_actor_ref:-example.invalid/orka/acp-codex@${placeholder_digest}}"
  kubectl -n orka-system create configmap acp-runtime-images \
    --from-literal="ORKA_ACP_CODEX_RUNTIME_IMAGE=${codex_runtime_image}" \
    --from-literal="ORKA_ACP_CLAUDE_RUNTIME_IMAGE=example.invalid/orka/acp-claude@${placeholder_digest}" \
    --from-literal="ORKA_ACP_COPILOT_RUNTIME_IMAGE=example.invalid/orka/acp-copilot@${placeholder_digest}" \
    --from-literal="ORKA_ACP_OPENCODE_RUNTIME_IMAGE=example.invalid/orka/acp-opencode@${placeholder_digest}" \
    --dry-run=client -o yaml | kubectl apply -f -

  local capability_dir snapshot_key_field artifact_capability_field publisher_controller_field publisher_operation_field provider_field
  capability_dir="$(mktemp -d "${TMP_ROOT}/acp-capabilities.XXXXXX")"
  snapshot_key_field="snapshot-key"
  artifact_capability_field="capability-secret"
  publisher_controller_field="controller-token"
  publisher_operation_field="operation-capability-secret"
  provider_field="token"
  chmod 0700 "${capability_dir}"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/snapshot-key"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/artifact-capability"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/publisher-token"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/publisher-capability"
  # The RuntimePool provider token must be printable (it is compared and
  # copied into pool Secrets); raw random bytes are rejected.
  openssl rand -hex 32 >"${capability_dir}/provider-token"
  chmod 0600 "${capability_dir}"/*
  kubectl -n orka-system create secret generic agent-execution-snapshot-key \
    --from-file="${snapshot_key_field}=${capability_dir}/snapshot-key" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n orka-system create secret generic acp-artifact-capability \
    --from-file="${artifact_capability_field}=${capability_dir}/artifact-capability" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n orka-system create secret generic workspace-publisher-auth \
    --from-file="${publisher_controller_field}=${capability_dir}/publisher-token" \
    --from-file="${publisher_operation_field}=${capability_dir}/publisher-capability" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n orka-system create secret generic provider-auth-proxy \
    --from-file="${provider_field}=${capability_dir}/provider-token" \
    --dry-run=client -o yaml | kubectl apply -f -
  rm -rf "${capability_dir}"

  bash "${ROOT_DIR}/scripts/lib/ensure-static-mode-namespace.sh" \
    kubectl "${ORKA_NAMESPACE}" harness-v2
  "${ROOT_DIR}/bin/kustomize" build "${tmp_config}/config/acp-workload" | kubectl apply -f -
  grant_substrate_worker_access
  if [[ "${SUBSTRATE_E2E_SUSPEND_RESUME}" == "1" ]]; then
    log "Deploying the dedicated fail-closed admission runtime"
    orka_e2e_deploy_admission "${controller_image}" kubectl "${ORKA_NAMESPACE}"
  fi
  # Substrate actor traffic originates from its single-workload WorkerPool Pod
  # in ate-demo rather than a native orka-runtimes Pod. Keep the same
  # authenticated proxy boundary while allowing only that provider namespace.
  kubectl -n orka-system patch networkpolicy orka-provider-auth-proxy --type=json -p '[
    {
      "op": "add",
      "path": "/spec/ingress/-",
      "value": {
        "from": [
          {
            "namespaceSelector": {
              "matchLabels": {
                "kubernetes.io/metadata.name": "ate-demo"
              }
            }
          }
        ],
        "ports": [
          {
            "protocol": "TCP",
            "port": 8080
          }
        ]
      }
    }
  ]'
  kubectl -n orka-system rollout status deployment/orka-provider-auth-proxy --timeout=5m

  local patch
  local workspace_dispatch="false"
  if [[ "${SUBSTRATE_E2E_ACP_TASK_SMOKE}" == "1" || "${SUBSTRATE_E2E_SUSPEND_RESUME}" == "1" || "${SUBSTRATE_E2E_LIFECYCLE}" == "1" ]]; then
    workspace_dispatch="true"
  fi
  local workspace_api="false"
  if [[ "${SUBSTRATE_E2E_SUSPEND_RESUME}" == "1" ]]; then
    workspace_api="true"
    # The dedicated admission runtime above is the API server boundary. These
    # controller flags also register equivalent local handlers, so give the
    # manager webhook server a certificate even though no Service routes to it.
    local webhook_cert_dir
    webhook_cert_dir="$(mktemp -d "${TMP_ROOT}/webhook-certs.XXXXXX")"
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
      -keyout "${webhook_cert_dir}/tls.key" -out "${webhook_cert_dir}/tls.crt" \
      -subj "/CN=orka-controller-manager.orka-system.svc" >/dev/null 2>&1
    kubectl -n orka-system create secret tls orka-webhook-serving-certs \
      --cert="${webhook_cert_dir}/tls.crt" --key="${webhook_cert_dir}/tls.key" \
      --dry-run=client -o yaml | kubectl apply -f -
    rm -rf "${webhook_cert_dir}"
  fi
  patch="$(jq -cn \
    --arg bootstrap_secret_name "${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME}" \
    --arg bootstrap_secret_key "${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY}" \
    --arg workspaceDispatch "${workspace_dispatch}" \
    --arg workspaceAPI "${workspace_api}" \
    --arg ambiguityMarker "${LIFECYCLE_AMBIGUITY_MARKER}" \
    '{
      spec: {
        template: {
          spec: {
            containers: [
              {
                name: "manager",
                # The Substrate-only deployment removes the Publisher workload.
                # Disable the client explicitly so fail-closed capability
                # negotiation does not target a Service that is intentionally
                # absent from this direct provider evaluation cluster.
                env: [
                  {
                    name: "ORKA_WORKSPACE_PUBLISHER_URL",
                    "$patch": "delete"
                  }
                ],
                imagePullPolicy: "IfNotPresent",
                resources: {
                  requests: { cpu: "250m", memory: "256Mi" },
                  limits: { cpu: "2", memory: "1Gi" }
                },
                livenessProbe: {
                  httpGet: { path: "/healthz", port: 8081 },
                  initialDelaySeconds: 30,
                  periodSeconds: 20,
                  timeoutSeconds: 5,
                  failureThreshold: 6
                },
                readinessProbe: {
                  httpGet: { path: "/readyz", port: 8081 },
                  initialDelaySeconds: 10,
                  periodSeconds: 10,
                  timeoutSeconds: 5,
                  failureThreshold: 6
                },
                args: ([
                  "--leader-elect",
                  "--health-probe-bind-address=:8081",
                  "--agent-execution-snapshot-key-file=/var/run/orka/agent-execution-snapshot/key",
                  "--controller-url=http://orka-api.orka-system.svc:8080",
                  "--controller-mode=harness-v2",
                  "--watch-namespace=orka-system",
                  "--enforce-namespace-isolation=true",
                  "--execution-mode-controller-usernames=system:serviceaccount:orka-system:orka-controller-manager",
                  "--execution-workspace-default-provider=substrate",
                  "--agent-sandbox-enabled=false",
                  "--substrate-enabled=true",
                  "--substrate-direct-egress-enabled=true",
                  "--substrate-api-endpoint=api.ate-system.svc:443",
                  "--substrate-api-ca-file=/run/substrate-server/trust-bundle.pem",
                  "--substrate-api-cert-file=/run/substrate-client/credential-bundle.pem",
                  "--substrate-api-key-file=/run/substrate-client/credential-bundle.pem",
                  "--substrate-router-url=http://atenet-router.ate-system.svc",
                  "--substrate-actor-dns-suffix=actors.resources.substrate.ate.dev",
                  "--substrate-default-template=orka-direct",
                  "--substrate-default-template-namespace=orka-system",
                  "--substrate-bootstrap-token-secret-name=" + $bootstrap_secret_name,
                  "--substrate-bootstrap-token-secret-key=" + $bootstrap_secret_key,
                  "--substrate-claim-timeout=2m",
                  "--substrate-command-timeout=10m",
                  "--substrate-cleanup-policy=delete",
                  "--acp-workspace-dispatch-enabled=" + $workspaceDispatch,
                  "--acp-e2e-prompt-write-ambiguity-marker=" + $ambiguityMarker,
                  # RuntimePool reconciliation and prompt execution use the
                  # authenticated provider-proxy boundary deployed above; the
                  # token Secret is created above and mounted by the base manifest.
                  "--acp-provider-proxy-base-url=http://orka-provider-auth-proxy.orka-system.svc:8080",
                  "--acp-provider-proxy-namespace=orka-system",
                  "--acp-provider-proxy-pod-labels=orka.ai/network-role=provider-auth-proxy",
                  "--acp-provider-proxy-token-file=/var/run/orka/provider-auth/token"
                ] + (if $workspaceAPI == "true" then [
                  "--enable-workspace-provider-api=true",
                  "--workspace-class-use-admission-enabled=true",
                  "--task-provenance-admission-enabled=true"
                ] else [] end)),
                volumeMounts: (if $workspaceAPI == "true" then [
                  {
                    name: "webhook-serving-certs",
                    mountPath: "/tmp/k8s-webhook-server/serving-certs",
                    readOnly: true
                  }
                ] else [] end)
              }
            ],
            volumes: (if $workspaceAPI == "true" then [
              {
                name: "webhook-serving-certs",
                secret: { secretName: "orka-webhook-serving-certs" }
              }
            ] else [] end)
          }
        }
      }
    }')"
  kubectl -n orka-system patch deployment orka-controller-manager --type=strategic -p "${patch}"
  mount_substrate_identity deployment orka-controller-manager manager
  kubectl -n orka-system rollout status deployment/orka-controller-manager --timeout=5m
}
