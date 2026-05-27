# Report provider-neutral Execution Workspace status

Tasks that use an Execution Workspace should report a safe, provider-neutral lifecycle summary instead of exposing provider-native objects such as Substrate Actor snapshots or daemon URLs. Workers should emit workspace lifecycle updates through a small authenticated internal Task status endpoint because workers own provider lifecycle operations in the wrapper-first model. The controller records validation and lock failures directly, but it should not poll provider-native resources for normal workspace progress.

Intermediate workspace status updates are best effort. The Task result path remains the source of command completion, while requested cleanup state must still be reached for a workspace-backed Task to succeed.
