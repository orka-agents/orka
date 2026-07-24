# RepositoryMonitor PR review and repair

This example focuses on durable PR review, repair, branch update, and optional automerge command labels.

```bash
kubectl apply -k examples/repository-monitor-pr-review-repair
```

Useful commands:

```bash
orka monitor commands create pr-review-repair --kind pull_request --number <pr> --intent review --target-sha <head-sha>
orka monitor commands create pr-review-repair --kind pull_request --number <pr> --intent fix --target-sha <head-sha>
orka monitor commands create pr-review-repair --kind pull_request --number <pr> --intent automerge --target-sha <head-sha>
```

Automerge remains disabled by default. Set `spec.automerge.enabled: true` and configure the controller global merge gate only after validating CI and review gates.

This example includes `pr-reviewer` and `pr-repairer` Agent manifests. Create `claude-runtime-credentials`, `codex-runtime-credentials`, and `git-credentials` Secrets outside git before applying it.
