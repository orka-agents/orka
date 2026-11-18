# Autonomous planning and review

This example runs a native `type: ai` coordinator across multiple Jobs. It creates
an implementation plan, saves it, and asks a specialist to review it in the next
iteration. Review feedback can trigger another revision and review.

The planner child Task's result contains the document. The coordinator's final
result identifies that child and the review that approved it. Working plan state
tracks these Task names between iterations and is deleted on completion. The
example includes all three Agents and needs no Git repository or publication
credentials. For code publication, use [code-review](../code-review).

## Run it

Use the controller's watched namespace, normally `orka-system`. In `agents.yaml`,
replace all three `my-provider` references and model names with a configured
Provider and a model it serves. The Provider's credential Secret must exist in
the same namespace. The controller must have its native AI worker configured.

Apply the Agents first, then create a fresh Task:

```bash
kubectl -n orka-system apply -k examples/autonomous-task
task_name="$(kubectl -n orka-system create -f examples/autonomous-task/task.yaml -o jsonpath='{.metadata.name}')"
kubectl -n orka-system get task "$task_name" -w
```

`kustomization.yaml` contains only the Agents. Creating `task.yaml` starts a new
run with a generated name, so rerunning the command does not edit an old Task.

## Check the result

The first iteration delegates to `autonomous-planner` and saves a draft with
`goal_complete=false`, retaining the planner Task name. The next iteration
retrieves that child's full document and delegates to `autonomous-reviewer`.
An approved plan sets `goal_complete=true`; requested changes are saved for a
later revision. `status.iteration` is zero-based, so a complete draft/review run
has an iteration of at least `1`.

While the Task is running, open its Plan tab in the dashboard, or read
`GET /api/v1/tasks/<task-name>/plan?namespace=orka-system` with Orka API
authentication. After completion, use the Task result or
`GET /api/v1/tasks/<task-name>/result?namespace=orka-system`. The response's
`result` string contains a JSON envelope with `verdict`, `data.plan_task`, and
`data.review_task`. Read the named planner Task's result for the complete Markdown
document in `data.plan`, and the review Task's result for its verdict and feedback.
Both child results remain available after the coordinator finishes. The
`data.plan` field carries the document through `wait_for_tasks` without summary
truncation; the coordinator's working plan only needs the child names.

Check for `verdict: APPROVED`, a full document in the referenced child, and a Task status message
starting with `goal complete`. Phase alone is insufficient: the controller also
marks a Task `Succeeded` when it reaches `maxIterations`, even if the plan is
incomplete. This example permits four iterations, enough for a draft, review,
revision, and second review.

The five-minute timeout applies to each iteration. Delete the parent Task when
finished to remove its owned child Tasks:

```bash
kubectl -n orka-system delete task "$task_name"
kubectl -n orka-system delete -k examples/autonomous-task
```

Autonomous coordination uses the native AI worker. ACP runtime Tasks reject
`coordination.autonomous`; changing this coordinator to `type: agent` does not
provide an ACP repair loop. See [Autonomous Task execution](../../website/docs/guides/autonomous-tasks.md).
