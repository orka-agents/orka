# Orka source map

Topic → where the answer lives. Paths are relative to the Orka source root. Docs pages are
under `website/docs/`; ADRs under `docs/adr/`; types under `api/`. Chart values are in
`manifest_staging/charts/orka/values.yaml` (main) unless noted. ADR statuses are in
`docs/adr/README.md`; a Proposed, Deferred or not-yet-implemented ADR is a plan, not a
feature. Confirm each claim in the file itself; this map only says where to look.

| Topic | Docs (`website/docs/`) | Types, values, flags | ADRs and other |
| --- | --- | --- | --- |
| Vocabulary, components | `reference/glossary.md`, `concepts/architecture.md`, `overview.md` | | |
| Install, upgrade, release files | `operations/installation.md`, `operations/upgrading.md`, `reference/release-status.md`, `development/build-from-source.md`, `development/release-qualification.md` | `charts/orka/` at a tag = that release (main's copy is an old snapshot); `manifest_staging/charts/orka/` = main | GitHub Releases (`gh release view <tag>`) |
| CLI | `reference/cli-commands.md` (generated), `reference/cli.md` (install, workflows) | `cmd/`, `internal/cli/` | |
| REST and compatibility APIs | `reference/api-reference.md`, `reference/api-authorization.md`, `reference/execution-events.md`, `reference/openai-compat.md`, `reference/anthropic-compat.md`, `operations/shared-compatibility-api.md` | `internal/api/` | |
| Every resource field | `reference/resources.md` | `api/v1alpha1/*_types.go`, `api/gateway/v1alpha1/` | |
| Controller flags and Helm values | `reference/configuration.md` | `values.yaml`; `cmd/build/helmify/` (chart generator) | |
| Providers, models, native AI worker | `getting-started.md`, `reference/built-in-tools.md`, `guides/agents-skills-and-tools.md`, `concepts/soul.md` | `provider_types.go`, `agent_types.go`, `skill_types.go`, `tool_types.go` | |
| Coding-agent runtimes (Codex, Claude, Copilot, OpenCode) | `concepts/agent-runtimes.md`, `operations/provider-proxy.md`, `operations/agent-runtime-security.md`, `operations/harness-modes.md`, `guides/toolboxes.md`, `guides/codex-session-migration.md` | `task_types.go` (`AgentRuntimeType`), `runtime_pool_types.go` | `docs/adr/README.md` "Harness v1 → v2 migration" (superseded history); `.agents/skills/vekil-reverse-proxy-deploy` |
| Bring your own runtime | `guides/bring-your-own-agent-runtime.md`, `development/agent-runtime-adapter-contract.md` | `agent_runtime_crd_types.go` | ecosystem: agentkit, orka-runtime-foundry |
| Repository access and publication | `reference/resources.md` (Workspace), `concepts/security.md` | `task_types.go` (`WorkspaceConfig`) | |
| Execution workspaces (sandboxes) | `concepts/execution-workspaces.md`, `concepts/agent-sandbox.md`, `concepts/substrate.md` | `execution_types.go`; `workspace.orka.ai` types from the `orka-workspace` module in `go.mod` (schemas in `config/crd/bases/workspace.orka.ai_*.yaml`); flags in `reference/configuration.md` | ADRs 0003–0005, 0007 (Substrate control plane), 0014, 0015, 0021–0031; `docs/development/workspace-provider-authoring.md`; skills `agent-sandbox-deploy`, `agent-substrate-deploy`; repo orka-workspace |
| Container Tasks, schedules | `guides/container-tasks.md`, `guides/scheduled-tasks.md` | `task_types.go` (`Schedule`, `ConcurrencyPolicy`, `Suspend`) | |
| Multi-agent, autonomous, chat | `guides/multi-agent-coordination.md`, `guides/autonomous-tasks.md`, `guides/chat.md`, `guides/ui.md` | `agent_types.go` (`coordination`) | |
| Human approval | `guides/human-approval-v2.md`, `guides/autonomous-tasks.md` | CLI `orka task approvals/approve/decline` | |
| Memory governance | `concepts/memory.md`, `reference/built-in-tools.md` (memory tools), `operations/upgrading.md` (memory tool opt-in) | CLI `orka memory ...` | |
| Gateways (Teams, Telegram, A2A) | `guides/gateways.md`, `operations/gateways.md`, `reference/gateway-api.md` | `api/gateway/v1alpha1/` | ADRs 0012, 0013, 0020, 0032; `docs/development/gateway-protocol-v1.md`; adapter repos orka-gateway-teams, -telegram, -a2a |
| Agent hosts (AHP, copilotd) | `operations/agent-hosts.md` | `agenthost_types.go`; values `controller.agentHosts` | ADRs 0035, 0036 |
| Connectors, linked accounts, GitHub sign-in | `guides/connectors.md` | `connection_types.go`, `connector_provider_types.go` | ADRs 0033, 0037 (GitHub sign-in) |
| Repository automation | `guides/repository-monitors.md`, `guides/issue-to-pr-automation.md`, `guides/repository-security-scanning.md`, `guides/usage-and-pr-outcomes.md` | `repositorymonitor_types.go`, `repositoryscan_types.go` | ADR 0038 |
| Task webhooks | `guides/task-webhooks.md` | `task_types.go` (`webhookURL`) | ADR 0042 |
| Identity, transaction tokens, outbound access | `concepts/security.md`, `concepts/transaction-tokens.md`, `concepts/outbound-access.md`, `concepts/remote-mcp.md`, `guides/transaction-token-migration.md` | `outboundaccesspolicy_types.go`; values `controller.contextToken` | ADR 0011; repos orka-integration-kontxt, orka-integration-agentgateway |
| Telemetry and metrics | `guides/observability.md`, `reference/configuration.md` (telemetry) | `internal/tracing/genai`, `internal/metrics/metrics.go` | ADRs 0010, 0019 |
| Storage | `reference/configuration.md` (storage bindings), `operations/upgrading.md`, `operations/gateways.md` (backup) | values `store`, `storage` | `docs/development/storage-contracts.md`; issue #512 |
| Troubleshooting | `operations/troubleshooting.md`, `operations/runbook.md` | | |
| Developing and testing Orka | `development/development.md`, `development/testing.md` | `Makefile`, `scripts/` | `AGENTS.md`; skills `kindctl`, `orka-kind-deploy` |

Ecosystem repos version separately; the table in `website/docs/overview.md` names each one
and its owner (most are under `github.com/orka-agents/`). Read a README with
`gh api repos/<owner>/<repo>/readme --jq .content | base64 -d`.
