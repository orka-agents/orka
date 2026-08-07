#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
renderer="${root}/scripts/render-acp-runtime-images.sh"
kustomize="${KUSTOMIZE:-${root}/bin/kustomize}"

for command in "${renderer}" "${kustomize}" jq openssl ruby; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done

test_root="$(mktemp -d "${TMPDIR:-/tmp}/acp-runtime-image-rollout-test.XXXXXX")"
cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT
cp -R "${root}/config" "${test_root}/config"
overlay="${test_root}/config/acp-production"

codex_a="docker.io/example/acp-codex@sha256:$(printf 'a%.0s' {1..64})"
claude_a="docker.io/example/acp-claude@sha256:$(printf 'b%.0s' {1..64})"
copilot_a="docker.io/example/acp-copilot@sha256:$(printf 'c%.0s' {1..64})"
opencode_a="docker.io/example/acp-opencode@sha256:$(printf '0%.0s' {1..64})"
codex_b="registry--prod.example.com:5000/team/acp-codex@sha256:$(printf 'd%.0s' {1..64})"
claude_b="registry--prod.example.com:5000/team/acp-claude@sha256:$(printf 'e%.0s' {1..64})"
copilot_b="registry--prod.example.com:5000/team/acp-copilot@sha256:$(printf 'f%.0s' {1..64})"
opencode_b="registry--prod.example.com:5000/team/acp-opencode@sha256:$(printf '9%.0s' {1..64})"
ipv6_image="[2001:db8::1]:5000/team/acp-codex@sha256:$(printf 'a%.0s' {1..64})"
multiline_image="docker.io/example/acp-codex
#@sha256:$(printf 'b%.0s' {1..64})"

strip_agent_execution_control() {
  awk '
    function flush_document() {
      if (document != "" && document !~ /(^|\n)kind: AgentExecutionControl(\n|$)/) {
        printf "%s", document
      }
      document = ""
    }
    /^---[[:space:]]*$/ {
      flush_document()
      document = $0 ORS
      next
    }
    { document = document $0 ORS }
    END { flush_document() }
  '
}

yaml_to_json() {
  ruby -rjson -ryaml -e '
    YAML.load_stream(STDIN.read).each do |document|
      puts JSON.generate(document) unless document.nil?
    end
  '
}

render_snapshot() {
  local codex_image="$1"
  local claude_image="$2"
  local copilot_image="$3"
  local opencode_image="$4"
  local output="$5"

  "${renderer}" "${overlay}" "${codex_image}" "${claude_image}" "${copilot_image}" "${opencode_image}"
  "${kustomize}" build "${overlay}" \
    | strip_agent_execution_control \
    | yaml_to_json \
    | jq -sc '
        [.[] | if .kind == "List" then .items[] else . end] as $items |
        ($items | map(select(.kind == "ConfigMap" and .metadata.labels["orka.ai/acp-runtime-images"] == "true")) | .[0]) as $config |
        ($items | map(select(.kind == "Deployment" and .metadata.name == "orka-controller-manager")) | .[0]) as $deployment |
        {
          configName: $config.metadata.name,
          config: $config.data,
          references: [
            $deployment.spec.template.spec.containers[]?.env[]? |
            select(.name == "ORKA_ACP_CODEX_RUNTIME_IMAGE" or .name == "ORKA_ACP_CLAUDE_RUNTIME_IMAGE" or .name == "ORKA_ACP_COPILOT_RUNTIME_IMAGE" or .name == "ORKA_ACP_OPENCODE_RUNTIME_IMAGE") |
            {name, configMapName: .valueFrom.configMapKeyRef.name, key: .valueFrom.configMapKeyRef.key}
          ] | sort_by(.name)
        }
      ' >"${output}"
}

assert_snapshot() {
  local snapshot="$1"
  local codex_image="$2"
  local claude_image="$3"
  local copilot_image="$4"
  local opencode_image="$5"

  jq -e --arg codex "${codex_image}" --arg claude "${claude_image}" --arg copilot "${copilot_image}" --arg opencode "${opencode_image}" '
    .configName as $configName |
    ($configName | test("^acp-runtime-images-[a-z0-9]+$")) and
    .config.ORKA_ACP_CODEX_RUNTIME_IMAGE == $codex and
    .config.ORKA_ACP_CLAUDE_RUNTIME_IMAGE == $claude and
    .config.ORKA_ACP_COPILOT_RUNTIME_IMAGE == $copilot and
    .config.ORKA_ACP_OPENCODE_RUNTIME_IMAGE == $opencode and
    (.references | length) == 4 and
    ([.references[].configMapName] | all(. == $configName)) and
    ([.references[].key] | sort) == ["ORKA_ACP_CLAUDE_RUNTIME_IMAGE", "ORKA_ACP_CODEX_RUNTIME_IMAGE", "ORKA_ACP_COPILOT_RUNTIME_IMAGE", "ORKA_ACP_OPENCODE_RUNTIME_IMAGE"]
  ' "${snapshot}" >/dev/null
}

first="${test_root}/first.json"
repeat="${test_root}/repeat.json"
copilot_changed="${test_root}/copilot-changed.json"
opencode_changed="${test_root}/opencode-changed.json"
changed="${test_root}/changed.json"

render_snapshot "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_a}" "${first}"
assert_snapshot "${first}" "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_a}"
render_snapshot "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_a}" "${repeat}"
cmp -s "${first}" "${repeat}"

render_snapshot "${codex_a}" "${claude_a}" "${copilot_b}" "${opencode_a}" "${copilot_changed}"
assert_snapshot "${copilot_changed}" "${codex_a}" "${claude_a}" "${copilot_b}" "${opencode_a}"
if [[ "$(jq -r .configName "${first}")" == "$(jq -r .configName "${copilot_changed}")" ]]; then
  echo 'Copilot runtime image change did not create a new immutable ConfigMap generation' >&2
  exit 1
