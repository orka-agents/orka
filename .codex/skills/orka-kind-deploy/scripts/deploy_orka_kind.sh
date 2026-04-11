#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: deploy_orka_kind.sh [--repo PATH] [--cluster NAME] [--context NAME] [--controller-image IMAGE]

Rebuild all Orka images, load them into a local kind cluster, install CRDs,
deploy the controller, and wait for the rollout to finish.
EOF
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
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

for cmd in docker git kind kubectl make; do
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
  context_name="$(kubectl config current-context)"
fi

if [[ -z "$cluster_name" ]]; then
  if [[ "$context_name" == kind-* ]]; then
    cluster_name="${context_name#kind-}"
  else
    echo "Current context '$context_name' is not a kind context. Pass --cluster explicitly." >&2
    exit 1
  fi
fi

if ! kind get clusters | grep -Fxq "$cluster_name"; then
  echo "kind cluster '$cluster_name' not found" >&2
  exit 1
fi

backup_file="$(mktemp)"
kubectl_wrapper="$(mktemp)"
cp "$kustomization_file" "$backup_file"
cat >"$kubectl_wrapper" <<EOF
#!/usr/bin/env bash
exec kubectl --context "$context_name" "\$@"
EOF
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

(
  cd "$repo_root"
  KIND_CLUSTER="$cluster_name" IMG="$controller_image" make test-e2e-setup-only
)

(
  cd "$repo_root"
  KUBECTL="$kubectl_wrapper" make install deploy IMG="$controller_image"
)

"${kube_cmd[@]}" -n "$namespace" rollout status deployment/orka-controller-manager --timeout=180s
"${kube_cmd[@]}" -n "$namespace" get pods,svc,deploy
