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
the default (full-profile) AI and vendor worker ServiceAccounts are granted `use`
on Providers in the namespaces where they write Tasks and Agents, so Tasks and
Agents they write that name a Provider in their own namespace are not affected.
Admission checks the identity that writes the object, user or ServiceAccount,
not the original caller: a write that newly names a Provider, in another
namespace or under a role without `use`, is refused unless the writing identity
holds `use` on that Provider.

Providers an Agent named before the check was enabled were never checked, and
a Task that runs that Agent needs only `use` on the Agent. Review existing
Agents' `providerRef` and fallbacks before you enable it.

A Task that runs its Agent's Provider needs only `use` on the Agent; see
`config/agent-use-policy`. An Agent from another namespace is the exception:
it may be outside the namespaces these bindings cover, and its fallbacks and a
`providerRef` without a namespace resolve in the Task's namespace. Admission
cannot read the Agent, so with the flag set the controller refuses an `ai` Task
that would take a Provider from such an Agent each time it runs, and the API
refuses it when it is created.

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
