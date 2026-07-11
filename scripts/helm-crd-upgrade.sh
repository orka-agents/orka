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
  local expected_release=$4
  local expected_namespace=$5

  jq -e \
    --arg release "$expected_release" \
    --arg namespace "$expected_namespace" '
      (.items | length) > 0 and
      ([.items[].kind] | unique | length) == 1 and
      all(.items[];
        .metadata.namespace == $namespace and
        .metadata.labels.owner == "helm" and
        .metadata.labels.name == $release and
        ((.metadata.labels.version // "") | test("^[1-9][0-9]*$"))
      ) and
      (
        ([.items[].metadata.labels.version | tonumber] | max) as $latest |
        ([.items[] | select((.metadata.labels.version | tonumber) == $latest)] | length) == 1
      )
    ' "$storage_json" >/dev/null || \
    fail "Helm release storage is missing, ambiguous, or mislabeled"

  local selected encoded payload latest_kind latest_data latest_revision latest_label_status
  selected="${work_dir}/release-storage-selected.json"
  jq '
    ([.items[].metadata.labels.version | tonumber] | max) as $latest |
    [.items[] | select((.metadata.labels.version | tonumber) == $latest)][0]
  ' "$storage_json" >"$selected"
  latest_kind=$(jq -r '.kind' "$selected")
  latest_data=$(jq -r '.data.release // empty' "$selected")
  latest_revision=$(jq -r '.metadata.labels.version | tonumber' "$selected")
  latest_label_status=$(jq -r '.metadata.labels.status // empty' "$selected")
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
  jq -e \
    --arg release "$expected_release" \
    --arg namespace "$expected_namespace" \
    --argjson revision "$latest_revision" '
      type == "object" and
      .name == $release and
      .namespace == $namespace and
      .version == $revision and
      (.chart.metadata.name | type) == "string"
    ' "$output" >/dev/null || \
    fail "decoded Helm release identity or revision does not match its storage record"
  if [[ -n "$latest_label_status" ]]; then
    [[ "$(jq -r '.info.status // empty' "$output")" == "$latest_label_status" ]] || \
      fail "decoded Helm release status does not match its storage label"
  fi
}

capture_helm_storage_state() {
  local kube_context=$1
  local namespace=$2
  local release=$3
  local output=$4
  local work_dir=$5
  local prefix=$6

  local namespace_file="${work_dir}/${prefix}-namespace.json"
  local namespace_summary_file="${work_dir}/${prefix}-namespace-summary.json"
  if ! kubectl --context "$kube_context" get namespace "$namespace" \
    --ignore-not-found \
    -o json >"$namespace_file"; then
    fail "could not verify namespace ${namespace} in context ${kube_context}"
  fi
  if [[ -s "$namespace_file" ]]; then
    jq -e '
      .status.phase == "Active" and
      (.metadata.deletionTimestamp == null)
    ' "$namespace_file" >/dev/null || \
      fail "namespace ${namespace} is terminating or not Active; refusing cluster-wide CRD mutation"
    jq '
      {
        uid: .metadata.uid,
        phase: .status.phase,
        deletionTimestamp: (.metadata.deletionTimestamp // null)
      }
    ' "$namespace_file" >"$namespace_summary_file"
  else
    printf '%s\n' 'null' >"$namespace_summary_file"
  fi

  local secret_file="${work_dir}/${prefix}-secrets.json"
  local configmap_file="${work_dir}/${prefix}-configmaps.json"
  # shellcheck disable=SC2016 # jq program intentionally uses jq field syntax.
  local storage_summary_filter='
    {
      items: [
        .items[] |
        {
          kind: .kind,
          name: .metadata.name,
          namespace: .metadata.namespace,
          uid: .metadata.uid,
          resourceVersion: .metadata.resourceVersion,
          owner: .metadata.labels.owner,
          releaseName: .metadata.labels.name,
          status: .metadata.labels.status,
          version: .metadata.labels.version
        }
      ]
    }
  '
  if [[ -s "$namespace_file" ]]; then
    if ! kubectl --context "$kube_context" get secrets \
      --namespace "$namespace" \
      --selector "owner=helm,name=${release}" \
      -o json | jq "$storage_summary_filter" >"$secret_file"; then
      fail "could not read Helm Secret release storage for ${namespace}/${release}"
    fi
    if ! kubectl --context "$kube_context" get configmaps \
      --namespace "$namespace" \
      --selector "owner=helm,name=${release}" \
      -o json | jq "$storage_summary_filter" >"$configmap_file"; then
      fail "could not read Helm ConfigMap release storage for ${namespace}/${release}"
    fi
  else
    printf '%s\n' '{"items":[]}' >"$secret_file"
    printf '%s\n' '{"items":[]}' >"$configmap_file"
  fi

  jq -n \
    --slurpfile namespace "$namespace_summary_file" \
    --slurpfile secrets "$secret_file" \
    --slurpfile configmaps "$configmap_file" '
      {
        namespace: $namespace[0],
        secrets: ($secrets[0].items | sort_by(.name)),
        configmaps: ($configmaps[0].items | sort_by(.name))
      }
    ' >"$output"
  rm -f "$namespace_file" "$namespace_summary_file" "$secret_file" "$configmap_file"
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

wait_for_crd_target_ready() {
  local kube_context=$1
  local name=$2
  local expected=$3
  # The fourth argument is the server response from this invocation's
  # successful patch/create request, not the pre-mutation snapshot.
  local mutation_result=$4
  local deadline=$((SECONDS + 90))
  local prefix="${expected%.json}"
  local current="${prefix}-ready-current.json"
  local discovery="${prefix}-discovery.json"
  local openapi_index="${prefix}-openapi-index.json"
  local openapi_document="${prefix}-openapi-document.json"
  local target_schema="${prefix}-target-schema.json"
  local published_schema="${prefix}-published-schema.json"
  local target_normalized="${prefix}-target-schema-normalized.json"
  local published_normalized="${prefix}-published-schema-normalized.json"
  local group plural singular kind namespaced short_names categories served_versions version api_path openapi_url
  local expect_status expect_scale
  local discovery_ready remaining request_timeout normalize_filter

  # shellcheck disable=SC2016 # jq program intentionally uses jq variables.
  normalize_filter='
    def canonical_int_or_string:
      if type == "object" and .["x-kubernetes-int-or-string"] == true then
        if (.anyOf? | type) == "array" then
          .anyOf |= sort_by(.type // "")
        elif (.allOf? | type) == "array" and (.allOf | length) == 1 and ((.allOf[0].anyOf? | type) == "array") then
          .anyOf = (.allOf[0].anyOf | sort_by(.type // "")) | del(.allOf)
        else
          .anyOf = [{"type":"integer"},{"type":"string"}]
        end
      else . end;
    def normalize_schema:
      if type != "object" then .
      else
        del(.description)
        | if (.properties? | type) == "object" then .properties |= with_entries(.value |= normalize_schema) else . end
        | if (.patternProperties? | type) == "object" then .patternProperties |= with_entries(.value |= normalize_schema) else . end
        | if (.definitions? | type) == "object" then .definitions |= with_entries(.value |= normalize_schema) else . end
        | if (.["$defs"]? | type) == "object" then .["$defs"] |= with_entries(.value |= normalize_schema) else . end
        | if (.additionalProperties? | type) == "object" then .additionalProperties |= normalize_schema else . end
        | if (.propertyNames? | type) == "object" then .propertyNames |= normalize_schema else . end
        | if (.contains? | type) == "object" then .contains |= normalize_schema else . end
        | if (.not? | type) == "object" then .not |= normalize_schema else . end
        | if (.items? | type) == "object" then .items |= normalize_schema
          elif (.items? | type) == "array" then .items |= map(normalize_schema)
          else . end
        | if (.allOf? | type) == "array" then .allOf |= map(normalize_schema) else . end
        | if (.anyOf? | type) == "array" then .anyOf |= map(normalize_schema) else . end
        | if (.oneOf? | type) == "array" then .oneOf |= map(normalize_schema) else . end
        | canonical_int_or_string
      end;
    normalize_schema |
    del(.properties.apiVersion, .properties.kind, .properties.metadata, .["x-kubernetes-group-version-kind"])
  '

  while ((SECONDS < deadline)); do
    remaining=$((deadline - SECONDS))
    ((remaining > 0)) || break
    request_timeout="${remaining}s"
    if kubectl --context "$kube_context" --request-timeout="$request_timeout" \
      get customresourcedefinition "$name" -o json >"$current" 2>/dev/null &&
      managed_state_equal "$current" "$expected" &&
      jq -e --slurpfile mutation "$mutation_result" '
        ($mutation[0].metadata.generation) as $generation |
        (.metadata.generation == $generation) and
        (.metadata.deletionTimestamp == null) and
        all(.status.conditions[]?; .type != "Terminating" or .status != "True") and
        (
          .spec.names as $specNames |
          .status.acceptedNames as $accepted |
          $accepted.plural == $specNames.plural and
          $accepted.singular == $specNames.singular and
          $accepted.kind == $specNames.kind and
          $accepted.listKind == $specNames.listKind and
          (($accepted.shortNames // []) | sort) == (($specNames.shortNames // []) | sort) and
          (($accepted.categories // []) | sort) == (($specNames.categories // []) | sort) and
          any(.status.conditions[]?; .type == "Established" and .status == "True") and
          any(.status.conditions[]?; .type == "NamesAccepted" and .status == "True") and
          all(.status.conditions[]?; (.observedGeneration == null) or (.observedGeneration == $generation))
        )
      ' "$current" >/dev/null; then
      group=$(jq -r '.spec.group' "$current")
      plural=$(jq -r '.spec.names.plural' "$current")
      singular=$(jq -r '.spec.names.singular' "$current")
      kind=$(jq -r '.spec.names.kind' "$current")
      namespaced=$(jq -r '.spec.scope == "Namespaced"' "$current")
      short_names=$(jq -c '(.spec.names.shortNames // []) | sort' "$current")
      categories=$(jq -c '(.spec.names.categories // []) | sort' "$current")
      served_versions=$(jq -r '.spec.versions[] | select(.served == true) | .name' "$current")
      discovery_ready=true
      for version in $served_versions; do
        expect_status=$(jq -r --arg version "$version" '
          [.spec.versions[] | select(.name == $version)][0].subresources.status != null
        ' "$current")
        expect_scale=$(jq -r --arg version "$version" '
          [.spec.versions[] | select(.name == $version)][0].subresources.scale != null
        ' "$current")
        remaining=$((deadline - SECONDS))
        if ((remaining <= 0)); then
          discovery_ready=false
          break
        fi
        request_timeout="${remaining}s"
        if ! kubectl --context "$kube_context" --request-timeout="$request_timeout" \
          get --raw "/apis/${group}/${version}" >"$discovery" 2>/dev/null ||
          ! jq -e \
            --arg plural "$plural" \
            --arg singular "$singular" \
            --arg kind "$kind" \
            --argjson namespaced "$namespaced" \
            --argjson short_names "$short_names" \
            --argjson categories "$categories" \
            --argjson expect_status "$expect_status" \
            --argjson expect_scale "$expect_scale" '
              (any(.resources[]?;
                .name == $plural and
                .singularName == $singular and
                .kind == $kind and
                .namespaced == $namespaced and
                ((.shortNames // []) | sort) == $short_names and
                ((.categories // []) | sort) == $categories
              )) and
              ((any(.resources[]?; .name == ($plural + "/status"))) == $expect_status) and
              ((any(.resources[]?; .name == ($plural + "/scale"))) == $expect_scale)
            ' "$discovery" >/dev/null; then
          discovery_ready=false
          break
        fi

        remaining=$((deadline - SECONDS))
        if ((remaining <= 0)); then
          discovery_ready=false
          break
        fi
        request_timeout="${remaining}s"
        api_path="apis/${group}/${version}"
        if ! kubectl --context "$kube_context" --request-timeout="$request_timeout" \
          get --raw /openapi/v3 >"$openapi_index" 2>/dev/null; then
          discovery_ready=false
          break
        fi
        openapi_url=$(jq -r --arg path "$api_path" '.paths[$path].serverRelativeURL // empty' "$openapi_index")
        remaining=$((deadline - SECONDS))
        if [[ -z "$openapi_url" ]] || ((remaining <= 0)); then
          discovery_ready=false
          break
        fi
        request_timeout="${remaining}s"
        if ! kubectl --context "$kube_context" --request-timeout="$request_timeout" \
          get --raw "$openapi_url" >"$openapi_document" 2>/dev/null; then
          discovery_ready=false
          break
        fi
        if ! jq -e --arg version "$version" '
          [.spec.versions[] | select(.name == $version) | .schema.openAPIV3Schema] |
          if length == 1 then .[0] else error("target version schema is missing or ambiguous") end
        ' "$current" >"$target_schema" ||
          ! jq -e \
            --arg group "$group" \
            --arg version "$version" \
            --arg kind "$kind" '
              [
                .components.schemas | to_entries[] |
                select(any(.value["x-kubernetes-group-version-kind"][]?;
                  .group == $group and .version == $version and .kind == $kind
                )) |
                .value
              ] |
              if length == 1 then .[0] else error("published schema is missing or ambiguous") end
            ' "$openapi_document" >"$published_schema"; then
          discovery_ready=false
          break
        fi
        if ! jq -S "$normalize_filter" "$target_schema" >"$target_normalized" ||
          ! jq -S "$normalize_filter" "$published_schema" >"$published_normalized"; then
          discovery_ready=false
          break
        fi
        if ! cmp -s "$target_normalized" "$published_normalized"; then
          discovery_ready=false
          break
        fi
      done
      if [[ "$discovery_ready" == true ]]; then
        rm -f \
          "$current" "$discovery" "$openapi_index" "$openapi_document" \
          "$target_schema" "$published_schema" "$target_normalized" "$published_normalized"
        return 0
      fi
    fi
    ((SECONDS < deadline)) && sleep 1
  done

  echo "orka-crd-upgrade: CRD ${name} did not publish its target generation, accepted names, discovery, and OpenAPI schema within 90s" >&2
  return 1
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
  local jq_version jq_major jq_minor
  jq_version=$(jq --version)
  jq_version=${jq_version#jq-}
  jq_major=${jq_version%%.*}
  jq_minor=${jq_version#*.}
  jq_minor=${jq_minor%%.*}
  [[ "$jq_major" =~ ^[0-9]+$ && "$jq_minor" =~ ^[0-9]+$ ]] || \
    fail "could not parse jq version ${jq_version}"
  ((jq_major > 1 || (jq_major == 1 && jq_minor >= 6))) || \
    fail "jq 1.6 or newer is required for CRD schema publication verification"
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

  local initial_release_state="${temp_dir}/helm-state-initial.json"
  capture_helm_storage_state \
    "$kube_context" "$namespace" "$release" \
    "$initial_release_state" "$temp_dir" initial

  local secret_storage_count configmap_storage_count
  secret_storage_count=$(jq -r '.secrets | length' "$initial_release_state")
  configmap_storage_count=$(jq -r '.configmaps | length' "$initial_release_state")
  if [[ $secret_storage_count -gt 0 && $configmap_storage_count -gt 0 ]]; then
    fail "Helm release ${namespace}/${release} exists in both Secret and ConfigMap storage; reconcile the duplicate release records before migrating CRDs"
  fi

  local discovered_driver=""
  if [[ $secret_storage_count -gt 0 ]]; then
    discovered_driver=secret
  elif [[ $configmap_storage_count -gt 0 ]]; then
    discovered_driver=configmap
  fi

  local release_chart=""
  local release_status=""
  if [[ -n "$discovered_driver" ]]; then
    [[ "$helm_driver" == "$discovered_driver" ]] || \
      fail "HELM_DRIVER=${helm_driver} does not match the existing ${discovered_driver}-backed release ${namespace}/${release}; set HELM_DRIVER=${discovered_driver} before migrating or upgrading"
    release_exists=true
    release_state="existing"
    if [[ "$discovered_driver" == "secret" ]]; then
      kubectl --context "$kube_context" get secrets \
        --namespace "$namespace" \
        --selector "owner=helm,name=${release}" \
        -o json >"${temp_dir}/release-storage.json" || \
        fail "could not read selected Helm Secret release storage"
    else
      kubectl --context "$kube_context" get configmaps \
        --namespace "$namespace" \
        --selector "owner=helm,name=${release}" \
        -o json >"${temp_dir}/release-storage.json" || \
        fail "could not read selected Helm ConfigMap release storage"
    fi
    decode_helm_release_record \
      "${temp_dir}/release-storage.json" \
      "${temp_dir}/release.json" \
      "$temp_dir" \
      "$release" \
      "$namespace"
    release_chart=$(jq -r '.chart.metadata.name' "${temp_dir}/release.json")
    release_status=$(jq -r '.info.status // empty' "${temp_dir}/release.json")
    [[ "$release_chart" == "orka" ]] || \
      fail "Helm release ${namespace}/${release} uses top-level chart ${release_chart:-unknown}, expected orka"
    case "$release_status" in
      deployed|failed) ;;
      "") fail "Helm release ${namespace}/${release} has no status; refusing cluster-wide CRD mutation" ;;
      *) fail "Helm release ${namespace}/${release} has non-upgradeable status ${release_status}; finish, roll back, or purge that Helm operation before migrating CRDs" ;;
    esac
    rm -f \
      "${temp_dir}/release-storage.json" \
      "${temp_dir}/release-storage-selected.json" \
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
    if [[ -n "$(jq -r '.namespace.uid // empty' "$initial_release_state")" ]]; then
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
  jq -e '
    all(.[];
      all(.spec.versions[];
        ((.selectableFields // []) | length) == 0 and
        ([((.schema.openAPIV3Schema // {}) | .. | objects) |
          select(.["x-kubernetes-embedded-resource"] == true)] | length) == 0 and
        all(((.schema.openAPIV3Schema // {}) | .. | objects |
          select(.["x-kubernetes-int-or-string"] == true));
          ((.allOf // []) | length) == 0 and
          (
            (.anyOf == null) or
            ((.anyOf | sort_by(.type // "")) == [{"type":"integer"},{"type":"string"}])
          )
        )
      )
    )
  ' "${temp_dir}/targets.json" >/dev/null || \
    fail "target CRDs use selectableFields, x-kubernetes-embedded-resource, or a custom IntOrString composition that requires an updated publication verifier before migration"

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
      jq -e '
        .metadata.deletionTimestamp == null and
        all(.status.conditions[]?; .type != "Terminating" or .status != "True")
      ' "$snapshot" >/dev/null || \
        fail "existing CRD ${name} is terminating; refusing migration"
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
      jq -e --slurpfile expected "$expected" '
        def unverified_projection:
          .spec |
          .names |= del(.singular, .shortNames, .categories) |
          .versions |= map(
            del(.schema, .subresources.status) |
            if .subresources == {} then del(.subresources) else . end
          );
        unverified_projection == ($expected[0] | unverified_projection)
      ' "$snapshot" >/dev/null || \
        fail "target CRD ${name} changes conversion, scope, version serving/storage, printer columns, or scale settings that this migration verifier does not yet publish-check"
    elif ! kubectl --context "$kube_context" create \
      --field-manager="$CRD_FIELD_MANAGER" \
      --dry-run=server \
      -f "$target" \
      -o json >"$expected"; then
      fail "server dry-run rejected target CRD ${name}; no CRDs were changed"
    fi
  done

  # Probe the discovery and OpenAPI endpoints before any irreversible CRD
  # mutation. Existing group/versions must already be readable; a missing-CRD
  # install still proves generic discovery and OpenAPI document access.
  local capability_index="${temp_dir}/capability-openapi-index.json"
  local capability_document="${temp_dir}/capability-openapi-document.json"
  local capability_discovery="${temp_dir}/capability-discovery.json"
  local capability_path capability_url capability_group capability_versions capability_version
  kubectl --context "$kube_context" --request-timeout=10s \
    get --raw /openapi/v3 >"$capability_index" || \
    fail "cannot read Kubernetes OpenAPI v3 index required for post-mutation verification"
  capability_url=$(jq -r '.paths["api/v1"].serverRelativeURL // empty' "$capability_index")
  [[ -n "$capability_url" ]] || fail "Kubernetes OpenAPI v3 index has no core/v1 document"
  kubectl --context "$kube_context" --request-timeout=10s \
    get --raw "$capability_url" >"$capability_document" || \
    fail "cannot read Kubernetes OpenAPI v3 documents required for post-mutation verification"
  kubectl --context "$kube_context" --request-timeout=10s \
    get --raw /api/v1 >"$capability_discovery" || \
    fail "cannot read Kubernetes API discovery required for post-mutation verification"
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    snapshot="${temp_dir}/snapshots/${name}.json"
    [[ -s "$snapshot" ]] || continue
    capability_group=$(jq -r '.spec.group' "$snapshot")
    capability_versions=$(jq -r '.spec.versions[] | select(.served == true) | .name' "$snapshot")
    for capability_version in $capability_versions; do
      kubectl --context "$kube_context" --request-timeout=10s \
        get --raw "/apis/${capability_group}/${capability_version}" >"$capability_discovery" || \
        fail "cannot read discovery for existing ${capability_group}/${capability_version}"
      capability_path="apis/${capability_group}/${capability_version}"
      capability_url=$(jq -r --arg path "$capability_path" '.paths[$path].serverRelativeURL // empty' "$capability_index")
      [[ -n "$capability_url" ]] || \
        fail "Kubernetes OpenAPI v3 index has no document for existing ${capability_group}/${capability_version}"
      kubectl --context "$kube_context" --request-timeout=10s \
        get --raw "$capability_url" >"$capability_document" || \
        fail "cannot read OpenAPI for existing ${capability_group}/${capability_version}"
    done
  done
  rm -f "$capability_index" "$capability_document" "$capability_discovery"

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

  # Re-resolve the namespace and both Helm storage backends after the
  # potentially long preflight and confirmation window. Any change means a
  # concurrent Helm operation may be starting, so mutation must not begin.
  local pre_mutation_release_state="${temp_dir}/helm-state-pre-mutation.json"
  capture_helm_storage_state \
    "$kube_context" "$namespace" "$release" \
    "$pre_mutation_release_state" "$temp_dir" pre-mutation
  cmp -s "$initial_release_state" "$pre_mutation_release_state" || \
    fail "Helm release storage or namespace changed during CRD preflight; retry after the Helm operation settles"
  rm -f "$pre_mutation_release_state"

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

  local current
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    current="${temp_dir}/verify-${name}.json"
    expected="${temp_dir}/expected/${name}.json"
    actual="${temp_dir}/actual/${name}.json"
    # `actual` is the persisted API response from the mutation loop above.
    wait_for_crd_target_ready "$kube_context" "$name" "$expected" "$actual" || \
      fail "CRD ${name} did not become ready for its target generation"
    kubectl --context "$kube_context" get customresourcedefinition "$name" -o json >"$current" || \
      fail "could not verify CRD ${name}"
    managed_state_equal "$current" "$expected" || \
      fail "live CRD ${name} does not exactly match the server-normalized target spec and metadata"
  done

  [[ "$(sha256_file "$chart_source")" == "$chart_digest" ]] || \
    fail "target chart archive changed during CRD migration"

  local post_mutation_release_state="${temp_dir}/helm-state-post-mutation.json"
  capture_helm_storage_state \
    "$kube_context" "$namespace" "$release" \
    "$post_mutation_release_state" "$temp_dir" post-mutation
  cmp -s "$initial_release_state" "$post_mutation_release_state" || \
    fail "Helm release storage or namespace changed during CRD migration; recovery artifacts were preserved"
  rm -f "$post_mutation_release_state"

  # Read every CRD again in one tight final pass so changes made while later
  # CRDs were waiting cannot be hidden by earlier successful checks.
  local final_current
  for name in "${EXPECTED_CRD_NAMES[@]}"; do
    final_current="${temp_dir}/final-${name}.json"
    expected="${temp_dir}/expected/${name}.json"
    actual="${temp_dir}/actual/${name}.json"
    kubectl --context "$kube_context" --request-timeout=10s \
      get customresourcedefinition "$name" -o json >"$final_current" || \
      fail "could not perform final verification for CRD ${name}"
    managed_state_equal "$final_current" "$expected" || \
      fail "CRD ${name} changed after readiness verification"
    jq -e \
      --slurpfile mutation "$actual" \
      --arg annotation "$CRD_MIGRATION_ANNOTATION" \
      --arg migration_id "$migration_id" '
        .metadata.uid == $mutation[0].metadata.uid and
        .metadata.generation == $mutation[0].metadata.generation and
        .metadata.deletionTimestamp == null and
        all(.status.conditions[]?; .type != "Terminating" or .status != "True") and
        .metadata.annotations[$annotation] == $migration_id
      ' "$final_current" >/dev/null || \
      fail "CRD ${name} no longer matches this migration response and marker"
  done

  migration_complete=true
  echo "All ${EXPECTED_CRD_COUNT} Orka CRDs exactly match ${resolved_name}-${resolved_version} and are Established in ${kube_context}."
  if [[ "$release_exists" == true ]]; then
    echo "Run helm upgrade with --kube-context ${kube_context} and the same archive: ${chart_source}"
  else
    echo "Run helm install with --skip-crds, --kube-context ${kube_context}, and the same archive: ${chart_source}"
  fi
}

main "$@"
