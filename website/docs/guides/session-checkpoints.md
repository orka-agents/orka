---
slug: /session-checkpoints
description: "Opt-in saved context for long AI Tasks and fresh ACP runtimes."
---

# Session checkpoints

A Session stores conversation history across Tasks. Model context is the smaller
set of messages sent with one model request. A checkpoint is a short saved note
that carries the goal, constraints, findings, source references, and remaining
work into later requests. The exact current user request stays separate.

Checkpoint creation is opt-in for native `type: ai` Tasks. Fresh ACP runtimes can
consume an existing checkpoint from their authorized Session history. Checkpoints
stay in Session storage and are never promoted to durable memory automatically.
They cannot grant permission, prove that an action finished, or decide whether
repeating a tool is safe.

## Enable an AI Task

Use an appending Session and set the model allowance explicitly in `spec.env`:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: investigate-parser
spec:
  type: ai
  agentRef:
    name: researcher
  prompt: "Investigate the parser failure. Keep the public API unchanged."
  sessionRef:
    name: parser-investigation
    create: true
    append: true
    maxMessages: 50
  env:
    - name: ORKA_SESSION_CHECKPOINTS_ENABLED
      value: "true"
    - name: ORKA_AI_CONTEXT_WINDOW_TOKENS
      value: "32768"
