# Harness v1 compatibility wrapper

This Kustomize base is an opt-in compatibility data plane. It is intentionally
absent from `config/default` and `config/acp-production`; enabling it does not
change the durable `AgentExecutionControl` backend mode or create a v1 fallback.

Before applying the base:

1. Replace the all-zero wrapper image digest through an operator-controlled
   overlay with the exact reviewed `repository@sha256:<digest>` reference.
2. Create the dedicated bearer Secret in `orka-system` without printing its
   value:

   ```bash
   kubectl create namespace orka-system --dry-run=client -o yaml | kubectl apply -f -
   if ! kubectl -n orka-system get secret harness-wrapper-auth >/dev/null 2>&1; then
     openssl rand -hex 32 | \
       kubectl -n orka-system create secret generic harness-wrapper-auth \
         --from-file=token=/dev/stdin
   fi
   ```

The wrapper has a dedicated ServiceAccount with token automount disabled, a
PVC-backed admission ledger, controller-only ingress, and egress limited to DNS
and public HTTPS provider/read-only SCM endpoints. Do not mount Git, forge,
publisher, provider-proxy, or other publication credentials into this Pod.
