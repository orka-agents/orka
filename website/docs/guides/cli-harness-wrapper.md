# CLI harness wrapper

The CLI harness runtime is the agent execution path for CLI-backed runtimes. It lets Orka speak the `orka.harness.v1` turn protocol while the underlying runtime can still run as a normal per-turn subprocess.

Agent tasks use the harness protocol around the CLI invocation:

```text
Task -> harness provider -> CLI harness wrapper -> CLI subprocess per turn -> harness frames -> Orka execution events
```

The first implementation is **observed mode**. Orka observes command lifecycle, stdout/stderr previews, cancellation, and terminal result frames. It does not broker the runtime's internal tool calls unless a future runtime adapter exposes a native/brokered protocol.

## What is supported

- `GET /v1/health`
- `GET /v1/capabilities`
- `POST /v1/turns`
- `GET /v1/turns/{turnID}/events?afterSeq=N`
- `POST /v1/turns/{turnID}/cancel`

The wrapper emits harness frames such as:

- `TurnStarted`
- `RuntimeLog`
- `RuntimeOutput`
- `TurnCompleted`
- `TurnFailed`
- `TurnCancelled`

The CLI subprocess is **per turn**. The wrapper process can stay up and accept more turns, but it does not make opaque CLIs resident. RuntimeSession persistence and resident process reuse are future work.

## Runtime adapters

Current adapters:

- `generic` — simple command adapter for tests and bring-your-own commands.
- `codex` — adapter for the Codex CLI using Orka runtime settings.
- `claude` — adapter for the Claude Code CLI using Orka runtime settings.
- `copilot` — adapter for GitHub Copilot through the Copilot SDK helper subprocess.

## Generic command example

Run the wrapper locally:

```bash
ORKA_HARNESS_WRAPPER_RUNTIME=generic \
ORKA_HARNESS_WRAPPER_COMMAND=/bin/sh \
ORKA_HARNESS_WRAPPER_ARGS='["-c","cat; printf done > /tmp/result.txt"]' \
ORKA_HARNESS_WRAPPER_RESULT_MODE=file \
ORKA_HARNESS_WRAPPER_RESULT_FILE=/tmp/result.txt \
ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE=/var/run/secrets/orka-wrapper/token \
go run ./cmd/orka-agent-harness-wrapper --listen-addr=:8080
```

Prompt delivery modes:

- `stdin` (default)
- `env` via `ORKA_HARNESS_WRAPPER_PROMPT_ENV` (default `ORKA_TURN_PROMPT`)
- `file`, which writes a per-turn prompt file and passes its path in the prompt env var

Result extraction modes:

- `stdout` (default)
- `file` via `ORKA_HARNESS_WRAPPER_RESULT_FILE`

## Kubernetes configuration

Agent tasks always use the harness runtime path. The first provider path targets an operator-configured wrapper HTTP endpoint (for example, a trusted Service, sidecar, port-forwarded local wrapper, or test server). Configure the controller with `ORKA_HARNESS_WRAPPER_ENDPOINT`; tasks select agents normally:

```yaml
apiVersion: orka.io/v1alpha1
kind: Task
metadata:
  name: codex-wrapper-smoke
spec:
  type: agent
  agentRef:
    name: codex-agent
  prompt: "Inspect the repository and summarize it."
  agentRuntime:
    maxTurns: 20
    allowBash: true
```

Operators must set `ORKA_HARNESS_WRAPPER_ENDPOINT=<trusted wrapper URL>` on the controller for agent tasks. If the wrapper requires bearer auth (the default), configure the same auth value for the controller with `ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE` or `ORKA_HARNESS_WRAPPER_BEARER_TOKEN`.

## Compatibility notes

