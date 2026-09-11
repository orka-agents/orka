# Native conformance

The single provider/protocol pin is `hack/agent-substrate/upstream.env`. Verify
that the installer checkout has that origin and commit and no tracked changes.
Use the kubeconfig under the selected run directory for all cluster commands.

The bundled suite must pass direct claim, sealed credential delivery, a long
command, file download, timeout, native Data Tag restore into independent Actors,
MCP execution, ACP completion, controller restart, DataOnly suspension and cold
continuation, public checkpoint export, source deletion, fresh restore,
cancellation, and final Actor/Tag cleanup. It uses the actual Codex runtime
with a local Responses fixture and requires no external model account.

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
