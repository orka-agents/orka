#!/usr/bin/env bash
# Install the kubernetes-sigs agent-sandbox stack and a demo SandboxTemplate.
#
# Mirrors scripts/live-agent-sandbox-e2e.sh:501-649, but uses the orka-live
# template at hack/demos/cluster/templates/orka-live-template.yaml and a
# runtime image that bundles git + gh so Demo 60's builder can open PRs.
#
# Requires: kind, kubectl. ORKA_SANDBOX_RUNTIME_IMAGE selects the runtime
# image; the default is an image you must build/load yourself (see
# RECORDING.md §11.5 "Known unknowns").

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"
agent_sandbox_version="${ORKA_AGENT_SANDBOX_VERSION:-v0.4.6}"
demo_namespace="${DEMO_NAMESPACE:-demo-magic}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
sandbox_router_url="${ORKA_SANDBOX_ROUTER_URL:-http://sandbox-router-svc.${demo_namespace}.svc.cluster.local:8080}"
sandbox_default_template="${ORKA_SANDBOX_DEFAULT_TEMPLATE:-orka-live-template}"
sandbox_cleanup_policy="${ORKA_SANDBOX_CLEANUP_POLICY:-retain}"

# The runtime image is *intentionally* not built by this script. You either
# point at a published image, or build/load a local one before invoking
# Demo 60. RECORDING.md §11.5 documents the requirements (git + gh + a
# writable /workspace, exec'ing sandbox-runtime on port 8888).
runtime_image="${ORKA_SANDBOX_RUNTIME_IMAGE:-orka-sandbox-runtime:demo}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
template_file="${script_dir}/templates/orka-live-template.yaml"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v kind    >/dev/null 2>&1 || die "missing required command: kind"
command -v kubectl >/dev/null 2>&1 || die "missing required command: kubectl"
command -v jq      >/dev/null 2>&1 || die "missing required command: jq"

if ! kind get clusters | grep -qx "${cluster_name}"; then
  die "kind cluster ${cluster_name} not found — run hack/demos/cluster/cluster-up.sh first"
fi

log "Selecting kubectl context kind-${cluster_name}"
kubectl config use-context "kind-${cluster_name}" >/dev/null

log "Installing agent-sandbox ${agent_sandbox_version} CRDs + controllers"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/manifest.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/extensions.yaml"

log "Ensuring namespace ${demo_namespace}"
kubectl create namespace "${demo_namespace}" --dry-run=client -o yaml | kubectl apply -f -

log "Applying orka-live SandboxTemplate (runtime image: ${runtime_image})"
sed "s|REPLACE_RUNTIME_IMAGE|${runtime_image}|g; s|namespace: demo-magic|namespace: ${demo_namespace}|" \
  "${template_file}" \
  | kubectl apply -f -

# Patch the Orka controller-manager Deployment so the sandbox-runner code
# path is enabled and points at the in-cluster router + the default
# template we just applied. Idempotent via jq upsert_arg, mirroring
# scripts/live-agent-sandbox-e2e.sh:565-602.
if kubectl -n "${orka_namespace}" get deployment "${controller_deployment}" >/dev/null 2>&1; then
  log "Patching ${controller_deployment} agent-sandbox flags"
  kubectl -n "${orka_namespace}" get deployment "${controller_deployment}" -o json \
    | jq \
        --arg routerURL "${sandbox_router_url}" \
        --arg template "${sandbox_default_template}" \
        --arg cleanup  "${sandbox_cleanup_policy}" \
        '
        def upsert_arg($name; $value):
          . as $args
          | if any($args[]?; startswith($name + "=")) then
              map(if startswith($name + "=") then $name + "=" + $value else . end)
            else
              $args + [$name + "=" + $value]
            end;
        .spec.template.spec.containers |= map(
          if .name == "manager" then
            .args = ((.args // []) | upsert_arg("--agent-sandbox-enabled"; "true"))
            | .args = ((.args // []) | upsert_arg("--agent-sandbox-router-url"; $routerURL))
            | .args = ((.args // []) | upsert_arg("--agent-sandbox-default-template"; $template))
            | .args = ((.args // []) | upsert_arg("--agent-sandbox-cleanup-policy"; $cleanup))
          else . end
        )
        ' \
    | kubectl apply -f -
  kubectl -n "${orka_namespace}" rollout status deployment/"${controller_deployment}" --timeout=300s
else
  log "orka controller deployment ${controller_deployment} not found — skipping flag patch"
fi

log "agent-sandbox stack installed. Verify with:"
log "  kubectl get sandboxtemplate -n ${demo_namespace} orka-live-template"
log "If the runtime image isn't loaded into kind yet, do:"
log "  kind load docker-image ${runtime_image} --name ${cluster_name}"