- The runtime emits harness turn frames first; the controller maps those frames into normal Orka execution events.
- The selected endpoint is operator-configured with `ORKA_HARNESS_WRAPPER_ENDPOINT`; tasks do not supply runtime endpoints.
- Completed turns are retained in wrapper memory only for the configured retention TTL, so controller recovery must resume promptly or the task fails instead of silently duplicating work.
- Terminal result frames are size-bounded to stay within the harness SSE envelope. Results that exceed the limit fail explicitly instead of being stored partially.
- `codex`, `claude`, `copilot`, and `opencode` all run through the wrapper; there are no per-runtime agent-worker images.
- Durable execution workspaces are validated but not yet launched by the harness runtime path.

The focused test matrix covers generic command success, result-file extraction, non-zero failure, timeout, cancellation including process-group cleanup, redaction, event-stream cursoring, harness conformance, Codex fake-CLI success/failure, and controller routing through the harness runtime.

## Event mapping

Harness-backed agent tasks emit Orka execution events from harness turn frames. A successful observed-mode turn normally produces task lifecycle events plus runtime events such as:

- `AgentRuntimeStarted`
- `AgentRuntimeCommandStarted`
- `ModelMessage` when stdout/stderr or model output previews are observed
- `AgentRuntimeCompleted`
- `TaskSucceeded`

The controller persists the task result from the harness `TurnCompleted` frame. Harness-backed agent tasks do **not** currently emit the worker-side `ResultSubmitted` event used by the older worker HTTP recorder path.

## Live validation checklist

For post-merge or release validation, use a real cluster with the harness wrapper Deployment and an in-cluster model proxy such as Vekil. Recommended coverage:

1. Verify `GET /v1/health` and `GET /v1/capabilities` on `svc/orka-agent-harness-wrapper`.
2. Verify `POST /v1/turns` rejects missing or bad bearer tokens. Health and capabilities are intentionally public.
3. Run a real `codex` Agent task through the wrapper and Vekil.
4. Run a real `claude` Agent task through the wrapper and Vekil when a Claude-compatible model is exposed.
5. Exercise workspace clone/read with a public repository and assert `AgentRuntimeCompleted` plus `TaskSucceeded` events.
6. Fork a harness-backed task and verify checkpoint context plus the fork task result.
7. Exercise direct generic-wrapper cancellation and deadline timeout; expected terminal frames are `TurnCancelled` and `TurnFailed` respectively.
8. Verify unsafe workspace URLs such as `https://localhost/...` fail closed.
9. When a GitHub token is available, run a temporary branch push and PR creation/cleanup flow against a disposable branch.

A validated real-world flow is: Orka harness-backed `codex` task clones Orka, writes a temporary validation file, pushes `live/harness-wrapper-pr-validation-*`, opens a real GitHub PR, verifies the diff, then closes the PR and deletes the branch.

## Cancellation

`CancelTurn` cancels the turn context. For subprocess adapters, the wrapper sends `SIGTERM` to the process group, waits the configured grace period, then sends `SIGKILL`. This is designed to stop child processes as well as the direct CLI process. The event stream ends with `TurnCancelled` when cancellation wins, or `TurnFailed` for timeouts and non-zero exits.

## Security notes

- Mutating turn endpoints (`POST /v1/turns`, event streams for a turn, and cancel) require a bearer token unless `ORKA_HARNESS_WRAPPER_ALLOW_UNAUTHENTICATED=true` is explicitly set for local-only tests. Health and capabilities endpoints are intentionally public for readiness discovery.
- Wrapper frames must not include raw environment dumps, API keys, TxTokens, service-account tokens, cookies, or authorization headers.
- Stdout/stderr previews are redacted and truncated before becoming frames.
- Prompt-file mode uses per-turn temp files and removes wrapper-created temp files after the command is parsed.
- Observed-mode wrappers cannot prove what an opaque CLI did internally; use least-privilege credentials and runtime sandboxing exactly as for CLI-backed runtimes.
- GitHub/LLM credentials belong in Kubernetes Secrets and must not be logged. For live PR-flow tests, create temporary branches/PRs and close/delete them after validation.

## Substrate relationship

Substrate is not required. The wrapper speaks normal HTTP+SSE and can run as a Kubernetes Service, sidecar, local test server, or future Substrate provider target.
