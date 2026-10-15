# aimock fixtures for the E2E suite

When `E2E_OPENAI_API_KEY` or `E2E_ANTHROPIC_API_KEY` is unset, the suite
deploys [aimock](https://github.com/CopilotKit/aimock) into the test namespace
and points that provider at it. Set `E2E_DISABLE_MOCK_LLM=1` to skip those
specs instead. aimock loads every `*.json` file here in name order, and the
first matching fixture wins, so `zz-catch-all.json` must stay last.

Conventions:

- One file per spec file, named after it (`tools.json` for `tools_test.go`).
- Every scripted prompt carries a unique `[e2e:<case>]` marker, and its
  fixtures match on it with `userMessage`. Do not reuse a marker across specs.
- Script multi-step tool loops with stateless matchers (`hasToolResult`,
  `turnIndex`, `toolCallId`, `toolResultContains`). The suite sets
  `AIMOCK_STRICT_TURN_INDEX=1`, so `turnIndex` matches only its exact turn. Do
  not use `sequenceIndex`: its server-side counter breaks on reruns and retries.
- When a runtime rehydrates a Session (after a controller restart, Pod
  replacement, or scale to zero), Orka sends the prior transcript inside the
  new user message, so it carries every earlier marker too. Put fixtures for
  later turns before those for earlier turns in the file.
- Steps that must name a generated resource (a delegated child Task, a created
  Agent) are injected at run time with `injectMockLLMFixtures` once the name is
  known, while the conversation is parked in a scripted wait.
- Final turns return non-empty text. An empty answer makes the AI worker send a
  retry prompt that no longer carries the marker.
- In mock mode (`e2eMockOpenAI`), specs assert on what reached the model with
  `expectMockLLMToolResult` and `expectMockLLMServed` from `mock_llm_test.go`.

The fake forge publication, agent runtime matrix, and agent runtime lifecycle
specs deploy a second aimock with these same fixtures as `vekil-system/vekil`,
the upstream of the ACP provider proxy, so the built-in Codex, OpenCode,
Claude runtimes follow the scripts too (`fake_forge.json`,
`agent_runtime_matrix.json`, `agent_runtime_lifecycle.json`). A fixture's
`latency` (milliseconds) holds a prompt in flight, which the timeout, cancel,
and controller restart checks use instead of a real `sleep`.
Read its requests with `providerProxyMockLLM.requestsFor`.

`native_session_migration.json` runs in the `agent-runtime` lane on Linux.
It imports a supported Codex 0.160.0 paginated source through the public CLI,
continues on the deployed Codex RuntimePool, exports through the CLI, and
executes `codex exec resume` in a fresh local home using the pinned binary
from the tested runtime image. Continuation fixtures match their user markers, not `toolResultContains`
(which checks only the final message). Journal assertions require both legs
to send the original native tool result to aimock; the nonce is absent from prompts and assistant replies.
The return leg also proves the cluster reply reaches inference, the UUID
and native prefix survive, the source stays untouched, and newly supplied
local credentials and read-only policy are used. The non-reasoning mock model
`gpt-4.1` avoids aimock synthesizing encrypted reasoning, which native migration
deliberately rejects. No live provider key is needed.

The fixture format is documented in aimock's
[write-fixtures skill](https://github.com/CopilotKit/aimock/blob/v1.43.0/skills/write-fixtures/SKILL.md).

`hyperlight.json` drives the KVM-only deployment lane
(`scripts/hyperlight-kind-e2e.sh`, Ginkgo label `hyperlight`). Only the model
is mocked; the production workers, ACP harnesses, provider proxy, MCP tool and
Hyperlight guests are real. It covers:

- AI `code_exec` guest identity, stdout/stderr, denied sockets and the existing
  missing-bundle fail-closed control (no host fallback).
- OpenCode `orka_sandbox_exec` on a read-intent workspace (the pinned
  `octocat/Hello-World` clone): the first guest reads it and sees each write
  refused with `EROFS`, and a second guest finds it unchanged. Native Bash is
  absent from the actual model-facing catalog, and a forced Bash call returns
  a real rejection without creating a host marker.
- Claude `mcp__orka__sandbox_exec` over Anthropic Messages. The policy allows
  native `Read` and `sandbox_exec`, but disables native shell. The fixture
  forces an excluded `Bash` call, requires its real rejection result, executes
  a guest whose writes to the read-intent workspace are refused, reads the
  repository's README with the allowed native tool, and executes another guest
  that finds no probe files. The journal must omit native command tools.
- Copilot exclusive command mode is rejected before dispatch (see below).

The first Claude guest also tries to plant bounded workspace hook and
unmanaged MCP configs before the allowed native read; the read-only mount must
refuse each file. The guests check that the forced native command created no
host-side marker and that no unmanaged tool appeared in the model catalog.
Cross-guest workspace persistence is covered by the KVM `hyperlight_e2e` tests
in `workers/acp/supervisor`. Native read/edit capabilities are not claimed to
run in the micro-VM.

Fixtures use deterministic call IDs and stateless last-result matchers, never
server-side sequence counters. aimock 1.43.0 normalizes both Anthropic and
Responses requests into the chat-shaped journal used by the assertions.
Final canned replies are not evidence of execution. The lane requires
`/dev/kvm`, real device-plugin allocation, and no live provider credentials.

Codex's pinned Responses protocol uses a `mcp__orka` namespace for
`sandbox_exec`. Pinned aimock 1.43.0 does not emit that namespace on scripted
function calls and omits namespace catalog entries from its normalized
journal. Codex is therefore not represented as a passing or permanently
skipped deployment spec here. A separate pinned-runtime conformance probe can
exercise the actual namespace and Hyperlight callback without changing aimock;
that is **runtime conformance**, not full Orka/Kubernetes E2E.

Copilot exclusive command mode is rejected before provider dispatch because the
pinned CLI lacks verified immutable hook/unmanaged-MCP controls. There is no
Copilot guest-success fixture or skipped success spec. Ordinary Copilot behavior
is covered elsewhere. The separate Codex runtime conformance lane uses real KVM
and private namespace-aware fixture responses without upgrading aimock.
