#!/usr/bin/env bash
# shellcheck disable=SC2031 # Sourced TLS helpers use subshell-local namespace/image variables.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: deploy_orka_kind.sh [--repo PATH] [--cluster NAME] [--context NAME] [--controller-image IMAGE]

Rebuild all Orka images, load them into a local kind cluster, publish them to
its local registry, and deploy digest-pinned workloads. Bootstrap test-only
admission TLS when absent and wait for all five Deployments to roll out.

Pass --cluster or --context explicitly; current-context is never used.
When --cluster is provided without --context, the script uses the standard
kind context name: kind-<cluster>. For kindctl clusters, supply its scoped
KUBECONFIG (for example, run via kindctl exec).
EOF
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
}

context_cluster_for() {
  local context="$1"

  kubectl --context "$context" config view -o jsonpath='{range .contexts[*]}{.name}{"\t"}{.context.cluster}{"\n"}{end}' \
    | awk -F'\t' -v context="$context" '$1 == context { print $2; exit }'
}

validate_admission_tls() {
  local secret="$1" cert private_key ca cert_public_key key_public_key san_extension
  jq -e '
    .type == "kubernetes.io/tls" and
    (.data["tls.crt"] | type == "string" and length > 0) and
    (.data["tls.key"] | type == "string" and length > 0) and
    (.data["ca.crt"] | type == "string" and length > 0)
  ' <<<"$secret" >/dev/null || return 1
  cert="$(jq -er '.data["tls.crt"]' <<<"$secret" | base64 -d)" || return 1
  private_key="$(jq -er '.data["tls.key"]' <<<"$secret" | base64 -d)" || return 1
  ca="$(jq -er '.data["ca.crt"]' <<<"$secret" | base64 -d)" || return 1
  cert_public_key="$(openssl x509 -in <(printf '%s\n' "$cert") -pubkey -noout)" || return 1
  key_public_key="$(openssl pkey -in <(printf '%s\n' "$private_key") -passin pass: -pubout)" || return 1
  [[ "$cert_public_key" == "$key_public_key" ]] || return 1
  san_extension="$(openssl x509 -in <(printf '%s\n' "$cert") -noout -ext subjectAltName)" || return 1
  [[ "$san_extension" == *"X509v3 Subject Alternative Name"* ]] || return 1
  openssl verify -x509_strict -purpose sslserver -verify_hostname "orka-admission.$namespace.svc" \
    -CAfile <(printf '%s\n' "$ca") -untrusted <(printf '%s\n' "$cert") <(printf '%s\n' "$cert")
}

repo_root=""
cluster_name=""
context_name=""
controller_image="controller:kind"
namespace="orka-system"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      repo_root="${2:?missing value for --repo}"
      shift 2
      ;;
    --cluster)
      cluster_name="${2:?missing value for --cluster}"
      shift 2
      ;;
    --context)
      context_name="${2:?missing value for --context}"
      shift 2
      ;;
    --controller-image)
      controller_image="${2:?missing value for --controller-image}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

for cmd in base64 curl docker git jq kind kubectl make openssl; do
  require_cmd "$cmd"
done

if [[ -z "$repo_root" ]]; then
  if git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
    repo_root="$git_root"
  else
    repo_root="$(pwd)"
  fi
fi
repo_root="$(cd "$repo_root" && pwd)"

if [[ ! -f "$repo_root/Makefile" ]]; then
  echo "Not an Orka repository root: missing Makefile in $repo_root" >&2
  exit 1
fi

kustomization_file="$repo_root/config/manager/kustomization.yaml"
if [[ ! -f "$kustomization_file" ]]; then
  echo "Not an Orka repository root: missing $kustomization_file" >&2
  exit 1
fi

if [[ -z "$context_name" ]]; then
  if [[ -n "$cluster_name" ]]; then
    context_name="kind-$cluster_name"
  else
    echo "Pass --cluster or --context explicitly; current-context is not used." >&2
    exit 1
  fi
fi

context_cluster="$(context_cluster_for "$context_name")"
if [[ -z "$context_cluster" ]]; then
  echo "kubectl context '$context_name' not found" >&2
  exit 1
fi

if [[ "$context_cluster" == kind-* ]]; then
  context_kind_cluster="${context_cluster#kind-}"
else
  context_kind_cluster=""
fi

if [[ -z "$cluster_name" ]]; then
  if [[ -n "$context_kind_cluster" ]]; then
    cluster_name="$context_kind_cluster"
  else
    echo "Context '$context_name' targets '$context_cluster', not a kind cluster. Pass --cluster explicitly." >&2
    exit 1
  fi
elif [[ -z "$context_kind_cluster" || "$context_kind_cluster" != "$cluster_name" ]]; then
  echo "Context '$context_name' targets '$context_cluster', which does not match kind cluster '$cluster_name'." >&2
  exit 1
fi

if ! kind get clusters | grep -Fxq "$cluster_name"; then
  echo "kind cluster '$cluster_name' not found" >&2
  exit 1
fi

backup_file="$(mktemp)"
kubectl_wrapper="$(mktemp)"
cp "$kustomization_file" "$backup_file"
{
  printf '#!/usr/bin/env bash\n'
  printf 'exec kubectl --context %q "$@"\n' "$context_name"
} >"$kubectl_wrapper"
chmod +x "$kubectl_wrapper"

kube_cmd=("$kubectl_wrapper")

restore_kustomization() {
  if [[ -f "$backup_file" ]]; then
    cp "$backup_file" "$kustomization_file"
    rm -f "$backup_file"
  fi
  if [[ -f "$kubectl_wrapper" ]]; then
    rm -f "$kubectl_wrapper"
  fi
}

