# Two-stage self-bootstrapping coordinator

This example creates specialist Agents first, then requires an operator to
allowlist them before a **new Task** delegates planning and review work. It is
not an automatic create-and-delegate bootstrap. For repository changes, use
[code-review](../code-review).

## Why two stages?

An empty or omitted `spec.coordination.allowedAgents` denies `delegate_task`,
including to Agents just created with `create_agent`. The native worker's policy
is frozen in its Job environment: editing the coordinator does **not** refresh
an already-running worker, and the same worker cannot resume with the new list.
The controller retains a legacy label/parent-task dynamic-Agent exemption, but
the tool does not use it. Automatic bootstrap delegation would require future
provenance work, not a dynamic bypass here.

## 1. Create specialists without delegation

Update `spec.providerRef.name` in `coordinator-agent.yaml` to match the Provider
in your cluster, then apply the Agent and submit the bootstrap Task:

```bash
kubectl -n orka-system apply -f examples/self-bootstrapping/coordinator-agent.yaml
task_name="$(kubectl -n orka-system create -f examples/self-bootstrapping/coordinator-task.yaml -o jsonpath='{.metadata.name}')"
kubectl -n orka-system get task "$task_name" -w
```

Read the Task result in the UI or CLI. It returns the created Agents' exact names,
namespaces, roles, and suggested prompts, **not** specialist work results or a
reviewed implementation plan.

Alternatively, submit a bootstrap Task through the CLI:

```bash
orka -n orka-system task create --agent coordinator "BOOTSTRAP Agents for API design, test planning, and review of a TODO REST API in Go. Create native AI specialists and return their names and namespaces. Do not delegate."
```

## 2. Operator allowlists the created Agents

Keep the creation Task: `create_agent` gives each specialist an owner reference
to it, so deleting that Task garbage-collects the specialists. Do not delete it
while later Tasks still need these Agents.

Edit the coordinator:

```bash
kubectl -n orka-system edit agent coordinator
```

Populate `spec.coordination.allowedAgents` using the **actual names returned by
the bootstrap**, for example (replace these illustrative names before use):

```yaml
allowedAgents:
  - name: api-designer
    namespace: orka-system
  - name: test-planner
    namespace: orka-system
  - name: plan-reviewer
    namespace: orka-system
```

Entries match both name and namespace. If `namespace` is omitted, it defaults to
the coordinating Agent's namespace, not the child Task's namespace.

## 3. Submit a NEW delegation Task

Once the list is saved, create a new Task naming the approved specialists (again,
replace the example names with the exact names from stage 1):

```bash
orka -n orka-system task create --agent coordinator "DELEGATION: use existing allowlisted orka-system/api-designer, orka-system/test-planner, and orka-system/plan-reviewer. Plan a TODO REST API in Go with CRUD endpoints, in-memory storage, handler tests, and error handling. Delegate planning and review, then combine results into a reviewed plan under 800 words. Do not create replacement Agents or publish code."
```

This new native worker receives the updated explicit allowlist. The original
bootstrap worker does not. Children of the delegation Task are owned by that
Task; specialists remain owned by the original creation Task.

## Key concepts

- **Explicit policy**: tool delegation always needs an allowlisted Agent, even
  when the coordinator created it dynamically.
- **Auto-cleanup**: deleting the creation Task removes its specialists; deleting
  a delegation Task removes that Task's child Tasks.
- **Provider inheritance**: specialists inherit the creator's LLM provider config.
- **Depth and concurrency**: the coordinator config limits delegation to three
  levels and five concurrent children.

See [Multi-agent coordination](../../website/docs/reference/multi-agent-coordination.md)
for policy semantics and upgrade behavior.
