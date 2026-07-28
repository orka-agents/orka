# Agent Substrate Deploy — Troubleshooting

- `... requires substrate to be enabled`: controller missing
  `--substrate-enabled=true`.
- `bootstrap token secret name is required`: set
  `--substrate-bootstrap-token-secret-name` and create that Secret in the Task
  namespace **and** the `ActorTemplate` namespace.
- `ActorTemplate ... missing label/annotation` / `not Orka-compatible`: the
  template needs `orka.ai/execution-workspace: "true"`,
  `orka.ai/workspace-provider: substrate`, the daemon-port/protocol/staging-root
  annotations, and must run `/orka-workspace-agent` from the agent harness
  wrapper image. See the ActorTemplate contract in the concept doc.
- `ActorTemplate ... is not Ready`: inspect Substrate `WorkerPool`, snapshot
  config, image pulls, and `runsc` configuration.
- Direct workspace-agent exec returns exit code 1 with empty output: confirm the
  installer applied the reviewed compatibility patch. It adds
  `CAP_SETUID`/`CAP_SETGID` and makes only the workspace-agent rootfs traversable
  after the UID/GID 1000 credential drop. The workspace agent stays a root
  supervisor; do not work around a capability or rootfs-permission mismatch by
  running task commands as root.
- Task `Failed` with `WorkspaceCleanupFailed` after `resultRef.available=true`:
  command + result succeeded but Substrate failed to checkpoint/delete the actor
  (a known pinned-revision `runsc delete` flake in GitHub-hosted kind). Inspect
  `atelet` / `ateom-gvisor` logs.
- Inspect Substrate directly:
  `kubectl --context "$ctx" -n ate-system get pods`,
  `kubectl --context "$ctx" -n ate-demo get workerpool,actortemplate`,
  `kubectl --context "$ctx" -n ate-system logs deployment/atenet-router`.
- Full troubleshooting matrix: `website/docs/concepts/substrate.md`.
