#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

usage() {
  echo "Usage: $0 OVERLAY_DIR KUSTOMIZE KUBECTL" >&2
}

[[ $# -eq 3 ]] || {
  usage
  exit 2
}

overlay_dir="$1"
kustomize="$2"
kubectl="$3"

for command in "${kustomize}" "${kubectl}" base64 dd jq sleep tr wc; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done
[[ -d "${overlay_dir}" && ! -L "${overlay_dir}" ]] || {
  echo "ACP production overlay must be a real directory: ${overlay_dir}" >&2
  exit 1
}

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/apply-acp-production.XXXXXX")"
trap 'rm -rf "${work_dir}"' EXIT

manifest="${work_dir}/manifest.yaml"
rendered_json="${work_dir}/rendered.json"
namespace_resource="${work_dir}/namespace.json"
namespace_metadata_patch="${work_dir}/namespace-metadata-patch.json"
existing_namespace="${work_dir}/existing-namespace.json"
existing_controller="${work_dir}/existing-controller.json"
runtime_config="${work_dir}/runtime-images-configmap.json"
workload_manifest="${work_dir}/workload-manifest.json"
workload_prerequisite_manifest="${work_dir}/workload-prerequisite-manifest.json"
controller_manifest="${work_dir}/controller-manifest.json"
workload_dependency_endpoints="${work_dir}/workload-dependency-endpoints.json"
snapshot_secret="${work_dir}/agent-execution-snapshot-key.json"
snapshot_key="${work_dir}/snapshot-key"
snapshot_key_data="${work_dir}/snapshot-key-data"
snapshot_key_encoded="${work_dir}/snapshot-key-encoded"
snapshot_key_decoded="${work_dir}/snapshot-key-decoded"
admission_webhooks="${work_dir}/orka-admission-webhooks.json"
"${kustomize}" build "${overlay_dir}" >"${manifest}"
"${kubectl}" create --dry-run=client --validate=false -f "${manifest}" -o json >"${rendered_json}"

jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | map(select(.kind == "Namespace" and .metadata.name == "orka-system"))
  | if length == 1 and .[0].metadata.labels["orka.ai/controller-mode"] == "harness-v2"
    then .[0]
    else error("expected exactly one orka-system Namespace claimed by harness-v2")
    end
' "${rendered_json}" >"${namespace_resource}"
jq -e '
  def json_pointer_escape: gsub("~"; "~0") | gsub("/"; "~1");
  .metadata.labels as $labels
  | if ($labels | type) != "object" or $labels["orka.ai/controller-mode"] != "harness-v2"
    then error("namespace metadata patch requires the harness-v2 mode claim")
    else [
      {
        op: "test",
        path: "/metadata/labels/orka.ai~1controller-mode",
        value: "harness-v2"
      },
      ($labels | to_entries[]
        | select(.key != "orka.ai/controller-mode")
        | {
            op: "add",
            path: ("/metadata/labels/" + (.key | json_pointer_escape)),
            value: .value
          })
    ]
    end
' "${namespace_resource}" >"${namespace_metadata_patch}"
jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | map(select(.kind == "ConfigMap" and .metadata.labels["orka.ai/acp-runtime-images"] == "true"))
  | if length == 1 then .[0] else error("expected exactly one generated ACP runtime image ConfigMap") end
' "${rendered_json}" >"${runtime_config}"
jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | {
      apiVersion: "v1",
      kind: "List",
      items: map(select(
        (.kind != "Namespace" or .metadata.name != "orka-system") and
        (.kind != "ConfigMap" or .metadata.labels["orka.ai/acp-runtime-images"] != "true")
      ))
    }
' "${rendered_json}" >"${workload_manifest}"
jq '
  {
    apiVersion: "v1",
    kind: "List",
    items: [.items[] | select((
      .apiVersion == "apps/v1" and
      .kind == "Deployment" and
      .metadata.namespace == "orka-system" and
      .metadata.name == "orka-controller-manager"
    ) | not)]
  }
' "${workload_manifest}" >"${workload_prerequisite_manifest}"
jq '
  [.items[] | select(
    .apiVersion == "apps/v1" and
    .kind == "Deployment" and
    .metadata.namespace == "orka-system" and
    .metadata.name == "orka-controller-manager"
  )]
  | if length == 1 then
      {apiVersion: "v1", kind: "List", items: .}
    else
      error("expected exactly one harness-v2 controller Deployment")
    end
' "${workload_manifest}" >"${controller_manifest}"
jq -e '
  ([.items[] | select(
    .apiVersion == "apps/v1" and
    .kind == "Deployment" and
    .metadata.namespace == "orka-system" and
    (.metadata.name == "orka-provider-auth-proxy" or
     .metadata.name == "orka-scm-egress-proxy" or
     .metadata.name == "orka-workspace-publisher")
  ) | .metadata.name] | sort) == [
    "orka-provider-auth-proxy",
    "orka-scm-egress-proxy",
    "orka-workspace-publisher"
  ] and
  ([.items[] | select(
    .apiVersion == "apps/v1" and
    .kind == "Deployment" and
    .metadata.namespace == "orka-system" and
    .metadata.name == "orka-controller-manager"
  )] | length) == 0
' "${workload_prerequisite_manifest}" >/dev/null || {
  echo "workload prerequisite wave must contain each publisher/proxy Deployment exactly once and no controller Deployment" >&2
  exit 1
}
jq -esc '
  [.[] | if .kind == "List" then .items[] else . end] as $items
  | ($items | map(select(.apiVersion == "apps/v1" and .kind == "Deployment" and
      .metadata.namespace == "orka-system" and .metadata.name == "orka-controller-manager"))) as $controllers
  | ($items | map(select(.apiVersion == "admissionregistration.k8s.io/v1" and
      .kind == "ValidatingWebhookConfiguration" and .metadata.name == "orka-admission"))) as $webhooks
  | ($controllers[0].spec.template.spec.containers[]? | select(.name == "manager") | .args) as $args
  | ($controllers | length) == 1 and
    ($controllers[0].spec.template.spec.containers | map(select(.name == "manager")) | length) == 1 and
    ([$args[] | select(startswith("--controller-mode="))] | length) == 1 and
    ([$args[] | select(. == "--controller-mode=harness-v2")] | length) == 1 and
    ([$args[] | select(startswith("--watch-namespace="))] | length) == 1 and
    ([$args[] | select(. == "--watch-namespace=orka-system")] | length) == 1 and
    ([$args[] | select(. == "--leader-elect" or . == "--leader-elect=true")] | length) == 1 and
    ([$args[] | select(startswith("--harness-v1-"))] | length) == 0 and
    ($controllers[0].spec.template.spec.containers | any(
      .name == "manager" and
      (.image | test("@sha256:[a-f0-9]{64}$"))
    )) and
    ($items | map(select(
      .metadata.labels["app.kubernetes.io/component"] == "agent-harness-wrapper" or
      (.kind == "Deployment" and (.metadata.name | endswith("agent-harness-wrapper")))
    )) | length) == 0 and
    ([$args[] | select(. == "--webhook-cert-rotation-secret=orka-webhook-tls")] | length) == 1 and
    ([$args[] | select(. == "--webhook-cert-rotation-webhook=orka-admission")] | length) == 1 and
    ([$args[] | select(. == "--webhook-cert-rotation-dns-name=orka-webhook.orka-system.svc")] | length) == 1 and
    ($webhooks | length) == 1 and
    ($webhooks[0].webhooks | length) == 9 and
    ([$webhooks[0].webhooks[].name] | unique | length) == 9 and
    ($webhooks[0].webhooks | all(
      .failurePolicy == "Fail" and
      .sideEffects == "None" and
      (.clientConfig | has("caBundle") | not) and
      .clientConfig.service.name == "orka-webhook" and
      .clientConfig.service.namespace == "orka-system" and
      .namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "orka-system" and
      .namespaceSelector.matchLabels["orka.ai/controller-mode"] == "harness-v2"
    ))
