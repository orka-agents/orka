#!/usr/bin/env bash
# Install Agent Substrate + an Orka-compatible workspace template into a
# dedicated kind cluster for Demo 70.
#
# Substrate's standup is heavy (clone Substrate at a pinned ref, create a kind
# cluster + local registry, deploy the ate-system control plane, build/push the
# controller + codex-worker + workspace-agent images via docker + ko, create a
# WorkerPool + gVisor ActorTemplate, deploy Orka wired with --substrate-* flags,
# and a local RustFS snapshot bucket). That exact sequence is already proven in
# CI by scripts/agent-substrate-e2e.sh.
#
# Rather than duplicate ~600 lines (and drift from CI), this installer is a thin
# wrapper over that script with KEEP_CLUSTER=1, so it leaves behind a fully
# wired cluster Demo 70 can drive. The e2e's own task exercises run as a
# built-in smoke test (a few seconds each) before it hands the cluster back.
#
# Unlike install-kontxt.sh / install-agent-sandbox.sh (which attach to the
# shared demo-magic kind cluster), Substrate needs its OWN cluster: it uses
# Substrate's create-kind-cluster.sh (custom registry + gVisor node config),
# so it cannot bolt onto an existing cluster.
#
# Requires: kind, ko, docker, go, git, jq, kubectl, curl. The cluster is named
# by KIND_CLUSTER (default orka-agent-substrate-e2e); its context is
# kind-<KIND_CLUSTER>. Tear down with: kind delete cluster --name <KIND_CLUSTER>
#
# Secret-free: the only secret is a bootstrap token the e2e generates itself.

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Pin to the same Substrate revision CI uses, unless overridden.
SUBSTRATE_REF="${SUBSTRATE_REF:-b80031d260959b1fc5c6f61e3099fe2a6d368af1}"
KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
# Exercise the retained-workspace path during standup so the warm-reuse beat is
# smoke-tested before Demo 70 runs. Override to 0 to skip.
SUBSTRATE_E2E_EXTENDED="${SUBSTRATE_E2E_EXTENDED:-1}"

# Put the Go bin dir on PATH so a `go install`ed ko is found.
if command -v go >/dev/null 2>&1; then
  PATH="$(go env GOPATH)/bin:${PATH}"
  export PATH
fi

for cmd in kind ko docker go git jq kubectl curl; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    if [[ "${cmd}" == "ko" ]]; then
      die "missing required command: ko — install with: go install github.com/google/ko@v0.18.1"
    fi
    die "missing required command: ${cmd}"
  fi
done

docker info >/dev/null 2>&1 || die "docker daemon is not reachable — start Docker and retry"

e2e_script="${repo_root}/scripts/agent-substrate-e2e.sh"
[[ -f "${e2e_script}" ]] || die "expected ${e2e_script} to exist"

log "Standing up Agent Substrate (ref ${SUBSTRATE_REF}, cluster kind-${KIND_CLUSTER})"
log "This builds 4 images and the Substrate control plane — first run takes several minutes."

KEEP_CLUSTER=1 \
  SUBSTRATE_REF="${SUBSTRATE_REF}" \
  KIND_CLUSTER="${KIND_CLUSTER}" \
  SUBSTRATE_E2E_EXTENDED="${SUBSTRATE_E2E_EXTENDED}" \
  bash "${e2e_script}"

log "Agent Substrate installed. Demo 70 can run against context kind-${KIND_CLUSTER}:"
log "  kubectl config use-context kind-${KIND_CLUSTER}"
log "  DEMO_SUBSTRATE_NAMESPACE=default ./hack/demos/70-agent-substrate.sh"
log "Tear down with: kind delete cluster --name ${KIND_CLUSTER}"
