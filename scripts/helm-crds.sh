#!/usr/bin/env bash
set -euo pipefail

LC_ALL=C
export LC_ALL
shopt -s dotglob nullglob

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GENERATED_CRD_DIR="${ROOT_DIR}/config/crd/bases"
CHART_CRD_DIR="${ROOT_DIR}/charts/orka/crds"
EXPECTED_CRD_FILENAMES=(
  core.orka.ai_agentruntimes.yaml
  core.orka.ai_agents.yaml
  core.orka.ai_providers.yaml
  core.orka.ai_repositorymonitors.yaml
  core.orka.ai_repositoryscans.yaml
  core.orka.ai_skills.yaml
  core.orka.ai_substrateactorpools.yaml
  core.orka.ai_tasks.yaml
  core.orka.ai_tools.yaml
)
EXPECTED_CRD_COUNT=${#EXPECTED_CRD_FILENAMES[@]}

usage() {
  cat <<'USAGE'
Usage:
  scripts/helm-crds.sh sync [<chart-root>]
  scripts/helm-crds.sh check [<chart-root>]
  scripts/helm-crds.sh check-package <orka-chart.tgz>
  scripts/helm-crds.sh check-package-against-chart <chart-root> <orka-chart.tgz>
  scripts/helm-crds.sh apply-package <orka-chart.tgz> --kube-context <context>

Commands:
  sync [CHART_ROOT]    Replace CHART_ROOT/crds with the canonical generated
                       CRDs from config/crd/bases. Defaults to charts/orka.
  check [CHART_ROOT]   Verify the canonical CRDs and CHART_ROOT/crds mirror.
                       Defaults to charts/orka.
  check-package PATH   Verify the packaged chart contains the exact current
                       canonical CRDs and that `helm show crds` reports all nine.
  check-package-against-chart CHART_ROOT PATH
                       Verify the package contains CRDs byte-identical to the
                       supplied promoted chart snapshot and reports all nine.
  apply-package PATH --kube-context CONTEXT
                       Preflight, apply, and verify the exact packaged CRD specs
                       against an explicit Kubernetes context.
USAGE
}

fail() {
  printf 'helm-crds: %s\n' "$*" >&2
  exit 1
}

require_command() {
  local command_name=$1
  command -v "$command_name" >/dev/null 2>&1 || fail "required command not found: ${command_name}"
}

set_chart_root() {
  local chart_root=$1
  if [[ "$chart_root" != /* ]]; then
    chart_root="${ROOT_DIR}/${chart_root}"
  fi
  CHART_CRD_DIR="${chart_root%/}/crds"
}

validate_static_crd_directory() {
  local directory=$1
  local description=$2
  local entries=()
  local entry
  local kind_count

  [[ -d "$directory" ]] || fail "missing ${description}: ${directory}"

  entries=("$directory"/*)
  for entry in "${entries[@]}"; do
    [[ -f "$entry" && ! -L "$entry" ]] || fail "unexpected non-regular file in ${description}: $(basename "$entry")"

    kind_count=$(grep -Ec '^kind:[[:space:]]+CustomResourceDefinition[[:space:]]*$' "$entry" || true)
    if [[ "$kind_count" -ne 1 ]]; then
      fail "${description}/$(basename "$entry") must contain exactly one CustomResourceDefinition kind, found ${kind_count}"
    fi

    if grep -Fq '{{' "$entry" || grep -Fq '}}' "$entry"; then
      fail "${description}/$(basename "$entry") contains Helm template delimiters; files under crds/ must be static"
    fi
  done
}

validate_crd_directory() {
  local directory=$1
  local description=$2
  local entries=()
  local entry
  local filename

  validate_static_crd_directory "$directory" "$description"

  entries=("$directory"/*)
  if [[ ${#entries[@]} -ne $EXPECTED_CRD_COUNT ]]; then
    fail "expected exactly ${EXPECTED_CRD_COUNT} files in ${description}, found ${#entries[@]}"
  fi

  for filename in "${EXPECTED_CRD_FILENAMES[@]}"; do
    entry="${directory}/${filename}"
    [[ -f "$entry" && ! -L "$entry" ]] || fail "missing expected CRD in ${description}: ${filename}"
  done
}

sync_crds() {
  local chart_parent
  local temp_directory
  local backup_directory=""
  local filename

  validate_crd_directory "$GENERATED_CRD_DIR" "canonical generated CRD directory"

  chart_parent=$(dirname "$CHART_CRD_DIR")
  [[ -d "$chart_parent" ]] || fail "missing chart directory: ${chart_parent}"
  temp_directory=$(mktemp -d "${chart_parent}/.crds.sync.XXXXXX")

  cleanup_sync() {
    local status=$?
    trap - EXIT

    if [[ $status -ne 0 && -n "$backup_directory" && -e "$backup_directory" ]]; then
      rm -rf "$CHART_CRD_DIR"
      mv "$backup_directory" "$CHART_CRD_DIR" || true
    fi
    if [[ -n "$temp_directory" ]]; then
      rm -rf "$temp_directory"
    fi

    exit "$status"
  }
  trap cleanup_sync EXIT

  for filename in "${EXPECTED_CRD_FILENAMES[@]}"; do
    cp "${GENERATED_CRD_DIR}/${filename}" "${temp_directory}/${filename}"
  done
  validate_crd_directory "$temp_directory" "staged chart CRD directory"

  if [[ -e "$CHART_CRD_DIR" || -L "$CHART_CRD_DIR" ]]; then
    backup_directory="${chart_parent}/.crds.backup.$$"
    [[ ! -e "$backup_directory" && ! -L "$backup_directory" ]] || fail "temporary backup path already exists: ${backup_directory}"
    mv "$CHART_CRD_DIR" "$backup_directory"
  fi

  mv "$temp_directory" "$CHART_CRD_DIR"
  temp_directory=""

  trap - EXIT
  if [[ -n "$backup_directory" ]] && ! rm -rf "$backup_directory"; then
    fail "synchronized CRDs installed, but backup cleanup failed: ${backup_directory}"
  fi
  printf 'Synced %s CRDs from %s to %s.\n' "$EXPECTED_CRD_COUNT" "$GENERATED_CRD_DIR" "$CHART_CRD_DIR"
}

check_crds() {
  local filename

  validate_crd_directory "$GENERATED_CRD_DIR" "canonical generated CRD directory"
  validate_crd_directory "$CHART_CRD_DIR" "chart CRD directory"

  for filename in "${EXPECTED_CRD_FILENAMES[@]}"; do
    if ! cmp -s "${GENERATED_CRD_DIR}/${filename}" "${CHART_CRD_DIR}/${filename}"; then
      fail "chart CRD differs from canonical generated CRD: ${filename}; run scripts/helm-crds.sh sync"
    fi
  done

  printf 'Verified %s chart CRDs are byte-identical to the canonical generated CRDs.\n' "$EXPECTED_CRD_COUNT"
}

check_package() {
  local package_path=$1
  local expected_crd_directory=${2:-$GENERATED_CRD_DIR}
  local expected_description=${3:-canonical generated CRD directory}
  local membership_mode=${4:-canonical}
  local expected_crd_filenames=()
  local expected_crd_count=0
  local expected_entry
  local temp_directory
  local expected_members
  local actual_members
  local archive_members
  local extract_directory
  local helm_output
  local filename
  local kind_count
  local member_arguments=()

  [[ -f "$package_path" && ! -L "$package_path" ]] || fail "chart package is not a regular file: ${package_path}"
  require_command tar
  require_command helm
  case "$membership_mode" in
    canonical)
      validate_crd_directory "$expected_crd_directory" "$expected_description"
      expected_crd_filenames=("${EXPECTED_CRD_FILENAMES[@]}")
      expected_crd_count=$EXPECTED_CRD_COUNT
      ;;
    snapshot)
      if [[ -e "$expected_crd_directory" || -L "$expected_crd_directory" ]]; then
        [[ -d "$expected_crd_directory" && ! -L "$expected_crd_directory" ]] || \
          fail "snapshot CRD path is not a regular directory: ${expected_crd_directory}"
        validate_static_crd_directory "$expected_crd_directory" "$expected_description"
        for expected_entry in "$expected_crd_directory"/*; do
          expected_crd_filenames+=("$(basename "$expected_entry")")
          expected_crd_count=$((expected_crd_count + 1))
        done
      fi
      ;;
    *)
      fail "unknown package CRD membership mode: ${membership_mode}"
      ;;
  esac

  temp_directory=$(mktemp -d "${TMPDIR:-/tmp}/orka-helm-crds.XXXXXX")
  cleanup_package() {
    local status=$?
    trap - EXIT
    rm -rf "$temp_directory"
    exit "$status"
  }
  trap cleanup_package EXIT

  expected_members="${temp_directory}/expected-members.txt"
  actual_members="${temp_directory}/actual-members.txt"
  archive_members="${temp_directory}/archive-members.txt"
  extract_directory="${temp_directory}/extract"
  helm_output="${temp_directory}/helm-show-crds.yaml"
  mkdir -p "$extract_directory"

  : > "$expected_members"
  if [[ $expected_crd_count -gt 0 ]]; then
    for filename in "${expected_crd_filenames[@]}"; do
      printf 'orka/crds/%s\n' "$filename" >> "$expected_members"
      member_arguments+=("orka/crds/${filename}")
    done
  fi

  tar -tzf "$package_path" > "$archive_members"
  awk 'index($0, "orka/crds/") == 1 && $0 != "orka/crds/" { print }' "$archive_members" | sort > "$actual_members"
  if ! diff -u "$expected_members" "$actual_members"; then
    fail "chart package must contain exactly the ${expected_crd_count} expected CRD members"
  fi

  if [[ $expected_crd_count -gt 0 ]]; then
    tar -xzf "$package_path" -C "$extract_directory" "${member_arguments[@]}"
  fi
  if [[ $expected_crd_count -gt 0 ]]; then
    for filename in "${expected_crd_filenames[@]}"; do
      if ! cmp -s "${expected_crd_directory}/${filename}" "${extract_directory}/orka/crds/${filename}"; then
        fail "packaged CRD differs from ${expected_description}: ${filename}"
      fi
    done
  fi

  helm show crds "$package_path" > "$helm_output"
  kind_count=$(awk '$0 == "kind: CustomResourceDefinition" { count++ } END { print count + 0 }' "$helm_output")
  if [[ "$kind_count" -ne $expected_crd_count ]]; then
    fail "helm show crds reported ${kind_count} CRDs; expected ${expected_crd_count}"
  fi

  trap - EXIT
  rm -rf "$temp_directory"
  printf 'Verified package %s contains %s expected CRDs and helm show crds reports all of them.\n' "$package_path" "$expected_crd_count"
}

openapi_index_url() {
  local kube_context=$1
  local group=$2
  local version=$3
  kubectl --context "$kube_context" get --raw /openapi/v3 2>/dev/null | \
    jq -er --arg path "apis/${group}/${version}" '.paths[$path].serverRelativeURL'
}

discovery_has_resource() {
  local kube_context=$1
  local group=$2
  local version=$3
  local plural=$4
  local kind=$5
  local namespaced=$6
  local discovery

  discovery=$(kubectl --context "$kube_context" get --raw "/apis/${group}/${version}" 2>/dev/null) || return 1
  jq -e \
    --arg plural "$plural" \
    --arg kind "$kind" \
    --argjson namespaced "$namespaced" \
    '.resources | any(.name == $plural and .kind == $kind and .namespaced == $namespaced)' \
    <<< "$discovery" >/dev/null
}

discovery_matches_target_version() {
  local kube_context=$1
  local target_json=$2
  local version=$3
  local group
  local plural
  local singular
  local kind
  local scope
  local namespaced
  local short_names
  local categories
  local has_status
  local has_scale
  local discovery

  group=$(jq -er '.spec.group' "$target_json")
  plural=$(jq -er '.spec.names.plural' "$target_json")
  singular=$(jq -er '.spec.names.singular' "$target_json")
  kind=$(jq -er '.spec.names.kind' "$target_json")
  scope=$(jq -er '.spec.scope' "$target_json")
  if [[ "$scope" == "Namespaced" ]]; then namespaced=true; else namespaced=false; fi
  short_names=$(jq -c '.spec.names.shortNames // [] | sort' "$target_json")
  categories=$(jq -c '.spec.names.categories // [] | sort' "$target_json")
  has_status=$(jq -e --arg version "$version" '
    first(.spec.versions[] | select(.name == $version))
    | (.subresources.status // null) != null
  ' "$target_json" >/dev/null && printf true || printf false)
  has_scale=$(jq -e --arg version "$version" '
    first(.spec.versions[] | select(.name == $version))
    | (.subresources.scale // null) != null
  ' "$target_json" >/dev/null && printf true || printf false)

  discovery=$(kubectl --context "$kube_context" get --raw "/apis/${group}/${version}" 2>/dev/null) || return 1
  jq -e \
    --arg plural "$plural" \
    --arg singular "$singular" \
    --arg kind "$kind" \
    --argjson namespaced "$namespaced" \
    --argjson shortNames "$short_names" \
    --argjson categories "$categories" \
    --argjson hasStatus "$has_status" \
    --argjson hasScale "$has_scale" '
      (.resources | any(
        .name == $plural and
        .singularName == $singular and
        .kind == $kind and
        .namespaced == $namespaced and
        ((.shortNames // []) | sort) == $shortNames and
        ((.categories // []) | sort) == $categories
      )) and
      ((.resources | any(.name == ($plural + "/status"))) == $hasStatus) and
      ((.resources | any(.name == ($plural + "/scale"))) == $hasScale)
    ' <<< "$discovery" >/dev/null
}

write_openapi_gvk_schema() {
  local kube_context=$1
  local group=$2
  local version=$3
  local kind=$4
  local output_file=$5
  local server_relative_url
  local document

  server_relative_url=$(openapi_index_url "$kube_context" "$group" "$version" 2>/dev/null) || return 1
  document=$(kubectl --context "$kube_context" get --raw "$server_relative_url" 2>/dev/null) || return 1
  jq -e --sort-keys \
    --arg group "$group" \
    --arg version "$version" \
    --arg kind "$kind" '
      [
        .components.schemas[]?
        | select(
            [. ["x-kubernetes-group-version-kind"][]?]
            | any(.group == $group and .version == $version and .kind == $kind)
          )
      ]
      | if length == 1 then .[0] else error("expected exactly one matching GVK schema") end
      | walk(if type == "object" then del(. ["x-kubernetes-group-version-kind"]) else . end)
      | if .properties.metadata? then .properties.metadata = {type: "object"} else . end
      | walk(
          if type == "object" and (.description? | type == "string") then
            .description |= gsub("[[:space:]]+"; " ")
          else . end
        )
    ' <<< "$document" > "$output_file" 2>/dev/null
}

wait_for_crd_serving() {
  local kube_context=$1
  local crd_name=$2
  local target_json=$3
  local existing_json=$4
  local target_versions_file=$5
  local existing_versions_file=$6
  local target_schemas_dir=$7
  local group
  local kind
  local old_group=""
  local old_plural=""
  local old_kind=""
  local old_scope=""
  local old_namespaced=false
  local deadline
  local version
  local status_json
  local ready
  local previous_ready=false
  local current_schema
  local expected_schema
  local current_snapshot="${target_schemas_dir}/current-served-snapshot"
  local previous_snapshot="${target_schemas_dir}/previous-served-snapshot"
  local removed_schema="${target_schemas_dir}/removed-version-schema"

  group=$(jq -er '.spec.group' "$target_json")
  kind=$(jq -er '.spec.names.kind' "$target_json")

  if [[ -s "$existing_json" ]]; then
    old_group=$(jq -er '.spec.group' "$existing_json")
    old_plural=$(jq -er '.spec.names.plural' "$existing_json")
    old_kind=$(jq -er '.spec.names.kind' "$existing_json")
    old_scope=$(jq -er '.spec.scope' "$existing_json")
    if [[ "$old_scope" == "Namespaced" ]]; then old_namespaced=true; fi
  fi

  deadline=$((SECONDS + 120))
  while (( SECONDS < deadline )); do
    ready=true
    : > "$current_snapshot"
    status_json=$(kubectl --context "$kube_context" get customresourcedefinition "$crd_name" --output json 2>/dev/null) || ready=false
    if [[ "$ready" == true ]]; then
      jq -e '
        (.status.conditions // [] | any(.type == "Established" and .status == "True")) and
        (.status.conditions // [] | any(.type == "NamesAccepted" and .status == "True"))
      ' <<< "$status_json" >/dev/null || ready=false
    fi

    if [[ "$ready" == true ]]; then
      while IFS= read -r version; do
        [[ -n "$version" ]] || continue
        if ! discovery_matches_target_version "$kube_context" "$target_json" "$version"; then
          ready=false
          break
        fi
        current_schema="${target_schemas_dir}/current-${version}.json"
        expected_schema="${target_schemas_dir}/expected-${version}.json"
        if ! write_openapi_gvk_schema "$kube_context" "$group" "$version" "$kind" "$current_schema"; then
          ready=false
          break
        fi
        if ! cmp -s "$expected_schema" "$current_schema"; then
          ready=false
          break
        fi
        printf '%s\n' "$version" >> "$current_snapshot"
        cat "$current_schema" >> "$current_snapshot"
      done < "$target_versions_file"
    fi

    if [[ "$ready" == true && -s "$existing_json" ]]; then
      while IFS= read -r version; do
        [[ -n "$version" ]] || continue
        if grep -Fxq "$version" "$target_versions_file"; then continue; fi
        if discovery_has_resource "$kube_context" "$old_group" "$version" "$old_plural" "$old_kind" "$old_namespaced"; then
          ready=false
          break
        fi
        if write_openapi_gvk_schema "$kube_context" "$old_group" "$version" "$old_kind" "$removed_schema"; then
          ready=false
          break
        fi
      done < "$existing_versions_file"
    fi

    if [[ "$ready" == true ]]; then
      if [[ "$previous_ready" == true ]] && cmp -s "$previous_snapshot" "$current_snapshot"; then
        return 0
      fi
      cp "$current_snapshot" "$previous_snapshot"
      previous_ready=true
    else
      previous_ready=false
    fi
    sleep 1
  done

  fail "timed out waiting for discovery and OpenAPI to serve the target CRD schema: ${crd_name}"
}

apply_package() {
  local package_path=$1
  local kube_context=$2
  local temp_directory
  local private_package
  local extract_directory
  local plan_directory
  local filename
  local manifest
  local target_json
  local existing_json
  local expected_json
  local expected_spec
  local live_spec
  local patch_file
  local mode_file
  local name_file
  local target_versions_file
  local existing_versions_file
  local target_schemas_dir
  local crd_name
  local version
  local mode
  local applied_count=0

  [[ -n "$kube_context" ]] || fail "a non-empty --kube-context is required"
  require_command kubectl
  require_command jq
  require_command tar
  require_command helm
  [[ -f "$package_path" && ! -L "$package_path" ]] || fail "chart package is not a regular file: ${package_path}"

  kubectl config get-contexts "$kube_context" >/dev/null 2>&1 || \
    fail "Kubernetes context not found: ${kube_context}"

  temp_directory=$(mktemp -d "${TMPDIR:-/tmp}/orka-helm-crd-apply.XXXXXX")
  private_package="${temp_directory}/target-chart.tgz"
  extract_directory="${temp_directory}/extract"
  plan_directory="${temp_directory}/plan"
  mkdir -p "$extract_directory" "$plan_directory"

  cleanup_apply() {
    local status=$?
    trap - EXIT
    rm -rf "$temp_directory"
    if [[ $status -ne 0 && $applied_count -gt 0 ]]; then
      printf 'helm-crds: apply stopped after changing %s CRD(s); changes are not rolled back. Inspect the cluster and rerun the same command.\n' \
        "$applied_count" >&2
    fi
    exit "$status"
  }
  trap cleanup_apply EXIT

  cp "$package_path" "$private_package"
  (check_package "$private_package")
  tar -xzf "$private_package" -C "$extract_directory" orka/crds

  # Preflight every create/replace before mutating any CRD. Existing CRDs use
  # JSON Patch tests for UID and resourceVersion so concurrent changes fail
  # closed instead of being overwritten.
  for filename in "${EXPECTED_CRD_FILENAMES[@]}"; do
    manifest="${extract_directory}/orka/crds/${filename}"
    target_json="${plan_directory}/${filename}.target.json"
    existing_json="${plan_directory}/${filename}.existing.json"
    expected_json="${plan_directory}/${filename}.expected.json"
    expected_spec="${plan_directory}/${filename}.expected-spec.json"
    patch_file="${plan_directory}/${filename}.patch.json"
    mode_file="${plan_directory}/${filename}.mode"
    name_file="${plan_directory}/${filename}.name"
    target_versions_file="${plan_directory}/${filename}.target-served-versions"
    existing_versions_file="${plan_directory}/${filename}.existing-served-versions"
    target_schemas_dir="${plan_directory}/${filename}.target-openapi-schemas"
    mkdir -p "$target_schemas_dir"

    kubectl --context "$kube_context" create \
      --dry-run=client \
      --validate=false \
      --filename "$manifest" \
      --output json > "$target_json"
    jq -e '
      .apiVersion == "apiextensions.k8s.io/v1" and
      .kind == "CustomResourceDefinition" and
      (.metadata.name | type == "string" and length > 0) and
      (.spec | type == "object")
    ' "$target_json" >/dev/null || fail "invalid packaged CRD manifest: ${filename}"
    crd_name=$(jq -er '.metadata.name' "$target_json")
    printf '%s\n' "$crd_name" > "$name_file"

    kubectl --context "$kube_context" get customresourcedefinition "$crd_name" \
      --ignore-not-found --output json > "$existing_json"
    : > "$existing_versions_file"

    if [[ -s "$existing_json" ]]; then
      jq -r '.spec.versions[] | select(.served == true) | .name' "$existing_json" | sort -u > "$existing_versions_file"
      jq -cn \
        --slurpfile existing "$existing_json" \
        --slurpfile target "$target_json" \
        '[
          {op: "test", path: "/metadata/uid", value: $existing[0].metadata.uid},
          {op: "test", path: "/metadata/resourceVersion", value: $existing[0].metadata.resourceVersion},
          {op: "replace", path: "/spec", value: $target[0].spec}
        ]' > "$patch_file"
      kubectl --context "$kube_context" patch customresourcedefinition "$crd_name" \
        --type json \
        --patch-file "$patch_file" \
        --dry-run=server \
        --output json > "$expected_json"
      printf 'patch\n' > "$mode_file"
    else
      kubectl --context "$kube_context" create \
        --filename "$manifest" \
        --dry-run=server \
        --output json > "$expected_json"
      printf 'create\n' > "$mode_file"
    fi
    jq --sort-keys '.spec' "$expected_json" > "$expected_spec"
    jq -r '.spec.versions[] | select(.served == true) | .name' "$expected_json" | sort -u > "$target_versions_file"

    while IFS= read -r version; do
      [[ -n "$version" ]] || continue
      jq -e --sort-keys --arg version "$version" '
        first(.spec.versions[] | select(.name == $version)).schema.openAPIV3Schema
        | if . == null then error("served version has no OpenAPI schema") else . end
        | if .properties.metadata? then .properties.metadata = {type: "object"} else . end
        | walk(
            if type == "object" and (.description? | type == "string") then
              .description |= gsub("[[:space:]]+"; " ")
            else . end
          )
      ' "$expected_json" > "${target_schemas_dir}/expected-${version}.json"
    done < "$target_versions_file"
  done

  for filename in "${EXPECTED_CRD_FILENAMES[@]}"; do
    manifest="${extract_directory}/orka/crds/${filename}"
    expected_spec="${plan_directory}/${filename}.expected-spec.json"
    live_spec="${plan_directory}/${filename}.live-spec.json"
    patch_file="${plan_directory}/${filename}.patch.json"
    mode_file="${plan_directory}/${filename}.mode"
    name_file="${plan_directory}/${filename}.name"
    target_json="${plan_directory}/${filename}.target.json"
    existing_json="${plan_directory}/${filename}.existing.json"
    target_versions_file="${plan_directory}/${filename}.target-served-versions"
    existing_versions_file="${plan_directory}/${filename}.existing-served-versions"
    target_schemas_dir="${plan_directory}/${filename}.target-openapi-schemas"
    crd_name=$(cat "$name_file")
    mode=$(cat "$mode_file")

    case "$mode" in
      patch)
        kubectl --context "$kube_context" patch customresourcedefinition "$crd_name" \
          --type json \
          --patch-file "$patch_file" \
          --output name >/dev/null
        ;;
      create)
        kubectl --context "$kube_context" create \
          --filename "$manifest" \
          --output name >/dev/null
        ;;
      *)
        fail "unknown CRD apply mode for ${crd_name}: ${mode}"
        ;;
    esac
    applied_count=$((applied_count + 1))

    kubectl --context "$kube_context" get customresourcedefinition "$crd_name" \
      --output json | jq --sort-keys '.spec' > "$live_spec"
    if ! cmp -s "$expected_spec" "$live_spec"; then
      diff -u "$expected_spec" "$live_spec" >&2 || true
      fail "live CRD spec does not match the server-normalized target: ${crd_name}"
    fi
    wait_for_crd_serving \
      "$kube_context" \
      "$crd_name" \
      "$target_json" \
      "$existing_json" \
      "$target_versions_file" \
      "$existing_versions_file" \
      "$target_schemas_dir"
    kubectl --context "$kube_context" get customresourcedefinition "$crd_name" \
      --output json | jq --sort-keys '.spec' > "$live_spec"
    if ! cmp -s "$expected_spec" "$live_spec"; then
      diff -u "$expected_spec" "$live_spec" >&2 || true
      fail "live CRD spec changed after serving convergence: ${crd_name}"
    fi
  done

  trap - EXIT
  rm -rf "$temp_directory"
  printf 'Applied and verified %s packaged CRD specs against context %s.\n' \
    "$EXPECTED_CRD_COUNT" "$kube_context"
}

command_name=${1:-}
case "$command_name" in
  sync)
    [[ $# -ge 1 && $# -le 2 ]] || { usage >&2; exit 2; }
    if [[ $# -eq 2 ]]; then set_chart_root "$2"; fi
    sync_crds
    ;;
  check)
    [[ $# -ge 1 && $# -le 2 ]] || { usage >&2; exit 2; }
    if [[ $# -eq 2 ]]; then set_chart_root "$2"; fi
    check_crds
    ;;
  check-package)
    [[ $# -eq 2 ]] || { usage >&2; exit 2; }
    check_package "$2"
    ;;
  check-package-against-chart)
    [[ $# -eq 3 ]] || { usage >&2; exit 2; }
    set_chart_root "$2"
    check_package "$3" "$CHART_CRD_DIR" "promoted chart CRD directory" snapshot
    ;;
  apply-package)
    [[ $# -eq 4 && "$3" == "--kube-context" ]] || { usage >&2; exit 2; }
    apply_package "$2" "$4"
    ;;
  -h|--help)
    usage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
