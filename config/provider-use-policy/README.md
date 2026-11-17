# Provider use policies

Opt-in ValidatingAdmissionPolicies that require the `use` verb on a Provider
before a direct Kubernetes write names it: a Task's `spec.ai.providerRef`, or
an Agent's `spec.providerRef` or `spec.model.fallbacks[].providerRef`. Edits
that keep the same Providers are not checked again. A Provider reference
without a namespace is checked in the object's namespace, and an Agent's
fallbacks always are.

Apply this directory together with `--provider-use-authorization-enabled` on
the controller, which applies the same rule to API callers and also checks
every Provider a chat request resolves. The Helm value
`controller.providerUseAuthorization.enabled` does both. Orka's controller and
worker ServiceAccounts hold `use` on every Provider, so Orka-created Tasks and
Agents are not affected.

A Task that runs its Agent's Provider needs only `use` on the Agent; see
`config/agent-use-policy`. One case is checked by the API but not here: when a
Task runs an Agent from another namespace, that Agent's fallbacks and a
`providerRef` without a namespace resolve in the Task's namespace. Admission
cannot read the Agent, so restrict direct Task writes that name another
namespace's Agent, or enforce namespace isolation.

These bindings match every namespace. With more than one installation on a
cluster, add a `matchResources.namespaceSelector` on
`kubernetes.io/metadata.name` for this installation's watched namespace, as
the Helm chart does, so the check does not reach installations that leave it
off.

Grant `use` with a Role rule:

```yaml
- apiGroups: ["core.orka.ai"]
  resources: ["providers"]
  verbs: ["use"]
  resourceNames: ["anthropic"]
```
