#!/usr/bin/env bash
set -euo pipefail

LC_ALL=C
export LC_ALL

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
EXPECTED_CRD_COUNT=${#EXPECTED_CRD_NAMES[@]}
CRD_FIELD_MANAGER=orka-helm-crd-upgrade
CRD_MIGRATION_ANNOTATION=core.orka.ai/crd-migration-id

usage() {
  cat <<'USAGE'
Usage:
  upgrade-crds.sh --chart LOCAL_CHART_ARCHIVE --kube-context CONTEXT \
    --release RELEASE --namespace NAMESPACE [--allow-missing-release] [--yes]

The chart must be one local packaged .tgz file. Use that exact same file for the
subsequent Helm command. The helper rejects HELM_KUBE* endpoint or credential
overrides so Helm cannot target a different cluster than kubectl. It validates
all nine CRDs with server-side dry
runs before changing the cluster, exactly replaces each CRD spec, and verifies
the result. If mutation or verification fails, changed and newly created CRDs
are left in place: automatic schema rollback can invalidate custom resources or
alter API field ownership. Recovery artifacts and a unique migration marker are
preserved for manual reconciliation. Use --allow-missing-release only before a
replacement install when retained or independently managed Orka CRDs already
exist; then install the same chart archive with --skip-crds. Without --yes, type
the context name to confirm the cluster-scoped change.
USAGE
}

fail() {
  echo "orka-crd-upgrade: $*" >&2
  exit 1
}

sha256_file() {
  local file=$1
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{ print $1 }'
  else
    fail "sha256sum or shasum is required to identify the chart artifact"
  fi
}

decode_helm_release_record() {
  local storage_json=$1
  local output=$2
  local work_dir=$3

  jq -e '
    (.items | length) > 0 and
    ([.items[].kind] | unique | length) == 1 and
    all(.items[]; (.metadata.labels.version | tonumber) >= 1)
  ' "$storage_json" >/dev/null || fail "Helm release storage is missing or ambiguous"

  local latest_kind latest_data encoded payload
  latest_kind=$(jq -r '.items | max_by(.metadata.labels.version | tonumber) | .kind' "$storage_json")
  latest_data=$(jq -r '.items | max_by(.metadata.labels.version | tonumber) | .data.release // empty' "$storage_json")
  [[ -n "$latest_data" ]] || fail "Helm release storage has no encoded release payload"

  encoded="${work_dir}/release-encoded"
  payload="${work_dir}/release-payload"
  if [[ "$latest_kind" == "Secret" ]]; then
    printf '%s' "$latest_data" | base64 --decode >"$encoded" || fail "could not decode Helm Secret payload"
  elif [[ "$latest_kind" == "ConfigMap" ]]; then
    printf '%s' "$latest_data" >"$encoded"
  else
    fail "unsupported Helm release storage kind ${latest_kind}"
  fi
  base64 --decode <"$encoded" >"$payload" || fail "could not decode Helm release record"
  if gzip -t "$payload" >/dev/null 2>&1; then
    gzip -dc "$payload" >"$output" || fail "could not decompress Helm release record"
  else
    cp "$payload" "$output"
  fi
  jq -e 'type == "object" and (.chart.metadata.name | type) == "string"' "$output" >/dev/null || \
    fail "decoded Helm release record has no structured chart metadata"
}

expected_crd_resources() {
  local name
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    printf 'customresourcedefinition.apiextensions.k8s.io/%s\n' "$name"
  done
}

validate_crd_bundle() {
  local bundle=$1
  local description=$2
  local kube_context=$3

  [[ -s "$bundle" ]] || fail "${description} is empty"
  if grep -Fq '{{' "$bundle"; then
    fail "${description} contains Helm template syntax; CRDs must be static"
  fi

  local actual_resources expected_resources
  if ! actual_resources=$(kubectl --context "$kube_context" create --dry-run=client --validate=false -f "$bundle" -o name); then
    fail "${description} is not a structurally valid Kubernetes resource bundle"
  fi
  actual_resources=$(printf '%s\n' "$actual_resources" | sort)
  expected_resources=$(expected_crd_resources | sort)
  if [[ "$actual_resources" != "$expected_resources" ]]; then
    echo "Expected resources:" >&2
    printf '%s\n' "$expected_resources" >&2
    echo "Found resources:" >&2
    printf '%s\n' "$actual_resources" >&2
    fail "${description} does not contain exactly the nine Orka CRDs"
  fi
}

managed_state_equal() {
  local actual=$1
  local expected=$2
  jq -e --slurpfile expected "$expected" '
    .spec == $expected[0].spec and
    (.metadata.annotations // {}) == ($expected[0].metadata.annotations // {}) and
    (.metadata.labels // {}) == ($expected[0].metadata.labels // {})
  ' "$actual" >/dev/null
}

write_target_patch() {
  local live=$1
  local target=$2
  local output=$3

  jq -n \
    --slurpfile live "$live" \
    --slurpfile target "$target" '
      [
        {
          op: "test",
          path: "/metadata/uid",
          value: $live[0].metadata.uid
        },
        {
          op: "test",
          path: "/metadata/resourceVersion",
          value: $live[0].metadata.resourceVersion
        },
        {
          op: "replace",
          path: "/spec",
          value: $target[0].spec
        }
      ]
      +
      (if (($target[0].metadata.annotations // {}) | length) > 0 then
        [{
          op: "add",
          path: "/metadata/annotations",
          value: (($live[0].metadata.annotations // {}) + ($target[0].metadata.annotations // {}))
        }]
      else [] end)
      +
      (if (($target[0].metadata.labels // {}) | length) > 0 then
        [{
          op: "add",
          path: "/metadata/labels",
          value: (($live[0].metadata.labels // {}) + ($target[0].metadata.labels // {}))
        }]
      else [] end)
    ' >"$output"
}

main() {
  local chart=""
  local kube_context=""
  local release=""
  local namespace=""
  local helm_driver=${HELM_DRIVER:-secret}
  local assume_yes=false
  local allow_missing_release=false
  local chart_seen=false
  local kube_context_seen=false
  local release_seen=false
  local namespace_seen=false
  local test_fail_after=0
  if [[ "${ORKA_HELM_CRD_UPGRADE_TESTING:-}" == "1" ]]; then
    test_fail_after=${ORKA_HELM_CRD_UPGRADE_TEST_FAIL_AFTER:-0}
    [[ "$test_fail_after" =~ ^[0-9]+$ ]] || fail "test failure injection count must be a non-negative integer"
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --chart)
        [[ $# -ge 2 ]] || fail "--chart requires a value"
        [[ "$chart_seen" == false ]] || fail "--chart may be specified only once"
        chart_seen=true
        chart=$2
        shift 2
        ;;
      --kube-context|--context)
        [[ $# -ge 2 ]] || fail "$1 requires a value"
        [[ "$kube_context_seen" == false ]] || fail "--kube-context may be specified only once"
        kube_context_seen=true
        kube_context=$2
        shift 2
        ;;
      --release)
        [[ $# -ge 2 ]] || fail "--release requires a value"
        [[ "$release_seen" == false ]] || fail "--release may be specified only once"
        release_seen=true
        release=$2
        shift 2
        ;;
      --namespace)
        [[ $# -ge 2 ]] || fail "--namespace requires a value"
        [[ "$namespace_seen" == false ]] || fail "--namespace may be specified only once"
        namespace_seen=true
        namespace=$2
        shift 2
        ;;
      --allow-missing-release)
        [[ "$allow_missing_release" == false ]] || fail "--allow-missing-release may be specified only once"
        allow_missing_release=true
        shift
        ;;
      --yes)
        assume_yes=true
        shift
        ;;
      -h|--help|help)
        usage
        return 0
        ;;
      *)
        fail "unknown argument: $1"
        ;;
    esac
  done

  [[ -n "$chart" ]] || fail "--chart is required"
  [[ -n "$kube_context" ]] || fail "--kube-context is required; ambient contexts are not allowed"
  [[ -n "$release" ]] || fail "--release is required"
  [[ -n "$namespace" ]] || fail "--namespace is required"

  command -v base64 >/dev/null 2>&1 || fail "base64 is required"
  command -v gzip >/dev/null 2>&1 || fail "gzip is required"
  command -v helm >/dev/null 2>&1 || fail "helm is required"
  command -v jq >/dev/null 2>&1 || fail "jq is required"
  command -v kubectl >/dev/null 2>&1 || fail "kubectl is required"
  command -v tar >/dev/null 2>&1 || fail "tar is required"

  local helm_override helm_override_value
  for helm_override in \
    HELM_KUBEAPISERVER \
    HELM_KUBEASGROUPS \
    HELM_KUBEASUSER \
    HELM_KUBECAFILE \
    HELM_KUBECONFIG \
    HELM_KUBECONTEXT \
    HELM_KUBETLS_SERVER_NAME \
    HELM_KUBETOKEN; do
    helm_override_value=${!helm_override-}
    [[ -z "$helm_override_value" ]] || \
      fail "unset ${helm_override}; Helm-specific Kubernetes overrides can target a different cluster than kubectl"
  done
  case "${HELM_KUBEINSECURE_SKIP_TLS_VERIFY:-false}" in
    false|"") ;;
    *) fail "unset HELM_KUBEINSECURE_SKIP_TLS_VERIFY; Helm-specific Kubernetes overrides can target a different cluster than kubectl" ;;
  esac

  case "$helm_driver" in
    secret|configmap) ;;
    *) fail "HELM_DRIVER must be secret or configmap for structured release verification" ;;
  esac
  [[ -f "$chart" ]] || fail "--chart must be one local packaged chart archive; pull or package it first"

  local chart_parent chart_name_on_disk chart_source chart_digest
  chart_parent=$(cd "$(dirname "$chart")" && pwd -P)
  chart_name_on_disk=$(basename "$chart")
  chart_source="${chart_parent}/${chart_name_on_disk}"
  tar -tzf "$chart_source" >/dev/null || fail "${chart_source} is not a readable packaged chart archive"
  chart_digest=$(sha256_file "$chart_source")

  local selected_context
  selected_context=$(kubectl config get-contexts "$kube_context" -o name 2>/dev/null || true)
  [[ "$selected_context" == "$kube_context" ]] || fail "Kubernetes context ${kube_context} does not exist in the active kubeconfig"

  local release_exists=false
  local release_state=""

  temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/orka-helm-crd-upgrade.XXXXXX")
  mutation_started=false
  migration_complete=false
  migration_id="$(basename "$temp_dir")-${chart_digest:0:12}"
  chart="${temp_dir}/target-chart.tgz"
  cp "$chart_source" "$chart"
  [[ "$(sha256_file "$chart")" == "$chart_digest" ]] || \
    fail "target chart archive changed while it was being staged"


  cleanup() {
    local exit_code=$?
    local cleanup_failed=0
    local name actual
    trap - EXIT INT TERM
    set +e
    if [[ "$mutation_started" == true && "$migration_complete" != true ]]; then
      echo "orka-crd-upgrade: partial CRD migration was retained; automatic schema rollback can invalidate custom resources or alter field ownership" >&2
      for name in "${EXPECTED_CRD_NAMES[@]}"; do
        actual="${temp_dir}/actual/${name}.json"
        if [[ -s "$actual" ]]; then
          echo "orka-crd-upgrade: retained CRD ${name} with migration marker ${migration_id}" >&2
        fi
      done
      echo "orka-crd-upgrade: recovery artifacts preserved at ${temp_dir}" >&2
    else
      rm -rf "$temp_dir" || cleanup_failed=1
    fi
    if [[ $cleanup_failed -ne 0 && $exit_code -eq 0 ]]; then
      exit_code=1
    fi
    exit "$exit_code"
  }
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  local namespace_name
  if ! namespace_name=$(kubectl --context "$kube_context" get namespace "$namespace" \
    --ignore-not-found \
    -o name); then
    fail "could not verify namespace ${namespace} in context ${kube_context}"
  fi

  local secret_storage_file="${temp_dir}/release-secrets.json"
  local configmap_storage_file="${temp_dir}/release-configmaps.json"
  local secret_storage_count=0
  local configmap_storage_count=0
  if [[ -n "$namespace_name" ]]; then
    kubectl --context "$kube_context" get secrets \
      --namespace "$namespace" \
      --selector "owner=helm,name=${release}" \
      -o json >"$secret_storage_file" || \
      fail "could not read Helm Secret release storage for ${namespace}/${release}"
    kubectl --context "$kube_context" get configmaps \
      --namespace "$namespace" \
      --selector "owner=helm,name=${release}" \
      -o json >"$configmap_storage_file" || \
      fail "could not read Helm ConfigMap release storage for ${namespace}/${release}"
    secret_storage_count=$(jq -r '.items | length' "$secret_storage_file")
    configmap_storage_count=$(jq -r '.items | length' "$configmap_storage_file")
  else
    printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[]}' >"$secret_storage_file"
    printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[]}' >"$configmap_storage_file"
  fi

  if [[ $secret_storage_count -gt 0 && $configmap_storage_count -gt 0 ]]; then
    fail "Helm release ${namespace}/${release} exists in both Secret and ConfigMap storage; reconcile the duplicate release records before migrating CRDs"
  fi

  local discovered_driver=""
  local discovered_storage_file=""
  if [[ $secret_storage_count -gt 0 ]]; then
    discovered_driver=secret
    discovered_storage_file=$secret_storage_file
  elif [[ $configmap_storage_count -gt 0 ]]; then
    discovered_driver=configmap
    discovered_storage_file=$configmap_storage_file
  fi

  if [[ -n "$discovered_driver" ]]; then
    [[ "$helm_driver" == "$discovered_driver" ]] || \
      fail "HELM_DRIVER=${helm_driver} does not match the existing ${discovered_driver}-backed release ${namespace}/${release}; set HELM_DRIVER=${discovered_driver} before migrating or upgrading"
    release_exists=true
    release_state="existing"
    cp "$discovered_storage_file" "${temp_dir}/release-storage.json"
    decode_helm_release_record \
      "${temp_dir}/release-storage.json" \
      "${temp_dir}/release.json" \
      "$temp_dir"
    local release_chart release_status release_revision
    release_chart=$(jq -r '.chart.metadata.name' "${temp_dir}/release.json")
    release_status=$(jq -r '.info.status // empty' "${temp_dir}/release.json")
    release_revision=$(jq -r '.version // empty' "${temp_dir}/release.json")
    [[ "$release_chart" == "orka" ]] || \
      fail "Helm release ${namespace}/${release} uses top-level chart ${release_chart:-unknown}, expected orka"
    case "$release_status" in
      deployed|failed) ;;
      "") fail "Helm release ${namespace}/${release} has no status; refusing cluster-wide CRD mutation" ;;
      *) fail "Helm release ${namespace}/${release} has non-upgradeable status ${release_status}; finish, roll back, or purge that Helm operation before migrating CRDs" ;;
    esac
    rm -f \
      "$secret_storage_file" \
      "$configmap_storage_file" \
      "${temp_dir}/release-storage.json" \
      "${temp_dir}/release-encoded" \
      "${temp_dir}/release-payload" \
      "${temp_dir}/release.json"
  else
    [[ "$allow_missing_release" == true ]] || \
      fail "Helm release ${namespace}/${release} was not found in context ${kube_context}; use --allow-missing-release only for retained CRDs before replacement install"
    local retained_crd_count=0
    local retained_name
    for retained_name in "${EXPECTED_CRD_NAMES[@]}"; do
      if [[ -n "$(kubectl --context "$kube_context" get customresourcedefinition "$retained_name" --ignore-not-found -o name)" ]]; then
        retained_crd_count=$((retained_crd_count + 1))
      fi
    done
    [[ $retained_crd_count -gt 0 ]] || \
      fail "no Helm release or existing Orka CRDs were found; use the normal fresh-install workflow instead"
    if [[ -n "$namespace_name" ]]; then
      release_state="missing; pre-install mode with ${retained_crd_count} retained CRD(s)"
    else
      release_state="missing namespace and release; pre-install mode with ${retained_crd_count} retained CRD(s)"
    fi
  fi

  helm show chart "$chart" >"${temp_dir}/chart.yaml"
  local resolved_name resolved_version
  resolved_name=$(awk '/^name:[[:space:]]/ { print $2; exit }' "${temp_dir}/chart.yaml" | tr -d '"')
  resolved_version=$(awk '/^version:[[:space:]]/ { print $2; exit }' "${temp_dir}/chart.yaml" | tr -d '"')
  [[ "$resolved_name" == "orka" ]] || fail "target chart is ${resolved_name:-unknown}, expected orka"
  [[ -n "$resolved_version" ]] || fail "could not resolve target chart version"

  helm show crds "$chart" >"${temp_dir}/crds.yaml"
  validate_crd_bundle \
    "${temp_dir}/crds.yaml" \
    "target chart ${resolved_name}-${resolved_version} CRD bundle" \
    "$kube_context"
  if ! kubectl --context "$kube_context" create --dry-run=client --validate=false -f "${temp_dir}/crds.yaml" -o json \
    | jq -s '.' >"${temp_dir}/targets.json"; then
    fail "could not normalize the target CRD bundle"
  fi
  jq -e --argjson count "$EXPECTED_CRD_COUNT" 'length == $count' \
    "${temp_dir}/targets.json" >/dev/null || fail "normalized target bundle does not contain ${EXPECTED_CRD_COUNT} CRDs"

  mkdir -p \
    "${temp_dir}/actual" \
    "${temp_dir}/expected" \
    "${temp_dir}/patches" \
    "${temp_dir}/snapshots" \
    "${temp_dir}/targets"

  local name target snapshot patch expected
  local existing_count=0
  local missing_count=0
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    target="${temp_dir}/targets/${name}.json"
    snapshot="${temp_dir}/snapshots/${name}.json"
    jq -e \
      --arg name "$name" \
      --arg annotation "$CRD_MIGRATION_ANNOTATION" \
      --arg migration_id "$migration_id" '
        [.[] | select(.metadata.name == $name)] |
        if length == 1 then
          .[0] |
          .metadata.annotations = ((.metadata.annotations // {}) + {($annotation): $migration_id})
        else
          error("target CRD must appear exactly once")
        end
      ' "${temp_dir}/targets.json" >"$target" || fail "could not isolate target CRD ${name}"

    if ! kubectl --context "$kube_context" get customresourcedefinition "$name" \
      --ignore-not-found \
      -o json >"$snapshot"; then
      fail "could not snapshot CRD ${name}"
    fi
    if [[ -s "$snapshot" ]]; then
      existing_count=$((existing_count + 1))
    else
      rm -f "$snapshot"
      missing_count=$((missing_count + 1))
    fi
  done

  local permission
  for permission in get list watch; do
    [[ "$(kubectl --context "$kube_context" auth can-i "$permission" customresourcedefinitions.apiextensions.k8s.io --all-namespaces)" == "yes" ]] || \
      fail "current identity cannot ${permission} CustomResourceDefinitions in context ${kube_context}"
  done
  if [[ $existing_count -gt 0 ]]; then
    [[ "$(kubectl --context "$kube_context" auth can-i patch customresourcedefinitions.apiextensions.k8s.io --all-namespaces)" == "yes" ]] || \
      fail "current identity cannot patch CustomResourceDefinitions in context ${kube_context}"
  fi
  if [[ $missing_count -gt 0 ]]; then
    [[ "$(kubectl --context "$kube_context" auth can-i create customresourcedefinitions.apiextensions.k8s.io --all-namespaces)" == "yes" ]] || \
      fail "current identity cannot create CustomResourceDefinitions in context ${kube_context}"
  fi

  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    target="${temp_dir}/targets/${name}.json"
    snapshot="${temp_dir}/snapshots/${name}.json"
    patch="${temp_dir}/patches/${name}.json"
    expected="${temp_dir}/expected/${name}.json"
    if [[ -s "$snapshot" ]]; then
      write_target_patch "$snapshot" "$target" "$patch"
      if ! kubectl --context "$kube_context" patch customresourcedefinition "$name" \
        --type=json \
        --field-manager="$CRD_FIELD_MANAGER" \
        --patch-file "$patch" \
        --dry-run=server \
        -o json >"$expected"; then
        fail "server dry-run rejected target CRD ${name}; no CRDs were changed"
      fi
    elif ! kubectl --context "$kube_context" create \
      --field-manager="$CRD_FIELD_MANAGER" \
      --dry-run=server \
      -f "$target" \
      -o json >"$expected"; then
      fail "server dry-run rejected target CRD ${name}; no CRDs were changed"
    fi
  done

  local cluster_server
  cluster_server=$(kubectl --context "$kube_context" config view --minify -o jsonpath='{.clusters[0].cluster.server}')
  cat <<EOF_SUMMARY
Pre-upgrade Orka CRD target:
  context:   ${kube_context}
  server:    ${cluster_server}
  release:   ${namespace}/${release} (${release_state})
  chart:     ${resolved_name}-${resolved_version}
  archive:   ${chart_source}
  sha256:    ${chart_digest}
  migration: ${migration_id}
  resources: ${existing_count} existing, ${missing_count} missing (${EXPECTED_CRD_COUNT} total)

All nine exact patch/create operations passed server-side dry-run. This replaces
the shared Orka CRD specs for the entire cluster. Coordinate this change if
multiple Orka releases share the cluster, and use this exact archive for the
subsequent helm upgrade.
EOF_SUMMARY

  if [[ "$assume_yes" != true ]]; then
    [[ -t 0 ]] || fail "refusing a non-interactive cluster-scoped change without --yes"
    local confirmation
    read -r -p "Type the Kubernetes context name to continue: " confirmation
    [[ "$confirmation" == "$kube_context" ]] || fail "context confirmation did not match ${kube_context}"
  fi

  # Re-read Helm storage after the potentially long preflight and confirmation
  # window so a concurrent Helm operation cannot overlap CRD mutation silently.
  if [[ "$release_exists" == true ]]; then
    local recheck_secret_file="${temp_dir}/release-recheck-secrets.json"
    local recheck_configmap_file="${temp_dir}/release-recheck-configmaps.json"
    kubectl --context "$kube_context" get secrets \
      --namespace "$namespace" \
      --selector "owner=helm,name=${release}" \
      -o json >"$recheck_secret_file" || fail "could not re-read Helm Secret storage"
    kubectl --context "$kube_context" get configmaps \
      --namespace "$namespace" \
      --selector "owner=helm,name=${release}" \
      -o json >"$recheck_configmap_file" || fail "could not re-read Helm ConfigMap storage"
    local recheck_secret_count recheck_configmap_count recheck_storage_file
    recheck_secret_count=$(jq -r '.items | length' "$recheck_secret_file")
    recheck_configmap_count=$(jq -r '.items | length' "$recheck_configmap_file")
    if [[ "$discovered_driver" == "secret" ]]; then
      [[ $recheck_secret_count -gt 0 && $recheck_configmap_count -eq 0 ]] || \
        fail "Helm release storage changed during CRD preflight"
      recheck_storage_file=$recheck_secret_file
    else
      [[ $recheck_configmap_count -gt 0 && $recheck_secret_count -eq 0 ]] || \
        fail "Helm release storage changed during CRD preflight"
      recheck_storage_file=$recheck_configmap_file
    fi
    decode_helm_release_record \
      "$recheck_storage_file" \
      "${temp_dir}/release-recheck.json" \
      "$temp_dir"
    local recheck_chart recheck_status recheck_revision
    recheck_chart=$(jq -r '.chart.metadata.name' "${temp_dir}/release-recheck.json")
    recheck_status=$(jq -r '.info.status // empty' "${temp_dir}/release-recheck.json")
    recheck_revision=$(jq -r '.version // empty' "${temp_dir}/release-recheck.json")
    rm -f \
      "$recheck_secret_file" \
      "$recheck_configmap_file" \
      "${temp_dir}/release-encoded" \
      "${temp_dir}/release-payload" \
      "${temp_dir}/release-recheck.json"
    [[ "$recheck_chart" == "$release_chart" && \
       "$recheck_status" == "$release_status" && \
       "$recheck_revision" == "$release_revision" ]] || \
      fail "Helm release ${namespace}/${release} changed during CRD preflight; retry after the Helm operation settles"
  else
    local appeared_secret_count=0
    local appeared_configmap_count=0
    if [[ -n "$namespace_name" ]]; then
      appeared_secret_count=$(kubectl --context "$kube_context" get secrets \
        --namespace "$namespace" \
        --selector "owner=helm,name=${release}" \
        -o json | jq -r '.items | length')
      appeared_configmap_count=$(kubectl --context "$kube_context" get configmaps \
        --namespace "$namespace" \
        --selector "owner=helm,name=${release}" \
        -o json | jq -r '.items | length')
    fi
    [[ $appeared_secret_count -eq 0 && $appeared_configmap_count -eq 0 ]] || \
      fail "Helm release ${namespace}/${release} appeared during CRD preflight; retry against the live release"
  fi

  mutation_started=true
  local actual
  local applied_count=0
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    target="${temp_dir}/targets/${name}.json"
    snapshot="${temp_dir}/snapshots/${name}.json"
    patch="${temp_dir}/patches/${name}.json"
    actual="${temp_dir}/actual/${name}.json"
    if [[ -s "$snapshot" ]]; then
      if ! kubectl --context "$kube_context" patch customresourcedefinition "$name" \
        --type=json \
        --field-manager="$CRD_FIELD_MANAGER" \
        --patch-file "$patch" \
        -o json >"$actual"; then
        fail "failed to update CRD ${name} after server preflight"
      fi
    elif ! kubectl --context "$kube_context" create \
      --field-manager="$CRD_FIELD_MANAGER" \
      -f "$target" \
      -o json >"$actual"; then
      fail "failed to create CRD ${name} after server preflight"
    fi
    applied_count=$((applied_count + 1))
    if [[ $test_fail_after -gt 0 && $applied_count -eq $test_fail_after ]]; then
      fail "injected test failure after ${applied_count} CRD mutations"
    fi
  done

  local -a resources=()
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    resources+=("customresourcedefinition/${name}")
  done
  kubectl --context "$kube_context" wait \
    --for=condition=Established \
    --timeout=90s \
    "${resources[@]}" >/dev/null || fail "not all target CRDs became Established"

  local current
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    current="${temp_dir}/verify-${name}.json"
    expected="${temp_dir}/expected/${name}.json"
    kubectl --context "$kube_context" get customresourcedefinition "$name" -o json >"$current" || \
      fail "could not verify CRD ${name}"
    managed_state_equal "$current" "$expected" || \
      fail "live CRD ${name} does not exactly match the server-normalized target spec and metadata"
  done

  [[ "$(sha256_file "$chart_source")" == "$chart_digest" ]] || \
    fail "target chart archive changed during CRD migration"

  migration_complete=true
  echo "All ${EXPECTED_CRD_COUNT} Orka CRDs exactly match ${resolved_name}-${resolved_version} and are Established in ${kube_context}."
  if [[ "$release_exists" == true ]]; then
    echo "Run helm upgrade with --kube-context ${kube_context} and the same archive: ${chart_source}"
  else
    echo "Run helm install with --skip-crds, --kube-context ${kube_context}, and the same archive: ${chart_source}"
  fi
}

main "$@"
