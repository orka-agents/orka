# Human approval v2 E2E

Run `bash scripts/human-approval-v2-e2e.sh` from an Orka checkout with Docker,
kind, kubectl, Go, Python 3.11 or newer, jq, and OpenSSL installed. The launcher
creates a fresh kindctl cluster, uses its scoped kubeconfig, and deletes the
cluster and registry on exit. It never uses the global kubeconfig.

The test builds the current Orka controller and supervisors, plus pinned
AgentKit and Foundry adapters. AgentKit runs its real MAF/OpenAI/MCP clients.
Foundry runs its real ACP child and lifecycle broker against a separate
AgentKit hosted process. The model and Azure session/identity transport are
deterministic fixtures, so the PR gate needs no provider credentials.
The gateway uses HTTPS with a per-run CA. Its serving key stays in a private
Secret; the Foundry broker receives only the public CA certificate.

Both adapters must pass approval, decline, cancellation, reviewer permissions,
and the actual 600-second approval expiry. Approval waits for at least 125
seconds while an independent Task completes. Invocation counters prove that
approved actions run once and rejected actions never start.

Recovery checks enroll each runtime Deployment before admission and verify its
immutable boot witness. AgentKit uses one runtime container; Foundry uses a
supervisor and a broker, with the hosted process and identity service elsewhere.
The checks kill the exact supervisor container on both adapters, then kill the
controller with pending reviews and with an invocation in flight. They verify
new boot/epoch fences, rejection of late decisions, unchanged immutable request
Secrets, and invocation counts. A lost invocation remains unknown until its
outcome can be proved.

Foundry recovery also requires the original broker ledger and configuration.
The replacement supervisor asks that broker to seal the lost boot and prove
retirement of every owner. Missing or changed evidence fails closed. Normal
Task deletion must produce an exact cleanup receipt, remove the finalizer, and
reclaim owned request records. Completed ExternalEffect receipts remain for
audit; cleanup checks verify their original identities and digests. These
checks do not patch status or strip finalizers to make cleanup succeed.
Historical Tasks without enrollment or
termination evidence cannot acquire that evidence retroactively.

The workflow uploads only `bin/human-approval-v2-e2e/*/evidence/`, containing
source revisions, image digests, scenario results, counters, and Pod status.
Private credentials and port-forward logs stay outside that directory.

This CI gate does not qualify Azure ingress or hosted-session behavior. A live
qualification must use an active `kind: hosted` agent version with matching
tool schemas, a response-state TTL and session idle timeout of at least 1800
seconds, and the same acceptance checks in `examples/human-approval-v2`.
The production approval capability remains opt-in for each exact profile.
