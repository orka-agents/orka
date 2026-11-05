# Agent use policies

Opt-in ValidatingAdmissionPolicies that require the `use` verb on an Agent
before a direct Kubernetes write names it: creating a Task with `agentRef`, or
creating or editing a RepositoryScan, RepositoryMonitor, or GatewayBinding so
that it names a new Agent. Edits that keep the same Agents are not checked
again.

Apply this directory together with `--agent-use-authorization-enabled` on the
controller, which applies the same rule to API callers. The Helm value
`controller.agentUseAuthorization.enabled` does both. Orka's controller and
worker ServiceAccounts hold `use` on every Agent, so Orka-created Tasks are
not affected.

Grant `use` with a Role rule:

```yaml
- apiGroups: ["core.orka.ai"]
  resources: ["agents"]
  verbs: ["use"]
  resourceNames: ["reviewer"]
```
