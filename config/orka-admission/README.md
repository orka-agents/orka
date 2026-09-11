# Orka admission runtime

This opt-in production base deploys the stateless `orka-admission` process
independently from the controller. It intentionally does not install the
fail-closed webhook configuration; apply `../orka-admission-webhooks` only
after both replicas are ready and an AdmissionReview smoke test succeeds.

Before applying this base:

1. Replace `controller:latest` with the exact digest-pinned Orka controller
   image that contains `/orka-admission`.
2. Create `orka-admission-tls` in `orka-system` with `tls.crt`, `tls.key`, and
   `ca.crt`. When using cert-manager direct CA injection, annotate the Secret
   with `cert-manager.io/allow-direct-injection: "true"`.
3. Patch any trusted controller or worker identities used by the execution
   authority, attachment Secret, and provenance handlers. The checked-in
   example matches the canonical direct-Kustomize controller in `orka-system`, plus Helm releases
   `orka-v1` in `orka-v1-system` and `orka-v2` in `orka-v2-system`. If any
   release namespace, release name, or ServiceAccount name differs,
   patch `--controller-usernames`, `--task-provenance-trusted-users`, and
   `--task-provenance-trusted-service-accounts`. Patch the exact same
   controller usernames in every `route-unless-controller-cleanup-safe`
   condition in `../orka-admission-webhooks/validating_webhook.yaml`.

The dedicated ServiceAccount can read namespace mode claims and create
SubjectAccessReviews for workspace-class authorization. It has no Orka CRD,
Lease, SQLite, runtime-credential, dispatcher, or controller reconciliation
access. Static harness mode and namespace ownership are controller startup
contracts; this service does not provide dynamic backend-mode,
classification, or adjudication APIs.

Install this base once in the platform-owned `orka-system` namespace. Neither
the v1 nor v2 release owns these cluster-scoped RBAC objects or the shared
`ValidatingWebhookConfiguration`.

Uninstall in reverse order: delete the webhook configuration first, then the
runtime base after API server propagation is complete.
