#!/usr/bin/env bash
set -euo pipefail

LC_ALL=C
export LC_ALL

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_DIR="${ROOT_DIR}/charts/orka"
GENERATED_CRD_DIR="${ROOT_DIR}/config/crd/bases"
CHART_CRD_DIR="${CHART_DIR}/crds"
LEGACY_TASK_CRD_FIXTURE="${ROOT_DIR}/testdata/helm-crd-upgrade/legacy-task-crd.yaml"
CHART_README="${CHART_DIR}/README.md"
CRD_UPGRADE_HELPER="${ROOT_DIR}/scripts/helm-crd-upgrade.sh"
ROOT_README="${ROOT_DIR}/README.md"
WEBSITE_GETTING_STARTED="${ROOT_DIR}/website/docs/getting-started.md"
EXPECTED_CRD_COUNT=9
CRD_FIELD_MANAGER=orka-helm-crd-upgrade
EXPECTED_CRD_NAMES=(
  agentruntimes.core.orka.ai
  agents.core.orka.ai
  providers.core.orka.ai
  repositorymonitors.core.orka.ai
  repositoryscans.core.orka.ai
  skills.core.orka.ai
  substrateactorpools.core.orka.ai
  tasks.core.orka.ai
  tools.core.orka.ai
)

usage() {
  cat <<'USAGE'
Usage:
  scripts/helm-chart.sh sync
  scripts/helm-chart.sh check
  scripts/helm-chart.sh upgrade-crds --chart LOCAL_CHART_ARCHIVE \
    --kube-context CONTEXT --release RELEASE --namespace NAMESPACE \
    [--allow-missing-release] [--yes]
  scripts/helm-chart.sh test-upgrade --kind-cluster KIND_CLUSTER \
    --kube-context KIND_CONTEXT

Commands:
  sync          Replace charts/orka/crds with the generated CRDs from
                config/crd/bases.
  check         Verify the CRD mirror, packaged chart, upgrade documentation,
                and other Helm chart reliability invariants.
  upgrade-crds  Run the trusted repository migration helper against a local
                packaged target chart. The archive is treated only as data.
                The helper server-preflights all nine CRDs, exactly replaces
                their specs, and verifies the result. Partial mutations are
                retained with recovery artifacts for manual reconciliation.
                Use --allow-missing-release only to migrate retained CRDs before
                a replacement install, then install the same archive with
                --skip-crds. Otherwise use the same archive for Helm upgrade.
  test-upgrade  Run the deterministic CRD install/upgrade migration test against
                an empty, dedicated kind cluster. The test verifies the context
                maps to the named kind cluster, refuses non-empty targets, and
                fails if it cannot clean up everything it creates.
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

require_text() {
  local file=$1
  local expected=$2
  local description=$3
  grep -Fq -- "$expected" "$file" || fail "missing ${description}: ${expected}"
}

check_replacement_section() {
  local file=$1
  local heading=$2
  local artifact_command=$3
  local section
  section=$(awk -v heading="$heading" '
    $0 == heading { found = 1; print; next }
    found && /^#{1,6}[[:space:]]/ { exit }
    found { print }
  ' "$file")
  [[ -n "$section" ]] || fail "missing retained-CRD replacement section ${heading} in ${file}"
  grep -Fq 'set -euo pipefail' <<<"$section" || fail "replacement section ${heading} is not fail-fast"
  grep -Fq "TARGET_CONTEXT=\"\${TARGET_CONTEXT:?set TARGET_CONTEXT}\"" <<<"$section" || \
    fail "replacement section ${heading} does not require an explicit target context"
  grep -Fq "$artifact_command" <<<"$section" || fail "replacement section ${heading} does not create a target chart archive"
  grep -Fq -- '--allow-missing-release' <<<"$section" || fail "replacement section ${heading} lacks retained-CRD opt-in"
  grep -Fq 'helm install' <<<"$section" || fail "replacement section ${heading} lacks the guarded install"
  grep -Fq -- '--skip-crds' <<<"$section" || fail "replacement section ${heading} does not preserve CRD ownership"
  grep -Fq 'KEEP_WORK_DIR=true' <<<"$section" || fail "replacement section ${heading} does not preserve its archive on failure"
  grep -Fq 'KEEP_WORK_DIR=false' <<<"$section" || fail "replacement section ${heading} does not clean up after success"
}

check_upgrade_documentation() {
  local file
  for file in "$CHART_README" "$ROOT_README" "$WEBSITE_GETTING_STARTED"; do
    [[ -f "$file" ]] || fail "missing Helm upgrade documentation file ${file}"
    require_text "$file" "--chart \"\$TARGET_CHART\"" "local target chart archive in ${file}"
    require_text "$file" '--kube-context' "explicit Helm target context in ${file}"
    require_text "$file" 'set -euo pipefail' "fail-fast upgrade workflow in ${file}"
    require_text "$file" 'helm package charts/orka' "single packaged source artifact in ${file}"
    require_text "$file" 'recovery artifacts' "partial-migration recovery behavior in ${file}"
    require_text "$file" '--allow-missing-release' "retained-CRD pre-install mode in ${file}"
    require_text "$file" '--skip-crds' "replacement install CRD ownership in ${file}"
    require_text "$file" 'HELM_KUBE' "Helm and kubectl cluster binding in ${file}"
    require_text "$file" 'KEEP_WORK_DIR=true' "target archive preservation on failure in ${file}"
    require_text "$file" 'KEEP_WORK_DIR=false' "target archive cleanup after success in ${file}"
    require_text "$file" 'deployed' "upgradeable Helm release status in ${file}"
  done

  check_replacement_section "$ROOT_README" '#### Reinstall with retained CRDs' 'helm package charts/orka'
  check_replacement_section "$WEBSITE_GETTING_STARTED" '#### Reinstalling when CRDs were retained' 'helm package charts/orka'
  check_replacement_section "$CHART_README" '### Replacement install with retained CRDs' 'helm pull'

  require_text "$ROOT_README" 'scripts/helm-chart.sh upgrade-crds' "trusted source-checkout helper in root README"
  require_text "$WEBSITE_GETTING_STARTED" 'scripts/helm-chart.sh upgrade-crds' "trusted source-checkout helper in website guide"
  require_text "$CHART_README" 'helm pull' "single-fetch target chart workflow in chart README"
  require_text "$CHART_README" 'MIGRATOR=' "trusted external migrator in chart README"
  require_text "$CHART_README" 'data only' "chart archive trust boundary in chart README"
  require_text "$CHART_README" '--dry-run=server' "server-side preflight in chart README"
  require_text "$CHART_README" 'same local archive' "artifact binding in chart README"
  require_text "$CHART_README" "Helm does not install or upgrade files in \`crds/\` during \`helm upgrade\`" "Helm CRD upgrade semantics in chart README"
  require_text "$CHART_README" "Helm retains resources from \`crds/\`" "CRD uninstall retention semantics in chart README"

  [[ -x "$CRD_UPGRADE_HELPER" ]] || fail "missing executable trusted CRD migration helper"
  require_text "$CRD_UPGRADE_HELPER" '--dry-run=server' "server-side CRD preflight in trusted helper"
  require_text "$CRD_UPGRADE_HELPER" '--type=json' "exact JSON CRD patch in trusted helper"
  require_text "$CRD_UPGRADE_HELPER" 'partial CRD migration was retained' "fail-closed partial migration handling in trusted helper"
  require_text "$CRD_UPGRADE_HELPER" 'one local packaged chart archive' "local archive requirement in trusted helper"
  require_text "$CRD_UPGRADE_HELPER" '--allow-missing-release' "retained-CRD mode in trusted helper"
  require_text "$CRD_UPGRADE_HELPER" 'Run helm install with --skip-crds' "replacement-install instruction in trusted helper"
  require_text "$CRD_UPGRADE_HELPER" 'HELM_KUBEAPISERVER' "Helm endpoint override guard in trusted helper"
  require_text "$CRD_UPGRADE_HELPER" 'deployed|failed' "upgradeable Helm release status guard in trusted helper"

  local notes_file="${CHART_DIR}/templates/NOTES.txt"
  require_text "$notes_file" 'scripts/helm-chart.sh upgrade-crds' "trusted upgrade helper in chart notes"
  require_text "$notes_file" 'set -euo pipefail' "fail-fast guarded subshell in chart notes"
  require_text "$notes_file" 'trap cleanup_work_dir EXIT' "conditional temporary-directory cleanup in chart notes"
  require_text "$notes_file" 'same archive' "artifact binding in chart notes"
  require_text "$notes_file" '--kube-context' "explicit Helm target context in chart notes"
  require_text "$notes_file" 'recovery artifacts' "partial-migration recovery behavior in chart notes"
  require_text "$notes_file" '--allow-missing-release' "retained-CRD pre-install mode in chart notes"
  require_text "$notes_file" '--skip-crds' "replacement install CRD ownership in chart notes"
  require_text "$notes_file" 'HELM_KUBE' "Helm and kubectl cluster binding in chart notes"
  require_text "$notes_file" 'deployed or failed' "upgradeable Helm release status in chart notes"
  require_text "$notes_file" 'retained' "CRD uninstall retention semantics in chart notes"
}

check_chart() (
  command -v helm >/dev/null 2>&1 || fail "helm is required for chart validation"
  command -v tar >/dev/null 2>&1 || fail "tar is required for packaged chart validation"

  [[ -x "$CRD_UPGRADE_HELPER" ]] || \
    fail "missing executable chart CRD migration helper ${CRD_UPGRADE_HELPER}"
  [[ -f "$LEGACY_TASK_CRD_FIXTURE" ]] || \
    fail "missing pinned legacy Task CRD fixture ${LEGACY_TASK_CRD_FIXTURE}"
  bash -n "$CRD_UPGRADE_HELPER" || fail "chart CRD migration helper has invalid Bash syntax"

  check_crd_mirror
  check_upgrade_documentation

  if grep -Eq '^[[:space:]]*crds:' "$CHART_DIR/values.yaml"; then
    fail "values.yaml contains a CRD toggle, but Helm crds/ lifecycle is controlled by Helm flags, not values"
  fi

  temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/orka-helm-chart.XXXXXX")
  trap 'rm -rf "$temp_dir"' EXIT

  local release_name=custom-release
  local release_namespace=callback-ns
  local fullname=callback-name
  local service_port=18080
  local general_repository=registry.example/general
  local general_tag=canary

  helm show crds "$CHART_DIR" >"$temp_dir/source-crds.yaml"
  local source_crd_count
  source_crd_count=$(grep -c '^kind: CustomResourceDefinition$' "$temp_dir/source-crds.yaml" || true)
  [[ "$source_crd_count" -eq $EXPECTED_CRD_COUNT ]] || \
    fail "helm show crds returned ${source_crd_count} CRDs, expected ${EXPECTED_CRD_COUNT}"

  helm show readme "$CHART_DIR" >"$temp_dir/source-readme.md"
  require_text "$temp_dir/source-readme.md" 'MIGRATOR=' "packaged chart trusted external migrator documentation"
  require_text "$temp_dir/source-readme.md" 'data only' "packaged chart archive trust boundary documentation"

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
  chart_name=$(awk '/^name:[[:space:]]/ { print $2; exit }' "$CHART_DIR/Chart.yaml")
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
  grep -Fqx "${chart_name}/README.md" "$temp_dir/package-files.txt" || fail "packaged chart is missing ${chart_name}/README.md"
  local packaged_shell_files
  packaged_shell_files=$(grep -E "^${chart_name}/.*[.]sh$" "$temp_dir/package-files.txt" || true)
  if [[ -n "$packaged_shell_files" ]]; then
    echo "Unexpected shell files in packaged chart:" >&2
    printf '%s\n' "$packaged_shell_files" >&2
    fail "packaged chart must remain data-only and contain no shell executables"
  fi

  helm show crds "$package_path" >"$temp_dir/package-crds.yaml"
  local rendered_package_crd_count
  rendered_package_crd_count=$(grep -c '^kind: CustomResourceDefinition$' "$temp_dir/package-crds.yaml" || true)
  [[ "$rendered_package_crd_count" -eq $EXPECTED_CRD_COUNT ]] || \
    fail "packaged chart rendered ${rendered_package_crd_count} CRDs, expected ${EXPECTED_CRD_COUNT}"
  helm show readme "$package_path" >"$temp_dir/package-readme.md"
  require_text "$temp_dir/package-readme.md" 'MIGRATOR=' "packaged chart trusted external migrator"
  require_text "$temp_dir/package-readme.md" 'data only' "packaged chart archive trust boundary"

  echo "Helm chart check passed: ${EXPECTED_CRD_COUNT} packaged CRDs, documented pre-upgrade migration, callback URL, controller RBAC, and worker image arguments are valid."
)

upgrade_crds() {
  [[ -x "$CRD_UPGRADE_HELPER" ]] || \
    fail "missing executable trusted CRD migration helper ${CRD_UPGRADE_HELPER}"
  "$CRD_UPGRADE_HELPER" "$@"
}

assert_orka_crds_absent() {
  local kube_context=$1
  local stage=$2
  local existing
  existing=$(kubectl --context "$kube_context" get customresourcedefinitions -o name | awk -F/ '$2 ~ /[.]core[.]orka[.]ai$/ { print }')
  if [[ -n "$existing" ]]; then
    echo "Unexpected Orka CRDs after ${stage}:" >&2
    printf '%s\n' "$existing" >&2
    fail "expected no Orka CRDs after ${stage}"
  fi
}

wait_for_all_orka_crds() {
  local kube_context=$1
  local stage=$2
  local -a resources=()
  local name
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    resources+=("customresourcedefinition/${name}")
  done

  if ! kubectl --context "$kube_context" wait \
    --for=condition=Established \
    --timeout=90s \
    "${resources[@]}" >/dev/null; then
    echo "helm-chart: ${stage} did not make all Orka CRDs Established" >&2
    return 1
  fi

  local actual_names expected_names
  if ! actual_names=$(kubectl --context "$kube_context" get customresourcedefinitions -o name | \
    awk -F/ '$2 ~ /[.]core[.]orka[.]ai$/ { print $2 }' | sort); then
    echo "helm-chart: could not verify the Orka CRD set ${stage}" >&2
    return 1
  fi
  expected_names=$(printf '%s\n' "${EXPECTED_CRD_NAMES[@]}" | sort)
  if [[ "$actual_names" != "$expected_names" ]]; then
    echo "helm-chart: ${stage} did not establish the exact ${EXPECTED_CRD_COUNT}-CRD Orka set" >&2
    return 1
  fi
  echo "Verified ${EXPECTED_CRD_COUNT} Established Orka CRDs ${stage}."
}

verify_test_target_ownership() {
  local kube_context=$1
  local expected_server=$2
  local namespace=$3
  local expected_namespace_uid=$4
  local current_server current_namespace_uid

  current_server=$(kubectl --context "$kube_context" config view --minify -o jsonpath='{.clusters[0].cluster.server}') || return 1
  [[ "$current_server" == "$expected_server" ]] || {
    echo "helm-chart: context ${kube_context} no longer targets the dedicated test cluster" >&2
    return 1
  }
  current_namespace_uid=$(kubectl --context "$kube_context" get namespace "$namespace" \
    --ignore-not-found \
    -o jsonpath='{.metadata.uid}') || return 1
  [[ -n "$expected_namespace_uid" && "$current_namespace_uid" == "$expected_namespace_uid" ]] || {
    echo "helm-chart: namespace ${namespace} is absent or no longer owned by this test run" >&2
    return 1
  }
}

remove_test_crds() {
  local kube_context=$1
  local expected_server=$2
  local namespace=$3
  local expected_namespace_uid=$4
  verify_test_target_ownership \
    "$kube_context" "$expected_server" "$namespace" "$expected_namespace_uid" || return 1

  local -a resources=()
  local name
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    resources+=("customresourcedefinition/${name}")
  done
  kubectl --context "$kube_context" delete \
    --ignore-not-found \
    --wait=false \
    "${resources[@]}" >/dev/null || return 1
  if kubectl --context "$kube_context" wait \
    --for=delete \
    --timeout=90s \
    "${resources[@]}" >/dev/null; then
    return 0
  fi

  # CRD deletion can remain blocked after repeated schema publication tests
  # while apiextensions storage is reinitializing. This path is restricted to
  # the preflight-verified empty, dedicated kind cluster. Force-remove only the
  # known cleanup finalizer in that specific transient state, then verify the
  # CRDs are gone.
  local current reason message deletion_timestamp
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    current=$(kubectl --context "$kube_context" get customresourcedefinition "$name" \
      --ignore-not-found \
      -o json) || return 1
    [[ -n "$current" ]] || continue
    deletion_timestamp=$(jq -r '.metadata.deletionTimestamp // empty' <<<"$current")
    reason=$(jq -r '.status.conditions[]? | select(.type == "Terminating") | .reason // empty' <<<"$current")
    message=$(jq -r '.status.conditions[]? | select(.type == "Terminating") | .message // empty' <<<"$current")
    if [[ -n "$deletion_timestamp" && "$reason" == "InstanceDeletionFailed" && "$message" == *"storage is (re)initializing"* ]]; then
      echo "helm-chart: forcing dedicated-test cleanup for CRD ${name} after apiextensions storage reinitialization timeout" >&2
      kubectl --context "$kube_context" patch customresourcedefinition "$name" \
        --type=merge \
        -p '{"metadata":{"finalizers":[]}}' >/dev/null || return 1
    else
      echo "helm-chart: refusing forced cleanup for CRD ${name}: unexpected termination state" >&2
      return 1
    fi
  done
  kubectl --context "$kube_context" wait \
    --for=delete \
    --timeout=30s \
    "${resources[@]}" >/dev/null
}

test_upgrade() (
  command -v helm >/dev/null 2>&1 || fail "helm is required for the upgrade test"
  command -v jq >/dev/null 2>&1 || fail "jq is required for the upgrade test"
  command -v kind >/dev/null 2>&1 || fail "kind is required for the upgrade test"
  command -v kubectl >/dev/null 2>&1 || fail "kubectl is required for the upgrade test"

  local kind_cluster=""
  kube_context=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --kind-cluster)
        [[ $# -ge 2 ]] || fail "--kind-cluster requires a value"
        kind_cluster=$2
        shift 2
        ;;
      --kube-context|--context)
        [[ $# -ge 2 ]] || fail "$1 requires a value"
        kube_context=$2
        shift 2
        ;;
      -h|--help|help)
        usage
        exit 0
        ;;
      *)
        fail "unknown test-upgrade argument: $1"
        ;;
    esac
  done

  [[ -n "$kind_cluster" ]] || fail "test-upgrade requires --kind-cluster"
  [[ -n "$kube_context" ]] || fail "test-upgrade requires --kube-context"
  [[ "$kube_context" == "kind-${kind_cluster}" ]] || \
    fail "context ${kube_context} does not match kind cluster ${kind_cluster}"
  [[ "$(kubectl config get-contexts "$kube_context" -o name 2>/dev/null || true)" == "$kube_context" ]] || \
    fail "Kubernetes context ${kube_context} does not exist in the active kubeconfig"
  [[ "$(kind get clusters | grep -Fx "$kind_cluster" || true)" == "$kind_cluster" ]] || \
    fail "kind cluster ${kind_cluster} is not running"

  local kind_server
  context_server=$(kubectl --context "$kube_context" config view --minify -o jsonpath='{.clusters[0].cluster.server}')
  kind_server=$(kind get kubeconfig --name "$kind_cluster" | awk '$1 == "server:" { print $2; exit }')
  [[ -n "$kind_server" && "$context_server" == "$kind_server" ]] || \
    fail "context ${kube_context} does not target kind cluster ${kind_cluster}"
  kubectl --context "$kube_context" get nodes >/dev/null

  local helm_driver=${HELM_DRIVER:-secret}
  case "$helm_driver" in
    secret|configmap) ;;
    *) fail "test-upgrade requires HELM_DRIVER=secret or HELM_DRIVER=configmap" ;;
  esac

  local existing_crds helm_storage_objects namespace_names unexpected_namespaces unexpected_pods
  existing_crds=$(kubectl --context "$kube_context" get customresourcedefinitions -o name)
  [[ -z "$existing_crds" ]] || fail "dedicated kind cluster ${kind_cluster} already contains CRDs"
  helm_storage_objects=$(kubectl --context "$kube_context" get secrets,configmaps \
    --all-namespaces \
    --selector owner=helm \
    -o name)
  [[ -z "$helm_storage_objects" ]] || \
    fail "dedicated kind cluster ${kind_cluster} already contains Helm storage objects"
  namespace_names=$(kubectl --context "$kube_context" get namespaces -o name)
  unexpected_namespaces=$(printf '%s\n' "$namespace_names" | \
    grep -Ev '^namespace/(default|kube-node-lease|kube-public|kube-system|local-path-storage)$' || true)
  [[ -z "$unexpected_namespaces" ]] || \
    fail "dedicated kind cluster ${kind_cluster} contains non-system namespaces: ${unexpected_namespaces}"
  unexpected_pods=$(kubectl --context "$kube_context" get pods --all-namespaces --no-headers | \
    awk '$1 != "kube-system" && $1 != "local-path-storage" { print }')
  [[ -z "$unexpected_pods" ]] || fail "dedicated kind cluster ${kind_cluster} contains non-system pods"

  namespace=orka-helm-crd-test
  legacy_release=orka-legacy-crd-test
  fresh_release=orka-fresh-crd-test
  replacement_release=orka-replacement-crd-test
  test_release_selector="app.kubernetes.io/instance in (${legacy_release},${fresh_release},${replacement_release})"
  local stale_rbac
  stale_rbac=$(kubectl --context "$kube_context" get clusterroles,clusterrolebindings \
    --selector "$test_release_selector" \
    -o name)
  [[ -z "$stale_rbac" ]] || fail "dedicated kind cluster ${kind_cluster} contains stale test RBAC"
  local existing_namespace_uid
  if ! existing_namespace_uid=$(kubectl --context "$kube_context" get namespace "$namespace" \
    --ignore-not-found \
    -o jsonpath='{.metadata.uid}'); then
    fail "could not verify whether test namespace ${namespace} exists in ${kube_context}"
  fi
  [[ -z "$existing_namespace_uid" ]] || \
    fail "test namespace ${namespace} already exists in ${kube_context}; refusing destructive cleanup"
  assert_orka_crds_absent "$kube_context" "test preflight"

  temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/orka-helm-upgrade-test.XXXXXX")
  local target_package_dir="$temp_dir/target-package"
  mkdir -p "$target_package_dir"
  helm package "$CHART_DIR" --destination "$target_package_dir" >/dev/null
  local -a target_packages=("$target_package_dir"/orka-*.tgz)
  [[ ${#target_packages[@]} -eq 1 && -f "${target_packages[0]}" ]] || \
    fail "expected exactly one packaged target chart for the upgrade test"
  local target_chart=${target_packages[0]}

  local extra_chart="$temp_dir/extra-chart"
  local extra_package_dir="$temp_dir/extra-package"
  mkdir -p "$extra_chart" "$extra_package_dir"
  cp -R "$CHART_DIR/." "$extra_chart/"
  awk '
    !changed && /^version:[[:space:]]/ {
      print "version: 0.1.0-extra-resource-test"
      changed = 1
      next
    }
    { print }
  ' "$extra_chart/Chart.yaml" >"$temp_dir/extra-Chart.yaml"
  mv "$temp_dir/extra-Chart.yaml" "$extra_chart/Chart.yaml"
  printf '%s\n' \
    '---' \
    '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"must-be-rejected"}}' \
    >"$extra_chart/crds/unexpected.json"
  helm package "$extra_chart" --destination "$extra_package_dir" >/dev/null
  local -a extra_packages=("$extra_package_dir"/orka-*.tgz)
  [[ ${#extra_packages[@]} -eq 1 && -f "${extra_packages[0]}" ]] || \
    fail "expected exactly one extra-resource chart package"
  local extra_target_chart=${extra_packages[0]}

  local invalid_chart="$temp_dir/invalid-chart"
  local invalid_package_dir="$temp_dir/invalid-package"
  mkdir -p "$invalid_chart" "$invalid_package_dir"
  cp -R "$CHART_DIR/." "$invalid_chart/"
  awk '
    !changed && /^version:[[:space:]]/ {
      print "version: 0.1.0-invalid-test"
      changed = 1
      next
    }
    { print }
  ' "$invalid_chart/Chart.yaml" >"$temp_dir/invalid-Chart.yaml"
  mv "$temp_dir/invalid-Chart.yaml" "$invalid_chart/Chart.yaml"
  {
    printf '%s\n' '---'
    kubectl --context "$kube_context" create --dry-run=client --validate=false \
      -f "$invalid_chart/crds/core.orka.ai_agentruntimes.yaml" \
      -o json \
      | jq '.spec.versions[0].schema.openAPIV3Schema.description = "server preflight must not persist"'
  } >"$temp_dir/invalid-agentruntimes.json"
  mv "$temp_dir/invalid-agentruntimes.json" "$invalid_chart/crds/core.orka.ai_agentruntimes.yaml"
  {
    printf '%s\n' '---'
    kubectl --context "$kube_context" create --dry-run=client --validate=false \
      -f "$invalid_chart/crds/core.orka.ai_tools.yaml" \
      -o json \
      | jq '.spec.versions += [(.spec.versions[0] | .name = "v1beta1")]'
  } >"$temp_dir/invalid-tools.json"
  mv "$temp_dir/invalid-tools.json" "$invalid_chart/crds/core.orka.ai_tools.yaml"
  helm package "$invalid_chart" --destination "$invalid_package_dir" >/dev/null
  local -a invalid_packages=("$invalid_package_dir"/orka-*.tgz)
  [[ ${#invalid_packages[@]} -eq 1 && -f "${invalid_packages[0]}" ]] || \
    fail "expected exactly one invalid packaged chart for the server-preflight test"
  local invalid_target_chart=${invalid_packages[0]}

  local partial_chart="$temp_dir/partial-chart"
  local partial_package_dir="$temp_dir/partial-package"
  mkdir -p "$partial_chart" "$partial_package_dir"
  cp -R "$CHART_DIR/." "$partial_chart/"
  awk '
    !changed && /^version:[[:space:]]/ {
      print "version: 0.2.0-replacement-test"
      changed = 1
      next
    }
    { print }
  ' "$partial_chart/Chart.yaml" >"$temp_dir/partial-Chart.yaml"
  mv "$temp_dir/partial-Chart.yaml" "$partial_chart/Chart.yaml"
  local partial_crd_file partial_crd_basename
  for partial_crd_file in \
    "$partial_chart/crds/core.orka.ai_agentruntimes.yaml" \
    "$partial_chart/crds/core.orka.ai_agents.yaml" \
    "$partial_chart/crds/core.orka.ai_providers.yaml"; do
    partial_crd_basename=$(basename "$partial_crd_file")
    {
      printf '%s\n' '---'
      kubectl --context "$kube_context" create --dry-run=client --validate=false -f "$partial_crd_file" -o json \
        | jq --arg description "partial fixture for ${partial_crd_basename}" \
          '.spec.versions[0].schema.openAPIV3Schema.description = $description'
    } >"$temp_dir/${partial_crd_basename}.json"
    mv "$temp_dir/${partial_crd_basename}.json" "$partial_crd_file"
  done
  helm package "$partial_chart" --destination "$partial_package_dir" >/dev/null
  local -a partial_packages=("$partial_package_dir"/orka-*.tgz)
  [[ ${#partial_packages[@]} -eq 1 && -f "${partial_packages[0]}" ]] || \
    fail "expected exactly one partial-fixture chart package"
  local partial_target_chart=${partial_packages[0]}

  local legacy_chart="$temp_dir/legacy-chart"
  mkdir -p "$legacy_chart"
  cp -R "$CHART_DIR/." "$legacy_chart/"
  rm -rf "$legacy_chart/crds"
  awk '
    !changed && /^version:[[:space:]]/ {
      print "version: 0.0.0-legacy"
      changed = 1
      next
    }
    { print }
  ' "$legacy_chart/Chart.yaml" >"$temp_dir/Chart.yaml"
  mv "$temp_dir/Chart.yaml" "$legacy_chart/Chart.yaml"

  helm show crds "$legacy_chart" >"$temp_dir/legacy-crds.yaml"
  [[ ! -s "$temp_dir/legacy-crds.yaml" ]] || fail "legacy test chart unexpectedly contains CRDs"

  test_namespace_uid=""
  # shellcheck disable=SC2329 # Invoked by the EXIT trap below.
  cleanup_upgrade_test() {
    local exit_code=$?
    local cleanup_failed=0
    local release remaining_crds remaining_helm_storage remaining_namespace_uid remaining_rbac
    local owns_namespace=false
    trap - EXIT
    set +e

    if [[ -n "$test_namespace_uid" ]]; then
      if verify_test_target_ownership \
        "$kube_context" "$context_server" "$namespace" "$test_namespace_uid"; then
        owns_namespace=true
      else
        echo "helm-chart: cleanup lost ownership of the dedicated test target; skipping destructive cleanup" >&2
        cleanup_failed=1
      fi
    fi

    if [[ "$owns_namespace" == true ]]; then
      for release in "$legacy_release" "$fresh_release" "$replacement_release"; do
        if helm status "$release" --namespace "$namespace" --kube-context "$kube_context" >/dev/null 2>&1; then
          verify_test_target_ownership \
            "$kube_context" "$context_server" "$namespace" "$test_namespace_uid" && \
            helm uninstall "$release" --namespace "$namespace" --kube-context "$kube_context" >/dev/null || cleanup_failed=1
        fi
      done
      remove_test_crds \
        "$kube_context" "$context_server" "$namespace" "$test_namespace_uid" || cleanup_failed=1
      verify_test_target_ownership \
        "$kube_context" "$context_server" "$namespace" "$test_namespace_uid" && \
        kubectl --context "$kube_context" delete namespace "$namespace" \
          --wait=true \
          --timeout=90s >/dev/null || cleanup_failed=1
    fi

    if ! remaining_namespace_uid=$(kubectl --context "$kube_context" get namespace "$namespace" \
      --ignore-not-found \
      -o jsonpath='{.metadata.uid}'); then
      echo "helm-chart: cleanup could not verify namespace removal in ${kube_context}" >&2
      cleanup_failed=1
    elif [[ -n "$remaining_namespace_uid" ]]; then
      echo "helm-chart: cleanup left namespace ${namespace} in ${kube_context}" >&2
      cleanup_failed=1
    fi
    if ! remaining_crds=$(kubectl --context "$kube_context" get customresourcedefinitions -o name); then
      echo "helm-chart: cleanup could not verify CRD removal in ${kube_context}" >&2
      cleanup_failed=1
    elif [[ -n "$remaining_crds" ]]; then
      echo "helm-chart: cleanup left CRDs in ${kube_context}:" >&2
      printf '%s\n' "$remaining_crds" >&2
      cleanup_failed=1
    fi
    if ! remaining_helm_storage=$(kubectl --context "$kube_context" get secrets,configmaps \
      --all-namespaces \
      --selector owner=helm \
      -o name); then
      echo "helm-chart: cleanup could not verify Helm storage removal in ${kube_context}" >&2
      cleanup_failed=1
    elif [[ -n "$remaining_helm_storage" ]]; then
      echo "helm-chart: cleanup left Helm storage objects in ${kube_context}:" >&2
      printf '%s\n' "$remaining_helm_storage" >&2
      cleanup_failed=1
    fi
    if ! remaining_rbac=$(kubectl --context "$kube_context" get clusterroles,clusterrolebindings \
      --selector "$test_release_selector" \
      -o name); then
      echo "helm-chart: cleanup could not verify test RBAC removal in ${kube_context}" >&2
      cleanup_failed=1
    elif [[ -n "$remaining_rbac" ]]; then
      echo "helm-chart: cleanup left cluster-scoped test RBAC in ${kube_context}:" >&2
      printf '%s\n' "$remaining_rbac" >&2
      cleanup_failed=1
    fi
    rm -rf "$temp_dir" || cleanup_failed=1

    if [[ $cleanup_failed -ne 0 ]]; then
      echo "helm-chart: upgrade test cleanup was incomplete" >&2
      [[ $exit_code -ne 0 ]] || exit_code=1
    fi
    exit "$exit_code"
  }
  trap cleanup_upgrade_test EXIT

  test_namespace_uid=$(kubectl --context "$kube_context" create namespace "$namespace" \
    -o jsonpath='{.metadata.uid}')
  [[ -n "$test_namespace_uid" ]] || fail "created test namespace ${namespace} has no UID"

  ensure_test_namespace_for_cleanup() {
    local current_uid
    current_uid=$(kubectl --context "$kube_context" get namespace "$namespace" \
      --ignore-not-found \
      -o jsonpath='{.metadata.uid}') || return 1
    if [[ -z "$current_uid" ]]; then
      current_uid=$(kubectl --context "$kube_context" create namespace "$namespace" \
        -o jsonpath='{.metadata.uid}') || return 1
    fi
    [[ -n "$current_uid" ]] || return 1
    test_namespace_uid=$current_uid
  }

  local -a release_args=(
    --namespace "$namespace"
    --kube-context "$kube_context"
    --set controller.replicas=0
    --set-string workers.harnessWrapper.image.repository=invalid.local/orka-helm-test
    --set-string workers.harnessWrapper.image.tag=never
    --set workers.harnessWrapper.image.pullPolicy=Never
  )

  echo "[1/15] Verifying a fresh install still creates all chart CRDs."
  helm install "$fresh_release" "$target_chart" \
    "${release_args[@]}" >/dev/null
  wait_for_all_orka_crds "$kube_context" "after a fresh install"

  echo "[2/15] Verifying structural validation rejects an additional non-CRD document."
  if upgrade_crds \
    --chart "$extra_target_chart" \
    --kube-context "$kube_context" \
    --release "$fresh_release" \
    --namespace "$namespace" \
    --yes >"$temp_dir/extra-resource.out" 2>"$temp_dir/extra-resource.err"; then
    fail "extra-resource chart unexpectedly passed exact CRD bundle validation"
  fi
  if ! grep -Fq 'does not contain exactly the nine Orka CRDs' "$temp_dir/extra-resource.err"; then
    cat "$temp_dir/extra-resource.err" >&2
    fail "extra-resource chart failed for a reason other than exact bundle validation"
  fi
  [[ -z "$(kubectl --context "$kube_context" get secret must-be-rejected \
    --namespace default \
    --ignore-not-found \
    -o name)" ]] || fail "unexpected non-CRD resource was applied"

  if env HELM_KUBEAPISERVER=https://127.0.0.1:1 \
    "$CRD_UPGRADE_HELPER" \
      --chart "$target_chart" \
      --kube-context "$kube_context" \
      --release "$fresh_release" \
      --namespace "$namespace" \
      --yes >"$temp_dir/helm-override.out" 2>"$temp_dir/helm-override.err"; then
    fail "Helm-specific Kubernetes endpoint override unexpectedly passed validation"
  fi
  grep -Fq 'unset HELM_KUBEAPISERVER' "$temp_dir/helm-override.err" || \
    fail "Helm-specific endpoint override was not rejected by the expected guard"

  if upgrade_crds \
    --chart "$target_chart" \
    --chart "$extra_target_chart" \
    --kube-context "$kube_context" \
    --release "$fresh_release" \
    --namespace "$namespace" \
    --yes >"$temp_dir/duplicate-chart.out" 2>"$temp_dir/duplicate-chart.err"; then
    fail "duplicate --chart options unexpectedly passed validation"
  fi
  grep -Fq -- '--chart may be specified only once' "$temp_dir/duplicate-chart.err" || \
    fail "duplicate --chart options were not rejected by the expected guard"

  echo "[3/15] Verifying server preflight rejects one invalid CRD before changing earlier CRDs."
  local baseline_agentruntime_description actual_agentruntime_description
  baseline_agentruntime_description=$(kubectl --context "$kube_context" get \
    customresourcedefinition/agentruntimes.core.orka.ai \
    -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}')
  if upgrade_crds \
    --chart "$invalid_target_chart" \
    --kube-context "$kube_context" \
    --release "$fresh_release" \
    --namespace "$namespace" \
    --yes >"$temp_dir/invalid-migration.out" 2>"$temp_dir/invalid-migration.err"; then
    fail "server-invalid CRD bundle unexpectedly passed migration preflight"
  fi
  if ! grep -Fq 'server dry-run rejected target CRD tools.core.orka.ai' "$temp_dir/invalid-migration.err"; then
    cat "$temp_dir/invalid-migration.err" >&2
    fail "invalid migration failed for a reason other than the expected server preflight rejection"
  fi
  actual_agentruntime_description=$(kubectl --context "$kube_context" get \
    customresourcedefinition/agentruntimes.core.orka.ai \
    -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}')
  [[ "$actual_agentruntime_description" == "$baseline_agentruntime_description" ]] || \
    fail "server preflight failure partially changed an earlier CRD"

  echo "[4/15] Verifying partial existing-CRD changes are retained with recovery artifacts and can be reconciled."
  local existing_recovery_parent="$temp_dir/helper-recovery-existing"
  local existing_recovery_dir partial_description untouched_marker
  mkdir -p "$existing_recovery_parent"
  if env \
    TMPDIR="$existing_recovery_parent" \
    ORKA_HELM_CRD_UPGRADE_TESTING=1 \
    ORKA_HELM_CRD_UPGRADE_TEST_FAIL_AFTER=3 \
    "$CRD_UPGRADE_HELPER" \
      --chart "$partial_target_chart" \
      --kube-context "$kube_context" \
      --release "$fresh_release" \
      --namespace "$namespace" \
      --yes \
      >"$temp_dir/partial-existing.out" 2>"$temp_dir/partial-existing.err"; then
    fail "injected existing-CRD migration failure unexpectedly succeeded"
  fi
  grep -Fq 'injected test failure after 3 CRD mutations' "$temp_dir/partial-existing.err" || \
    fail "existing-CRD recovery test did not reach the injected mutation failure"
  grep -Fq 'partial CRD migration was retained' "$temp_dir/partial-existing.err" || \
    fail "existing-CRD partial migration was not retained for reconciliation"
  existing_recovery_dir=$(sed -n 's/^orka-crd-upgrade: recovery artifacts preserved at //p' \
    "$temp_dir/partial-existing.err" | tail -1)
  [[ -n "$existing_recovery_dir" && -d "$existing_recovery_dir" ]] || \
    fail "existing-CRD partial migration did not preserve a recovery directory"
  case "$existing_recovery_dir" in
    "$existing_recovery_parent"/*) ;;
    *) fail "existing-CRD recovery directory escaped the test workspace: ${existing_recovery_dir}" ;;
  esac
  if find "$existing_recovery_dir" -maxdepth 1 -type f -name 'release*' -print | grep -q .; then
    fail "existing-CRD recovery directory retained decoded Helm release data"
  fi
  local partial_name partial_file partial_marker partial_migration_id=""
  for partial_file in \
    core.orka.ai_agentruntimes.yaml \
    core.orka.ai_agents.yaml \
    core.orka.ai_providers.yaml; do
    partial_name=${partial_file#core.orka.ai_}
    partial_name=${partial_name%.yaml}.core.orka.ai
    partial_description=$(kubectl --context "$kube_context" get \
      "customresourcedefinition/${partial_name}" \
      -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}')
    [[ "$partial_description" == "partial fixture for ${partial_file}" ]] || \
      fail "partial existing-CRD migration did not retain ${partial_name}"
    partial_marker=$(kubectl --context "$kube_context" get \
      "customresourcedefinition/${partial_name}" \
      -o jsonpath='{.metadata.annotations.core\.orka\.ai/crd-migration-id}')
    [[ -n "$partial_marker" ]] || fail "partial migration did not mark ${partial_name}"
    if [[ -z "$partial_migration_id" ]]; then
      partial_migration_id=$partial_marker
    else
      [[ "$partial_marker" == "$partial_migration_id" ]] || \
        fail "partial migration used inconsistent recovery markers"
    fi
  done
  untouched_marker=$(kubectl --context "$kube_context" get \
    customresourcedefinition/repositorymonitors.core.orka.ai \
    -o jsonpath='{.metadata.annotations.core\.orka\.ai/crd-migration-id}')
  [[ -z "$untouched_marker" ]] || fail "failure injection changed a CRD after the configured mutation limit"

  upgrade_crds \
    --chart "$target_chart" \
    --kube-context "$kube_context" \
    --release "$fresh_release" \
    --namespace "$namespace" \
    --yes >/dev/null
  wait_for_all_orka_crds "$kube_context" "after reconciling a partial existing-CRD migration"
  local reconciled_migration_id
  reconciled_migration_id=$(kubectl --context "$kube_context" get \
    customresourcedefinition/agentruntimes.core.orka.ai \
    -o jsonpath='{.metadata.annotations.core\.orka\.ai/crd-migration-id}')
  [[ -n "$reconciled_migration_id" && "$reconciled_migration_id" != "$partial_migration_id" ]] || \
    fail "successful reconciliation did not replace the partial migration marker"

  echo "[5/15] Verifying exact migration updates validation and removes a stale schema field."
  local target_task_type_enum legacy_task_type_enum actual_task_type_enum actual_legacy_only managed_fields
  target_task_type_enum=$(kubectl --context "$kube_context" create --dry-run=client --validate=false \
    -f "$CHART_CRD_DIR/core.orka.ai_tasks.yaml" \
    -o json | jq -c '.spec.versions[] | select(.name == "v1alpha1") | .schema.openAPIV3Schema.properties.spec.properties.type.enum')
  legacy_task_type_enum='["legacy-only"]'
  cp "$LEGACY_TASK_CRD_FIXTURE" "$temp_dir/legacy-task-crd.yaml"

  kubectl --context "$kube_context" apply \
    --server-side \
    --force-conflicts \
    --field-manager=legacy-orka-crd-test \
    -f "$temp_dir/legacy-task-crd.yaml" >/dev/null
  actual_task_type_enum=$(kubectl --context "$kube_context" get \
    customresourcedefinition/tasks.core.orka.ai \
    -o json | jq -c '.spec.versions[] | select(.name == "v1alpha1") | .schema.openAPIV3Schema.properties.spec.properties.type.enum')
  [[ "$actual_task_type_enum" == "$legacy_task_type_enum" ]] || \
    fail "failed to install the legacy Task type validation fixture"
  actual_legacy_only=$(kubectl --context "$kube_context" get \
    customresourcedefinition/tasks.core.orka.ai \
    -o jsonpath='{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.legacyOnly.type}')
  [[ "$actual_legacy_only" == "string" ]] || fail "failed to install the stale Task schema field fixture"

  if kubectl --context "$kube_context" apply \
    --server-side \
    --field-manager=orka-helm-crd-upgrade-probe \
    -f "$CHART_CRD_DIR/core.orka.ai_tasks.yaml" \
    >"$temp_dir/pre-migration-apply.out" 2>"$temp_dir/pre-migration-apply.err"; then
    fail "target Task CRD apply unexpectedly succeeded without taking legacy field ownership"
  fi
  grep -Fq 'legacy-orka-crd-test' "$temp_dir/pre-migration-apply.err" || \
    fail "target Task CRD apply failed for a reason other than the legacy ownership conflict"

  local legacy_runtime_name=legacy-http-runtime
  kubectl --context "$kube_context" apply -f - >/dev/null <<EOF_LEGACY_RUNTIME
apiVersion: v1
kind: Service
metadata:
  name: ${legacy_runtime_name}
  namespace: ${namespace}
spec:
  selector:
    app: ${legacy_runtime_name}
  ports:
    - port: 8080
      targetPort: 8080
---
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata:
  name: ${legacy_runtime_name}
  namespace: ${namespace}
spec:
  contractVersion: orka.harness.v1
  deployment:
    mode: external-endpoint
    endpoint: http://${legacy_runtime_name}.${namespace}.svc.cluster.local:8080
  clientAuth:
    bearerTokenSecretRef:
      name: ${legacy_runtime_name}-token
      key: token
EOF_LEGACY_RUNTIME
  local agentruntime_version_index agentruntime_default_path
  agentruntime_version_index=$(kubectl --context "$kube_context" get \
    customresourcedefinition/agentruntimes.core.orka.ai \
    -o json | jq -r '.spec.versions | map(.name) | index("v1alpha1") // empty')
  [[ "$agentruntime_version_index" =~ ^[0-9]+$ ]] || \
    fail "could not locate AgentRuntime v1alpha1 schema for legacy default fixture"
  agentruntime_default_path="/spec/versions/${agentruntime_version_index}/schema/openAPIV3Schema/properties/spec/properties/deployment/properties/transportSecurity/default"
  kubectl --context "$kube_context" patch \
    customresourcedefinition/agentruntimes.core.orka.ai \
    --type=json \
    -p "[{\"op\":\"add\",\"path\":\"${agentruntime_default_path}\",\"value\":\"tls\"}]" \
    >/dev/null
  [[ "$(kubectl --context "$kube_context" get \
    agentruntime.core.orka.ai "$legacy_runtime_name" \
    --namespace "$namespace" \
    -o jsonpath='{.spec.deployment.transportSecurity}')" == "tls" ]] || \
    fail "legacy AgentRuntime omission was not masked by the simulated read-time default"

  upgrade_crds \
    --chart "$target_chart" \
    --kube-context "$kube_context" \
    --release "$fresh_release" \
    --namespace "$namespace" \
    --yes >/dev/null
  wait_for_all_orka_crds "$kube_context" "after updating the legacy Task type validation"
  local legacy_runtime_transport legacy_runtime_marker
  legacy_runtime_transport=$(kubectl --context "$kube_context" get \
    agentruntime.core.orka.ai "$legacy_runtime_name" \
    --namespace "$namespace" \
    -o jsonpath='{.spec.deployment.transportSecurity}')
  legacy_runtime_marker=$(kubectl --context "$kube_context" get \
    agentruntime.core.orka.ai "$legacy_runtime_name" \
    --namespace "$namespace" \
    -o json | jq -r '.metadata.annotations["orka.ai/transport-security-migration"] // ""')
  if [[ "$legacy_runtime_transport" != "insecure-cluster-local-http" ]]; then
    [[ -z "$legacy_runtime_transport" && "$legacy_runtime_marker" == "legacy-v1" ]] || \
      fail "legacy AgentRuntime was neither marked nor backfilled during CRD migration"
  fi
  [[ "$(kubectl --context "$kube_context" get \
    customresourcedefinition/agentruntimes.core.orka.ai \
    -o json | jq -r '.metadata.annotations["core.orka.ai/agent-runtime-transport-migration"] // ""')" == "legacy-v1-complete" ]] || \
    fail "AgentRuntime CRD transport migration was not marked complete"
  kubectl --context "$kube_context" delete \
    agentruntime.core.orka.ai "$legacy_runtime_name" \
    service "$legacy_runtime_name" \
    --namespace "$namespace" \
    --ignore-not-found >/dev/null

  local prepolicy_runtime_name=prepolicy-http-runtime
  kubectl --context "$kube_context" apply -f - >/dev/null <<EOF_PREPOLICY_RUNTIME
apiVersion: v1
kind: Service
metadata:
  name: ${prepolicy_runtime_name}
  namespace: ${namespace}
spec:
  selector:
    app: ${prepolicy_runtime_name}
  ports:
    - port: 8080
      targetPort: 8080
---
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata:
  name: ${prepolicy_runtime_name}
  namespace: ${namespace}
spec:
  contractVersion: orka.harness.v1
  deployment:
    mode: external-endpoint
    endpoint: http://${prepolicy_runtime_name}.${namespace}.svc.cluster.local:8080
  clientAuth:
    bearerTokenSecretRef:
      name: ${prepolicy_runtime_name}-token
      key: token
EOF_PREPOLICY_RUNTIME
  local agentruntime_transport_property_path agentruntime_transport_validations_path
  agentruntime_transport_property_path="/spec/versions/${agentruntime_version_index}/schema/openAPIV3Schema/properties/spec/properties/deployment/properties/transportSecurity"
  agentruntime_transport_validations_path="/spec/versions/${agentruntime_version_index}/schema/openAPIV3Schema/properties/spec/properties/deployment/x-kubernetes-validations"
  kubectl --context "$kube_context" annotate \
    customresourcedefinition/agentruntimes.core.orka.ai \
    core.orka.ai/agent-runtime-transport-migration- \
    --overwrite >/dev/null
  kubectl --context "$kube_context" patch \
    customresourcedefinition/agentruntimes.core.orka.ai \
    --type=json \
    -p "[{\"op\":\"remove\",\"path\":\"${agentruntime_transport_validations_path}\"},{\"op\":\"remove\",\"path\":\"${agentruntime_transport_property_path}\"}]" \
    >/dev/null
  upgrade_crds \
    --chart "$target_chart" \
    --kube-context "$kube_context" \
    --release "$fresh_release" \
    --namespace "$namespace" \
    --yes >/dev/null
  local prepolicy_transport prepolicy_marker
  prepolicy_transport=$(kubectl --context "$kube_context" get \
    agentruntime.core.orka.ai "$prepolicy_runtime_name" \
    --namespace "$namespace" \
    -o jsonpath='{.spec.deployment.transportSecurity}')
  prepolicy_marker=$(kubectl --context "$kube_context" get \
    agentruntime.core.orka.ai "$prepolicy_runtime_name" \
    --namespace "$namespace" \
    -o json | jq -r '.metadata.annotations["orka.ai/transport-security-migration"] // ""')
  if [[ "$prepolicy_transport" != "insecure-cluster-local-http" ]]; then
    [[ -z "$prepolicy_transport" && "$prepolicy_marker" == "legacy-v1" ]] || \
      fail "pre-policy AgentRuntime was neither marked nor backfilled during skipped-version migration"
  fi
  kubectl --context "$kube_context" delete \
    agentruntime.core.orka.ai "$prepolicy_runtime_name" \
    service "$prepolicy_runtime_name" \
    --namespace "$namespace" \
    --ignore-not-found >/dev/null

  local post_migration_runtime_name=post-migration-http-runtime
  kubectl --context "$kube_context" apply -f - >/dev/null <<EOF_POST_MIGRATION_RUNTIME
apiVersion: v1
kind: Service
metadata:
  name: ${post_migration_runtime_name}
  namespace: ${namespace}
spec:
  selector:
    app: ${post_migration_runtime_name}
  ports:
    - port: 8080
      targetPort: 8080
---
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata:
  name: ${post_migration_runtime_name}
  namespace: ${namespace}
spec:
  contractVersion: orka.harness.v1
  deployment:
    mode: external-endpoint
    endpoint: http://${post_migration_runtime_name}.${namespace}.svc.cluster.local:8080
  clientAuth:
    bearerTokenSecretRef:
      name: ${post_migration_runtime_name}-token
      key: token
EOF_POST_MIGRATION_RUNTIME
  upgrade_crds \
    --chart "$target_chart" \
    --kube-context "$kube_context" \
    --release "$fresh_release" \
    --namespace "$namespace" \
    --yes >/dev/null
  local post_migration_transport post_migration_marker
  post_migration_transport=$(kubectl --context "$kube_context" get \
    agentruntime.core.orka.ai "$post_migration_runtime_name" \
    --namespace "$namespace" \
    -o jsonpath='{.spec.deployment.transportSecurity}')
  post_migration_marker=$(kubectl --context "$kube_context" get \
    agentruntime.core.orka.ai "$post_migration_runtime_name" \
    --namespace "$namespace" \
    -o json | jq -r '.metadata.annotations["orka.ai/transport-security-migration"] // ""')
  [[ -z "$post_migration_transport" && -z "$post_migration_marker" ]] || \
    fail "later CRD migration reclassified a post-migration AgentRuntime omission"
  kubectl --context "$kube_context" delete \
    agentruntime.core.orka.ai "$post_migration_runtime_name" \
    service "$post_migration_runtime_name" \
    --namespace "$namespace" \
    --ignore-not-found >/dev/null
  actual_task_type_enum=$(kubectl --context "$kube_context" get \
    customresourcedefinition/tasks.core.orka.ai \
    -o json | jq -c '.spec.versions[] | select(.name == "v1alpha1") | .schema.openAPIV3Schema.properties.spec.properties.type.enum')
  [[ "$actual_task_type_enum" == "$target_task_type_enum" ]] || \
    fail "exact CRD migration did not restore the target Task type validation"
  actual_legacy_only=$(kubectl --context "$kube_context" get \
    customresourcedefinition/tasks.core.orka.ai \
    -o jsonpath='{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.legacyOnly.type}')
  [[ -z "$actual_legacy_only" ]] || fail "exact CRD migration retained a stale Task schema field"
  managed_fields=$(kubectl --context "$kube_context" get \
    customresourcedefinition/tasks.core.orka.ai \
    -o jsonpath='{range .metadata.managedFields[*]}{.manager}{"\n"}{end}')
  grep -Fxq "$CRD_FIELD_MANAGER" <<<"$managed_fields" || \
    fail "exact CRD migration did not record ${CRD_FIELD_MANAGER} field ownership"

  if kubectl --context "$kube_context" apply \
    --server-side \
    --field-manager=legacy-orka-crd-test \
    -f "$temp_dir/legacy-task-crd.yaml" \
    >"$temp_dir/post-migration-apply.out" 2>"$temp_dir/post-migration-apply.err"; then
    fail "legacy Task validation unexpectedly overwrote the migrated CRD without --force-conflicts"
  fi
  grep -Fq "$CRD_FIELD_MANAGER" "$temp_dir/post-migration-apply.err" || \
    fail "legacy Task validation was not blocked by ${CRD_FIELD_MANAGER} ownership"

  echo "[6/15] Verifying Helm uninstall retains chart CRDs."
  verify_test_target_ownership \
    "$kube_context" "$context_server" "$namespace" "$test_namespace_uid"
  helm uninstall "$fresh_release" --namespace "$namespace" --kube-context "$kube_context" >/dev/null
  wait_for_all_orka_crds "$kube_context" "after fresh-release uninstall"
  verify_test_target_ownership \
    "$kube_context" "$context_server" "$namespace" "$test_namespace_uid"
  kubectl --context "$kube_context" delete namespace "$namespace" \
    --wait=true \
    --timeout=90s >/dev/null
  test_namespace_uid=""

  echo "[7/15] Migrating retained CRDs before a replacement install with no namespace or live release."
  local missing_release_guard_ok=false
  if upgrade_crds \
    --chart "$partial_target_chart" \
    --kube-context "$kube_context" \
    --release "$replacement_release" \
    --namespace "$namespace" \
    --yes >"$temp_dir/missing-release-denied.out" 2>"$temp_dir/missing-release-denied.err"; then
    missing_release_guard_ok=false
  elif grep -Fq 'use --allow-missing-release only for retained CRDs' "$temp_dir/missing-release-denied.err"; then
    missing_release_guard_ok=true
  fi
  if [[ "$missing_release_guard_ok" != true ]]; then
    ensure_test_namespace_for_cleanup || true
    fail "missing-release migration was not rejected by the expected guard"
  fi

  if ! upgrade_crds \
    --chart "$partial_target_chart" \
    --kube-context "$kube_context" \
    --release "$replacement_release" \
    --namespace "$namespace" \
    --allow-missing-release \
    --yes >"$temp_dir/preinstall-migration.out" 2>"$temp_dir/preinstall-migration.err"; then
    ensure_test_namespace_for_cleanup || true
    cat "$temp_dir/preinstall-migration.err" >&2
    fail "pre-install CRD migration failed"
  fi

  if ! grep -Fq 'Run helm install with --skip-crds' "$temp_dir/preinstall-migration.out"; then
    ensure_test_namespace_for_cleanup || true
    fail "pre-install migration did not emit the replacement-install instruction"
  fi
  if ! wait_for_all_orka_crds "$kube_context" "before replacement install"; then
    ensure_test_namespace_for_cleanup || true
    fail "pre-install migration returned before all target CRDs were Established"
  fi
  local replacement_crd_file replacement_crd_name retained_replacement_description
  for replacement_crd_file in \
    core.orka.ai_agentruntimes.yaml \
    core.orka.ai_agents.yaml \
    core.orka.ai_providers.yaml; do
    replacement_crd_name=${replacement_crd_file#core.orka.ai_}
    replacement_crd_name=${replacement_crd_name%.yaml}.core.orka.ai
    if ! retained_replacement_description=$(kubectl --context "$kube_context" get \
      "customresourcedefinition/${replacement_crd_name}" \
      -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.description}'); then
      ensure_test_namespace_for_cleanup || true
      fail "could not verify replacement CRD ${replacement_crd_name} before install"
    fi
    if [[ "$retained_replacement_description" != "partial fixture for ${replacement_crd_file}" ]]; then
      ensure_test_namespace_for_cleanup || true
      fail "pre-install migration did not apply replacement schema ${replacement_crd_name}"
    fi
  done

  if ! helm install "$replacement_release" "$partial_target_chart" \
    --skip-crds \
    --create-namespace \
    "${release_args[@]}" >/dev/null; then
    ensure_test_namespace_for_cleanup || true
    fail "replacement Helm install failed after pre-install CRD migration"
  fi
  ensure_test_namespace_for_cleanup || fail "replacement install namespace has no UID"
  wait_for_all_orka_crds "$kube_context" "after replacement install"
  verify_test_target_ownership \
    "$kube_context" "$context_server" "$namespace" "$test_namespace_uid"
  helm uninstall "$replacement_release" --namespace "$namespace" --kube-context "$kube_context" >/dev/null
  wait_for_all_orka_crds "$kube_context" "after replacement-release uninstall"
  remove_test_crds \
    "$kube_context" "$context_server" "$namespace" "$test_namespace_uid"
  assert_orka_crds_absent "$kube_context" "replacement-install reset"

  if upgrade_crds \
    --chart "$target_chart" \
    --kube-context "$kube_context" \
    --release "$replacement_release" \
    --namespace "$namespace" \
    --allow-missing-release \
    --yes >"$temp_dir/empty-preinstall.out" 2>"$temp_dir/empty-preinstall.err"; then
    fail "missing-release mode unexpectedly created CRDs on an empty cluster"
  fi
  grep -Fq 'no Helm release or existing Orka CRDs were found' "$temp_dir/empty-preinstall.err" || \
    fail "empty-cluster missing-release mode was not rejected by the expected guard"
  assert_orka_crds_absent "$kube_context" "empty missing-release rejection"

  echo "[8/15] Installing a legacy release from a chart with no crds/ directory."
  helm install "$legacy_release" "$legacy_chart" "${release_args[@]}" >/dev/null
  assert_orka_crds_absent "$kube_context" "legacy install"

  echo "[9/15] Proving an ordinary Helm upgrade still does not install missing crds/ files."
  helm upgrade "$legacy_release" "$target_chart" "${release_args[@]}" >/dev/null
  assert_orka_crds_absent "$kube_context" "ordinary upgrade to the current chart"

  echo "[10/15] Returning the release to the legacy chart before the supported migration."
  helm upgrade "$legacy_release" "$legacy_chart" "${release_args[@]}" >/dev/null
  assert_orka_crds_absent "$kube_context" "legacy reset"

  echo "[11/15] Verifying a partial create retains CRDs and recovery artifacts instead of risking data loss."
  local recovery_parent="$temp_dir/helper-recovery"
  local recovery_dir partial_crds expected_partial_crds
  mkdir -p "$recovery_parent"
  if env \
    TMPDIR="$recovery_parent" \
    ORKA_HELM_CRD_UPGRADE_TESTING=1 \
    ORKA_HELM_CRD_UPGRADE_TEST_FAIL_AFTER=3 \
    "$CRD_UPGRADE_HELPER" \
      --chart "$target_chart" \
      --kube-context "$kube_context" \
      --release "$legacy_release" \
      --namespace "$namespace" \
      --yes \
      >"$temp_dir/rollback-missing.out" 2>"$temp_dir/rollback-missing.err"; then
    fail "injected missing-CRD migration failure unexpectedly succeeded"
  fi
  grep -Fq 'injected test failure after 3 CRD mutations' "$temp_dir/rollback-missing.err" || \
    fail "missing-CRD recovery test did not reach the injected mutation failure"
  grep -Fq 'partial CRD migration was retained' "$temp_dir/rollback-missing.err" || \
    fail "partial create did not report fail-closed retention"
  recovery_dir=$(sed -n 's/^orka-crd-upgrade: recovery artifacts preserved at //p' \
    "$temp_dir/rollback-missing.err" | tail -1)
  [[ -n "$recovery_dir" && -d "$recovery_dir" ]] || \
    fail "partial create did not preserve a recovery directory"
  case "$recovery_dir" in
    "$recovery_parent"/*) ;;
    *) fail "recovery directory escaped the test workspace: ${recovery_dir}" ;;
  esac
  [[ -s "$recovery_dir/actual/agentruntimes.core.orka.ai.json" ]] || \
    fail "recovery directory is missing the first created CRD response"
  partial_crds=$(kubectl --context "$kube_context" get customresourcedefinitions -o name | \
    awk -F/ '$2 ~ /[.]core[.]orka[.]ai$/ { print $2 }' | sort)
  expected_partial_crds=$(printf '%s\n' \
    agentruntimes.core.orka.ai \
    agents.core.orka.ai \
    providers.core.orka.ai | sort)
  [[ "$partial_crds" == "$expected_partial_crds" ]] || \
    fail "partial create did not retain exactly the CRDs created before failure"
  local created_partial_name created_partial_marker created_partial_migration_id=""
  for created_partial_name in \
    agentruntimes.core.orka.ai \
    agents.core.orka.ai \
    providers.core.orka.ai; do
    created_partial_marker=$(kubectl --context "$kube_context" get \
      "customresourcedefinition/${created_partial_name}" \
      -o jsonpath='{.metadata.annotations.core\.orka\.ai/crd-migration-id}')
    [[ -n "$created_partial_marker" ]] || fail "partial create did not mark ${created_partial_name}"
    if [[ -z "$created_partial_migration_id" ]]; then
      created_partial_migration_id=$created_partial_marker
    else
      [[ "$created_partial_marker" == "$created_partial_migration_id" ]] || \
        fail "partial create used inconsistent recovery markers"
    fi
  done
  [[ "$created_partial_migration_id" != "$partial_migration_id" && \
     "$created_partial_migration_id" != "$reconciled_migration_id" ]] || \
    fail "partial create reused a prior migration marker"

  echo "[12/15] Reconciling the retained partial create with the supported migration."
  upgrade_crds \
    --chart "$target_chart" \
    --kube-context "$kube_context" \
    --release "$legacy_release" \
    --namespace "$namespace" \
    --yes

  echo "[13/15] Proving all nine CRDs are Established before Helm upgrade."
  wait_for_all_orka_crds "$kube_context" "before the supported Helm upgrade"

  echo "[14/15] Upgrading only after the CRD migration succeeds."
  helm upgrade "$legacy_release" "$target_chart" "${release_args[@]}" >/dev/null
  wait_for_all_orka_crds "$kube_context" "after the supported Helm upgrade"

  echo "[15/15] Verifying the upgraded release also retains CRDs on uninstall."
  verify_test_target_ownership \
    "$kube_context" "$context_server" "$namespace" "$test_namespace_uid"
  helm uninstall "$legacy_release" --namespace "$namespace" --kube-context "$kube_context" >/dev/null
  wait_for_all_orka_crds "$kube_context" "after upgraded-release uninstall"

  echo "Helm CRD migration test passed: exact bundle validation rejected an extra resource, server preflight prevented partial mutation, retained CRDs were migrated before replacement install, partial existing and newly created CRDs were retained with recovery artifacts and reconciled by rerun, exact migration removed stale schema, all ${EXPECTED_CRD_COUNT} target CRDs became Established, and uninstall retained CRDs."
)

case "${1:-}" in
  sync)
    shift
    [[ $# -eq 0 ]] || fail "sync does not accept arguments"
    sync_crds
    ;;
  check)
    shift
    [[ $# -eq 0 ]] || fail "check does not accept arguments"
    check_chart
    ;;
  upgrade-crds)
    shift
    upgrade_crds "$@"
    ;;
  test-upgrade)
    shift
    test_upgrade "$@"
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
