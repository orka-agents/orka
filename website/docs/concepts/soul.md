---
slug: /soul
description: "Versioned Agent persona for AI workers and built-in harness v2 runtimes."
---

# Agent soul (SOUL.md)

`Agent.spec.soul` supplies persistent persona and communication defaults, separate
from the Agent's role instructions and a Task's assignment. It supports AI worker
Tasks and all four built-in harness v2 runtimes: Codex, Claude, Copilot, and
OpenCode. External `runtimeRef` Agents, including harness v1 registrations, do not
support this field.

A soul is configuration, not durable memory or an authorization policy. Tool,
workspace, publication, credential, and approval controls remain authoritative.
There is no automatic repository `SOUL.md` discovery or autonomous soul editor.

## Inline persona

Add a soul to an otherwise configured Agent:

```yaml
spec:
  systemPrompt:
    inline: Review the supplied change for actionable correctness defects.
  soul:
    inline: |
      # Reviewer
      Be direct, skeptical, and respectful.
      Distinguish demonstrated bugs from suspicions.
      Lead with the most consequential finding.
      State what you could not verify.
```

Exactly one source is required when `soul` is present. Text must be nonempty UTF-8
without NUL bytes and no larger than **8192 bytes**. Oversized or invalid sources
are rejected, not truncated. The composed role and persona prompt must also fit
the selected runtime's delivery limits. Omitted souls preserve existing prompt
composition.

For AI Tasks, `Task.spec.ai.systemPrompt` retains its existing precedence over the
Agent role prompt; it does not remove a separately configured soul.

## ConfigMap-backed SOUL.md

Publish the Markdown as a ConfigMap key in the **Agent's namespace**. Prefer a
new immutable ConfigMap for each revision. Set `soul.digest` to `sha256:` followed
by the lowercase SHA-256 of the exact UTF-8 file bytes, including its final newline.
For example, compute it with `sha256sum SOUL.md` before publishing the file.

```yaml
spec:
  soul:
    configMapRef:
      name: reviewer-soul-v1
      key: SOUL.md
    digest: sha256:<replace-with-the-64-character-file-digest>
```

The expected digest is mandatory for ConfigMap sources and optional for inline
text. Missing sources/keys and digest mismatches fail closed. Deleting and
recreating a ConfigMap under the same name cannot silently publish different
persona text while the Agent's expected digest remains unchanged.

The digest proves content identity, not human approval or publisher identity.
Existing namespace and resource-write permissions still apply. Do not put secrets,
credentials, raw transcripts, or private user profiles in persona configuration.

## Runtime delivery

| Execution path | Delivery |
| --- | --- |
| AI worker | Literal-safe system prompt environment, checked against a controller-owned Task binding |
| Codex v2 | Native developer instructions |
| Claude v2 | Native ACP system-prompt metadata |
| Copilot v2 | Private `copilot-instructions.md`, additive to native system instructions |
| OpenCode v2 | Explicit native instruction-file entry, preserving the stock provider preamble |

Copilot and OpenCode instruction files live outside `Task.spec.workspace`. The
supervisor creates them before spawning the child, after ordinary ownership
finalization. The intended child can read but cannot modify, unlink, or replace
these files or their protected ancestors. Runtime state remains writable. Workspace validation restores these protections
before thawing a retained child. A cold
runtime recreates these files from frozen controller configuration, not workspace
contents.

**Copilot transport restriction:** the pinned CLI processes native `@file`
imports. This implementation conservatively rejects `@` characters in the
composed Agent instructions rather than loading mutable dependencies outside the
configuration digest. Inline the desired text without import references. Native
repository instructions may still be loaded by the CLI; a soul does not disable
those sources or create an enforced model-instruction priority boundary.

## Revisions, retries, and Sessions

For ACP Tasks, the effective prompt is part of the existing immutable execution
snapshot and runtime-profile digest. Already-bound Tasks retain those inputs.
An incompatible Session configuration is a terminal error, not a capacity retry.

Soul-enabled AI Tasks record only Agent identity and content digests in `status.soulBinding`;
no persona text enters that status. Each new Job attempt must resolve the same
Agent revision and composed prompt. Missing or changed inputs reject the retry
rather than silently switching persona. An AI Task that started without a soul
cannot acquire one during a retry or autonomous iteration; create a new Task to
enable it. Dollar syntax in soul-enabled AI Task and
system prompts is transported literally, without Kubernetes environment expansion.

For AI Tasks using Sessions, conversation continuity pins controller-authored
revision metadata under the exact Task's Session lock **before the first Job
starts**. A soul-enabled turn pins its digest; a turn without a soul pins explicit
absence, so even an empty turn or `append: false` cannot allow a later Task to add
its first persona to that Session. The pin uses existing Session transcript
storage, is hidden from transcript reads and message counts, and survives Task
cleanup without a new SQLite schema or a separate persona store. Canonical turns
also carry the same revision. A later transcript or result-write failure, or an
empty initial turn, cannot erase the Session identity.
The pin is not rolled back if later Job creation fails or the Task is cancelled.
Transient API, Session-store, and pin-write failures retry reconciliation without
consuming an execution attempt; invalid configuration and revision drift still
fail closed. A new AI soul Session must append its initial turn; an established
Session can subsequently be used without appending. Legacy conversations without
a soul cannot acquire one in-place. For soul-enabled Sessions, removing or changing
the soul or effective role/Agent revision requires a new Session.

An explicit no-soul pin records **absence only**. Sessions without `spec.soul`
retain their legacy Agent/role-change behavior; the absence pin prevents later
soul opt-in without introducing full revision pinning for those Sessions.
Gateway-owned conversations retain the same rule through their canonical terminal
projection, independently of queued future user messages. Explicitly attested
pre-binding, pre-execution soul-configuration errors remain visible in history but
do not establish an intentional no-soul revision; correcting the source can still
establish the first revision. Ambiguous legacy failures are not reinterpreted.
Event retention preserves an established digest-only identity anchor while removing
expired content; the anchor is hidden from transcript reads and deleted with the
Session.

Keep the original Agent object and spec unchanged for existing **soul-enabled**
Sessions. Agent UID and generation participate in their configuration identity:
reverting text or recreating an Agent does not restore its earlier identity.
Publish a new Agent and source revision for new soul-enabled Sessions instead.

Deploy matching controller and runtime images before enabling this feature; do not
expect an older controller to interpret new Agent fields.

## Changes and governance

Use authorized operator or GitOps updates to publish new revisions. An Agent may
suggest a change through the existing proposal workflow, but this feature does
not automatically apply proposals to Agent configuration. Proposal acceptance is
not itself proof of authenticated human approval. Protect configuration writers
according to the deployment's trust model; adding a soul does not grant workers
new Kubernetes or publication permissions.
