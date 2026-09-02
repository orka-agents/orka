#!/usr/bin/env bash
# Shared test-only bootstrap for fresh-cluster E2E runs. Production deployment
# remains fail-closed and requires callers to provision this Secret explicitly.

_orka_e2e_render_admission_runtime() {
  local namespace="$1"
  local controller_image="$2"

  awk -v image="${controller_image}" -v target_namespace="${namespace}" '
    /image: controller:latest$/ {
      sub(/image: controller:latest$/, "image: " image)
      replacements++
    }
    {
      gsub(/orka-system/, target_namespace)
      print
    }
    END { if (replacements != 1) exit 42 }
  '
}

_orka_e2e_render_admission_webhooks() {
  local namespace="$1"
  local ca_bundle="$2"

  awk -v ca="${ca_bundle}" -v target_namespace="${namespace}" '
    {
      gsub(/orka-system/, target_namespace)
      print
    }
    /^[[:space:]]*clientConfig:[[:space:]]*$/ {
      indentation = $0
      sub(/clientConfig:[[:space:]]*$/, "", indentation)
      print indentation "  caBundle: " ca
      replacements++
    }
    END { if (replacements == 0) exit 43 }
  '
}

orka_e2e_bootstrap_admission_tls() (
  set -Eeuo pipefail

  local kubectl_bin="${1:-kubectl}"
  local namespace="${2:-${ORKA_NAMESPACE:-orka-system}}"
  local secret_name="orka-admission-tls"
  local service_name="orka-admission.${namespace}.svc"
  local library_dir tls_dir

  library_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

  for command in openssl "${kubectl_bin}"; do
    command -v "${command}" >/dev/null 2>&1 || {
      echo "missing required command: ${command}" >&2
      return 1
    }
  done

  tls_dir="$(mktemp -d "${TMPDIR:-/tmp}/orka-e2e-admission-tls.XXXXXX")"
  chmod 700 "${tls_dir}"
  trap 'rm -rf -- "${tls_dir}"' EXIT
  umask 077

  cat >"${tls_dir}/ca.conf" <<'EOF_CA_CONFIG'
[req]
prompt = no
distinguished_name = ca_name
x509_extensions = ca_extensions

[ca_name]
CN = Orka E2E admission CA

[ca_extensions]
basicConstraints = critical, CA:true
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always
EOF_CA_CONFIG

  cat >"${tls_dir}/serving.conf" <<EOF_SERVING_CONFIG
[req]
prompt = no
distinguished_name = serving_name
req_extensions = serving_extensions

[serving_name]
CN = ${service_name}

[serving_extensions]
basicConstraints = critical, CA:false
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:${service_name},DNS:${service_name}.cluster.local
subjectKeyIdentifier = hash
EOF_SERVING_CONFIG

  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 7 \
    -config "${tls_dir}/ca.conf" \
    -keyout "${tls_dir}/ca.key" \
    -out "${tls_dir}/ca.crt" >/dev/null 2>&1
  openssl req -new -newkey rsa:2048 -nodes -sha256 \
    -config "${tls_dir}/serving.conf" \
    -keyout "${tls_dir}/tls.key" \
    -out "${tls_dir}/tls.csr" >/dev/null 2>&1
  openssl x509 -req -sha256 -days 7 \
    -in "${tls_dir}/tls.csr" \
    -CA "${tls_dir}/ca.crt" \
    -CAkey "${tls_dir}/ca.key" \
    -CAcreateserial \
    -extfile "${tls_dir}/serving.conf" \
    -extensions serving_extensions \
    -out "${tls_dir}/tls.crt" >/dev/null 2>&1
  openssl verify -CAfile "${tls_dir}/ca.crt" "${tls_dir}/tls.crt" >/dev/null 2>&1

  bash "${library_dir}/ensure-static-mode-namespace.sh" \
    "${kubectl_bin}" "${namespace}" harness-v2
  "${kubectl_bin}" -n "${namespace}" create secret generic "${secret_name}" \
    --type=kubernetes.io/tls \
    --from-file="tls.crt=${tls_dir}/tls.crt" \
    --from-file="tls.key=${tls_dir}/tls.key" \
    --from-file="ca.crt=${tls_dir}/ca.crt" \
    --dry-run=client -o yaml \
    | "${kubectl_bin}" apply -f - >/dev/null
)

# A reused E2E cluster may still have a fail-closed webhook configuration that
# trusts the previous run's CA. Remove that routing before rotating the
# test-only serving certificate. The ready admission runtime is installed and
# the webhook routing is restored by orka_e2e_deploy_admission.
orka_e2e_remove_admission_webhooks() {
  local kubectl_bin="${1:-kubectl}"

  "${kubectl_bin}" delete validatingwebhookconfiguration orka-admission \
    --ignore-not-found=true >/dev/null
}

