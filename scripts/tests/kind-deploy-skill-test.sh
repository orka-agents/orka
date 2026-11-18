#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${root}/.agents/skills/orka-kind-deploy/scripts/deploy_orka_kind.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin" "${work}/repo/config/manager" "${work}/repo/scripts"
cp "${root}/Makefile" "${work}/repo/Makefile"
cp -R "${root}/scripts/lib" "${work}/repo/scripts/lib"
printf 'original kustomization\n' >"${work}/repo/config/manager/kustomization.yaml"
export TEST_LOG="${work}/commands.log" TEST_ROOT="${root}"
export TEST_CONTEXT='kind-skill-test' TEST_TLS_PRESENT=1
TEST_DIGEST="sha256:$(printf 'a%.0s' {1..64})"
export TEST_DIGEST
real_make="$(command -v make)"
export TEST_MAKE="${real_make}"

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj '/CN=orka-admission.orka-system.svc' \
  -addext 'subjectAltName=DNS:orka-admission.orka-system.svc' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign,cRLSign' \
  -addext 'extendedKeyUsage=serverAuth' -keyout "${work}/tls.key" \
  -out "${work}/valid.crt" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/mismatched.key" >/dev/null 2>&1
ruby -ropenssl - "${work}/valid.crt" "${work}/tls.key" "${work}/expired.crt" "${work}/ca-bundle.crt" "${work}/no-san.crt" <<'RUBY'
certificate = OpenSSL::X509::Certificate.new(File.read(ARGV[0]))
key = OpenSSL::PKey.read(File.read(ARGV[1]))
certificate.not_before = Time.now - 86_400
certificate.not_after = Time.now - 3_600
certificate.sign(key, OpenSSL::Digest::SHA256.new)
File.write(ARGV[2], certificate.to_pem)
certificate.subject = OpenSSL::X509::Name.parse('/CN=old-kind-deploy-test-ca')
certificate.issuer = certificate.subject
certificate.sign(key, OpenSSL::Digest::SHA256.new)
File.write(ARGV[3], certificate.to_pem + File.read(ARGV[0]))
certificate = OpenSSL::X509::Certificate.new(File.read(ARGV[0]))
certificate.extensions = certificate.extensions.reject { |extension| extension.oid == 'subjectAltName' }
certificate.sign(key, OpenSSL::Digest::SHA256.new)
File.write(ARGV[4], certificate.to_pem)
RUBY
TEST_VALID_CERT="$(base64 <"${work}/valid.crt" | tr -d '\n')"
TEST_EXPIRED_CERT="$(base64 <"${work}/expired.crt" | tr -d '\n')"
TEST_CA_BUNDLE="$(base64 <"${work}/ca-bundle.crt" | tr -d '\n')"
TEST_NO_SAN_CERT="$(base64 <"${work}/no-san.crt" | tr -d '\n')"
TEST_VALID_KEY="$(base64 <"${work}/tls.key" | tr -d '\n')"
TEST_MISMATCHED_KEY="$(base64 <"${work}/mismatched.key" | tr -d '\n')"
export TEST_VALID_CERT TEST_EXPIRED_CERT TEST_CA_BUNDLE TEST_NO_SAN_CERT TEST_VALID_KEY TEST_MISMATCHED_KEY

