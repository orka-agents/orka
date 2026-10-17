---
description: "Label a GitHub issue orka:implement and let Orka plan, implement, open, review, and repair the pull request."
---

# Issue-to-PR automation

Label a GitHub issue `orka:implement` and Orka takes it from there. It plans the change,
implements it with a coding agent, opens a pull request, reviews it, repairs it when review
or CI asks for changes, and marks it ready to merge. A [RepositoryMonitor](repository-monitors.md)
runs the whole loop, and every step leaves a durable record you can inspect.

:::tip[Runnable example]
[`examples/github-label-triggered-issue-loop`](https://github.com/orka-agents/orka/tree/main/examples/github-label-triggered-issue-loop)
configures the Agents, Secrets, and monitor for this workflow.
:::

## How it flows

1. A maintainer labels an issue `orka:implement`, or runs
   `orka monitor issue implement <monitor> <number>`.
2. Orka checks who asked, then records a durable command event. For a label, it verifies the
   webhook signature and the sender's current GitHub permission. For the CLI or API, it uses
   your Orka API authorization (`orka:monitors:operate` and `orka:monitors:write` under
   transaction-token authorization; see [Create monitor command](../reference/api-reference.md#create-monitor-command)).
3. A workflow action is queued, bound to the monitor generation, a snapshot digest of the
   issue, a dedupe key, and the command ID.
4. Orka reads the issue and computes a content digest that excludes Orka's own labels and
   comments.
5. With `spec.issueWorkflow.implementation.requirePlan: true`, the default, Orka queues a
   read-only planning Task when there is no current ready plan.
6. A `ready` plan continues the original command automatically. A `blocked` or
   `needs_human` plan stops and records why.
7. Implementation runs in a writable, sanitized workspace on a branch the controller
   chooses. The agent only edits files; the separate Workspace Publisher validates the
   change, publishes it, and verifies the remote branch.
8. The controller accepts only a matching `VerifiedExact` delivery receipt, then creates or
   reuses the pull request and records each GitHub write in its audit log.
9. Review and bounded repair continue on each exact pull request head until the pull
   request is merge-ready or clearly blocked.

Triage and research are not part of the automatic flow. Request them, or any step on its
own, through the API or CLI.

## Set it up

1. **Create the Agents.** A planner, an implementer, a reviewer, and a repairer, all
   built-in coding-agent runtimes. Implementation supports Codex and Claude; see the
   [safety model](#safety-model) for what each role accepts.
2. **Create four credential Secrets** — source read, publication read, publication write, and
   forge — as described in [Repository monitors](repository-monitors.md#prerequisites).
   Orka refuses to run a write workflow when any role is missing or two roles share a Secret.
3. **Create the RepositoryMonitor** with issue targets, label triggers, and the Agents:

   ```yaml
   spec:
     targets:
       issues:
         enabled: true
     triggers:
       github:
         labels:
           enabled: true
     agents:
       planner:
         name: issue-planner
       implementer:
         name: issue-implementer
       reviewer:
         name: pr-reviewer
       repairer:
         name: pr-repairer
     repair:
       enabled: true
     review:
       exactEventEnabled: true
       publish:
         enabled: true
   ```

   The [Resource reference](../reference/resources.md#repositorymonitor) lists every field.
4. **Connect the GitHub webhook.** Store a shared secret in the controller's namespace and
   point the Helm release at it:

   ```bash
   umask 077
   openssl rand -hex 32 | tr -d '\n' > github-webhook.secret
   kubectl -n orka-system create secret generic github-webhook \
     --from-file=secret=github-webhook.secret
   helm upgrade orka orka/orka --namespace orka-system --reuse-values \
     --set github.webhook.secretName=github-webhook
   ```

   Then add a webhook to the GitHub repository that sends **Issues** and **Pull requests**
   events to `https://<orka-api-host>/webhooks/github`, paste the contents of
   `github-webhook.secret` as its secret, then delete the file. The endpoint must
   be reachable from GitHub, so expose the Orka API through your ingress.
5. **Require the readiness check.** With `review.publish.enabled: true`, Orka publishes the
   commit status `orka/<namespace>/<monitor-name>/ready`. Require it in branch protection,
   alongside your CI and approving-review rules.

## Start from a GitHub label

When a labeled issue arrives at `/webhooks/github`, the controller verifies
`X-Hub-Signature-256`, checks that the issue belongs to the monitor's repository, checks the
sender's current repository permission
(`triggers.github.labels.requireActorPermission`, default `write`), and records a durable
command before starting any work. Rename the label with
`spec.triggers.github.labels.issues.implement`.

Set `consumeCommandLabels: true` to remove the label once the command is recorded. Replayed
GitHub deliveries do not create another command. The webhook response confirms intake, not
a finished implementation.

There is one command label. There is no `agent:*` label handler and no generic
label-to-Task trigger, and ordinary progress needs no label for each step. Planning,
research, decomposition, review, and repair can still be requested through the monitor API
or CLI for ad hoc work.

## Pause and stop

- **`orka:pause`** (configurable with `spec.policy.pauseLabels`) is a persistent guard on one
  issue or pull request. It prevents further workflow actions and keeps merge readiness
  blocked. A bounded Task that is already running may finish, including its publication;
  use the Task cancellation API to stop it immediately. Removing the label queues fresh
  reconciliation, not an approval or a replay of an old command.
- **A `stop` command** cancels queued workflow actions and active monitor Tasks, and prevents
  stale Task results from mutating anything afterwards. It does not depend on label
  propagation.
- **`spec.suspend`** pauses background runs for the whole monitor. Queued and manual runs can
  still finish.

A pause label is not an atomic merge lock. To stop a merge that GitHub already considers
eligible, turn off GitHub auto-merge on that pull request. GitHub sender permission is
checked when the label is added and when it is removed.

## Review, repair, and merge readiness

Pull request events on each new head trigger an exact-head review when
`spec.review.exactEventEnabled` is set. Automatic repair needs `spec.repair.enabled`, a
repairer Agent, and the publication credentials, and is bounded by
`maxRepairsPerPR` and `maxRepairsPerHead`. Workflow polling observes CI and completed Tasks
without requiring another label.

GitHub's per-pull-request auto-merge setting owns the merge decision. Orka never enables
auto-merge or calls the merge endpoint. When auto-merge is off, the pull request stays open
and ready. [Repository monitors](repository-monitors.md#github-merge-readiness) describes the
readiness status and the token permissions it needs.

Before disabling readiness publication, deleting a monitor, or changing its repository or
tracked branch, remove or replace its required status in the repository's branch protection.
GitHub commit statuses persist after the monitor is removed or repointed, and Orka does not
revoke them.

## Safety model

- Issue and pull request text is untrusted input.
- Read-only agents never receive GitHub mutation credentials or direct Git credentials.
  Claude and OpenCode roles receive only scoped read tools with Bash denied. Codex is
  supported for pull request review and read-only issue triage, research, and planning; it
  runs inside the RuntimeSession boundary, where the controller rejects elevation requests
  and classifies any workspace change as a read violation. The monitor API currently accepts
  Codex only for the reviewer role. Copilot and external `runtimeRef` runtimes are rejected
  for this hardened mode.
- Implementation agents receive only runtime model access and a pre-cloned writable
  workspace, never Git push credentials. Codex and Claude are supported; Copilot is rejected
  because its runtime credential can mutate GitHub.
- Code-changing Tasks must produce a validated patch before any branch push. Denied paths and
  secret scanning are always enforced, before `allowedPaths` is checked.
- GitHub writes are controller-owned and recorded as mutation records.
- Repair runs only when `spec.repair.enabled` is true and stays within its limits.
- Plans and implementations are bound to the issue's content digest, so a human edit makes
  downstream artifacts stale.
- Policy labels follow the repository's current GitHub state.
  `triggers.github.labels.requireActorPermission` authorizes command intake; it does not
  override GitHub permissions for editing policy labels. A rejected pause-label webhook does
  not queue a run, but later inventory still observes the repository's labels.

## Implementation limits

`spec.issueWorkflow.implementation` bounds code-changing work:

| Field | Default | What it limits |
| --- | --- | --- |
| `maxActive` | `2` | Active implementation jobs per monitor |
| `maxAttemptsPerIssue` | `2` | Implementation attempts for one issue |
| `maxChangedFiles` | `12` | Files in one `orka.patch.v1` patch |
| `allowedPaths` | none | Path globs or prefixes, such as `api/**` or `docs/**`, that a patch may touch |

## Watch and debug

```bash
orka monitor issue plan orka-main 123
orka monitor issue implement orka-main 123
orka monitor issue implementation get orka-main 123
orka monitor issue patch preview orka-main 123
orka monitor mutations list orka-main --kind issue --number 123
orka monitor pr review orka-main 456 --target-sha '<head-sha>'
orka monitor pr fix orka-main 456 --target-sha '<head-sha>'
orka monitor pr ready readiness orka-main 456
orka monitor work-actions list orka-main --kind issue --number 123
```

Start debugging with the workflow timeline:

```bash
orka monitor work-actions list orka-main --status blocked
orka monitor doctor orka-main
```

Blocked records carry a short reason such as `stale_command_snapshot`, `security_sensitive`,
`patch_path_denied`, `validation_failed`, or `stopped_by_command`. The dashboard's
**Monitors** page shows the same reason next to implementation jobs and GitHub mutation
records. Patch previews return safe `orka.patch.v1` metadata, also available from
`GET /api/v1/monitors/implementation-jobs/{id}/patch-preview`.

Transient failures are classified and attached to monitor events and metrics:
`github_rate_limited`, `llm_rate_limited`, `cluster_capacity_blocked`, and
`retry_scheduled`. Use `orka monitor events <monitor> --event-type run_failed` or the
dashboard's audit timeline to see them.

A pull request the controller creates starts a targeted inventory run, with bounded retries.
If that run fails terminally before recording the pull request, fixing the cause (for
example, the credentials) does not restart discovery on its own when no schedule or pull
request event queues another run. Start a fresh targeted run without a head SHA:

```bash
orka monitor run orka-main --namespace orka --target-kind pull_request --target-number 456
```

## Known limitations

- Pause-label intake and workflow reconciliation can update the same item concurrently, and
  a stale write can overwrite the stored label or workflow progress. The workflow does not
  yet guarantee pause enforcement across this race.
- With automatic repair enabled and readiness publication disabled, a full workflow poll
  fetches details for every open pull request before applying `maxPerRun`. That limit bounds
  the work started, not GitHub reads.
- Automatic rediscovery after a credential fix is not implemented; use the targeted run above.