fi

render_snapshot "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_b}" "${opencode_changed}"
assert_snapshot "${opencode_changed}" "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_b}"
if [[ "$(jq -r .configName "${first}")" == "$(jq -r .configName "${opencode_changed}")" ]]; then
  echo 'OpenCode runtime image change did not create a new immutable ConfigMap generation' >&2
  exit 1
fi

render_snapshot "${codex_b}" "${claude_b}" "${copilot_b}" "${opencode_b}" "${changed}"
assert_snapshot "${changed}" "${codex_b}" "${claude_b}" "${copilot_b}" "${opencode_b}"
if [[ "$(jq -r .configName "${first}")" == "$(jq -r .configName "${changed}")" ]]; then
  echo 'runtime image change did not create a new immutable ConfigMap generation' >&2
  exit 1
fi

[[ -f "${overlay}/runtime-images.env" ]]
[[ ! -e "${overlay}/runtime-images-configmap.yaml" ]]
[[ ! -e "${overlay}/runtime-images-rollout-patch.yaml" ]]
rendered_manifest="${test_root}/rendered-manifest.yaml"
"${kustomize}" build "${overlay}" >"${rendered_manifest}"
[[ "$(grep -c '^kind: AgentExecutionControl$' "${rendered_manifest}")" == "1" ]]
awk '
  /^kind: AgentExecutionControl$/ { in_control = 1 }
  in_control && /^  name: cluster$/ { found_name = 1 }
  in_control && /^  namespace: orka-system$/ { found_namespace = 1 }
  in_control && /^---$/ { exit !(found_name && found_namespace) }
  END { if (in_control) exit !(found_name && found_namespace) }
' "${rendered_manifest}"
rendered_inventory="${test_root}/rendered-inventory.json"
strip_agent_execution_control <"${rendered_manifest}" \
  | yaml_to_json \
  | jq -sc '[.[] | if .kind == "List" then .items[] else . end]' >"${rendered_inventory}"
jq -e '
  ([.[] | select(.kind == "Deployment" and .metadata.name == "orka-admission" and
    .metadata.namespace == "orka-system" and .spec.replicas == 2 and
    .spec.strategy.type == "RollingUpdate" and .spec.strategy.rollingUpdate.maxUnavailable == 0 and
    (.spec.template.spec.containers | any(.name == "admission" and (.image | test("@sha256:[a-f0-9]{64}$")))))] | length) == 1 and
  ([.[] | select(.kind == "Service" and .metadata.name == "orka-admission")] | length) == 1 and
  ([.[] | select(.kind == "PodDisruptionBudget" and .metadata.name == "orka-admission" and .spec.minAvailable == 1)] | length) == 1 and
  ([.[] | select(.kind == "ValidatingWebhookConfiguration" and .metadata.name == "orka-admission")] | length) == 0
' "${rendered_inventory}" >/dev/null
grep -F 'scripts/render-acp-runtime-images.sh' "${root}/Makefile" >/dev/null
grep -F 'controller=${IMG}' "${root}/Makefile" >/dev/null
grep -F 'docker-build-acp-copilot-runtime' "${root}/Makefile" >/dev/null
grep -F 'ACP_COPILOT_RUNTIME_IMG' "${root}/Makefile" >/dev/null
grep -F 'docker-build-acp-opencode-runtime' "${root}/Makefile" >/dev/null
grep -F 'ACP_OPENCODE_RUNTIME_IMG' "${root}/Makefile" >/dev/null
if grep -F 'rollout restart deployment/orka-controller-manager' "${root}/Makefile" >/dev/null; then
  echo 'deploy still relies on an imperative, non-retryable controller restart' >&2
  exit 1
fi

"${renderer}" "${overlay}" "${ipv6_image}" "${claude_b}" "${copilot_b}" "${opencode_b}"
"${renderer}" "${overlay}" "${codex_b}" "${claude_b}" "${copilot_b}" "${opencode_b}"

for invalid_image in \
  'not-digest-pinned' \
  "https://registry.example.com/team/acp@sha256:$(printf '1%.0s' {1..64})" \
  "registry.example.com:notaport/team/acp@sha256:$(printf '2%.0s' {1..64})" \
  "[127.0.0.1]/team/acp@sha256:$(printf '3%.0s' {1..64})" \
  "${multiline_image}"; do
  if "${renderer}" "${overlay}" "${invalid_image}" "${claude_b}" "${copilot_b}" "${opencode_b}" >/dev/null 2>&1; then
    echo "renderer accepted invalid runtime image: ${invalid_image}" >&2
    exit 1
  fi
done

if "${renderer}" "${overlay}" "${codex_b}" "${claude_b}" "not-digest-pinned" "${opencode_b}" >/dev/null 2>&1; then
  echo 'renderer accepted an invalid Copilot runtime image' >&2
  exit 1
fi

if "${renderer}" "${overlay}" "${codex_b}" "${claude_b}" "${copilot_b}" "not-digest-pinned" >/dev/null 2>&1; then
  echo 'renderer accepted an invalid OpenCode runtime image' >&2
  exit 1
fi

apply_script="${root}/scripts/apply-acp-production.sh"
tls_fixture_dir="${test_root}/admission-tls"
mkdir -p "${tls_fixture_dir}"
ruby -ropenssl - "${tls_fixture_dir}" <<'RUBY_TLS_FIXTURES'
directory = ARGV.fetch(0)
now = Time.now