# Install the checked-in admission runtime with the exact controller image
# under test, wait for both replicas to become Service endpoints, smoke every
# handler, then publish the checked-in fail-closed webhook configuration with
# the E2E CA bundle.
orka_e2e_deploy_admission() (
  set -Eeuo pipefail

  local controller_image="$1"
  local kubectl_bin="${2:-kubectl}"
  local namespace="${3:-${ORKA_NAMESPACE:-orka-system}}"
  local library_dir repository_root render_dir ca_bundle endpoint_count
  local admission_proxy_pid=""
  local admission_proxy_port=""
  local admission_handlers admission_proxy_log admission_smoke_request admission_smoke_response

  _orka_e2e_stop_admission_proxy() {
    if [[ -n "${admission_proxy_pid}" ]]; then
      kill "${admission_proxy_pid}" 2>/dev/null || true
      wait "${admission_proxy_pid}" 2>/dev/null || true
      admission_proxy_pid=""
    fi
  }

  _orka_e2e_cleanup_admission_deploy() {
    _orka_e2e_stop_admission_proxy
    rm -rf -- "${render_dir}"
  }

  _orka_e2e_start_admission_proxy() {
    local attempt line

    admission_proxy_port=""
    : >"${admission_proxy_log}"
    "${kubectl_bin}" proxy \
      --address=127.0.0.1 \
      --accept-hosts='^127\.0\.0\.1$' \
      --port=0 \
      >"${admission_proxy_log}" 2>&1 &
    admission_proxy_pid=$!

    for attempt in {1..50}; do
      while IFS= read -r line; do
        if [[ "${line}" =~ ^Starting[[:space:]]+to[[:space:]]+serve[[:space:]]+on[[:space:]]+127\.0\.0\.1:([0-9]+)$ ]]; then
          admission_proxy_port="${BASH_REMATCH[1]}"
          return 0
        fi
      done <"${admission_proxy_log}"
      if ! kill -0 "${admission_proxy_pid}" 2>/dev/null; then
        echo "kubectl proxy exited before the E2E admission smoke endpoint was ready" >&2
        return 1
      fi
      sleep 0.1
    done

    echo "kubectl proxy did not publish an E2E admission smoke endpoint" >&2
    return 1
  }

  _orka_e2e_smoke_admission_handlers() {
    local handler group version kind resource uid service_proxy

    _orka_e2e_start_admission_proxy || return 1
    service_proxy="http://127.0.0.1:${admission_proxy_port}/api/v1/namespaces/${namespace}/services/https:orka-admission:443/proxy"
    while IFS='|' read -r handler group version kind resource; do
      [[ -n "${handler}" ]] || continue
      uid="orka-admission-smoke-${resource}"
      jq -n \
        --arg uid "${uid}" \
        --arg group "${group}" \
        --arg version "${version}" \
        --arg kind "${kind}" \
        --arg resource "${resource}" \
        --arg namespace "${namespace}" '
        {
          apiVersion: "admission.k8s.io/v1",
          kind: "AdmissionReview",
          request: {
            uid: $uid,
            kind: {group: $group, version: $version, kind: $kind},
            resource: {group: $group, version: $version, resource: $resource},
            requestKind: {group: $group, version: $version, kind: $kind},
            requestResource: {group: $group, version: $version, resource: $resource},
            name: "orka-admission-smoke",
            namespace: (if $kind == "Namespace" then "" else $namespace end),
            operation: "CREATE",
            userInfo: {username: "system:admin", groups: ["system:masters"]},
            object: {
              apiVersion: (if $group == "" then $version else ($group + "/" + $version) end),
              kind: $kind,
              metadata: ({name: "orka-admission-smoke"} +
                (if $kind == "Namespace" then {labels: {"orka.ai/controller-mode": "harness-v2"}}
                 elif $kind == "Secret" then
                   {namespace: $namespace, labels: {"workspace.orka.ai/attachment-for": "smoke-workspace-uid"}}
                 else {namespace: $namespace} end))
            },
            oldObject: null,
            dryRun: true,
            options: {apiVersion: "meta.k8s.io/v1", kind: "CreateOptions"}
          }
        }
      ' >"${admission_smoke_request}"
      if ! curl \
        --fail \
        --silent \
        --show-error \
        --noproxy '*' \
        --max-time 15 \
        --header 'Content-Type: application/json' \
        --data-binary "@${admission_smoke_request}" \
        "${service_proxy}${handler}" \
        >"${admission_smoke_response}" \
        || ! jq -e --arg uid "${uid}" '
          .apiVersion == "admission.k8s.io/v1" and
          .kind == "AdmissionReview" and
          .response.uid == $uid and
          (.response.allowed | type == "boolean")
        ' "${admission_smoke_response}" >/dev/null; then
        echo "orka-admission E2E handler smoke failed: ${handler}" >&2
        return 1
      fi
    done <"${admission_handlers}"
    _orka_e2e_stop_admission_proxy
  }

  [[ -n "${controller_image}" ]] || {
    echo "controller image is required for E2E admission" >&2
    return 1
  }
  for command in awk curl jq "${kubectl_bin}"; do
    command -v "${command}" >/dev/null 2>&1 || {
      echo "missing required command: ${command}" >&2
      return 1
    }
  done

  library_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
  repository_root="$(cd "${library_dir}/../.." && pwd -P)"
  render_dir="$(mktemp -d "${TMPDIR:-/tmp}/orka-e2e-admission.XXXXXX")"
  admission_handlers="${render_dir}/admission-handlers.txt"
  admission_proxy_log="${render_dir}/kubectl-proxy.log"
  admission_smoke_request="${render_dir}/admission-smoke-request.json"
  admission_smoke_response="${render_dir}/admission-smoke-response.json"
  trap _orka_e2e_cleanup_admission_deploy EXIT

  cat >"${admission_handlers}" <<'EOF_ADMISSION_HANDLERS'
/validate-v1-namespace-execution-mode||v1|Namespace|namespaces
/validate-v1-secret-workspace-attachment||v1|Secret|secrets
/validate-coordination-k8s-io-v1-acp-suspend-quota-lease|coordination.k8s.io|v1|Lease|leases
/validate-core-orka-ai-v1alpha1-task-provenance|core.orka.ai|v1alpha1|Task|tasks
/validate-core-orka-ai-v1alpha1-task-workspace-class-use|core.orka.ai|v1alpha1|Task|tasks-workspace-class-use
/validate-core-orka-ai-v1alpha1-tool-workspace-class-use|core.orka.ai|v1alpha1|Tool|tools
/validate-core-orka-ai-v1alpha1-agent-contract|core.orka.ai|v1alpha1|Agent|agents
/validate-core-orka-ai-v1alpha1-agentruntime-contract|core.orka.ai|v1alpha1|AgentRuntime|agentruntimes
/validate-core-orka-ai-v1alpha1-task-execution-authority|core.orka.ai|v1alpha1|Task|tasks-execution-authority
EOF_ADMISSION_HANDLERS

  ca_bundle="$("${kubectl_bin}" -n "${namespace}" get secret orka-admission-tls \
    -o jsonpath='{.data.ca\.crt}')"
  [[ -n "${ca_bundle}" ]] || {
    echo "${namespace}/orka-admission-tls has no ca.crt" >&2
    return 1
  }

  "${kubectl_bin}" kustomize "${repository_root}/config/orka-admission" |
    _orka_e2e_render_admission_runtime "${namespace}" "${controller_image}" \
      >"${render_dir}/runtime.yaml"
  "${kubectl_bin}" apply -f "${render_dir}/runtime.yaml" >/dev/null
  "${kubectl_bin}" -n "${namespace}" rollout status deployment/orka-admission --timeout=3m

  endpoint_count=0
  for ((attempt = 0; attempt < 120; attempt++)); do
    endpoint_count="$("${kubectl_bin}" -n "${namespace}" get endpoints orka-admission -o json |
      jq '[.subsets[]?.addresses[]?] | length')"
    if ((endpoint_count >= 2)); then
      break
    fi
    sleep 1
  done
  if ((endpoint_count < 2)); then
    echo "${namespace}/orka-admission exposed ${endpoint_count} ready endpoint(s), want 2" >&2
    return 1
  fi

  "${kubectl_bin}" kustomize "${repository_root}/config/orka-admission-webhooks" |
    _orka_e2e_render_admission_webhooks "${namespace}" "${ca_bundle}" \
      >"${render_dir}/webhooks.yaml"
  if ! awk '
    NR == FNR {
      split($0, fields, "|")
      expected[fields[1]]++
      expected_count++
      next
    }
    {
      line = $0
      sub(/^[[:space:]]*/, "", line)
      split(line, fields, /[[:space:]]+/)
      if (fields[1] == "path:" && fields[2] ~ /^\/validate-/) {
        actual[fields[2]]++
        actual_count++
      }
    }
    END {
      if (expected_count != actual_count) exit 1
      for (path in expected) {
        if (expected[path] != 1 || actual[path] != 1) exit 1
      }
      for (path in actual) {
        if (expected[path] != 1) exit 1
      }
    }
  ' "${admission_handlers}" "${render_dir}/webhooks.yaml"; then
    echo "E2E admission smoke inventory does not match the rendered webhook handlers" >&2
    return 1
  fi

  _orka_e2e_smoke_admission_handlers
  "${kubectl_bin}" apply -f "${render_dir}/webhooks.yaml" >/dev/null

  "${kubectl_bin}" get validatingwebhookconfiguration orka-admission -o json |
    jq -e --arg ca "${ca_bundle}" \
      '.webhooks | length > 0 and all(.[]; .clientConfig.caBundle == $ca)' >/dev/null
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  orka_e2e_bootstrap_admission_tls "$@"
fi
