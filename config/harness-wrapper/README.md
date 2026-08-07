# Harness v1 compatibility wrapper

This Kustomize base is an opt-in compatibility data plane. It is intentionally
absent from `config/default` and `config/acp-production`; enabling it does not
change the durable `AgentExecutionControl` backend mode or create a v1 fallback.

Before applying the base:

1. Replace the all-zero wrapper image digest through an operator-controlled
   overlay with the exact reviewed `repository@sha256:<digest>` reference.
2. Create the dedicated bearer and TLS Secret in `orka-system` without printing
   the bearer value. The serving certificate must authenticate
   `agent-harness-wrapper.orka-system.svc`:

   ```bash
   kubectl create namespace orka-system --dry-run=client -o yaml | kubectl apply -f -
   if ! kubectl -n orka-system get secret harness-wrapper-auth >/dev/null 2>&1; then
     openssl rand -hex 32 | \
       kubectl -n orka-system create secret generic harness-wrapper-auth \
         --from-file=token=/dev/stdin \
         --from-file=tls.crt=/path/to/tls.crt \
         --from-file=tls.key=/path/to/tls.key \
         --from-file=ca.crt=/path/to/ca.crt
   fi
   ```

The wrapper has a dedicated ServiceAccount with token automount disabled, a
PVC-backed admission ledger, controller-only ingress, and egress limited to DNS
and public HTTPS provider/read-only SCM endpoints. Do not mount Git, forge,
publisher, provider-proxy, or other publication credentials into this Pod.
Terminal and rejected ledger rows become reclaimable only after the controller
acknowledges their exact durable settlement. The shipped 720-hour retention
window preserves duplicate-suppression and audit/backup evidence before bounded
garbage collection; configure a longer window when policy requires it.

Before changing any wrapper Pod-template field, run the authenticated drain
client from the currently deployed wrapper image and wait for it to succeed:

```bash
/orka-agent-harness-wrapper drain \
  --endpoint=https://agent-harness-wrapper.orka-system.svc:8080 \
  --bearer-token-file=/var/run/orka/harness-wrapper/token \
  --ca-file=/var/run/orka/harness-wrapper/ca.crt \
  --timeout=15m \
  --poll-interval=2s \
  --next-generation=2
```

Run the command from a narrowly isolated Pod labeled
`app.kubernetes.io/name=orka,app.kubernetes.io/component=agent-harness-wrapper-drain`
that can read only the dedicated wrapper auth Secret and reach only the wrapper
Service. Never place the bearer token on the command line or in logs. A timeout
aborts the rollout: do not mutate the Deployment. After a successful drain, update
`ORKA_HARNESS_WRAPPER_LEDGER_GENERATION` in the overlay to the same
`--next-generation` value and apply the Deployment change. Increment the value
exactly once for every later wrapper Pod-template replacement; ordinary Pod
restart with an unchanged template keeps the current generation.

For permanent shutdown or uninstall, omit `--next-generation`; this leaves the
durable admission close in place and never authorizes a replacement wrapper to
reopen it. Retain the ledger PVC until the reviewed v1 removal gate and backup
procedure have completed.
