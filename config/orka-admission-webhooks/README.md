# Orka fail-closed admission webhooks

This is the second admission installation wave. Apply it only after:

- `../orka-admission` has at least two ready Service endpoints;
- `orka-admission-tls` contains a valid serving certificate for
  `orka-admission.orka-system.svc` and a `ca.crt` bundle;
- the Secret is annotated `cert-manager.io/allow-direct-injection: "true"`;
- an AdmissionReview smoke test has reached every protected handler.
- the singleton `orka-system/cluster` `AgentExecutionControl` exists and its
  observed generation and backend modes are current.

The controller and adjudication-controller usernames embedded in
`validating_webhook.yaml` must exactly match the corresponding arguments in
`../orka-admission/deployment.yaml`. If noncanonical ServiceAccounts are used,
patch both installation waves as one reviewed change before enabling the
webhooks; otherwise cleanup-safe outage bypasses and handler identity checks
would disagree.

The configuration uses `failurePolicy: Fail` and a parameterized binding
policy with `parameterNotFoundAction: Deny`. The policy requires exact control
identity, generation, enabled mode, and mode revision for executable bindings;
only the exact live `Open` migration inventory may create
`legacy-cleanup-only` bindings without backend control. Once the inventory is
`Sealed`, no additional migration binding may be introduced.
Delete this configuration before removing the last admission endpoint or its
TLS Secret.
