#!/usr/bin/env bash
# Prepare a gVisor kind cluster with the official Substrate pin for the
# workspace provider E2E. The Ginkgo suite then deploys Orka, aimock, and the
# out-of-tree Substrate provider into it:
#   bash scripts/workspace-substrate-cluster.sh
#   E2E_WORKSPACE_PROVIDER=substrate E2E_GINKGO_LABEL_FILTER=workspace-provider make test-e2e
# It prints the environment the suite needs (KUBECONFIG, kind, kubectl-ate).
set -Eeuo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:?set KIND_CLUSTER}"
RUN_DIR="${SUBSTRATE_E2E_RUN_DIR:-${ROOT_DIR}/bin/substrate-e2e-${KIND_CLUSTER}}"
source "${ROOT_DIR}/scripts/lib/substrate-upstream.sh"

substrate_prepare_upstream "${ROOT_DIR}" "${RUN_DIR}" "${KIND_CLUSTER}"
(cd "${SUBSTRATE_DIR}" && go build -o "${RUN_DIR}/kubectl-ate" ./cmd/kubectl-ate)
# Substrate's node image needs the kind release that created it to load images.
kind_bin="$(cd "${SUBSTRATE_DIR}" && hack/run-tool.sh --print-bin-path kind)"
kubectl_ate() { "${RUN_DIR}/kubectl-ate" --context "kind-${KIND_CLUSTER}" "$@"; }

worker="$(cd "${SUBSTRATE_DIR}" && ko build --platform="linux/$(go env GOARCH)" ./cmd/ateom-gvisor)"
registry_ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' kind-registry)"
[[ -n "${registry_ip}" ]] || { echo 'kind-registry is not on the kind network' >&2; exit 1; }

kubectl create namespace ate-demo --dry-run=client -o yaml | kubectl apply -f -
kubectl_ate get atespace orka-system >/dev/null 2>&1 || kubectl_ate create atespace orka-system
kubectl -n ate-demo apply -f - <<YAML
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: orka-native
  labels: {orka.ai/native-pool: conformance}
spec:
  replicas: 2
  workerImage: ${worker}
  template:
    resources:
      requests: {cpu: 250m, memory: 512Mi}
      limits: {cpu: "2", memory: 2Gi}
YAML
# The provider derives each Actor's template from this base and replaces its
# container with Core's runtime, so the base image never runs.
jq -n --arg image "${worker/localhost:${KIND_REGISTRY_PORT}/${registry_ip}:5000}" '
  {metadata:{atespace:"orka-system",name:"orka-acp-infra"},workerSelector:{matchLabels:{"orka.ai/native-pool":"conformance"}},
   containers:[{name:"server",image:$image,readyz:{httpGet:{path:"/healthz",port:80}}}],
   resources:{limits:[{name:"cpu",quantity:"1"},{name:"memory",quantity:"1Gi"}]},
   snapshotsConfig:{storageLocation:"s3://ate-snapshots/orka-workspace-e2e/",onPause:"SNAPSHOT_CONTENT_SCOPE_DATA",
     onCommit:"SNAPSHOT_CONTENT_SCOPE_DATA",onResume:{fromData:"RESUME_SOURCE_COLD_BOOT"}},
   sandboxConfig:{sandboxClass:"SANDBOX_CLASS_GVISOR",configName:"gvisor-default"}}' >"${RUN_DIR}/orka-acp-infra.json"
kubectl_ate get actor-template orka-acp-infra --atespace orka-system >/dev/null 2>&1 ||
  kubectl_ate create actor-template -f "${RUN_DIR}/orka-acp-infra.json"
# This template selects its own WorkerPool, whose workers can never start, so
# its Actors never boot (boot time limit check).
kubectl -n ate-demo apply -f - <<YAML
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: orka-unschedulable
  labels: {orka.ai/native-pool: unschedulable}
spec:
  replicas: 1
  workerImage: registry.invalid/orka/never-starts:e2e
  template:
    resources:
      requests: {cpu: 250m, memory: 512Mi}
      limits: {cpu: "2", memory: 2Gi}
YAML
jq '.metadata.name = "orka-acp-unschedulable" | .workerSelector.matchLabels["orka.ai/native-pool"] = "unschedulable"' \
  "${RUN_DIR}/orka-acp-infra.json" >"${RUN_DIR}/orka-acp-unschedulable.json"
kubectl_ate get actor-template orka-acp-unschedulable --atespace orka-system >/dev/null 2>&1 ||
  kubectl_ate create actor-template -f "${RUN_DIR}/orka-acp-unschedulable.json"

# Actor routes carry long-lived prompt streams; the router's default timeout
# cuts them off.
if ! kubectl -n ate-system get deployment atenet-router -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q -- '--route-timeout='; then
  kubectl -n ate-system patch deployment atenet-router --type=json \
    -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--route-timeout=30m"}]'
fi
kubectl -n ate-system rollout status deployment/atenet-router --timeout=3m

# Core reads worker Pod identity in the provider's worker namespace.
kubectl -n ate-demo apply -f - <<'YAML'
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: orka-external-runtime-reader}
rules:
  - {apiGroups: [""], resources: [pods], verbs: [get, list, watch]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: orka-external-runtime-reader}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: orka-external-runtime-reader}
subjects:
  - {kind: ServiceAccount, name: orka-controller-manager, namespace: orka-system}
YAML

# Core and the provider dial Actors at <actor>.<atespace>.actors.resources.substrate.ate.dev.
dns_ip="$(kubectl -n ate-system get service dns -o jsonpath='{.spec.clusterIP}')"
zone=actors.resources.substrate.ate.dev
corefile="$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')"
if [[ "${corefile}" != *"${zone}"* ]]; then
  printf '%s\n%s:53 {\n    errors\n    cache 30\n    forward . %s\n}\n' "${corefile}" "${zone}" "${dns_ip}" >"${RUN_DIR}/Corefile"
  kubectl -n kube-system create configmap coredns --from-file=Corefile="${RUN_DIR}/Corefile" --dry-run=client -o yaml |
    kubectl apply -f -
  kubectl -n kube-system rollout restart deployment/coredns
fi
kubectl -n kube-system rollout status deployment/coredns --timeout=2m
kubectl -n ate-demo wait --for=jsonpath='{.status.readyReplicas}'=2 workerpool/orka-native --timeout=5m

printf 'KUBECONFIG=%s\nKIND=%s\nE2E_SUBSTRATE_KUBECTL_ATE=%s\nE2E_KIND_REGISTRY=kind-registry\nE2E_RUNTIME_REGISTRY_HOST=%s:5000\n' \
  "${RUN_DIR}/kubeconfig" "${kind_bin}" "${RUN_DIR}/kubectl-ate" "${registry_ip}" >"${RUN_DIR}/e2e.env"
cat "${RUN_DIR}/e2e.env"
