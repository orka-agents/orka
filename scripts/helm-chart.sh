#!/usr/bin/env bash
set -euo pipefail

LC_ALL=C
export LC_ALL

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_DIR="${ROOT_DIR}/charts/orka"
GENERATED_CRD_DIR="${ROOT_DIR}/config/crd/bases"
CHART_CRD_DIR="${CHART_DIR}/crds"
EXPECTED_CRD_COUNT=9

usage() {
  cat <<'USAGE'
Usage: scripts/helm-chart.sh <sync|check>

  sync   Replace charts/orka/crds with the generated CRDs from config/crd/bases.
  check  Verify the CRD mirror and Helm chart reliability invariants.
USAGE
}

fail() {
  echo "helm-chart: $*" >&2
  exit 1
}

collect_generated_crds() {
  GENERATED_CRDS=()

  local file
  for file in "$GENERATED_CRD_DIR"/*.yaml; do
    [[ -f "$file" ]] || continue
    GENERATED_CRDS+=("$file")
  done

  if [[ ${#GENERATED_CRDS[@]} -ne $EXPECTED_CRD_COUNT ]]; then
    fail "expected ${EXPECTED_CRD_COUNT} generated CRDs in ${GENERATED_CRD_DIR}, found ${#GENERATED_CRDS[@]}"
  fi
}

sync_crds() {
  collect_generated_crds

  rm -rf "$CHART_CRD_DIR"
  mkdir -p "$CHART_CRD_DIR"

  local source
  for source in "${GENERATED_CRDS[@]}"; do
    cp "$source" "$CHART_CRD_DIR/$(basename "$source")"
  done

  echo "Synced ${#GENERATED_CRDS[@]} generated CRDs into ${CHART_CRD_DIR}."
}

check_crd_mirror() {
  collect_generated_crds

  [[ -d "$CHART_CRD_DIR" ]] || fail "missing ${CHART_CRD_DIR}; run scripts/helm-chart.sh sync"

  local chart_crd_count
  chart_crd_count=$(find "$CHART_CRD_DIR" -type f \( -name '*.yaml' -o -name '*.yml' \) -print | wc -l | tr -d '[:space:]')
  if [[ "$chart_crd_count" -ne $EXPECTED_CRD_COUNT ]]; then
    fail "expected ${EXPECTED_CRD_COUNT} chart CRDs, found ${chart_crd_count}; run scripts/helm-chart.sh sync"
  fi

  local source destination
  for source in "${GENERATED_CRDS[@]}"; do
    destination="$CHART_CRD_DIR/$(basename "$source")"
    [[ -f "$destination" ]] || fail "missing chart CRD $(basename "$source"); run scripts/helm-chart.sh sync"
    cmp -s "$source" "$destination" || fail "chart CRD $(basename "$source") differs from generated source; run scripts/helm-chart.sh sync"
    grep -Fqx 'kind: CustomResourceDefinition' "$destination" || fail "$(basename "$destination") is not a CustomResourceDefinition"
    if grep -Fq '{{' "$destination"; then
      fail "$(basename "$destination") contains Helm template syntax; files under crds/ must be static"
    fi
  done
}

require_line() {
  local file=$1
  local expected=$2
  local description=$3
  grep -Fqx -- "$expected" "$file" || fail "missing ${description}: ${expected}"
}

check_chart() {
  command -v helm >/dev/null 2>&1 || fail "helm is required for chart validation"
  command -v tar >/dev/null 2>&1 || fail "tar is required for packaged chart validation"

  check_crd_mirror

  if grep -Eq '^[[:space:]]*crds:' "$CHART_DIR/values.yaml"; then
    fail "values.yaml contains a CRD toggle, but Helm crds/ lifecycle is controlled by Helm flags, not values"
  fi

  local temp_dir
  temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/orka-helm-chart.XXXXXX")
  trap "rm -rf '$temp_dir'" EXIT

  local release_name=custom-release
  local release_namespace=callback-ns
  local fullname=callback-name
  local service_port=18080
  local general_repository=registry.example/general
  local general_tag=canary

  helm template "$release_name" "$CHART_DIR" \
    --namespace "$release_namespace" \
    --set-string fullnameOverride="$fullname" \
    --set service.port="$service_port" \
    --set-string workers.general.image.repository="$general_repository" \
    --set-string workers.general.image.tag="$general_tag" \
    --show-only templates/service.yaml >"$temp_dir/service.yaml"

  require_line "$temp_dir/service.yaml" "  name: ${fullname}" "custom Service name"
  require_line "$temp_dir/service.yaml" "    app.kubernetes.io/instance: ${release_name}" "custom release label"
  require_line "$temp_dir/service.yaml" "    - port: ${service_port}" "custom Service port"

  helm template "$release_name" "$CHART_DIR" \
    --namespace "$release_namespace" \
    --set-string fullnameOverride="$fullname" \
    --set service.port="$service_port" \
    --set-string workers.general.image.repository="$general_repository" \
    --set-string workers.general.image.tag="$general_tag" \
    --show-only templates/deployment.yaml >"$temp_dir/deployment.yaml"

  require_line "$temp_dir/deployment.yaml" \
    "            - --controller-url=http://${fullname}.${release_namespace}.svc:${service_port}" \
    "controller callback URL"
  require_line "$temp_dir/deployment.yaml" \
    "            - --general-worker-image=${general_repository}:${general_tag}" \
    "general worker image argument"

  helm template "$release_name" "$CHART_DIR" \
    --namespace "$release_namespace" \
    --show-only templates/rbac.yaml >"$temp_dir/rbac.yaml"
  awk '
    /^---$/ {
      document++
      if (document == 2) {
        exit
      }
      next
    }
    document == 1 { print }
  ' "$temp_dir/rbac.yaml" >"$temp_dir/controller-rbac.yaml"

  local resource
  for resource in \
    agentruntimes substrateactorpools \
    agentruntimes/status substrateactorpools/status \
    agentruntimes/finalizers substrateactorpools/finalizers; do
    grep -Fq "\"${resource}\"" "$temp_dir/controller-rbac.yaml" || fail "missing controller RBAC resource ${resource}"
  done

  mkdir -p "$temp_dir/package"
  helm package "$CHART_DIR" --destination "$temp_dir/package" >/dev/null

  local chart_name package_path package_crd_count candidate
  local package_candidates=()
  chart_name=$(awk '$1 == "name:" { print $2; exit }' "$CHART_DIR/Chart.yaml")
  for candidate in "$temp_dir/package/${chart_name}-"*.tgz; do
    [[ -f "$candidate" ]] || continue
    package_candidates+=("$candidate")
  done
  [[ ${#package_candidates[@]} -eq 1 ]] || fail "expected exactly one packaged chart archive"
  package_path=${package_candidates[0]}

  tar -tzf "$package_path" >"$temp_dir/package-files.txt"
  package_crd_count=$(awk -v root="$chart_name" '$0 ~ "^" root "/crds/[^/]+[.]ya?ml$" { count++ } END { print count + 0 }' "$temp_dir/package-files.txt")
  if [[ "$package_crd_count" -ne $EXPECTED_CRD_COUNT ]]; then
    fail "packaged chart contains ${package_crd_count} CRDs, expected ${EXPECTED_CRD_COUNT}"
  fi

  local source archive_path
  for source in "${GENERATED_CRDS[@]}"; do
    archive_path="${chart_name}/crds/$(basename "$source")"
    grep -Fqx "$archive_path" "$temp_dir/package-files.txt" || fail "packaged chart is missing ${archive_path}"
  done

  echo "Helm chart check passed: ${EXPECTED_CRD_COUNT} packaged CRDs, callback URL, controller RBAC, and worker image arguments are valid."
}

case "${1:-}" in
  sync)
    sync_crds
    ;;
  check)
    check_chart
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
