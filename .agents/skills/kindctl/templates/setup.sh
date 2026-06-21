#!/usr/bin/env bash
set -euo pipefail

# Runs after kindctl creates the repo-scoped kind cluster.
# KUBECONFIG is already scoped to ~/.kube/kind/<cluster>.kubeconfig.
# Available variables:
#   KINDCTL_CLUSTER
#   KINDCTL_CONTEXT
#   KINDCTL_ROOT

kubectl get nodes >/dev/null

# Examples to copy into a repo-specific setup hook:
# kubectl apply -f config/crd/bases
# kubectl apply -k config/default
# kind load docker-image myapp:dev --name "$KINDCTL_CLUSTER"
