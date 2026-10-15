#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

hyperlight_usage() {
  cat <<'USAGE'
Usage: scripts/hyperlight-kind-e2e.sh

Run the Hyperlight deployment E2E with aimock on a Linux KVM host (SSH vm
for local runs). Builds the pinned upstream device plugin, creates a fresh
repo-scoped kindctl cluster, then lets the Go suite build/deploy/clean Orka.
No live provider credentials or test/e2e/.env are permitted.

Environment:
  KIND                          absolute kind binary (defaults to the vm kind
                                path when installed, otherwise kind on PATH)
  E2E_HYPERLIGHT_KIND_TAG        kindctl tag (default: hyperlight-aimock)
  E2E_HYPERLIGHT_ARTIFACT_DIR    parent for fresh cleanup/diagnostic directories
                                (default: bin/hyperlight-kind-e2e)
  E2E_HYPERLIGHT_BUNDLE_IMAGE    optional suite build image override
                                (default: ghcr.io/orka-agents/orka/hyperlight-bundle:e2e)

The wrapper sets E2E_HYPERLIGHT=1, E2E_DISABLE_MOCK_LLM=0,
E2E_ACP_IDLE_POOL_TTL=5m, E2E_HYPERLIGHT_DEVICE_GID from /dev/kvm,
E2E_CLEANUP_REPORT_DIR, KIND_CLUSTER, and a scoped KUBECONFIG.
Exit 90 means normal resource cleanup or cluster teardown was not proved.
Existing clusters are never adopted or deleted. No keep-cluster mode.
USAGE
}

hyperlight_die() { printf 'hyperlight-e2e: %s\n' "$*" >&2; exit 1; }
hyperlight_log() { printf 'hyperlight-e2e: %s\n' "$*"; }

hyperlight_device_gid_valid() {
  [[ "$1" =~ ^[1-9][0-9]*$ && ${#1} -le 10 && "$1" -le 2147483647 ]]
}

hyperlight_require_host() {
  [[ "$(uname -s)" == Linux ]] || hyperlight_die 'requires Linux; use SSH vm for local execution'
  [[ -c /dev/kvm ]] || hyperlight_die '/dev/kvm must be a character device; refusing to skip KVM tests'
  E2E_HYPERLIGHT_DEVICE_GID="$(stat -c '%g' /dev/kvm)"
  hyperlight_device_gid_valid "${E2E_HYPERLIGHT_DEVICE_GID}" ||
    hyperlight_die '/dev/kvm GID must be positive and fit MaxInt32'
  export E2E_HYPERLIGHT_DEVICE_GID
}

hyperlight_require_aimock() {
  local key
  [[ ! -e "${repo_root}/test/e2e/.env" ]] || hyperlight_die 'test/e2e/.env is not permitted in this credential-free run'
  for key in E2E_OPENAI_API_KEY E2E_ANTHROPIC_API_KEY E2E_GITHUB_TOKEN \
    OPENAI_API_KEY ANTHROPIC_API_KEY COPILOT_GITHUB_TOKEN GH_TOKEN GITHUB_TOKEN \
    E2E_LIVE_COPILOT_PROXY_BASE_URL E2E_COPILOT_PROXY_BASE_URL; do
    [[ -z "${!key:-}" ]] || hyperlight_die "${key} must be unset for aimock (value withheld)"
  done
}

hyperlight_kubectl() { "${kindctl}" kubectl --tag "${kind_tag}" "$@"; }

# Adapt the existing cleanup proof helper to kindctl's metadata-aware deletion.
# A failed kind create can leave a node before kindctl registers ownership;
# that fallback still targets only this attempt's exact name and kubeconfig.
hyperlight_cleanup_kind_command() {
  if [[ "$*" == "delete cluster --name ${KIND_CLUSTER}" ]]; then
    if kindctl_registry_is_owned "${KIND_CLUSTER}"; then
      "${kindctl}" delete --tag "${kind_tag}"
    else
      "${kind_binary}" delete cluster --name "${KIND_CLUSTER}" --kubeconfig "${KUBECONFIG}"
    fi
  else
    "${kind_binary}" "$@"
  fi
}

hyperlight_diagnostics() {
  # Projections only: no logs, events, specs, Secrets, env, or kubeconfigs.
  hyperlight_kubectl get nodes -o json --request-timeout=15s 2>/dev/null |
    jq '{nodes:[.items[] | {name:.metadata.name,
      hypervisors:.status.allocatable["hyperlight.dev/hypervisor"],
      conditions:[.status.conditions[]? | {type,status}]}]}' \
    >"${artifact_dir}/diagnostics/nodes.json" || true
  hyperlight_kubectl get pods -A -o json --request-timeout=15s 2>/dev/null |
    jq '{pods:[.items[] | {namespace:.metadata.namespace,name:.metadata.name,
      phase:.status.phase,containers:[.status.containerStatuses[]? | {name,ready,restartCount}]}]}' \
    >"${artifact_dir}/diagnostics/pods.json" || true
}