' "${rendered_json}" >/dev/null || {
  echo "production overlay must contain one digest-pinned static harness-v2 controller that serves and certifies the nine fail-closed, orka-system-scoped orka-admission webhooks, and no v1 wrapper path" >&2
  exit 1
}

validate_existing_controller_identity() {
  : >"${existing_namespace}"
  if ! "${kubectl}" get namespace orka-system --ignore-not-found -o json >"${existing_namespace}"; then
    echo "unable to inspect the existing orka-system Namespace" >&2
    return 1
  fi
  : >"${existing_controller}"
  if ! "${kubectl}" -n orka-system get deployment orka-controller-manager \
    --ignore-not-found -o json >"${existing_controller}"; then
    echo "unable to inspect the existing orka-system/orka-controller-manager Deployment" >&2
    return 1
  fi

  if [[ ! -s "${existing_namespace}" && ! -s "${existing_controller}" ]]; then
    return 0
  fi
  if [[ ! -s "${existing_namespace}" ]] || ! jq -e '
    .apiVersion == "v1" and
    .kind == "Namespace" and
    .metadata.name == "orka-system" and
    .metadata.labels["orka.ai/controller-mode"] == "harness-v2"
  ' "${existing_namespace}" >/dev/null; then
    echo "existing namespace orka-system must already claim orka.ai/controller-mode=harness-v2; unlabeled, implicit, legacy, or opposite-mode namespaces cannot be adopted in place" >&2
    return 1
  fi
  [[ -s "${existing_controller}" ]] || return 0

  if ! jq -e '
    .apiVersion == "apps/v1" and
    .kind == "Deployment" and
    .metadata.namespace == "orka-system" and
    .metadata.name == "orka-controller-manager" and
    ([.spec.template.spec.containers[] | select(.name == "manager")] | length) == 1 and
    ([.spec.template.spec.containers[] | select(.name == "manager") | .args[]? |
      select(startswith("--controller-mode="))] == ["--controller-mode=harness-v2"]) and
    ([.spec.template.spec.containers[] | select(.name == "manager") | .args[]? |
      select(startswith("--watch-namespace="))] == ["--watch-namespace=orka-system"])
  ' "${existing_controller}" >/dev/null; then
    echo "implicit, legacy, differently scoped, or opposite-mode controllers cannot be upgraded in place; settle or retire the existing installation and deploy harness-v2 in a fresh namespace" >&2
    return 1
  fi
}