def issue_ca(common_name, key, not_before, not_after, serial)
  certificate = OpenSSL::X509::Certificate.new
  certificate.version = 2
  certificate.serial = serial
  certificate.subject = OpenSSL::X509::Name.parse("/CN=#{common_name}")
  certificate.issuer = certificate.subject
  certificate.public_key = key.public_key
  certificate.not_before = not_before
  certificate.not_after = not_after
  extensions = OpenSSL::X509::ExtensionFactory.new
  extensions.subject_certificate = certificate
  extensions.issuer_certificate = certificate
  certificate.add_extension(extensions.create_extension("basicConstraints", "CA:TRUE", true))
  certificate.add_extension(extensions.create_extension("keyUsage", "keyCertSign,cRLSign", true))
  certificate.add_extension(extensions.create_extension("subjectKeyIdentifier", "hash", false))
  certificate.add_extension(extensions.create_extension("authorityKeyIdentifier", "keyid:always", false))
  certificate.sign(key, OpenSSL::Digest::SHA256.new)
  certificate
end

def issue_server(ca_certificate, ca_key, server_key, dns_name, not_before, not_after, serial)
  certificate = OpenSSL::X509::Certificate.new
  certificate.version = 2
  certificate.serial = serial
  certificate.subject = OpenSSL::X509::Name.parse("/CN=#{dns_name}")
  certificate.issuer = ca_certificate.subject
  certificate.public_key = server_key.public_key
  certificate.not_before = not_before
  certificate.not_after = not_after
  extensions = OpenSSL::X509::ExtensionFactory.new
  extensions.subject_certificate = certificate
  extensions.issuer_certificate = ca_certificate
  certificate.add_extension(extensions.create_extension("basicConstraints", "CA:FALSE", true))
  certificate.add_extension(extensions.create_extension("keyUsage", "digitalSignature,keyEncipherment", true))
  certificate.add_extension(extensions.create_extension("extendedKeyUsage", "serverAuth", false))
  certificate.add_extension(extensions.create_extension("subjectAltName", "DNS:#{dns_name}", false))
  certificate.add_extension(extensions.create_extension("subjectKeyIdentifier", "hash", false))
  certificate.add_extension(extensions.create_extension("authorityKeyIdentifier", "keyid:always", false))
  certificate.sign(ca_key, OpenSSL::Digest::SHA256.new)
  certificate
end

ca_key = OpenSSL::PKey::RSA.new(2048)
ca_certificate = issue_ca("Orka Admission Test CA", ca_key, now - 3600, now + 86_400, 1)
other_ca_key = OpenSSL::PKey::RSA.new(2048)
other_ca_certificate = issue_ca("Untrusted Test CA", other_ca_key, now - 3600, now + 86_400, 2)
server_key = OpenSSL::PKey::RSA.new(2048)
mismatched_key = OpenSSL::PKey::RSA.new(2048)
service_dns = "orka-admission.orka-system.svc"

File.binwrite(File.join(directory, "ca.crt"), ca_certificate.to_pem)
File.binwrite(File.join(directory, "other-ca.crt"), other_ca_certificate.to_pem)
File.binwrite(File.join(directory, "tls.key"), server_key.to_pem)
File.binwrite(File.join(directory, "mismatched.key"), mismatched_key.to_pem)
File.binwrite(File.join(directory, "tls.crt"), issue_server(
  ca_certificate, ca_key, server_key, service_dns, now - 3600, now + 86_400, 3
).to_pem)
File.binwrite(File.join(directory, "wrong-san.crt"), issue_server(
  ca_certificate, ca_key, server_key, "not-orka-admission.orka-system.svc", now - 3600, now + 86_400, 4
).to_pem)
File.binwrite(File.join(directory, "expired.crt"), issue_server(
  ca_certificate, ca_key, server_key, service_dns, now - 7200, now - 3600, 5
).to_pem)
File.binwrite(File.join(directory, "future.crt"), issue_server(
  ca_certificate, ca_key, server_key, service_dns, now + 3600, now + 7200, 6
).to_pem)
File.binwrite(File.join(directory, "invalid.crt"), "not a certificate\n")
File.binwrite(File.join(directory, "invalid.key"), "not a private key\n")
RUBY_TLS_FIXTURES
export FAKE_TLS_FIXTURE_DIR="${tls_fixture_dir}"

fake_bin="${test_root}/fake-bin"
mkdir -p "${fake_bin}"
cat >"${fake_bin}/kubectl" <<'EOF_FAKE_KUBECTL'
#!/usr/bin/env bash
set -Eeuo pipefail

mkdir -p "${FAKE_KUBE_STATE}/configmaps"

strip_agent_execution_control() {
  awk '
    function flush_document() {
      if (document != "" && document !~ /(^|\n)kind: AgentExecutionControl(\n|$)/) {
        printf "%s", document
      }
      document = ""
    }
    /^---[[:space:]]*$/ {
      flush_document()
      document = $0 ORS
      next
    }
    { document = document $0 ORS }
    END { flush_document() }
  ' "$1"
}

yaml_to_json() {
  ruby -rjson -ryaml -e '
    YAML.load_stream(STDIN.read).each do |document|
      puts JSON.generate(document) unless document.nil?
    end
  '
}

emit_control_json() {
  case "${FAKE_CONTROL_MODE:-valid}" in
    valid)
      printf '%s\n' '{"apiVersion":"core.orka.ai/v1alpha1","kind":"AgentExecutionControl","metadata":{"name":"cluster","namespace":"orka-system"},"spec":{"backends":{"v1":{"desiredMode":"disabled"},"v2":{"desiredMode":"enabled"}}}}'
      ;;
    duplicate)
      emit_control_json_valid='{"apiVersion":"core.orka.ai/v1alpha1","kind":"AgentExecutionControl","metadata":{"name":"cluster","namespace":"orka-system"},"spec":{"backends":{"v1":{"desiredMode":"disabled"},"v2":{"desiredMode":"enabled"}}}}'
      printf '%s\n%s\n' "${emit_control_json_valid}" "${emit_control_json_valid}"
      ;;
    missing)
      ;;
    wrong-name)
      printf '%s\n' '{"apiVersion":"core.orka.ai/v1alpha1","kind":"AgentExecutionControl","metadata":{"name":"orka-cluster","namespace":"orka-system"},"spec":{"backends":{"v1":{"desiredMode":"disabled"},"v2":{"desiredMode":"enabled"}}}}'
      ;;
    *)
      echo "unknown FAKE_CONTROL_MODE: ${FAKE_CONTROL_MODE}" >&2
      exit 2
      ;;
  esac
}

