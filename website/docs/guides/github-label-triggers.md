---
slug: /github-label-triggers
description: "Starting the repository workflow from a GitHub issue label."
---

# GitHub label triggers

Configure a `RepositoryMonitor` and connect a GitHub webhook to `/webhooks/github`.
Enable `spec.triggers.github.labels.enabled` and select **Issues** and **Pull requests**
events. The controller verifies `X-Hub-Signature-256` using
`ORKA_GITHUB_WEBHOOK_SECRET`, checks repository/base scope and the sender's current
repository permission, and records a durable command before starting work.

## Implement an issue

Apply `orka:implement` to an issue. This queues planning when `requirePlan` is enabled,
then implementation and verified branch publication. The controller creates the PR.
Configured PR events and workflow polling drive review, bounded repair, and readiness.
The name can be changed with `spec.triggers.github.labels.issues.implement`.

Set `consumeCommandLabels: true` to remove the command label after durable intake.
Replayed GitHub deliveries do not create another command. The webhook response
confirms intake, not completed implementation.

## Pause

`orka:pause` is a persistent policy guard, configurable through `spec.policy.pauseLabels`.
It prevents additional workflow actions and keeps merge readiness blocked. An active,
bounded Task may finish, including its publication; use the task cancellation API when immediate cancellation is
needed. Removing the label queues fresh reconciliation, not an approval or a replay of
an old command. To stop a merge that GitHub already considers eligible, turn off
GitHub auto-merge; a webhook-driven pause is not an atomic GitHub merge lock. GitHub sender permission is checked for both changes.

## Review and merge

Enable `spec.review.exactEventEnabled` to receive exact-head PR events. Autonomous
repair requires `spec.repair.enabled`, a repairer Agent, and publication credentials.
Managed workflows with `spec.review.publish.enabled` publish the `orka/<namespace>/<monitor-name>/ready` check.
Configure GitHub to require that check from the trusted integration, alongside your
CI checks and approving-review rules.

GitHub's per-PR auto-merge setting owns the merge decision. Orka does not enable,
re-enable, or perform auto-merge. When it is disabled, the PR stays open and ready.

There is no `agent:*` label handler or generic label-to-Task fallback. Planning,
research, decomposition, review, and repair can still be requested through the
monitor API/CLI for ad hoc work. Ordinary workflow progression does not require
labels for each step. Direct Tasks remain available through the API/CLI.

## Verification

`make repository-monitor-fake-e2e` covers signed intake, permission and scope
checks, replay, pause events, issue-to-PR execution, automatic repair, and readiness
against isolated GitHub fixtures. These tests do not claim live model execution.
See [Repository monitors](repository-monitors.md) for credentials and runtime setup.
