# Orka admission runtime

This opt-in production base deploys the stateless `orka-admission` process
independently from the singleton controller. It intentionally does not install
the fail-closed webhook configuration; apply `../orka-admission-webhooks` only
after both replicas are ready and an AdmissionReview smoke test succeeds.

Before applying this base:

1. Replace `controller:latest` with the exact digest-pinned Orka controller
   image that contains `/orka-admission`.
2. Create `orka-admission-tls` in `orka-system` with `tls.crt`, `tls.key`, and
   `ca.crt`. When using cert-manager direct CA injection, annotate the Secret
   with `cert-manager.io/allow-direct-injection: "true"`.
3. Patch the controller, adjudication-controller, reviewed one-time
   classification usernames, and administrator groups when they differ from
   the canonical controller identity and `system:masters` group. Patch the
   matching controller identities in
   `../orka-admission-webhooks/validating_webhook.yaml` atomically.

The dedicated ServiceAccount has read-only access only to the objects needed
for live admission checks. It has no Lease verbs, SQLite volume, runtime
credentials, dispatcher, or controller reconciliation responsibility.

Uninstall in reverse order: delete the webhook configuration first, then the
runtime base after API server propagation is complete.
