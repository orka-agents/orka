#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: deploy_copilot_proxy_kind.sh [--repo PATH] [--context NAME] [--cluster NAME] [--namespace NAME] [--image IMAGE] [--manifest PATH] [--skip-build] [--dry-run]

Build the local copilot-proxy image, load it into a kind cluster, patch the
Kubernetes manifest to use that image, apply it, wait for rollout, and print
the current login state from the pod logs.
EOF
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
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

default_repo="/Users/sozercan/projects/copilot-proxy"
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
  if [[ -d "$default_repo" ]]; then
    repo_root="$default_repo"
  elif git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
    repo_root="$git_root"
  else
    repo_root="$(pwd)"
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
