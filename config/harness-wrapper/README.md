# Harness wrapper authentication Secret

The canonical Kustomize installer intentionally does **not** commit or generate
the shared bearer token. Before applying `deploy/orka.yaml` directly, create the
required Secret in `orka-system` without printing the token:

```bash
set -euo pipefail

kubectl create namespace orka-system --dry-run=client -o yaml | kubectl apply -f -
openssl rand -hex 32 | \
  kubectl -n orka-system create secret generic harness-wrapper-auth \
    --from-file=token=/dev/stdin \
    --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f deploy/orka.yaml
```

`make deploy` performs the same preflight and creates the Secret only when it is
absent. Helm installs use the chart-managed Secret or
`workers.harnessWrapper.auth.existingSecret` instead.
