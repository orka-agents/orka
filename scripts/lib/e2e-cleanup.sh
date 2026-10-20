#!/usr/bin/env bash

# The caller gives each attempt a fresh directory containing only allowlisted
# cleanup JSON. Never copy kubeconfigs, credentials, command output or logs here.
e2e_cleanup_evidence_passed() {
  local evidence_dir="$1"
  local reports=("${evidence_dir}"/cleanup-*.json)
  [[ -f "${evidence_dir}/suite-cleanup.json" && -f "${reports[0]}" ]] || return 1
  jq -e '.schemaVersion == 1 and .passed == true and .stage == "complete"' \
    "${evidence_dir}/suite-cleanup.json" >/dev/null 2>&1 || return 1
  jq -s -e '
    length > 0 and all(.[];
      .schemaVersion == 1 and .passed == true and .stage == "complete" and
      all((.tasks // [])[];
        .uid != "" and .deleteRequested == true and .productFinalizersReleased == true and
        .observerReleased == true and .absent == true and
        (.receiptRequired != true or (.receiptVerified == true and (.runtimeSessionCleanupDigest | startswith("sha256:"))))) and
      all((.sessions // [])[]; .absent == true))
  ' "${reports[@]}" >/dev/null 2>&1
}

e2e_cleanup_kind() {
  local cluster="$1" evidence_dir="$2"
  local kind_command="${KIND:-kind}"
  local delete_status=0 list_status=0 absent=false clusters="" item
  [[ "${cluster}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || return 1
  mkdir -p "${evidence_dir}" || return 1
  if "${kind_command}" delete cluster --name "${cluster}" >/dev/null 2>&1; then
    :
  else
    delete_status=$?
  fi
  if clusters="$("${kind_command}" get clusters 2>/dev/null)"; then
    absent=true
    while IFS= read -r item; do
      if [[ "${item}" == "${cluster}" ]]; then
        absent=false
      fi
    done <<<"${clusters}"
  else
    list_status=$?
  fi
  jq -n --arg cluster "${cluster}" --argjson deleteStatus "${delete_status}" \
    --argjson listStatus "${list_status}" --argjson absent "${absent}" \
    '{schemaVersion:1,kindCluster:$cluster,deleteExitCode:$deleteStatus,listExitCode:$listStatus,
      absent:$absent,passed:($deleteStatus == 0 and $listStatus == 0 and $absent)}' \
    >"${evidence_dir}/kind-cleanup.json" || return 1
  [[ "${delete_status}" -eq 0 && "${list_status}" -eq 0 && "${absent}" == true ]]
}
