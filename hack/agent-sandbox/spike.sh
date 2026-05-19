#!/usr/bin/env bash
set -euo pipefail

AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v0.4.6}"
NAMESPACE="${NAMESPACE:-orka-agent-sandbox-spike}"
TEMPLATE_NAME="${TEMPLATE_NAME:-orka-spike-template}"
DELETE_CLAIM="${DELETE_CLAIM:-orka-spike-delete}"
RETAIN_CLAIM="${RETAIN_CLAIM:-orka-spike-retain}"
INSTALL_AGENT_SANDBOX="${INSTALL_AGENT_SANDBOX:-1}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-180s}"
CLEANUP="${CLEANUP:-0}"

if [[ "${AGENT_SANDBOX_VERSION}" != "v0.4.6" ]]; then
  echo "error: this spike is pinned to agent-sandbox v0.4.6" >&2
  exit 1
fi

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: required command not found: $1" >&2
    exit 1
  fi
}

wait_for_crds() {
  kubectl wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout="${WAIT_TIMEOUT}"
  kubectl wait --for=condition=Established crd/sandboxclaims.extensions.agents.x-k8s.io --timeout="${WAIT_TIMEOUT}"
  kubectl wait --for=condition=Established crd/sandboxtemplates.extensions.agents.x-k8s.io --timeout="${WAIT_TIMEOUT}"
  kubectl wait --for=condition=Established crd/sandboxwarmpools.extensions.agents.x-k8s.io --timeout="${WAIT_TIMEOUT}"
}

wait_for_agent_sandbox_deployments() {
  if ! kubectl get namespace agent-sandbox-system >/dev/null 2>&1; then
    return
  fi

  deployments="$(kubectl -n agent-sandbox-system get deployment -o name 2>/dev/null || true)"
  for deployment in ${deployments}; do
    kubectl -n agent-sandbox-system rollout status "${deployment}" --timeout="${WAIT_TIMEOUT}"
  done
}

reset_claim() {
  local claim="$1"
  local sandbox=""

  sandbox="$(kubectl -n "${NAMESPACE}" get sandboxclaim "${claim}" \
    -o jsonpath='{.status.sandbox.name}' 2>/dev/null || true)"

  kubectl -n "${NAMESPACE}" delete sandboxclaim "${claim}" \
    --ignore-not-found --wait=true --timeout="${WAIT_TIMEOUT}" || true

  if [[ -n "${sandbox}" ]]; then
    kubectl -n "${NAMESPACE}" delete sandbox "${sandbox}" \
      --ignore-not-found --wait=true --timeout="${WAIT_TIMEOUT}" || true
  fi
}

wait_for_claim_sandbox() {
  local claim="$1"
  local sandbox=""

  for _ in $(seq 1 120); do
    sandbox="$(kubectl -n "${NAMESPACE}" get sandboxclaim "${claim}" \
      -o jsonpath='{.status.sandbox.name}' 2>/dev/null || true)"

    if [[ -z "${sandbox}" ]] && kubectl -n "${NAMESPACE}" get sandbox "${claim}" >/dev/null 2>&1; then
      sandbox="${claim}"
    fi

    if [[ -n "${sandbox}" ]]; then
      printf '%s\n' "${sandbox}"
      return 0
    fi

    sleep 2
  done

  echo "error: timed out waiting for SandboxClaim/${claim} to report a sandbox" >&2
  kubectl -n "${NAMESPACE}" get sandboxclaim "${claim}" -o yaml >&2 || true
  return 1
}

wait_for_sandbox_pod() {
  local sandbox="$1"
  local selector=""
  local pod=""

  for _ in $(seq 1 120); do
    selector="$(kubectl -n "${NAMESPACE}" get sandbox "${sandbox}" \
      -o jsonpath='{.status.selector}' 2>/dev/null || true)"

    if [[ -n "${selector}" ]]; then
      pod="$(kubectl -n "${NAMESPACE}" get pod -l "${selector}" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    fi

    if [[ -z "${pod}" ]] && kubectl -n "${NAMESPACE}" get pod "${sandbox}" >/dev/null 2>&1; then
      pod="${sandbox}"
    fi

    if [[ -n "${pod}" ]]; then
      kubectl -n "${NAMESPACE}" wait "pod/${pod}" --for=condition=Ready --timeout="${WAIT_TIMEOUT}" >&2
      printf '%s\n' "${pod}"
      return 0
    fi

    sleep 2
  done

  echo "error: timed out waiting for pod for Sandbox/${sandbox}" >&2
  kubectl -n "${NAMESPACE}" get sandbox "${sandbox}" -o yaml >&2 || true
  kubectl -n "${NAMESPACE}" get pods -o wide >&2 || true
  return 1
}

apply_claim() {
  local name="$1"
  local policy="$2"

  cat <<EOF2 | kubectl apply -f -
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
spec:
  sandboxTemplateRef:
    name: ${TEMPLATE_NAME}
  lifecycle:
    shutdownPolicy: ${policy}
EOF2
}