manifest_json() {
  local source="$1"
  if jq -e '.kind == "AgentExecutionControl"' "${source}" >/dev/null 2>&1; then
    cat "${source}"
    return
  fi

  local filtered="${FAKE_KUBE_STATE}/known-resources.yaml"
  strip_agent_execution_control "${source}" >"${filtered}"
  yaml_to_json <"${filtered}"
  if grep -q '^kind: AgentExecutionControl$' "${source}"; then
    emit_control_json
  fi
}

if [[ "$1" == "proxy" ]]; then
  [[ -e "${FAKE_KUBE_STATE}/admission-endpoints" ]] || { echo 'handler smoke ran before ready endpoints' >&2; exit 34; }
  [[ " $* " == *" --address=127.0.0.1 "* && " $* " == *" --port=0 "* ]] || {
    echo "invalid admission proxy invocation: $*" >&2
    exit 2
  }
  printf 'proxy-start\n' >>"${FAKE_KUBE_LOG}"
  stop_proxy() {
    printf 'proxy-stop\n' >>"${FAKE_KUBE_LOG}"
    exit 0
  }
  trap stop_proxy INT TERM
  printf 'Starting to serve on 127.0.0.1:43210\n'
  while true; do
    sleep 1
  done
fi

if [[ "$1" == "create" && "$2" == "-f" && $# -eq 3 ]] \
  && jq -e '.kind == "AgentExecutionControl"' "$3" >/dev/null 2>&1; then
  [[ "$(jq -r '.metadata.namespace + "/" + .metadata.name' "$3")" == "orka-system/cluster" ]] || {
    echo 'wrong AgentExecutionControl identity' >&2
    exit 27
  }
  [[ -e "${FAKE_KUBE_STATE}/namespace" ]] || { echo 'control created before namespace' >&2; exit 28; }
  config_count="$(find "${FAKE_KUBE_STATE}/configmaps" -type f | wc -l | tr -d '[:space:]')"
  [[ "${config_count}" -gt 0 ]] || { echo 'control created before runtime ConfigMap' >&2; exit 29; }
  [[ ! -e "${FAKE_KUBE_STATE}/control.json" ]] || { echo 'control already exists' >&2; exit 32; }
  if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "control" && ! -e "${FAKE_KUBE_STATE}/failed-control" ]]; then
    : >"${FAKE_KUBE_STATE}/failed-control"
    printf 'fail-control:cluster\n' >>"${FAKE_KUBE_LOG}"
    exit 30
  fi
  jq '
    .metadata.uid = "control-uid" |
    .metadata.generation = 1 |
    .status = {
      observedGeneration: 1,
      backends: {
        v1: {effectiveMode: .spec.backends.v1.desiredMode, modeRevision: 1},
        v2: {effectiveMode: .spec.backends.v2.desiredMode, modeRevision: 1}
      }
    }
  ' "$3" >"${FAKE_KUBE_STATE}/control.json"
  printf 'control-create:cluster\n' >>"${FAKE_KUBE_LOG}"
  exit 0
fi

if [[ "$1" == "create" ]]; then
  manifest_path=""
  args=("$@")
  for ((i = 0; i < ${#args[@]}; i++)); do
    if [[ "${args[$i]}" == "-f" && $((i + 1)) -lt ${#args[@]} ]]; then
      manifest_path="${args[$((i + 1))]}"
      break
    fi
  done
  [[ -n "${manifest_path}" && "${manifest_path}" != "-" ]] || {
    echo "unexpected fake kubectl create invocation: $*" >&2
    exit 2
  }
  manifest_json "${manifest_path}"
  exit 0
fi

if [[ "$1" == "-n" && "$2" == "orka-system" && "$3" == "get" && "$4" == "secret" && "$5" == "agent-execution-snapshot-key" ]]; then
  if [[ ! -f "${FAKE_KUBE_STATE}/snapshot-key" ]]; then
    if [[ " $* " == *" --ignore-not-found "* ]]; then
      exit 0
    fi
    exit 1
  fi
  encoded="$(base64 <"${FAKE_KUBE_STATE}/snapshot-key" | tr -d '\r\n')"
  jq -n --arg encoded "${encoded}" '{apiVersion:"v1",kind:"Secret",metadata:{name:"agent-execution-snapshot-key",namespace:"orka-system"},data:{"snapshot-key":$encoded}}'
  exit 0
fi

if [[ "$1" == "-n" && "$2" == "orka-system" && "$3" == "get" && "$4" == "secret" && "$5" == "orka-admission-tls" ]]; then
  tls_mode="${FAKE_TLS_MODE:-valid}"
  [[ "${tls_mode}" != "missing" ]] || exit 1
  tls_directory="${FAKE_TLS_FIXTURE_DIR:?}"
  cert_path="${tls_directory}/tls.crt"
  key_path="${tls_directory}/tls.key"
  ca_path="${tls_directory}/ca.crt"
  case "${tls_mode}" in
    valid|missing-ca) ;;
    invalid-cert) cert_path="${tls_directory}/invalid.crt" ;;
    invalid-key) key_path="${tls_directory}/invalid.key" ;;
    invalid-ca) ca_path="${tls_directory}/invalid.crt" ;;
    mismatched-key) key_path="${tls_directory}/mismatched.key" ;;
    expired) cert_path="${tls_directory}/expired.crt" ;;
    future) cert_path="${tls_directory}/future.crt" ;;
    wrong-san) cert_path="${tls_directory}/wrong-san.crt" ;;
    wrong-ca) ca_path="${tls_directory}/other-ca.crt" ;;
    *) echo "unknown FAKE_TLS_MODE: ${tls_mode}" >&2; exit 2 ;;
  esac
  cert="$(base64 <"${cert_path}" | tr -d '\r\n')"
  key="$(base64 <"${key_path}" | tr -d '\r\n')"
  ca="$(base64 <"${ca_path}" | tr -d '\r\n')"
  if [[ "${tls_mode}" == "missing-ca" ]]; then
    jq -n --arg cert "${cert}" --arg key "${key}" '{apiVersion:"v1",kind:"Secret",type:"kubernetes.io/tls",metadata:{name:"orka-admission-tls",namespace:"orka-system"},data:{"tls.crt":$cert,"tls.key":$key}}'
  else
    jq -n --arg cert "${cert}" --arg key "${key}" --arg ca "${ca}" '{apiVersion:"v1",kind:"Secret",type:"kubernetes.io/tls",metadata:{name:"orka-admission-tls",namespace:"orka-system"},data:{"tls.crt":$cert,"tls.key":$key,"ca.crt":$ca}}'
  fi
  exit 0