validate_snapshot_secret() {
  if ! jq -er '.data["snapshot-key"] | select(type == "string" and length > 0)' "${snapshot_secret}" \
    | base64 -d >"${snapshot_key_data}" 2>/dev/null; then
    echo "agent-execution-snapshot-key Secret must contain snapshot-key" >&2
    return 1
  fi

  local encoded_key key_size
  key_size="$(wc -c <"${snapshot_key_data}" | tr -d '[:space:]')"
  if [[ "${key_size}" == "32" ]]; then
    return 0
  fi

  # Match the controller parser: raw input is accepted only at exactly 32
  # bytes. Otherwise trim surrounding whitespace from base64 text, while
  # retaining Go's allowance for embedded CR/LF line wrapping.
  encoded_key="$(<"${snapshot_key_data}")"
  while [[ -n "${encoded_key}" && "${encoded_key}" == [[:space:]]* ]]; do
    encoded_key="${encoded_key:1}"
  done
  while [[ -n "${encoded_key}" && "${encoded_key}" == *[[:space:]] ]]; do
    encoded_key="${encoded_key:0:${#encoded_key}-1}"
  done
  encoded_key="${encoded_key//$'\r'/}"
  encoded_key="${encoded_key//$'\n'/}"
  if [[ "${encoded_key}" =~ ^[A-Za-z0-9+/]{43}=$ ]]; then
    printf '%s' "${encoded_key}" >"${snapshot_key_encoded}"
  else
    : >"${snapshot_key_encoded}"
  fi
  if [[ "$(wc -c <"${snapshot_key_encoded}" | tr -d '[:space:]')" == "44" ]] \
    && base64 -d <"${snapshot_key_encoded}" >"${snapshot_key_decoded}" 2>/dev/null \
    && [[ "$(wc -c <"${snapshot_key_decoded}" | tr -d '[:space:]')" == "32" ]]; then
    return 0
  fi

  echo "agent-execution-snapshot-key/snapshot-key must contain exactly 32 raw bytes or their base64 encoding" >&2
  return 1
}

