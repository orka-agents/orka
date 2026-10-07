# RepositoryMonitor PR review and repair

This example focuses on durable PR review, automatic bounded repair, and GitHub-native auto-merge.

```bash
kubectl -n orka-system apply -k examples/repository-monitor-pr-review-repair
```

Useful commands:

```bash
orka -n orka-system monitor commands create pr-review-repair --kind pull_request --number '<pr>' --intent review --target-sha '<head-sha>'
orka -n orka-system monitor commands create pr-review-repair --kind pull_request --number '<pr>' --intent fix --target-sha '<head-sha>'
orka -n orka-system monitor commands create pr-review-repair --kind pull_request --number '<pr>' --intent update_branch --target-sha '<head-sha>'
```

Require the `orka/<namespace>/<monitor-name>/ready` commit status. The forge credential needs commit-status write permission; no GitHub App is required. GitHub native auto-merge owns merging; Orka never enables it. When disabled, the PR remains open and ready.

## Secrets

This monitor writes: it pushes a branch and opens or updates a pull request. Orka keeps the
four jobs on separate GitHub tokens so a leak of one does not grant the others, and it
refuses to run if any two of them are the same Secret. Create all four outside git:

```bash
kubectl -n orka-system create secret generic orka-monitor-source-read \
  --from-literal=token='<read-only token for the source repository>'

kubectl -n orka-system create secret generic orka-monitor-publication-read \
  --from-literal=token='<read-only token for the target repository>'

kubectl -n orka-system create secret generic orka-monitor-publication \
  --from-literal=token='<token that may push branches to the target repository>'

kubectl -n orka-system create secret generic orka-monitor-forge \
  --from-literal=token='<token for GitHub API calls: PRs, comments, permission checks>'
```

Each Secret needs a non-empty `token`, `password`, or `GITHUB_TOKEN` key. The ACP runtime
never receives any of them — only the controller and the Workspace/Publisher resolve them.

This example includes `pr-reviewer` and `pr-repairer` Agent manifests. The built-in Claude
and Codex ACP runtimes obtain provider credentials through the controller-managed provider
proxy, so those Agents carry no `secretRef`.