# Exercise the real script and registry library; replace only external commands.
cat >"${work}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == --context && "$2" == "${TEST_CONTEXT}" ]] || {
  echo 'kubectl must use the explicit target context' >&2
  exit 1
}
shift 2
printf 'kubectl %s\n' "$*" >>"${TEST_LOG}"
case "$*" in
  'config view '*) printf '%s\tkind-skill-test\n' "${TEST_CONTEXT}" ;;
  *'get secret orka-admission-tls'*)
    [[ "${TEST_TLS_PRESENT}" != error ]] || exit 1
    if [[ "${TEST_TLS_PRESENT}" == 1 ]]; then
      if [[ "$*" == *'-o json' ]]; then
        tls_cert="${TEST_VALID_CERT}"
        ca_cert="${TEST_VALID_CERT}"
        tls_key="${TEST_VALID_KEY}"
        secret_type=kubernetes.io/tls
        case "${TEST_TLS_MODE:-valid}" in
          expired) tls_cert="${TEST_EXPIRED_CERT}" ;;
          expired-ca) ca_cert="${TEST_EXPIRED_CERT}" ;;
          ca-bundle) ca_cert="${TEST_CA_BUNDLE}" ;;
          missing) tls_cert="" ;;
          malformed) tls_cert=bm90IGEgY2VydGlmaWNhdGUK ;;
          missing-key) tls_key="" ;;
          malformed-key) tls_key=bm90IGEgcHJpdmF0ZSBrZXkK ;;
          mismatched-key) tls_key="${TEST_MISMATCHED_KEY}" ;;
          invalid-base64-key) tls_key="${TEST_VALID_KEY}!" ;;
          wrong-type) secret_type=Opaque ;;
          no-san) tls_cert="${TEST_NO_SAN_CERT}"; ca_cert="${TEST_NO_SAN_CERT}" ;;
        esac
        jq -n --arg tls "${tls_cert}" --arg ca "${ca_cert}" --arg key "${tls_key}" --arg type "${secret_type}" \
          '{type: $type, data: {"tls.crt": $tls, "ca.crt": $ca, "tls.key": $key}}'
      else
        echo secret/orka-admission-tls
      fi
    fi ;;
  *'rollout status '*) [[ "${TEST_FAIL_ROLLOUT:-0}" == 0 ]] ;;
  *'get pods,svc,deploy') ;;
  'get namespace vekil-system --ignore-not-found -o name')
    if [[ "${TEST_VEKIL_NAMESPACE_PRESENT:-1}" == 1 ]]; then echo namespace/vekil-system; fi ;;
  'create namespace vekil-system') ;;
  'get namespace orka-system --ignore-not-found -o json')
    printf '%s\n' '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"orka-system","labels":{"orka.ai/controller-mode":"harness-v2"}}}' ;;
  *'create secret generic orka-admission-tls '*) echo '{}' ;;
  'apply -f -') cat >/dev/null ;;
  *) echo "unexpected kubectl command: $*" >&2; exit 1 ;;
esac
STUB
cat >"${work}/bin/kind" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'get clusters') echo skill-test ;;
  'get nodes --name skill-test') echo skill-test-control-plane ;;
  *) exit 1 ;;
esac
STUB
cat >"${work}/bin/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker %s\n' "$*" >>"${TEST_LOG}"
case "$1" in
  inspect) echo true ;;
  port) echo 127.0.0.1:5001 ;;
  exec) if [[ "$2" == -i ]]; then cat >/dev/null; fi ;;
  tag) [[ "${TEST_FAIL_TAG:-0}" == 0 ]] ;;
  push) [[ "${TEST_FAIL_PUSH:-0}" == 0 ]] ;;
  *) exit 1 ;;
esac
STUB
cat >"${work}/bin/skopeo" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'skopeo %s\n' "$*" >>"${TEST_LOG}"
[[ "${TEST_FAIL_PUSH:-0}" == 0 ]]
STUB
cat >"${work}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *'/manifests/e2e'* ]]; then
  printf 'Docker-Content-Digest: %s\r\n' "${TEST_DIGEST}"
fi
STUB
cat >"${work}/bin/make" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'make %s\n' "$*" >>"${TEST_LOG}"
case " $* " in
  *' test-e2e-setup-only '*) ;;
  *' deploy '*)
    "${TEST_MAKE}" -s -f "${TEST_ROOT}/Makefile" verify-acp-runtime-images "$@" -o deploy -o install
    printf 'temporary deploy content\n' >config/manager/kustomization.yaml
    ;;
  *' install '*) ;;
  *) exit 1 ;;
esac
STUB
chmod +x "${work}/bin/"*
# A bounded PATH lets the same test exercise hosts both with and without skopeo.
for cmd in awk base64 bash cat chmod cp dirname git grep jq mktemp openssl rm tr; do
  ln -s "$(command -v "$cmd")" "${work}/bin/$cmd"
done
export PATH="${work}/bin"

run_script() {
  : >"${TEST_LOG}"
  "${script}" --repo "${work}/repo" "$@" >"${work}/output" 2>&1
}

if ! run_script --cluster skill-test; then
  cat "${work}/output" >&2
  exit 1