trap restore_kustomization EXIT

echo "Repository: $repo_root"
echo "Context: $context_name"
echo "Kind cluster: $cluster_name"
echo "Controller image: $controller_image"

# shellcheck source=scripts/lib/kind-local-registry.sh
. "$repo_root/scripts/lib/kind-local-registry.sh"
# shellcheck source=scripts/lib/e2e-admission-tls.sh
. "$repo_root/scripts/lib/e2e-admission-tls.sh"

# The test CA and serving certificate last seven days. Their renewal must not
# silently remove shared admission webhooks or bypass other controllers' trust.
admission_tls="$("${kube_cmd[@]}" -n "$namespace" get secret orka-admission-tls --ignore-not-found -o json)"
if [[ -n "$admission_tls" ]] && ! validate_admission_tls "$admission_tls" >/dev/null 2>&1; then
  echo "$namespace/orka-admission-tls has expired, missing, or invalid TLS data." >&2
  echo "Use a fresh kindctl cluster, or coordinate TLS renewal using config/orka-admission-webhooks/README.md before retrying." >&2
  exit 1
fi
unset admission_tls

ai_worker_image="${AI_WORKER_IMG:-ghcr.io/orka-agents/orka/ai-worker:kind}"
general_worker_image="${GENERAL_WORKER_IMG:-ghcr.io/orka-agents/orka/general-worker:kind}"
publisher_image="${WORKSPACE_PUBLISHER_IMG:-ghcr.io/orka-agents/orka/workspace-publisher:kind}"
codex_image="${ACP_CODEX_RUNTIME_IMG:-ghcr.io/orka-agents/orka/acp-codex-runtime:kind}"
claude_image="${ACP_CLAUDE_RUNTIME_IMG:-ghcr.io/orka-agents/orka/acp-claude-runtime:kind}"
copilot_image="${ACP_COPILOT_RUNTIME_IMG:-ghcr.io/orka-agents/orka/acp-copilot-runtime:kind}"
opencode_image="${ACP_OPENCODE_RUNTIME_IMG:-ghcr.io/orka-agents/orka/acp-opencode-runtime:kind}"

orka_kind_registry_start "$cluster_name"
(
  cd "$repo_root"
  make test-e2e-setup-only KIND_CLUSTER="$cluster_name" KUBECTL="$kubectl_wrapper" \
    IMG="$controller_image" AI_WORKER_IMG="$ai_worker_image" GENERAL_WORKER_IMG="$general_worker_image" \
    WORKSPACE_PUBLISHER_IMG="$publisher_image" \
    ACP_CODEX_RUNTIME_IMG="$codex_image" ACP_CLAUDE_RUNTIME_IMG="$claude_image" \
    ACP_COPILOT_RUNTIME_IMG="$copilot_image" ACP_OPENCODE_RUNTIME_IMG="$opencode_image"
)

# Bash clears errexit in command substitutions; keep failed tags from pushing stale images.
controller_ref="$(set -e; orka_kind_registry_push "$controller_image" orka/controller)"
publisher_ref="$(set -e; orka_kind_registry_push "$publisher_image" orka/workspace-publisher)"
codex_ref="$(set -e; orka_kind_registry_push "$codex_image" orka/acp-codex-runtime)"
claude_ref="$(set -e; orka_kind_registry_push "$claude_image" orka/acp-claude-runtime)"
copilot_ref="$(set -e; orka_kind_registry_push "$copilot_image" orka/acp-copilot-runtime)"
opencode_ref="$(set -e; orka_kind_registry_push "$opencode_image" orka/acp-opencode-runtime)"
ai_worker_ref="$(set -e; orka_kind_registry_push "$ai_worker_image" orka/ai-worker)"
general_worker_ref="$(set -e; orka_kind_registry_push "$general_worker_image" orka/general-worker)"

(
  cd "$repo_root"
  make install KUBECTL="$kubectl_wrapper"
  # Reuse existing TLS on redeploy; rotating its CA would invalidate live webhooks.
  admission_tls="$("${kube_cmd[@]}" -n "$namespace" get secret orka-admission-tls --ignore-not-found -o name)"
  if [[ -z "$admission_tls" ]]; then
    orka_e2e_bootstrap_admission_tls "$kubectl_wrapper" "$namespace"
  fi
  # The production overlay includes an ingress policy here even without a Vekil proxy.
  vekil_namespace="$("${kube_cmd[@]}" get namespace vekil-system --ignore-not-found -o name)"
  if [[ -z "$vekil_namespace" ]]; then
    "${kube_cmd[@]}" create namespace vekil-system
  fi
  make deploy KUBECTL="$kubectl_wrapper" \
    IMG="$controller_ref" WORKSPACE_PUBLISHER_IMG="$publisher_ref" \
    ACP_CODEX_RUNTIME_IMG="$codex_ref" ACP_CLAUDE_RUNTIME_IMG="$claude_ref" \
    ACP_COPILOT_RUNTIME_IMG="$copilot_ref" ACP_OPENCODE_RUNTIME_IMG="$opencode_ref" \
    AI_WORKER_IMG="$ai_worker_ref" GENERAL_WORKER_IMG="$general_worker_ref"
)

for deployment in orka-controller-manager orka-workspace-publisher orka-provider-auth-proxy orka-scm-egress-proxy orka-admission; do
  "${kube_cmd[@]}" -n "$namespace" rollout status "deployment/$deployment" --timeout=180s
done
"${kube_cmd[@]}" -n "$namespace" get pods,svc,deploy