exec_in_pod() {
  local pod="$1"
  shift
  kubectl -n "${NAMESPACE}" exec "pod/${pod}" -c agent -- "$@"
}

patch_shutdown_now() {
  local claim="$1"
  local shutdown_time

  shutdown_time="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  kubectl -n "${NAMESPACE}" patch sandboxclaim "${claim}" --type=merge \
    -p "{\"spec\":{\"lifecycle\":{\"shutdownTime\":\"${shutdown_time}\"}}}"
}

cleanup_namespace() {
  if [[ "${CLEANUP}" == "1" ]]; then
    kubectl delete namespace "${NAMESPACE}" --wait=false
  fi
}

require kubectl

trap cleanup_namespace EXIT

if [[ "${INSTALL_AGENT_SANDBOX}" == "1" ]]; then
  echo "+ kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/manifest.yaml"
  kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/manifest.yaml"

  echo "+ kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/extensions.yaml"
  kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/extensions.yaml"
fi

wait_for_crds
wait_for_agent_sandbox_deployments

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
reset_claim "${DELETE_CLAIM}"
reset_claim "${RETAIN_CLAIM}"

cat <<EOF2 | kubectl apply -f -
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: ${TEMPLATE_NAME}
  namespace: ${NAMESPACE}
spec:
  podTemplate:
    spec:
      securityContext:
        runAsUser: 1000
        runAsGroup: 3000
        fsGroup: 2000
        runAsNonRoot: true
      containers:
      - name: agent
        image: busybox:1.36
        command: ["/bin/sh", "-c", "sleep 36000"]
        workingDir: /workspace
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
        volumeMounts:
        - name: workspace
          mountPath: /workspace
  volumeClaimTemplates:
  - metadata:
      name: workspace
    spec:
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 1Gi
EOF2

echo "== Delete-policy sandbox claim =="
apply_claim "${DELETE_CLAIM}" "Delete"
delete_sandbox="$(wait_for_claim_sandbox "${DELETE_CLAIM}")"
delete_pod="$(wait_for_sandbox_pod "${delete_sandbox}")"

exec_in_pod "${delete_pod}" /bin/sh -c 'echo ok'
exec_in_pod "${delete_pod}" /bin/sh -c 'printf "delete-policy workspace\n" > /workspace/orka-spike-delete.txt && cat /workspace/orka-spike-delete.txt'

patch_shutdown_now "${DELETE_CLAIM}"

if ! kubectl -n "${NAMESPACE}" wait --for=delete "sandboxclaim/${DELETE_CLAIM}" --timeout="${WAIT_TIMEOUT}"; then
  echo "warning: SandboxClaim/${DELETE_CLAIM} was not deleted within ${WAIT_TIMEOUT}; inspect lifecycle behavior manually" >&2
fi

if ! kubectl -n "${NAMESPACE}" wait --for=delete "sandbox/${delete_sandbox}" --timeout="${WAIT_TIMEOUT}"; then
  echo "warning: Sandbox/${delete_sandbox} was not deleted within ${WAIT_TIMEOUT}; inspect lifecycle behavior manually" >&2
fi

echo "== Retain-policy sandbox claim =="
apply_claim "${RETAIN_CLAIM}" "Retain"
retain_sandbox="$(wait_for_claim_sandbox "${RETAIN_CLAIM}")"
retain_pod="$(wait_for_sandbox_pod "${retain_sandbox}")"

exec_in_pod "${retain_pod}" /bin/sh -c 'echo ok'
exec_in_pod "${retain_pod}" /bin/sh -c 'printf "retain-policy workspace\n" > /workspace/orka-spike-retain.txt && cat /workspace/orka-spike-retain.txt'

patch_shutdown_now "${RETAIN_CLAIM}"

if ! kubectl -n "${NAMESPACE}" wait --for=delete "sandbox/${retain_sandbox}" --timeout="${WAIT_TIMEOUT}"; then
  echo "warning: Sandbox/${retain_sandbox} was not deleted within ${WAIT_TIMEOUT}; inspect retained claim/status manually" >&2
fi

echo "+ retained SandboxClaim/${RETAIN_CLAIM}"
kubectl -n "${NAMESPACE}" get sandboxclaim "${RETAIN_CLAIM}" -o yaml || true

echo "+ kubectl get sandbox,sandboxclaim -A"
kubectl get sandbox,sandboxclaim -A || true

echo "+ kubectl -n ${NAMESPACE} get sandbox,sandboxclaim,pod,pvc"
kubectl -n "${NAMESPACE}" get sandbox,sandboxclaim,pod,pvc || true

if [[ "${CLEANUP}" != "1" ]]; then
  cat <<EOF2

Spike resources were left for inspection.

Cleanup:
  kubectl delete namespace ${NAMESPACE}
EOF2
fi
