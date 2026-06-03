# Use wrapper-first Execution Workspace providers

Orka should add new Execution Workspace providers behind the existing worker-wrapper path before moving provider lifecycle ownership into the controller. The controller validates, locks, and creates the normal worker Job; the outer worker claims the selected provider, stages the inner worker, runs it inside the workspace, reports provider-neutral status, and applies cleanup. This preserves Orka's existing worker auth, result submission, artifact upload, and agent runtime behavior while isolating provider-specific lifecycle code behind `WorkspaceExecutor`.

Controller-direct execution can be reconsidered after Substrate is stable enough to justify tighter lifecycle orchestration and stronger controller-owned observability.
