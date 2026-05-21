# Use an explicit default provider for Execution Workspaces

When a Task requests an Execution Workspace without setting `spec.execution.workspace.provider`, Orka resolves the provider from an operator-configured Default Workspace Provider and falls back to `agent-sandbox` for compatibility. Orka does not infer the provider from installed cluster components because ambient detection is ambiguous when multiple providers are installed, stale CRDs remain, or RBAC hides provider resources.

Standard Worker Execution is not a provider. Tasks that do not request an Execution Workspace keep the existing direct Kubernetes worker Job path.
