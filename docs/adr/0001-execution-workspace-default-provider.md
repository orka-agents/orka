# ADR 0001: Use an explicit default provider for Execution Workspaces

## Status

Superseded by the ACP core RuntimePool cutover.

The earlier API allowed a Task to select an execution-workspace provider through `spec.execution.workspace.provider`. The current built-in agent path rejects `Task.spec.execution.workspace` and never auto-detects or defaults an upstream provider.

Current agent repository input and publication policy belongs at top-level `Task.spec.workspace`. Future agent-sandbox or Substrate integration must be explicit, operator-configured, and implemented behind the `orka.harness.v2` RuntimeSession lifecycle; ambient provider discovery remains disallowed.
