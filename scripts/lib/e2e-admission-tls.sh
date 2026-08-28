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
# under test, wait for both replicas to become Service endpoints, then publish
# the checked-in fail-closed webhook configuration with the E2E CA bundle.
orka_e2e_deploy_admission() (
  set -Eeuo pipefail

  local controller_image="$1"
  local kubectl_bin="${2:-kubectl}"
  local namespace="${3:-${ORKA_NAMESPACE:-orka-system}}"
  local library_dir repository_root render_dir ca_bundle endpoint_count

  [[ -n "${controller_image}" ]] || {
    echo "controller image is required for E2E admission" >&2
    return 1
  }
  for command in awk jq "${kubectl_bin}"; do
    command -v "${command}" >/dev/null 2>&1 || {
      echo "missing required command: ${command}" >&2
      return 1
    }
  done

  library_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
  repository_root="$(cd "${library_dir}/../.." && pwd -P)"
  render_dir="$(mktemp -d "${TMPDIR:-/tmp}/orka-e2e-admission.XXXXXX")"
  trap 'rm -rf -- "${render_dir}"' EXIT

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
  "${kubectl_bin}" apply -f "${render_dir}/webhooks.yaml" >/dev/null

  "${kubectl_bin}" get validatingwebhookconfiguration orka-admission -o json |
    jq -e --arg ca "${ca_bundle}" \
      '.webhooks | length > 0 and all(.[]; .clientConfig.caBundle == $ca)' >/dev/null
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  orka_e2e_bootstrap_admission_tls "$@"
fi
