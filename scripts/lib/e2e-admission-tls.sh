#!/usr/bin/env bash
# Shared test-only bootstrap for fresh-cluster E2E runs. Production deployment
# remains fail-closed and requires callers to provision this Secret explicitly.

orka_e2e_bootstrap_admission_tls() (
  set -Eeuo pipefail

  local kubectl_bin="${1:-kubectl}"
  local namespace="orka-system"
  local secret_name="orka-admission-tls"
  local service_name="orka-admission.orka-system.svc"
  local tls_dir

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

  "${kubectl_bin}" create namespace "${namespace}" --dry-run=client -o yaml \
    | "${kubectl_bin}" apply -f - >/dev/null
  "${kubectl_bin}" -n "${namespace}" create secret generic "${secret_name}" \
    --type=kubernetes.io/tls \
    --from-file="tls.crt=${tls_dir}/tls.crt" \
    --from-file="tls.key=${tls_dir}/tls.key" \
    --from-file="ca.crt=${tls_dir}/ca.crt" \
    --dry-run=client -o yaml \
    | "${kubectl_bin}" apply -f - >/dev/null
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  orka_e2e_bootstrap_admission_tls "$@"
fi
