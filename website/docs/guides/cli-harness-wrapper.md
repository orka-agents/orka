# CLI harness wrapper

The CLI harness wrapper is an opt-in bridge for existing agent CLIs. It lets Orka speak the `orka.harness.v1` turn protocol while the underlying runtime still runs as a normal per-turn subprocess.

Legacy agent workers remain the default path:

```text
Task -> Job -> agent worker -> CLI subprocess -> result POST -> Job exits
```

Wrapper mode uses the harness protocol around the CLI invocation:

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

- `generic` — runs a configured command.
- `codex` — proof-of-bridge adapter for the Codex CLI using the same high-level argument contract as the legacy Codex worker.
- `claude` and `copilot` — reserved names, rejected at wrapper startup until adapters are implemented. Legacy workers remain the path for these runtimes.

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

## Kubernetes opt-in

Legacy workers are still the default. The controller only uses the harness wrapper path when the operator enables the feature gate and the task opts in with annotations. The first provider path targets an operator-configured wrapper HTTP endpoint (for example, a trusted Service, sidecar, port-forwarded local wrapper, or test server). Configure the controller with `ORKA_HARNESS_WRAPPER_ENDPOINT` and then opt in individual tasks with annotations:

```yaml
apiVersion: orka.io/v1alpha1
kind: Task
metadata:
  name: codex-wrapper-smoke
  annotations:
    orka.ai/harness-wrapper: "true"
    orka.ai/harness-wrapper-runtime: "codex"
spec:
  type: agent
  agentRef:
    name: codex-agent
  prompt: "Inspect the repository and summarize it."
  agentRuntime:
    maxTurns: 20
    allowBash: true
```

Operators must set `ORKA_ENABLE_HARNESS_WRAPPER=true` and `ORKA_HARNESS_WRAPPER_ENDPOINT=<trusted wrapper URL>` on the controller before these annotations take effect. If the wrapper requires bearer auth (the default), configure the same auth value for the controller with `ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE` or `ORKA_HARNESS_WRAPPER_BEARER_TOKEN`. With the gate disabled, agent tasks continue to use their existing legacy worker images and job path.

## Legacy compatibility notes

The wrapper path is intended to be behaviorally compatible with the legacy agent worker path, with these MVP differences:

- The wrapper emits harness turn frames first; the controller maps those frames into normal Orka execution events.
- The selected endpoint is operator-configured with `ORKA_HARNESS_WRAPPER_ENDPOINT`; the Task only opts in and selects the runtime metadata.
- Completed turns are retained in wrapper memory only for the configured retention TTL, so controller recovery must resume promptly or the task fails instead of silently duplicating work.
- Terminal result frames are size-bounded to stay within the harness SSE envelope. Results that exceed the limit fail explicitly instead of being stored partially.
- `claude` and `copilot` wrapper adapters are not enabled yet; their legacy workers remain the supported path.

The focused test matrix covers generic command success, result-file extraction, non-zero failure, timeout, cancellation including process-group cleanup, redaction, event-stream cursoring, harness conformance, Codex fake-CLI success/failure, and feature-gated controller routing.

## Cancellation

`CancelTurn` cancels the turn context. For subprocess adapters, the wrapper sends `SIGTERM` to the process group, waits the configured grace period, then sends `SIGKILL`. This is designed to stop child processes as well as the direct CLI process. The event stream ends with `TurnCancelled` when cancellation wins, or `TurnFailed` for timeouts and non-zero exits.

## Security notes

- Turn, event-stream, and cancel endpoints require a bearer token unless `ORKA_HARNESS_WRAPPER_ALLOW_UNAUTHENTICATED=true` is explicitly set for local-only tests.
- Wrapper frames must not include raw environment dumps, API keys, TxTokens, service-account tokens, cookies, or authorization headers.
- Stdout/stderr previews are redacted and truncated before becoming frames.
- Prompt-file mode uses per-turn temp files and removes wrapper-created temp files after the command is parsed.
- Observed-mode wrappers cannot prove what an opaque CLI did internally; use least-privilege credentials and runtime sandboxing exactly as with legacy workers.

## Substrate relationship

Substrate is not required. The wrapper speaks normal HTTP+SSE and can run as a Kubernetes Service, sidecar, local test server, or future Substrate provider target.
