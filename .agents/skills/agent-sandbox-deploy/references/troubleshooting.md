# Agent Sandbox Deploy — Troubleshooting

- **Workspace Task fails closed before allocation:** confirm the controller has
  `--enable-workspace-provider-api`, `--acp-workspace-dispatch-enabled`,
  class-use admission, and Task provenance admission; confirm the class is
  admitted, requires `acp.runtime.v2`, and references a provider registration
  with a fresh heartbeat. Remove any legacy `--agent-sandbox-*` arguments; the
  controller no longer accepts them.
- **Provider registration has no supported contracts:** check the provider
  Deployment logs, its pinned `SandboxProviderConfig` UID, and that the shared
  bundle and provider CRDs come from the orka-workspace revision in `go.mod`.
- **ImagePullBackOff on sandbox, router, or provider pods:** the `localhost:5001`
  registry is missing or not wired as a containerd mirror — see the registry
  precondition.
- **Inner agent CLI connection refused:** verify DNS/TCP reachability to the
  model/proxy base URL from inside the Sandbox Pod and that the admitted
  NetworkPolicies allow it.
- Full design and failure behavior: `website/docs/concepts/agent-sandbox.md`.
