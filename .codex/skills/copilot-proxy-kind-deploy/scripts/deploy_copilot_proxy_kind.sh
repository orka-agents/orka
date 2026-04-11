#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: deploy_copilot_proxy_kind.sh [--repo PATH] [--context NAME] [--cluster NAME] [--namespace NAME] [--image IMAGE] [--manifest PATH] [--skip-build] [--dry-run]

Build the local copilot-proxy image, load it into a kind cluster, patch the
Kubernetes manifest to use that image, apply it, wait for rollout, and print
the current login state from the pod logs.

When --cluster is provided without --context, the script uses the standard
kind context name: kind-<cluster>.

When --repo is omitted, the script tries COPILOT_PROXY_REPO, the current git
repository, and a sibling ../copilot-proxy checkout before failing.
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

run_cmd() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  if [[ "$dry_run" == true ]]; then
    return 0
  fi
  "$@"
}

is_copilot_proxy_repo() {
  local candidate="${1:-}"
  [[ -n "$candidate" ]] || return 1
  [[ -f "$candidate/Dockerfile" && -f "$candidate/k8s/copilot-proxy.yaml" ]]
}

discover_repo_root() {
  local git_root=""
  local candidate=""
  local -a candidates=()

  if [[ -n "${COPILOT_PROXY_REPO:-}" ]]; then
    candidates+=("$COPILOT_PROXY_REPO")
  fi

  if git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
    candidates+=("$git_root" "$git_root/../copilot-proxy")
  fi

  candidates+=("$(pwd)" "$(pwd)/../copilot-proxy")

  for candidate in "${candidates[@]}"; do
    if is_copilot_proxy_repo "$candidate"; then
      repo_root="$(cd "$candidate" && pwd)"
      return 0
    fi
  done

  return 1
}

print_login_status() {
  local logs line url code

  if [[ "$dry_run" == true ]]; then
    echo "Login status: skipped during dry-run"
    return
  fi

  logs="$(kubectl --context "$context_name" -n "$namespace" logs deployment/copilot-proxy --tail=50 2>&1 || true)"
  if [[ -z "$logs" ]]; then
    echo "Login status: no recent copilot-proxy logs found"
    return
  fi

  if grep -Fq "authenticated successfully" <<<"$logs"; then
    echo "Login status: copilot-proxy reports authenticated successfully"
    return
  fi

  line="$(grep -E 'Please visit .* and enter code: ' <<<"$logs" | tail -n 1 || true)"
  if [[ -n "$line" ]]; then
    url="$(sed -E 's/Please visit ([^ ]+) and enter code: .*/\1/' <<<"$line")"
    code="$(sed -E 's/Please visit [^ ]+ and enter code: (.*)/\1/' <<<"$line")"
    echo "Login status: login required"
    echo "Verification URL: $url"
    echo "User code: $code"
    return
  fi

  echo "Login status: no explicit login line found in recent logs"
  echo "$logs"
}

repo_root=""
context_name=""
cluster_name=""
namespace="default"
image="copilot-proxy:kind"
manifest_path=""
skip_build=false
dry_run=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      repo_root="${2:?missing value for --repo}"
      shift 2
      ;;
    --context)
      context_name="${2:?missing value for --context}"
      shift 2
      ;;
    --cluster)
      cluster_name="${2:?missing value for --cluster}"
      shift 2
      ;;
    --namespace)
      namespace="${2:?missing value for --namespace}"
      shift 2
      ;;
    --image)
      image="${2:?missing value for --image}"
      shift 2
      ;;
    --manifest)
      manifest_path="${2:?missing value for --manifest}"
      shift 2
      ;;
    --skip-build)
      skip_build=true
      shift
      ;;
    --dry-run)
      dry_run=true
      shift
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

for cmd in docker git kind kubectl sed mktemp; do
  require_cmd "$cmd"
done

if [[ -z "$repo_root" ]]; then
  if ! discover_repo_root; then
    echo "Unable to locate a copilot-proxy repository automatically." >&2
    echo "Pass --repo PATH or set COPILOT_PROXY_REPO to a checkout containing Dockerfile and k8s/copilot-proxy.yaml." >&2
    exit 1
  fi
fi
repo_root="$(cd "$repo_root" && pwd)"

if [[ ! -f "$repo_root/Dockerfile" ]]; then
  echo "Not a copilot-proxy repository root: missing Dockerfile in $repo_root" >&2
  exit 1
fi

if [[ -z "$manifest_path" ]]; then
  manifest_path="$repo_root/k8s/copilot-proxy.yaml"
fi
if [[ ! -f "$manifest_path" ]]; then
  echo "Missing manifest: $manifest_path" >&2
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

tmp_manifest="$(mktemp)"
cleanup() {
  rm -f "$tmp_manifest"
}
trap cleanup EXIT

sed \
  -e "s|^\([[:space:]]*image:[[:space:]]*\).*|\1${image}|" \
  -e "s|^\([[:space:]]*imagePullPolicy:[[:space:]]*\).*|\1IfNotPresent|" \
  "$manifest_path" >"$tmp_manifest"

echo "Repository: $repo_root"
echo "Context: $context_name"
echo "Kind cluster: $cluster_name"
echo "Namespace: $namespace"
echo "Image: $image"
echo "Manifest: $manifest_path"

if [[ "$skip_build" != true ]]; then
  run_cmd docker build -t "$image" "$repo_root"
fi

run_cmd kind load docker-image "$image" --name "$cluster_name"

if ! kubectl --context "$context_name" get namespace "$namespace" >/dev/null 2>&1; then
  run_cmd kubectl --context "$context_name" create namespace "$namespace"
fi

run_cmd kubectl --context "$context_name" apply -n "$namespace" -f "$tmp_manifest"
run_cmd kubectl --context "$context_name" -n "$namespace" rollout status deployment/copilot-proxy --timeout=180s
run_cmd kubectl --context "$context_name" -n "$namespace" get deploy,svc,pods -l app=copilot-proxy
print_login_status