fi
for entry in \
  'IMG=orka/controller' \
  'WORKSPACE_PUBLISHER_IMG=orka/workspace-publisher' \
  'ACP_CODEX_RUNTIME_IMG=orka/acp-codex-runtime' \
  'ACP_CLAUDE_RUNTIME_IMG=orka/acp-claude-runtime' \
  'ACP_COPILOT_RUNTIME_IMG=orka/acp-copilot-runtime' \
  'ACP_OPENCODE_RUNTIME_IMG=orka/acp-opencode-runtime' \
  'GENERAL_WORKER_IMG=orka/general-worker' \
  'AI_WORKER_IMG=orka/ai-worker'; do
  grep -F "${entry%%=*}=127.0.0.1:5001/${entry#*=}@${TEST_DIGEST}" "${TEST_LOG}" >/dev/null
done
grep -F 'test-e2e-setup-only' "${TEST_LOG}" >/dev/null
grep -F 'KIND_CLUSTER=skill-test' "${TEST_LOG}" >/dev/null
for deployment in orka-controller-manager orka-workspace-publisher orka-provider-auth-proxy orka-scm-egress-proxy orka-admission; do
  grep -F "rollout status deployment/${deployment}" "${TEST_LOG}" >/dev/null
done
[[ "$(<"${work}/repo/config/manager/kustomization.yaml")" == 'original kustomization' ]]

if grep -F 'create secret generic orka-admission-tls' "${TEST_LOG}" >/dev/null; then
  echo 'rotated existing admission TLS' >&2
  exit 1
fi
# An unused expired CA in an otherwise valid bundle must not block renewal overlap.
TEST_TLS_MODE=ca-bundle run_script --cluster skill-test
for tls_mode in expired expired-ca missing malformed missing-key malformed-key mismatched-key invalid-base64-key wrong-type no-san; do
  if TEST_TLS_MODE="${tls_mode}" run_script --cluster skill-test; then
    echo "accepted ${tls_mode} admission TLS" >&2
    exit 1
  fi
  grep -F 'coordinate TLS renewal' "${work}/output" >/dev/null
  if grep -Eq '^(make|docker|skopeo) |^kubectl (.* )?(create|apply|delete) ' "${TEST_LOG}"; then
    echo 'invalid admission TLS caused build or mutation commands' >&2
    exit 1
  fi
done
TEST_TLS_PRESENT=0 TEST_VEKIL_NAMESPACE_PRESENT=0 run_script --cluster skill-test
grep -F 'create secret generic orka-admission-tls' "${TEST_LOG}" >/dev/null
grep -F 'create namespace vekil-system' "${TEST_LOG}" >/dev/null

# A renamed context must still target the explicitly selected Kind cluster.
TEST_CONTEXT='local-kind-alias' run_script --cluster skill-test --context local-kind-alias
run_script --context kind-skill-test

if run_script --cluster wrong-cluster --context kind-skill-test; then
  echo 'accepted a context/cluster mismatch' >&2
  exit 1
fi
if grep -F 'make ' "${TEST_LOG}" >/dev/null; then exit 1; fi
if run_script; then
  echo 'silently selected current-context' >&2
  exit 1
fi
if grep -F 'make ' "${TEST_LOG}" >/dev/null; then exit 1; fi

# A failed push must never fall through to deploying a tag or stale reference.
if TEST_FAIL_PUSH=1 run_script --cluster skill-test; then
  echo 'accepted a failed registry push' >&2
  exit 1
fi
if grep -F 'make deploy' "${TEST_LOG}" >/dev/null; then exit 1; fi
if TEST_TLS_PRESENT=error run_script --cluster skill-test; then
  echo 'ignored a failed TLS Secret lookup' >&2
  exit 1
fi
if grep -F 'make deploy' "${TEST_LOG}" >/dev/null; then exit 1; fi
if TEST_FAIL_ROLLOUT=1 run_script --cluster skill-test; then
  echo 'ignored a failed rollout' >&2
  exit 1
fi
[[ "$(<"${work}/repo/config/manager/kustomization.yaml")" == 'original kustomization' ]]

# Docker-only hosts must not push a stale target tag after tagging the build fails.
rm "${work}/bin/skopeo"
run_script --cluster skill-test --controller-image custom-controller:kind
grep -F 'docker tag custom-controller:kind 127.0.0.1:5001/orka/controller:e2e' "${TEST_LOG}" >/dev/null
if TEST_FAIL_TAG=1 run_script --cluster skill-test; then
  echo 'deployed after docker tag failed' >&2
  exit 1
fi
if grep -F 'make deploy' "${TEST_LOG}" >/dev/null; then exit 1; fi
printf 'kind deploy skill tests passed\n'