hyperlight_on_exit() {
  local status="$1"
  trap - EXIT INT TERM
  if [[ "${suite_started}" == 1 ]] && ! e2e_cleanup_evidence_passed "${cleanup_dir}"; then
    hyperlight_log 'normal resource cleanup was not proved; cluster deletion is not a substitute'
    status=90
  fi
  if [[ "${cluster_attempted}" == 1 ]]; then
    hyperlight_diagnostics
    if ! KIND=hyperlight_cleanup_kind_command e2e_cleanup_kind "${KIND_CLUSTER}" "${cleanup_dir}"; then
      hyperlight_log 'exact cluster teardown was not proved'
      status=90
    else
      # Also remove an unregistered partial-create kubeconfig, after absence proof.
      rm -f -- "${KUBECONFIG}"
    fi
  fi
  rm -rf -- "${work_dir}"
  hyperlight_log "cleanup evidence: ${cleanup_dir}"
  exit "${status}"
}

hyperlight_install_plugin() {
  local revision=fc71b4501d23977fcc54f7be144d884fc8210667
  local image="orka-hyperlight-device-plugin:${revision}"
  mkdir -p "${work_dir}/source"
  curl --fail --silent --show-error --location --retry 3 \
    "https://codeload.github.com/hyperlight-dev/hyperlight-on-kubernetes/tar.gz/${revision}" \
    -o "${work_dir}/source.tar.gz"
  tar -xzf "${work_dir}/source.tar.gz" --strip-components=1 -C "${work_dir}/source"
  # Build the real upstream implementation; do not substitute a fake allocator.
  docker build --tag "${image}" "${work_dir}/source/device-plugin"
  "${kindctl}" load --tag "${kind_tag}" "${image}"
  python3 - "${work_dir}/source/deploy/manifests/device-plugin.yaml" \
    "${work_dir}/device-plugin.yaml" "${image}" "${E2E_HYPERLIGHT_DEVICE_GID}" <<'PY'
import pathlib, sys
source, target, image, gid = sys.argv[1:]
manifest = pathlib.Path(source).read_text()
for key, value in {"IMAGE": image, "DEVICE_COUNT": "16", "DEVICE_UID": "0", "DEVICE_GID": gid}.items():
    placeholder = "${" + key + "}"
    if placeholder not in manifest:
        raise SystemExit("pinned upstream manifest is missing a required placeholder")
    manifest = manifest.replace(placeholder, value)
if "${" in manifest or manifest.count("imagePullPolicy: Always") != 1:
    raise SystemExit("unexpected pinned upstream manifest structure")
# The locally built image is preloaded on every node; never pull a mutable tag.
# The read-only host /dev mount prevents runc from creating kubelet's
# default /dev/termination-log mountpoint. Keep /dev read-only and move only
# the termination-message mount outside that hostPath.
manifest = manifest.replace("imagePullPolicy: Always",
    "imagePullPolicy: Never\n          terminationMessagePath: /tmp/hyperlight-termination.log")
pathlib.Path(target).write_text(manifest)
PY
  hyperlight_kubectl label nodes --all hyperlight.dev/enabled=true --overwrite
  hyperlight_kubectl apply -f "${work_dir}/device-plugin.yaml"
  hyperlight_kubectl rollout status daemonset/hyperlight-device-plugin \
    -n hyperlight-system --timeout=180s
  local attempt
  for ((attempt=0; attempt<60; attempt++)); do
    if hyperlight_kubectl get nodes -l hyperlight.dev/enabled=true -o json --request-timeout=10s |
      jq -e '.items | length > 0 and all(.[];
        (.status.allocatable["hyperlight.dev/hypervisor"] // "0" | tonumber) > 0)' >/dev/null; then
      hyperlight_log 'real device plugin ready; hyperlight.dev/hypervisor allocatable is positive'
      return 0
    fi
    sleep 2
  done
  hyperlight_die 'device plugin did not advertise positive hyperlight.dev/hypervisor allocatable'
}

hyperlight_main() {
  if [[ $# == 1 && ( "$1" == --help || "$1" == -h ) ]]; then hyperlight_usage; return; fi
  [[ $# == 0 ]] || { hyperlight_usage >&2; return 2; }
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  repo_root="$(cd "${script_dir}/.." && pwd)"
  cd "${repo_root}"
  hyperlight_require_host
  hyperlight_require_aimock
  local command
  for command in curl tar docker kubectl python3 jq go flock; do
    command -v "${command}" >/dev/null || hyperlight_die "missing required command: ${command}"
  done
  kind_binary="${KIND:-}"
  if [[ -z "${kind_binary}" ]]; then
    if [[ -x /home/linuxbrew/.linuxbrew/bin/kind ]]; then
      kind_binary=/home/linuxbrew/.linuxbrew/bin/kind
    else
      kind_binary="$(command -v kind)" || hyperlight_die 'kind is required'
    fi
  fi
  [[ "${kind_binary}" == /* && -x "${kind_binary}" && "${kind_binary##*/}" == kind ]] ||
    hyperlight_die 'KIND must be an absolute executable path named kind'
  export KIND="${kind_binary}"
  PATH="$(dirname "${kind_binary}"):${PATH}"
  export PATH
  kindctl="${repo_root}/.agents/skills/kindctl/bin/kindctl"
  # Reuse kindctl's name/path derivation even when creation fails before registration.
  # shellcheck source=.agents/skills/kindctl/bin/kindctl
  . "${kindctl}"
  # shellcheck source=scripts/lib/e2e-cleanup.sh
  . "${script_dir}/lib/e2e-cleanup.sh"
  kind_tag="${E2E_HYPERLIGHT_KIND_TAG:-hyperlight-aimock}"
  kindctl_derive "${kind_tag}"
  export KIND_CLUSTER="${KINDCTL_NAME}" KUBECONFIG="${KINDCTL_KUBECONFIG}"
  # Two runs of one tag could both pass the check below, and the one whose
  # create then failed would tear down the other's cluster. Hold the tag from
  # the check until teardown ends; the lock goes with the process.
  kindctl_ensure_store_dir
  exec {run_lock}>>"${KINDCTL_STORE_DIR}/${KIND_CLUSTER}.e2e.lock"
  flock -n "${run_lock}" ||
    hyperlight_die 'another run holds this tag; choose a fresh tag (nothing deleted)'
  local clusters
  clusters="$("${kind_binary}" get clusters)"
  if grep -Fxq -- "${KIND_CLUSTER}" <<<"${clusters}" || kindctl_registry_is_owned "${KIND_CLUSTER}"; then
    hyperlight_die 'tag already has a cluster or kindctl metadata; choose a fresh tag (nothing deleted)'
  fi
  local artifact_parent="${E2E_HYPERLIGHT_ARTIFACT_DIR:-${repo_root}/bin/hyperlight-kind-e2e}"
  mkdir -p "${artifact_parent}"
  artifact_parent="$(cd "${artifact_parent}" && pwd)"
  artifact_dir="$(mktemp -d "${artifact_parent}/run.XXXXXX")"
  cleanup_dir="${artifact_dir}/cleanup"
  mkdir -p "${cleanup_dir}" "${artifact_dir}/diagnostics"
  work_dir="$(mktemp -d)"
  cluster_attempted=0
  suite_started=0
  trap 'hyperlight_on_exit "$?"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  export E2E_HYPERLIGHT=1 E2E_DISABLE_MOCK_LLM=0 E2E_ACP_IDLE_POOL_TTL=5m
  export E2E_CLEANUP_REPORT_DIR="${cleanup_dir}"
  hyperlight_log "creating ${KIND_CLUSTER}; KVM GID ${E2E_HYPERLIGHT_DEVICE_GID}"
  cluster_attempted=1
  "${kindctl}" create --tag "${kind_tag}" --config "${repo_root}/test/e2e/hyperlight-kind-config.yaml"
  hyperlight_kubectl wait --for=condition=Ready nodes --all --timeout=180s
  hyperlight_install_plugin
  suite_started=1
  # BeforeSuite owns all platform/bundle builds and the kind-local registry;
  # AfterSuite owns normal resource cleanup. Never call make test-e2e here.
  "${kindctl}" exec --tag "${kind_tag}" -- \
    go test -tags=e2e ./test/e2e/ -timeout 40m -v -ginkgo.v -ginkgo.label-filter=hyperlight
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then hyperlight_main "$@"; fi