fi

if [[ "$1" == "-n" && "$2" == "orka-system" && "$3" == "create" && "$4" == "secret" && "$5" == "generic" && "$6" == "agent-execution-snapshot-key" ]]; then
  [[ -e "${FAKE_KUBE_STATE}/namespace" ]] || {
    echo 'snapshot Secret created before namespace' >&2
    exit 24
  }
  [[ ! -e "${FAKE_KUBE_STATE}/snapshot-key" ]] || {
    echo 'snapshot Secret already exists' >&2
    exit 25
  }
  key_path=""
  for argument in "$@"; do
    if [[ "${argument}" == --from-file=snapshot-key=* ]]; then
      key_path="${argument#--from-file=snapshot-key=}"
      break
    fi
  done
  [[ -n "${key_path}" && -f "${key_path}" ]] || {
    echo 'snapshot Secret create is missing its key file' >&2
    exit 26
  }
  cp "${key_path}" "${FAKE_KUBE_STATE}/snapshot-key"
  printf 'secret:agent-execution-snapshot-key\n' >>"${FAKE_KUBE_LOG}"
  exit 0
fi

if [[ "$1" == "-n" && "$2" == "orka-system" && "$3" == "get" && "$4" == "agentexecutioncontrol.core.orka.ai" && "$5" == "cluster" ]]; then
  if [[ ! -f "${FAKE_KUBE_STATE}/control.json" ]]; then
    if [[ " $* " == *" --ignore-not-found "* ]]; then
      exit 0
    fi
    exit 1
  fi
  cat "${FAKE_KUBE_STATE}/control.json"
  exit 0
fi

if [[ "$1" == "-n" && "$2" == "orka-system" && "$3" == "rollout" && "$4" == "status" && "$5" == "deployment/orka-admission" ]]; then
  [[ -e "${FAKE_KUBE_STATE}/admission-runtime" ]] || { echo 'admission rollout waited before runtime apply' >&2; exit 36; }
  if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "rollout" && ! -e "${FAKE_KUBE_STATE}/failed-rollout" ]]; then
    : >"${FAKE_KUBE_STATE}/failed-rollout"
    printf 'fail-rollout:orka-admission\n' >>"${FAKE_KUBE_LOG}"
    exit 37
  fi
  printf 'rollout:orka-admission\n' >>"${FAKE_KUBE_LOG}"
  exit 0
fi

if [[ "$1" == "-n" && "$2" == "orka-system" && "$3" == "get" && "$4" == "endpoints" && "$5" == "orka-admission" ]]; then
  [[ -e "${FAKE_KUBE_STATE}/admission-runtime" ]] || exit 1
  if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "endpoints" && ! -e "${FAKE_KUBE_STATE}/failed-endpoints" ]]; then
    : >"${FAKE_KUBE_STATE}/failed-endpoints"
    printf 'fail-endpoints:orka-admission\n' >>"${FAKE_KUBE_LOG}"
    printf '%s\n' '{"apiVersion":"v1","kind":"Endpoints","subsets":[{"addresses":[{"ip":"10.0.0.1"}]}]}'
    exit 0
  fi
  : >"${FAKE_KUBE_STATE}/admission-endpoints"
  printf '%s\n' '{"apiVersion":"v1","kind":"Endpoints","subsets":[{"addresses":[{"ip":"10.0.0.1"},{"ip":"10.0.0.2"}]}]}'
  exit 0
fi

