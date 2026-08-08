# Orka fail-closed admission webhooks

This is the second admission installation wave for the immutable namespace
mode, Agent and AgentRuntime contracts, Task execution authority,
Task-provenance, and workspace-class authorization boundaries. Apply it only
after:

- `../orka-admission` has at least two ready Service endpoints;
- `orka-admission-tls` contains a valid serving certificate for
  `orka-admission.orka-system.svc` and a `ca.crt` bundle;
- the Secret is annotated `cert-manager.io/allow-direct-injection: "true"`;
- an AdmissionReview smoke test has reached every enabled handler.

Trusted identities embedded in `validating_webhook.yaml` must exactly match the
corresponding admission-runtime arguments. The checked-in example authorizes
the canonical direct-Kustomize controller in `orka-system`, plus the exact
controller identities for Helm releases `orka-v1` in `orka-v1-system` and
`orka-v2` in `orka-v2-system`. If any identity differs, patch the shared runtime
and all three `route-unless-controller-cleanup-safe` conditions as one reviewed
platform change before enabling the webhooks.

The namespace webhook permits a Namespace to be created with one valid
`orka.ai/controller-mode` claim and then makes that claim immutable. It rejects
adding a claim to an existing unlabeled Namespace. The resource webhooks require
contracts and new Task bindings to match that claim. The static harness
architecture does not install admission policies for dynamic backend modes,
cross-protocol binding, migration classification, or adjudication. Each
controller accepts one startup mode and one labeled watch namespace; the two
releases do not share a Task population.

All retained webhook entries use `failurePolicy: Fail`. Delete their
configurations before removing the last admission endpoint or its TLS Secret.
The platform owner installs this `ValidatingWebhookConfiguration` exactly
once; neither controller release owns a second copy.