ensure_snapshot_secret() {
  if ! "${kubectl}" -n orka-system get secret agent-execution-snapshot-key --ignore-not-found -o json >"${snapshot_secret}"; then
    echo "unable to inspect orka-system/agent-execution-snapshot-key" >&2
    return 1
  fi
  if [[ ! -s "${snapshot_secret}" ]]; then
    umask 077
    dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\r\n' >"${snapshot_key}"
    "${kubectl}" -n orka-system create secret generic agent-execution-snapshot-key \
      --from-file="snapshot-key=${snapshot_key}" >/dev/null
    "${kubectl}" -n orka-system get secret agent-execution-snapshot-key -o json >"${snapshot_secret}"
  fi
  validate_snapshot_secret
}

wait_for_workload_dependencies() {
  local deployment service attempt ready
  local -a dependencies=(
    orka-provider-auth-proxy
    orka-scm-egress-proxy
    orka-workspace-publisher
  )

  for deployment in "${dependencies[@]}"; do
    "${kubectl}" -n orka-system rollout status "deployment/${deployment}" --timeout=2m >/dev/null
  done

  for service in "${dependencies[@]}"; do
    ready=0
    for attempt in {1..50}; do
      if "${kubectl}" -n orka-system get endpoints "${service}" -o json >"${workload_dependency_endpoints}" \
        && jq -e '[.subsets[]?.addresses[]?.ip] | unique | length >= 1' \
          "${workload_dependency_endpoints}" >/dev/null; then
        ready=1
        break
      fi
      sleep 0.2
    done
    if (( ready == 0 )); then
      echo "${service} Service must expose at least one ready endpoint before controller rollout after ${attempt} attempts" >&2
      return 1
    fi
  done
}

# Establish every workload prerequisite, including the fail-closed webhook
# configuration, before rolling the static harness-v2 controller that serves
# it. The controller turns ready only after its certificate rotator has
# injected the CA into every webhook. Every retry repeats these idempotent
# phases, so interruption after any apply still converges on the desired
# generation without rotating an existing snapshot key.
bash "${script_dir}/lib/ensure-static-mode-namespace.sh" "${kubectl}" orka-system harness-v2
validate_existing_controller_identity
# Preserve the immutable mode claim as an atomic precondition while converging
# the remaining platform labels. This cannot relabel or adopt a namespace that
# changes identity after the preflight.
"${kubectl}" patch namespace orka-system --type=json --patch-file "${namespace_metadata_patch}" >/dev/null
ensure_snapshot_secret
"${kubectl}" apply -f "${runtime_config}"
"${kubectl}" apply -f "${workload_prerequisite_manifest}"
wait_for_workload_dependencies
"${kubectl}" apply -f "${controller_manifest}"
"${kubectl}" -n orka-system rollout status deployment/orka-controller-manager --timeout=5m >/dev/null
"${kubectl}" get validatingwebhookconfiguration orka-admission -o json >"${admission_webhooks}"
jq -e '(.webhooks | length) == 9 and all(.webhooks[]; (.clientConfig.caBundle // "") != "")' \
  "${admission_webhooks}" >/dev/null || {
  echo "orka-admission webhooks must carry the controller-managed CA after rollout" >&2
  exit 1
}
# Retire the standalone admission runtime from earlier releases now that the
# controller serves the webhooks.
"${kubectl}" -n orka-system delete deployment,service,poddisruptionbudget,networkpolicy,serviceaccount \
  orka-admission --ignore-not-found >/dev/null
"${kubectl}" delete clusterrolebinding,clusterrole orka-admission --ignore-not-found >/dev/null