[[ "$1" == "apply" && "$2" == "-f" && $# -eq 3 ]] || {
  echo "unexpected fake kubectl invocation: $*" >&2
  exit 2
}

if jq -e 'if .kind == "List" then any(.items[]?; .kind == "ValidatingWebhookConfiguration" and .metadata.name == "orka-admission") else false end' "$3" >/dev/null; then
  [[ -e "${FAKE_KUBE_STATE}/admission-endpoints" ]] || { echo 'admission policies applied before ready endpoints' >&2; exit 38; }
  [[ "$(grep -c '^smoke:' "${FAKE_KUBE_LOG}")" -ge 9 ]] || { echo 'admission policies applied before every handler smoke' >&2; exit 39; }
  jq -e '
    ([.items[] | select(.kind == "ValidatingAdmissionPolicy")] | length) == 3 and
    ([.items[] | select(.kind == "ValidatingAdmissionPolicyBinding")] | length) == 3 and
    ([.items[] | select(.kind == "ValidatingWebhookConfiguration")] | length) == 1 and
    ([.items[] | select(.kind == "ValidatingWebhookConfiguration") | .webhooks[] |
      (.failurePolicy == "Fail" and (.clientConfig.caBundle | length > 0))] | all) and
    ([.items[] | select(.kind == "ValidatingWebhookConfiguration") |
      .metadata.annotations["cert-manager.io/inject-ca-from-secret"]] | all(. == null))
  ' "$3" >/dev/null || { echo 'admission policy wave was not fail-closed and CA-pinned' >&2; exit 40; }
  if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "policies" && ! -e "${FAKE_KUBE_STATE}/failed-policies" ]]; then
    : >"${FAKE_KUBE_STATE}/failed-policies"
    printf 'fail-policies:orka-admission\n' >>"${FAKE_KUBE_LOG}"
    exit 41
  fi
  : >"${FAKE_KUBE_STATE}/admission-policies"
  printf 'policies:orka-admission\n' >>"${FAKE_KUBE_LOG}"
  exit 0
fi

payload="$(manifest_json "$3")"
summary="$(printf '%s\n' "${payload}" | jq -sc '
  [.[] | if .kind == "List" then .items[] else . end] as $items |
  {
    namespace: (($items | map(select(.kind == "Namespace" and .metadata.name == "orka-system")) | .[0].metadata.name) // ""),
    runtimeConfig: (($items | map(select(.kind == "ConfigMap" and .metadata.labels["orka.ai/acp-runtime-images"] == "true")) | .[0].metadata.name) // ""),
    deploymentRef: (($items | map(select(.kind == "Deployment" and .metadata.name == "orka-controller-manager")) | .[0].spec.template.spec.containers[0].env[]? | select(.name == "ORKA_ACP_CODEX_RUNTIME_IMAGE") | .valueFrom.configMapKeyRef.name) // ""),
    admissionDeployments: ($items | map(select(.kind == "Deployment" and .metadata.name == "orka-admission")) | length),
    admissionServices: ($items | map(select(.kind == "Service" and .metadata.name == "orka-admission")) | length)
  }
')"
namespace_name="$(jq -r .namespace <<<"${summary}")"
config_name="$(jq -r .runtimeConfig <<<"${summary}")"
deployment_ref="$(jq -r .deploymentRef <<<"${summary}")"
admission_deployments="$(jq -r .admissionDeployments <<<"${summary}")"
admission_services="$(jq -r .admissionServices <<<"${summary}")"
if [[ -n "${namespace_name}" && -z "${config_name}" && -z "${deployment_ref}" ]]; then
  if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "namespace" && ! -e "${FAKE_KUBE_STATE}/failed-namespace" ]]; then
    : >"${FAKE_KUBE_STATE}/failed-namespace"
    printf 'fail-namespace:%s\n' "${namespace_name}" >>"${FAKE_KUBE_LOG}"
    exit 18
  fi
  : >"${FAKE_KUBE_STATE}/namespace"
  printf 'namespace:%s\n' "${namespace_name}" >>"${FAKE_KUBE_LOG}"
  exit 0
fi

if [[ -z "${deployment_ref}" ]]; then
  [[ -n "${config_name}" ]] || { echo 'runtime ConfigMap apply was not identifiable' >&2; exit 1; }
  [[ -e "${FAKE_KUBE_STATE}/namespace" ]] || { echo 'runtime ConfigMap applied before namespace' >&2; exit 17; }
  if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "config" && ! -e "${FAKE_KUBE_STATE}/failed-config" ]]; then
    : >"${FAKE_KUBE_STATE}/failed-config"
    printf 'fail-config:%s\n' "${config_name}" >>"${FAKE_KUBE_LOG}"
    exit 19
  fi
  : >"${FAKE_KUBE_STATE}/configmaps/${config_name}"
  printf 'config:%s\n' "${config_name}" >>"${FAKE_KUBE_LOG}"
  exit 0
fi

if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "full" && ! -e "${FAKE_KUBE_STATE}/failed-full" ]]; then
  : >"${FAKE_KUBE_STATE}/failed-full"
  printf 'fail-full:%s\n' "${deployment_ref}" >>"${FAKE_KUBE_LOG}"
  exit 20
fi
[[ -e "${FAKE_KUBE_STATE}/namespace" ]] || {
  echo 'Deployment applied before namespace' >&2
  exit 22
}
[[ -e "${FAKE_KUBE_STATE}/configmaps/${deployment_ref}" ]] || {
  echo "Deployment referenced missing ConfigMap ${deployment_ref}" >&2
  exit 21
}
[[ -s "${FAKE_KUBE_STATE}/snapshot-key" ]] || {
  echo 'Deployment applied before snapshot Secret' >&2
  exit 23
}
[[ -e "${FAKE_KUBE_STATE}/control.json" ]] || {
  echo 'Deployment applied before AgentExecutionControl' >&2
  exit 31
}
[[ "${admission_deployments}" == "1" && "${admission_services}" == "1" ]] || {
  echo 'workload wave omitted the dedicated admission runtime or Service' >&2
  exit 42
}
if jq -e 'if .kind == "List" then any(.items[]?; .kind == "AgentExecutionControl") else .kind == "AgentExecutionControl" end' "$3" >/dev/null; then
  echo 'workload wave reapplied AgentExecutionControl bootstrap modes' >&2
  exit 33
fi
printf '%s\n' "${deployment_ref}" >"${FAKE_KUBE_STATE}/deployment-ref"
: >"${FAKE_KUBE_STATE}/admission-runtime"
printf 'full:%s\n' "${deployment_ref}" >>"${FAKE_KUBE_LOG}"
EOF_FAKE_KUBECTL
chmod +x "${fake_bin}/kubectl"
cat >"${fake_bin}/curl" <<'EOF_FAKE_CURL'
#!/usr/bin/env bash
set -Eeuo pipefail

