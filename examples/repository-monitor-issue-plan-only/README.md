# RepositoryMonitor issue plan-only workflow

Use this example when you want durable triage/research/planning and approval records but do **not** want Orka to implement code.

Apply with:

```bash
kubectl apply -k examples/repository-monitor-issue-plan-only
```

Then add `orka:plan` to an issue and inspect:

```bash
orka monitor actions list issue-plan-only --kind issue --number <issue-number>
```

Create referenced Secrets/Agents as needed. This example includes the `issue-researcher` Agent manifest, which expects a `claude-runtime-credentials` Secret.
