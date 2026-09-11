# Examples

CI strict-decodes every Orka document in this directory against the typed API
and checks each Agent's admission contract. These checks catch unknown fields
and incompatible Agent settings. Running an example also requires the Provider,
credentials, repository settings, and controller configuration in its README.

## What's here

| Example | What it shows | Read more |
| --- | --- | --- |
| [`code-review/`](code-review) | One code-and-review pass: opens a PR on approval, or reports requested changes and stops | [Multi-agent coordination](../website/docs/reference/multi-agent-coordination.md) |
| [`github-cicd/`](github-cicd) | Publishing one implementation, opening a PR, and reporting its CI status; includes an optional direct CI-repair workflow | [Agent runtimes](../website/docs/concepts/agent-runtimes.md) |
| [`self-bootstrapping/`](self-bootstrapping) | A coordinator that creates the specialist agents it needs, then delegates to them | [Multi-agent coordination](../website/docs/reference/multi-agent-coordination.md) |
| [`autonomous-task/`](autonomous-task) | A native AI coordinator that saves a plan and delegates review across multiple iterations | [Autonomous Task execution](../website/docs/guides/autonomous-tasks.md) |
| [`github-label-trigger/`](github-label-trigger) | Turning a GitHub label into an agent Task via a signed webhook | [GitHub label triggers](../website/docs/guides/github-label-triggers.md) |
| [`github-label-triggered-issue-loop/`](github-label-triggered-issue-loop) | The `orka:*` label workflow from issue through implementation, PR creation, and review; automerge is disabled by default | [Issue-to-PR automation](../website/docs/guides/issue-to-pr-automation.md) |
| [`repository-monitor-issue-plan-only/`](repository-monitor-issue-plan-only) | Durable triage and planning records, with no code written | [Repository monitors](../website/docs/guides/repository-monitors.md) |
| [`repository-monitor-pr-review-repair/`](repository-monitor-pr-review-repair) | Reviewing PRs, pushing repairs, and optional automerge command labels | [Repository monitors](../website/docs/guides/repository-monitors.md) |

Each subdirectory has its own README with the exact apply commands and the Secrets it
needs.

## Configure before applying

**Apply into the controller's watched namespace.** The static harness-v2 controller
reconciles exactly one namespace (`--watch-namespace`, which is `orka-system` for
`make deploy` and the Helm chart). No manifest here carries a `metadata.namespace`, so pass
it explicitly, for example
`kubectl -n orka-system apply -k examples/repository-monitor-pr-review-repair`; a resource
created anywhere else is never reconciled.

The same applies to the Secrets each example needs. Secret references are namespace-local,
so a Secret created in `default` is invisible to a monitor in `orka-system` — the commands
in each README use `-n orka-system` for exactly this reason.

**Models and providers are placeholders.** Agent `spec.model.name`, Provider names, and
credential Secret names reflect one working setup. Swap in the models and Secrets that
exist in your cluster. Note that built-in runtime Agents *must* set `spec.model.name` — the
ACP session has no default model, and admission rejects an Agent without one.

**Native coordinators need access to their Provider endpoint.** When a Provider points
at Orka's Vekil provider-auth proxy, the default proxy NetworkPolicy admits ACP runtime
Pods only. Native AI coordinators also need this namespace-local ingress rule. Apply it
in the controller's watched namespace; it allows the controller and native AI workers
to use the authenticated proxy on port 8080:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: orka-native-provider-access
spec:
  podSelector:
    matchLabels:
      orka.ai/network-role: provider-auth-proxy
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              orka.ai/task-type: ai
        - podSelector:
            matchLabels:
              orka.ai/network-role: controller
      ports:
        - protocol: TCP
          port: 8080
```

The Provider must reference that proxy's credential Secret in the same namespace.
For a different Provider endpoint, configure its network access and credentials instead.

**Repository examples need repository-specific values.** Replace repository URLs,
branches, and credential references before creating Tasks or monitors. Writing
RepositoryMonitors also need a real digest-pinned `spec.validation.image` with
your repository's validation tools; the all-`a` digest in the samples is a placeholder.

New to any of these terms? The [glossary](../website/docs/reference/glossary.md) defines
them once.