content_type=""
request_file=""
url=""
while (( $# > 0 )); do
  case "$1" in
    --fail|--silent|--show-error)
      shift
      ;;
    --noproxy|--max-time)
      [[ $# -ge 2 ]] || { echo "missing curl argument for $1" >&2; exit 2; }
      shift 2
      ;;
    --header)
      [[ $# -ge 2 ]] || { echo 'missing curl header' >&2; exit 2; }
      content_type="$2"
      shift 2
      ;;
    --data-binary)
      [[ $# -ge 2 && "$2" == @* ]] || { echo 'invalid curl data argument' >&2; exit 2; }
      request_file="${2#@}"
      shift 2
      ;;
    http://*)
      url="$1"
      shift
      ;;
    *)
      echo "unexpected fake curl argument: $1" >&2
      exit 2
      ;;
  esac
done

[[ "${content_type}" == "Content-Type: application/json" ]] || {
  echo 'admission smoke did not send application/json' >&2
  exit 43
}
[[ -f "${request_file}" ]] || { echo 'admission smoke request file missing' >&2; exit 2; }
if [[ ! "${url}" =~ ^http://127\.0\.0\.1:[0-9]+/api/v1/namespaces/orka-system/services/https:orka-admission:443/proxy(/.*)$ ]]; then
  echo "invalid admission smoke URL: ${url}" >&2
  exit 2
fi
handler="${BASH_REMATCH[1]}"
[[ -e "${FAKE_KUBE_STATE}/admission-endpoints" ]] || { echo 'handler smoke ran before ready endpoints' >&2; exit 34; }
if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "smoke" && ! -e "${FAKE_KUBE_STATE}/failed-smoke" ]]; then
  : >"${FAKE_KUBE_STATE}/failed-smoke"
  printf 'fail-smoke:%s\n' "${handler}" >>"${FAKE_KUBE_LOG}"
  exit 35
fi
uid="$(jq -r '.request.uid' "${request_file}")"
jq -n --arg uid "${uid}" '{apiVersion:"admission.k8s.io/v1",kind:"AdmissionReview",response:{uid:$uid,allowed:false,status:{message:"smoke reached handler"}}}'
printf 'smoke:%s\n' "${handler}" >>"${FAKE_KUBE_LOG}"
EOF_FAKE_CURL
chmod +x "${fake_bin}/curl"
export PATH="${fake_bin}:${PATH}"

assert_converged() {
  local state_dir="$1"
  local reference
  reference="$(cat "${state_dir}/deployment-ref")"
  [[ -e "${state_dir}/namespace" ]]
  [[ -e "${state_dir}/configmaps/${reference}" ]]
  [[ -s "${state_dir}/snapshot-key" ]]
  [[ -e "${state_dir}/control.json" ]]
  [[ -e "${state_dir}/admission-runtime" ]]
  [[ -e "${state_dir}/admission-endpoints" ]]
  [[ -e "${state_dir}/admission-policies" ]]
  [[ "$(grep -c '^proxy-start$' "${state_dir}/apply.log")" -ge 1 ]]
  [[ "$(grep -c '^proxy-start$' "${state_dir}/apply.log")" == "$(grep -c '^proxy-stop$' "${state_dir}/apply.log")" ]]
  [[ "$(grep -c '^secret:agent-execution-snapshot-key$' "${state_dir}/apply.log")" == "1" ]]
  [[ "$(grep -c '^control-create:cluster$' "${state_dir}/apply.log")" == "1" ]]
  [[ "$(grep '^smoke:' "${state_dir}/apply.log" | sort -u | wc -l | tr -d '[:space:]')" == "9" ]]
  [[ "$(grep -c '^policies:orka-admission$' "${state_dir}/apply.log")" == "1" ]]
}

run_apply_scenario() {
  local mode="$1"
  local state_dir="${test_root}/state-${mode:-success}"
  local log_file="${state_dir}/apply.log"
  mkdir -p "${state_dir}"
  : >"${log_file}"

  if [[ -n "${mode}" ]]; then
    if FAKE_KUBE_STATE="${state_dir}" FAKE_KUBE_LOG="${log_file}" FAKE_KUBE_FAIL_MODE="${mode}" \
      "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null 2>&1; then
      echo "expected injected ${mode} apply failure" >&2
      exit 1
    fi
  fi
  FAKE_KUBE_STATE="${state_dir}" FAKE_KUBE_LOG="${log_file}" FAKE_KUBE_FAIL_MODE="" \
    "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null
  assert_converged "${state_dir}"
}

run_apply_scenario ""
run_apply_scenario namespace
run_apply_scenario config
run_apply_scenario control
run_apply_scenario full
run_apply_scenario rollout
run_apply_scenario endpoints
run_apply_scenario smoke
run_apply_scenario policies

expected_existing_key="MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
existing_state="${test_root}/state-existing-key"
mkdir -p "${existing_state}"
printf '%s' "${expected_existing_key}" >"${existing_state}/snapshot-key"
cp "${existing_state}/snapshot-key" "${existing_state}/snapshot-key.before"
: >"${existing_state}/apply.log"
FAKE_KUBE_STATE="${existing_state}" FAKE_KUBE_LOG="${existing_state}/apply.log" FAKE_KUBE_FAIL_MODE="" \
  "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null
cmp -s "${existing_state}/snapshot-key.before" "${existing_state}/snapshot-key"
if grep -q '^secret:' "${existing_state}/apply.log"; then
  echo 'existing snapshot key was recreated' >&2
  exit 1
fi

newline_key_state="${test_root}/state-base64-newline-key"
mkdir -p "${newline_key_state}"
printf '%s\n' "${expected_existing_key}" >"${newline_key_state}/snapshot-key"
cp "${newline_key_state}/snapshot-key" "${newline_key_state}/snapshot-key.before"
: >"${newline_key_state}/apply.log"
FAKE_KUBE_STATE="${newline_key_state}" FAKE_KUBE_LOG="${newline_key_state}/apply.log" FAKE_KUBE_FAIL_MODE="" \
  "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null
cmp -s "${newline_key_state}/snapshot-key.before" "${newline_key_state}/snapshot-key"

raw_whitespace_key_state="${test_root}/state-raw-whitespace-key"
mkdir -p "${raw_whitespace_key_state}"
printf ' %030d\n' 0 >"${raw_whitespace_key_state}/snapshot-key"
[[ "$(wc -c <"${raw_whitespace_key_state}/snapshot-key" | tr -d '[:space:]')" == "32" ]]
cp "${raw_whitespace_key_state}/snapshot-key" "${raw_whitespace_key_state}/snapshot-key.before"
: >"${raw_whitespace_key_state}/apply.log"
FAKE_KUBE_STATE="${raw_whitespace_key_state}" FAKE_KUBE_LOG="${raw_whitespace_key_state}/apply.log" FAKE_KUBE_FAIL_MODE="" \
  "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null
cmp -s "${raw_whitespace_key_state}/snapshot-key.before" "${raw_whitespace_key_state}/snapshot-key"

preserved_control_state="${test_root}/state-preserved-control"
mkdir -p "${preserved_control_state}"
printf '%s' "${expected_existing_key}" >"${preserved_control_state}/snapshot-key"
cat >"${preserved_control_state}/control.json" <<'EOF_PRESERVED_CONTROL'
{"apiVersion":"core.orka.ai/v1alpha1","kind":"AgentExecutionControl","metadata":{"name":"cluster","namespace":"orka-system","uid":"control-uid","generation":9},"spec":{"backends":{"v1":{"desiredMode":"drain-only"},"v2":{"desiredMode":"disabled"}}},"status":{"observedGeneration":9,"backends":{"v1":{"effectiveMode":"drain-only","modeRevision":7},"v2":{"effectiveMode":"disabled","modeRevision":4}}}}
EOF_PRESERVED_CONTROL
cp "${preserved_control_state}/control.json" "${preserved_control_state}/control.before.json"
: >"${preserved_control_state}/apply.log"
FAKE_KUBE_STATE="${preserved_control_state}" FAKE_KUBE_LOG="${preserved_control_state}/apply.log" FAKE_KUBE_FAIL_MODE="" \
  "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null
cmp -s "${preserved_control_state}/control.before.json" "${preserved_control_state}/control.json"
if grep -q '^control-create:' "${preserved_control_state}/apply.log"; then
  echo 'existing AgentExecutionControl bootstrap modes were reapplied' >&2
  exit 1
fi

for tls_mode in missing missing-ca invalid-cert invalid-key invalid-ca mismatched-key expired future wrong-san wrong-ca; do
  tls_state="${test_root}/state-tls-${tls_mode}"
  mkdir -p "${tls_state}"
  : >"${tls_state}/apply.log"
  if tls_output="$(FAKE_KUBE_STATE="${tls_state}" FAKE_KUBE_LOG="${tls_state}/apply.log" FAKE_KUBE_FAIL_MODE="" FAKE_TLS_MODE="${tls_mode}" \
    "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" 2>&1)"; then
    echo "${tls_mode} admission TLS Secret unexpectedly passed deployment preflight" >&2
    exit 1
  fi
  grep -F 'orka-admission-tls' <<<"${tls_output}" >/dev/null
  if grep -Eq '^(config|control-create|full|smoke|policies):' "${tls_state}/apply.log"; then
    echo 'workloads or admission policies were applied after TLS validation failed' >&2
    exit 1
  fi
done

malformed_sentinel='malformed-snapshot-key-SENTINEL'
malformed_state="${test_root}/state-malformed-key"
mkdir -p "${malformed_state}"
printf '%s' "${malformed_sentinel}" >"${malformed_state}/snapshot-key"
: >"${malformed_state}/apply.log"
if malformed_output="$(FAKE_KUBE_STATE="${malformed_state}" FAKE_KUBE_LOG="${malformed_state}/apply.log" FAKE_KUBE_FAIL_MODE="" \
  "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" 2>&1)"; then
  echo 'malformed snapshot key unexpectedly passed deployment preflight' >&2
  exit 1
fi
grep -F 'must contain exactly 32 raw bytes or their base64 encoding' <<<"${malformed_output}" >/dev/null
if grep -F "${malformed_sentinel}" <<<"${malformed_output}" >/dev/null; then
  echo 'snapshot key material leaked in deployment output' >&2
  exit 1
fi
if grep -Eq '^(config|control|full):' "${malformed_state}/apply.log"; then
  echo 'workload prerequisites were applied after snapshot-key validation failed' >&2
  exit 1
fi

for control_mode in missing duplicate wrong-name; do
  control_state="${test_root}/state-control-${control_mode}"
  mkdir -p "${control_state}"
  : >"${control_state}/apply.log"
  if FAKE_KUBE_STATE="${control_state}" FAKE_KUBE_LOG="${control_state}/apply.log" FAKE_KUBE_FAIL_MODE="" FAKE_CONTROL_MODE="${control_mode}" \
    "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null 2>&1; then
    echo "${control_mode} AgentExecutionControl unexpectedly passed deployment preflight" >&2
    exit 1
  fi
  [[ ! -s "${control_state}/apply.log" ]]
done

grep -F 'scripts/apply-acp-production.sh' "${root}/Makefile" >/dev/null

printf '%s\n' 'ok - ACP deployment preserves modes and keys, rolls two admission replicas, smokes every handler, pins the CA, and installs fail-closed policies only after readiness'
