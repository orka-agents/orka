#!/usr/bin/env bash
# Orka deployment used by native Substrate MCP tool conformance.

deploy_orka() {
  local controller_image="$1"
  local tmp_config
  tmp_config="$(mktemp -d "${TMP_ROOT}/orka-config.XXXXXX")"

  log "Regenerating manifests and installing Orka CRDs"
  make -C "${ROOT_DIR}" manifests generate
  make -C "${ROOT_DIR}" install
  make -C "${ROOT_DIR}" kustomize
  log "Bootstrapping test-only admission TLS"
  orka_e2e_remove_admission_webhooks
  orka_e2e_bootstrap_admission_tls kubectl "${ORKA_NAMESPACE}"

  cp -R "${ROOT_DIR}/config" "${tmp_config}/config"
  (cd "${tmp_config}/config/manager" && "${ROOT_DIR}/bin/kustomize" edit set image "controller=${controller_image}")
  (cd "${tmp_config}/config/provider-proxy" && "${ROOT_DIR}/bin/kustomize" edit set image "controller=${controller_image}")
  # This run exercises MCP tools. Keep the base controller configuration and
  # provider proxy, while omitting unused publisher and SCM proxy workloads.
  (
    cd "${tmp_config}/config/acp-workload"
    "${ROOT_DIR}/bin/kustomize" edit remove resource ../publisher
    "${ROOT_DIR}/bin/kustomize" edit remove resource ../scm-egress-proxy
  )
  local placeholder_digest
  placeholder_digest="sha256:$(printf '0%.0s' {1..64})"
  kubectl -n orka-system create configmap acp-runtime-images \
    --from-literal="ORKA_ACP_CODEX_RUNTIME_IMAGE=example.invalid/orka/acp-codex@${placeholder_digest}" \
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
  # Publisher credentials must be printable, with identical bytes after both
  # the controller and Publisher read their mounted files.
  openssl rand -hex 32 | tr -d '\n' >"${capability_dir}/publisher-token"
  openssl rand -hex 32 | tr -d '\n' >"${capability_dir}/publisher-capability"
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
  log "Deploying the dedicated fail-closed admission runtime"
  orka_e2e_deploy_admission "${controller_image}" kubectl "${ORKA_NAMESPACE}"
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
  patch="$(jq -cn \
    --arg bootstrap_secret_name "${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME}" \
    --arg bootstrap_secret_key "${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY}" \
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
                  "--substrate-mcp-tools-enabled=true",
                  "--substrate-api-endpoint=api.ate-system.svc:443",
                  "--substrate-api-ca-file=/run/substrate-server/trust-bundle.pem",
                  "--substrate-api-cert-file=/run/substrate-client/credential-bundle.pem",
                  "--substrate-api-key-file=/run/substrate-client/credential-bundle.pem",
                  "--substrate-router-url=http://atenet-router.ate-system.svc",
                  "--substrate-actor-dns-suffix=actors.resources.substrate.ate.dev",
                  "--substrate-default-template=orka-mcp",
                  "--substrate-default-template-namespace=orka-system",
                  "--substrate-bootstrap-token-secret-name=" + $bootstrap_secret_name,
                  "--substrate-bootstrap-token-secret-key=" + $bootstrap_secret_key,
                  "--substrate-claim-timeout=2m",
                  "--substrate-command-timeout=10m",
                  "--substrate-cleanup-policy=delete",
                  "--acp-workspace-dispatch-enabled=false",
                  # RuntimePool reconciliation and prompt execution use the
                  # authenticated provider-proxy boundary deployed above; the
                  # token Secret is created above and mounted by the base manifest.
                  "--acp-provider-proxy-base-url=http://orka-provider-auth-proxy.orka-system.svc:8080",
                  "--acp-provider-proxy-namespace=orka-system",
                  "--acp-provider-proxy-pod-labels=orka.ai/network-role=provider-auth-proxy",
                  "--acp-provider-proxy-token-file=/var/run/orka/provider-auth/token"
                ])
              }
            ]
          }
        }
      }
    }')"
  kubectl -n orka-system patch deployment orka-controller-manager --type=strategic -p "${patch}"
  mount_substrate_identity deployment orka-controller-manager manager
  kubectl -n orka-system rollout status deployment/orka-controller-manager --timeout=5m
}
