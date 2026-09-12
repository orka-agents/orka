# Harness v1 wrapper

This Kustomize base is the data plane for a static `harness-v1` installation.
It is intentionally absent from `config/default` and
`config/acp-production`. Deploy it only with a v1 controller whose non-empty
watch namespace is labeled `orka.ai/controller-mode: harness-v1` and whose
startup mode is `--controller-mode=harness-v1`.

The v1 installation has its own controller namespace, watched namespace,
ServiceAccount, Lease, API endpoint, SQLite store, Secrets, and wrapper ledger.
It must not share those resources with a `harness-v2` installation. The wrapper
is never a fallback for failed ACP work, and its Tasks and Sessions cannot be
continued by v2.

Before applying the base:

1. Replace the all-zero wrapper image digest through an operator-controlled
   overlay with the exact reviewed `repository@sha256:<digest>` reference.
2. Create distinct bearer and TLS Secrets in `orka-system` without printing
   the bearer value. The serving certificate must authenticate
   `agent-harness-wrapper.orka-system.svc`:

   ```bash
   kubectl create namespace orka-system --dry-run=client -o yaml | kubectl apply -f -
   if ! kubectl -n orka-system get secret harness-wrapper-auth >/dev/null 2>&1; then
     openssl rand -hex 32 | \
       kubectl -n orka-system create secret generic harness-wrapper-auth \
         --from-file=token=/dev/stdin
   fi
   if ! kubectl -n orka-system get secret harness-wrapper-tls >/dev/null 2>&1; then
     kubectl -n orka-system create secret generic harness-wrapper-tls \
         --from-file=tls.crt=/path/to/tls.crt \
         --from-file=tls.key=/path/to/tls.key \
         --from-file=ca.crt=/path/to/ca.crt
   fi
   ```

The wrapper has a dedicated ServiceAccount with broad token automount disabled,
a short-lived projected token used only to authenticate artifact uploads to its
controller, a PVC-backed admission ledger, controller-only ingress, and egress
limited to that controller, DNS, and public HTTPS provider/read-only SCM
endpoints. Do not mount Git, forge, publisher, provider-proxy, or other
publication credentials into this Pod.
Terminal and rejected ledger rows become reclaimable only after the controller
acknowledges their exact durable settlement. The shipped 720-hour retention
window preserves duplicate-suppression and audit/backup evidence before bounded
garbage collection; configure a longer window when policy requires it.

Before changing any wrapper Pod-template field, run the authenticated wrapper
drain client from the currently deployed wrapper image and wait for it to
succeed:

```bash
/orka-agent-harness-wrapper drain \
  --endpoint=https://agent-harness-wrapper.orka-system.svc:8080 \
  --bearer-token-file=/var/run/orka/harness-wrapper-auth/token \
  --ca-file=/var/run/orka/harness-wrapper-tls/ca.crt \
  --timeout=15m \
  --poll-interval=2s \
  --next-generation=2
```

Run the command from a narrowly isolated Pod labeled
`app.kubernetes.io/name=orka,app.kubernetes.io/component=agent-harness-wrapper-drain`
that can read only the dedicated wrapper auth Secret and the current wrapper
TLS Secret, and reach only the wrapper Service. Never place the bearer token on
the command line or in logs. A timeout aborts the rollout: do not mutate the
Deployment. After a successful drain, update
`ORKA_HARNESS_WRAPPER_LEDGER_GENERATION` in the overlay to the same
`--next-generation` value and apply the Deployment change. Increment the value
exactly once for every later wrapper Pod-template replacement; ordinary Pod
restart with an unchanged template keeps the current generation.

Certificate renewal changes only `harness-wrapper-tls`; never add TLS material
to or update `harness-wrapper-auth`, because accepted attempts pin that Secret's
UID and resourceVersion as execution authority. Treat renewal as a drained Pod
template replacement: preferably create a versioned TLS Secret, drain with the
old Secret's CA, then patch the Deployment and controller CA mount to the new
Secret while advancing the ledger generation. For same-name renewal, retain a
CA bundle that trusts the currently served certificate until the drained
replacement is running.

This wrapper drain protects v1 turn state during a Pod-template replacement. It
is not a controller mode and does not transfer work to v2.

For permanent shutdown or uninstall, first stop every v1 producer and revoke
new Task creation. Then omit `--next-generation`; this leaves wrapper admission
closed and never authorizes a replacement wrapper to reopen it. Retain the
ledger PVC until all v1 execution and cleanup work is settled and the reviewed
backup/retention procedure has completed.
