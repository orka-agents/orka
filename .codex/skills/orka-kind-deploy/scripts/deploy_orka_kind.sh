#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: deploy_orka_kind.sh [--repo PATH] [--cluster NAME] [--context NAME] [--controller-image IMAGE]

Rebuild all Orka images, load them into a local kind cluster, install CRDs,
deploy the controller, and wait for the rollout to finish.

When --cluster is provided without --context, the script uses the standard
kind context name: kind-<cluster>.
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

  kubectl config view -o jsonpath='{range .contexts[*]}{.name}{"\t"}{.context.cluster}{"\n"}{end}' \
    | awk -F'\t' -v context="$context" '$1 == context { print $2; exit }'
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
  if [[ -n "$cluster_name" ]]; then
    context_name="kind-$cluster_name"
  else
    context_name="$(kubectl config current-context)"
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
