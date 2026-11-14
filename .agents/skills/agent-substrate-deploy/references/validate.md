# Native conformance

The single provider/protocol pin is `hack/agent-substrate/upstream.env`. Verify
that the installer checkout has that origin and commit and no tracked changes.
Use the kubeconfig under the selected run directory for all cluster commands.

The bundled suite (`scripts/agent-substrate-e2e.sh`) must pass:

- direct claim, sealed credential delivery, provider-proxy egress, a long
  command, file download, and timeout;
- a synchronous VOLUMES suspension, a Data Tag, source deletion, and restore
  into two independent Actors;
- pooled MCP Tool execution through the authenticated `orka-api` proxy, with
  default-deny Actor egress;
- egress-gateway credential injection: the upstream sees the Tool's Secret
  only as a digest, and a request without the header gets none;
- one caller trace spanning the `orka-api` server span, its `substrate.router`
  child span, and the Actor;
- Tool, pool, Actor, and Tag cleanup.

It requires no external model account. It installs no ACP workspace provider.
ACP completion, Session suspend and cold continuation, worker eviction, and
checkpoint export through the standalone provider are covered by the
`orka-workspace` Data and Core proofs, run by
`.github/workflows/agent-substrate-acp-e2e.yml`. Do not claim ACP conformance
from this suite alone.

Focused tests add lost Create/Suspend/Tag responses, Tag source changes,
replacement rejection, catalog acquisition/GC races, process-bound bootstrap,
client-certificate and bearer rotation, pagination, and explicit recovery.
Run repository-required Go checks and shell checks after changing the adapter.

`orka-substrate-doctor` is read-only. It verifies native connectivity and
identity, inventory, infrastructure template settings, and available workers.
It does not identify a server build or validate router streaming and execution.
Report each level of evidence accurately. If Docker or the provider is
unavailable, preserve logs privately and state that live conformance is blocked.

Full-memory restore is prohibited. Only workspace data survives. The native
provider has no atomic lifecycle preconditions and currently no authorization
layer; Orka's CAS journals and Kubernetes use checks do not add those provider
capabilities. WorkerPool Pods are operator-managed capacity and may remain
running with no Actors.