```

Replace `32768` with an allowance supported by the selected model. Orka does not
infer model capabilities from names. Every configured fallback also needs
`ORKA_AI_FALLBACK_<index>_CONTEXT_WINDOW_TOKENS`, starting at index `0`.
Allowances must be between 1,024 and 2,000,000 tokens. The configured response
limit must leave room for input; the worker's default response reserve is 4,096
tokens.

Creation requires `append: true`, no `throughMessageID`, and
`promptIncluded: false`. The opt-in must be a literal `true` in `spec.env`;
the API also verifies it in the controller-created AI worker Job.
Gateway-owned and read-only Sessions cannot create
checkpoints through this worker path. A Task needs the controller URL and its
normal ServiceAccount identity, supplied by the controller.

## What the worker saves

The worker commits the current request before its first model call, each model
response before executing its tools, and each tool result before starting the
next tool. Stable message IDs make identical persistence retries idempotent.
Controller completion handling recognizes those IDs and avoids appending the
prompt or final answer again.

Large content gets an 8 KiB preview in the transcript and a link to separately
stored source JSON. Both writes commit together. Sources retain their message
roles and tool-call metadata. A checkpoint cites committed source IDs, and its
record includes the Session identity, format version, and last included message
ID. Execution events remain diagnostic data, not the history archive.

Storage redacts recognizable credential patterns from source data. The worker
tracks credentials loaded for providers and custom tools, alongside configured
environment values, and removes them from saved text and tool arguments.
When quote syntax is ambiguous, redaction may remove nearby text to avoid
keeping part of a credential. Checkpoint notes containing those values are rejected.
Source retrieval returns the saved, sanitized representation.
The active current request remains unchanged.

The worker may load a complete saved source to check for credentials split across
a history-page boundary. This check is limited to 2 MiB and retains one source
copy in the worker. The requested page data and cursor remain unchanged. If a
credential is learned after a page was saved, new checkpoints omit affected
fragments while retaining their source references. Copied and nested history
receipts are checked against their original source ranges regardless of message
role. Copies are validated before redaction and stored with their canonical
receipt fields. Transcript previews, including copied and cached history
receipts, are rebuilt from the complete, redacted source when newly loaded
credentials affect them.
A check follows at most eight source messages within ten seconds. Checkpoint
reference preparation shares one ten-second budget across its sources; cycles
and deeper chains fail closed.

| Item | Limit |
| --- | --- |
| Encoded source message | 2 MiB |
| Transcript content preview | 8 KiB |
| Stored checkpoint note | 8 KiB, format version 1 |
| Generated note | 6,000 UTF-8 bytes and 1,024 output tokens |
| Checkpoint generation and save | 20 seconds |
| Sources supplied per generation | 2 to 64, with non-user text excerpted at 2,048 bytes |
| Cited sources per note | 64 |
| Retained checkpoints | Latest 4 per Session |
| AI bootstrap | At most 64 recent messages and 128 KiB including the checkpoint |
| History read | 4,096 bytes by default, at most 16 KiB |
| History persistence/read request | 10 seconds, at most one identical retry |

Checkpoint generation also stops at its source-count and input-token caps. A
large bootstrap or many small exchanges can exceed 64 active sources before
the token trigger. In that case, the worker keeps a still-fitting request or
stops when reduction is required. It does not discard the unsummarized history.

Source data lives as long as its Session. Removing or expiring the Session also
removes its checkpoints and saved outputs. Recovery does not recreate a deleted
Session or bypass a pending Session cleanup.

## Request fitting and history reads

The worker attempts a checkpoint at 80% of the configured allowance. It counts
instructions, message content and framing, tool names and IDs, arguments, tool
schemas, checkpoint text, retrieved content, and reserved output. Estimation
uses roughly four UTF-8 bytes per token plus framing allowances. It is not a
provider tokenizer, so provider context-limit errors still receive one bounded
recovery attempt.

The worker commits a valid checkpoint before replacing older active history.
The next request keeps normal instructions, the exact current request, the
checkpoint as assistant reference material, and recent complete exchanges.
Tool calls and their results stay together. A smaller fallback is checked before
dispatch; if needed, the worker reduces context using that smaller allowance.
Inputs that cannot fit without losing required content fail with an explanation.

The opt-in worker advertises `read_session_history` automatically. Its arguments
are `message_id`, `offset`, and `limit`. Results contain the original role, a
`data` fragment of source JSON, `nextOffset`, and `totalBytes`. Offsets count
bytes and must fall on UTF-8 boundaries. Continue from `nextOffset` until it
equals `totalBytes` to reconstruct the source JSON. Retrieved text remains
reference material in a tool result.

Every read derives the namespace, Session, and last readable message from the
authenticated Task. Callers cannot select a different Session or advance the
boundary. Gateway ownership comes from the immutable admitted event.
`sessionRef.maxMessages` limits the recent suffix; `throughMessageID` defines
which history is readable. A checkpoint can cite older messages outside that
suffix, but none after the authorized boundary. Existing cross-Session search
restrictions remain in place.

## Failures and continuation

| Condition | Behavior |
| --- | --- |
| Source save fails or its receipt is uncertain | Stop before the next tool; retain committed history. Retry only the identical source write. |
| Proactive checkpoint creation/save fails | Keep the original context if it still fits. Stop if reduction is required and cannot be committed. |
| Bootstrap read fails | Stop instead of starting with an empty history. |
| Referenced history read fails | Return a tool error without replacing saved history. |
| Cancellation | Stop model/tool execution. Allow at most 10 seconds to persist a result already produced, while the owner fence remains valid. |
| Same Task restarts after committing model/tool history | Stop automatic replay. Inspect execution records and continue with a new Task in the same Session. |
| Owner expires, Task becomes terminal, or Session is deleted | Reject new context writes. |
| Stored history ends in an incomplete tool exchange | Stop for execution-record inspection; do not replay the calls. |

This path does not implement crash recovery or execution receipts. Those remain
separate execution-record responsibilities. Since repeated Jobs of one Task
are rejected once it has committed model/tool history, do not enable this option
for autonomous mode that deliberately continues the same Task across Jobs.

## ACP and tested support

When Orka reconstructs a saved Session for a fresh ACP runtime, it loads the
latest eligible checkpoint and includes it as an assistant JSONL reference
alongside recent saved messages. The current request remains separate. The
checkpoint must fit intact within the existing bootstrap byte and message caps;
otherwise dispatch fails before creating a Session turn. A Gateway request with
`promptIncluded` is excluded from the checkpoint's boundary. With
`maxMessages: 1`, there is no prior boundary to load and the checkpoint is omitted.

Controller tests cover continued write Tasks, controller restart, runtime
replacement, live reuse, allowed-history boundaries, and failure before dispatch.
Context reduction inside a live Codex, Claude, Copilot, OpenCode, or external ACP
runtime remains that runtime's responsibility. Orka uses only messages it has
actually stored. Private runtime transcripts and unreported intermediate tool
results are not reconstructed. ACP receives saved references but this change
does not add an ACP history-read MCP tool or ACP checkpoint generation.

AI tests use the common completion interface with deterministic providers,
including smaller fallback admission. No live provider or runtime quality,
latency, or cost claim has been validated. Caller-managed OpenAI/Anthropic
compatibility requests do not become persistent Sessions.

Chat also gets independent persistence and truncation fixes. New messages are
saved exactly once even when active context shrinks. Chat keeps the current
request and complete tool exchanges, but does not generate checkpoints.

## Reproduce the comparison

```bash
go test ./workers/ai -run '^TestSessionCheckpointLongTaskComparison$' -count=1 -v
```

The fixture starts with an older API constraint and a ruled-out cache cause,
adds long investigation history, then performs three reads with 66,000-byte
results under a 6,000-token model allowance. Its scripted model repeats the
cache investigation when that finding disappears. Its scripted summarizer
retains only facts present in supplied sources or the previous checkpoint.
The actual worker, SQLite persistence, HTTP client, and reduction paths run.

One local run produced the following results. Token counts are estimates,
including checkpoint calls, and elapsed time excludes fixture setup. Rejected
input is reported separately and is not a billable-token estimate.

| Measurement | Checkpoints disabled | Checkpoints enabled |
| --- | ---: | ---: |
| Constraints present in final model context | 1 of 2 | 2 of 2 |
| Repeated cache investigations | 3 | 0 |
| Estimated accepted input tokens | 6,671 | 29,982 |
| Estimated rejected input tokens | 59,255 | 0 |
| Estimated output tokens | 141 | 501 |
| Total model calls, including rejected requests | 11 | 8 |
| Extra checkpoint model calls | 0 | 4 |
| Context-limit rejections | 4 | 0 |
| Local elapsed time | 0.33 ms | 219.10 ms |

This demonstrates retained context and recoverable history, with extra accepted
tokens and storage work. The deterministic responses cannot establish how well
a real model summarizes or whether checkpoints lower cost or latency. Keep the
feature opt-in until representative provider workloads have been measured.
