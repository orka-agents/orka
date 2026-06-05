# Model Substrate oversubscription as controller-owned actor pools

Substrate oversubscription should be exposed through an Orka-owned actor pool abstraction rather than by overloading per-Task workspace fields. A pool represents a bounded Substrate WorkerPool plus a target actor density, such as 250 stateful actors across 8 workers, and the controller reconciles pool membership, queued claims, and cleanup pressure against that budget. Tasks still select an Execution Workspace provider and template; they may reference a pool, but they should not choose individual workers or actor pods.

The pool controller should own scheduling and juggling decisions. It can pre-create or retain suspended actors, resume actors when workers have capacity, suspend idle actors to free workers, and use Substrate `ListActors` and `ListWorkers` to publish density and placement health. Workers continue to own command execution through the wrapper-first path, so result submission, artifact upload, token handling, and provider-neutral Task status stay unchanged.

The first implementation slice should keep Task status provider-neutral: surface density, placement, and latency, but do not expose raw Substrate snapshot URIs, daemon URLs, or tokens. Later snapshot restore work can reuse the vendored checkpoint/restore clients, but restoring arbitrary snapshots should be a pool/controller action with explicit template compatibility checks instead of a worker-side shortcut.

This keeps oversubscription an operator-controlled capacity feature, avoids encoding provider-native pod choices into user Tasks, and leaves room for MCP tool-actors to reuse the same pool machinery when Orka adds durable tool-hosting actors.
