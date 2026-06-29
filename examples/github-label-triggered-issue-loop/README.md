# GitHub label-triggered issue-to-PR loop

This example configures a `RepositoryMonitor` for the durable `orka:*` workflow:

```text
issue label -> command event -> triage/research/plan -> approval -> implementation -> PR -> exact-head review -> repair/readiness
```

## Secrets

Create these Secrets outside git:

```bash
kubectl create secret generic git-credentials \
  --from-literal='token=<github-token>'

kubectl create secret generic codex-runtime-credentials \
  --from-literal='OPENAI_API_KEY=<openai-api-key>'

kubectl create secret generic claude-runtime-credentials \
  --from-literal='ANTHROPIC_API_KEY=<anthropic-api-key>'
```

Configure your GitHub webhook to send `issues` and `pull_request` events to `/webhooks/github` with the controller webhook secret.

## Try it

1. Update `repoURL` in `repository-monitor.yaml`.
2. Apply the example: `kubectl apply -k examples/github-label-triggered-issue-loop`.
3. Add `orka:plan` or `orka:implement` to an issue.
4. Inspect state:

```bash
orka monitor commands list issue-to-pr-loop
orka monitor actions list issue-to-pr-loop --kind issue --number <issue-number>
orka monitor issues list issue-to-pr-loop
```

Automerge is intentionally disabled in this example. Enable `spec.automerge.enabled` only after validating review, CI, and merge-gate behavior in your environment.
