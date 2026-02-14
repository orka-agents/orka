# Iterative Multi-Agent Coordination Plan

## Problem Statement

Orka supports basic multi-agent coordination (delegate → wait → read results), but lacks the tooling for **iterative code review loops**: a coordinator agent delegates coding, reviews the output, provides feedback, and iterates until the code is approved — then creates a PR.

## Proposed Approach

Enhance existing LLM-driven coordination tools to support iterative workflows. The coordinator (AI worker) orchestrates; specialist agents (CLI runtimes like Copilot/Claude Code) do the actual coding and reviewing. Code changes persist between iterations via **cumulative diff-in-result** (stored in SQLite, applied by the next worker via `git apply`).

### Full Flow

```
USER: "Deliver auth feature for the API"
  │
COORDINATOR (AI worker)
  │
  ├── delegate_task(agent="coder", prompt="Implement JWT auth",
  │                 workspace={gitRepo: "upstream/repo"})
  │     Coder: clones repo → writes code → git diff --binary --full-index
  │            → posts {summary, diff, baseSHA, files}
  │
  ├── wait_for_tasks → gets {summary, files} (diff stripped from LLM context)
  │
  ├── delegate_task(agent="reviewer", prompt="Review auth changes",
  │                 prior_task="coder-task-xyz")
  │     Reviewer: clones repo → applies coder's diff → reviews in full workspace
  │               → posts {verdict: "CHANGES_NEEDED", feedback: "Add rate limiting"}
  │
  ├── wait_for_tasks → gets {verdict: "CHANGES_NEEDED", feedback: "..."}
  │
  ├── delegate_task(agent="coder", prompt="Fix: add rate limiting",
  │                 prior_task="coder-task-xyz", feedback="Add rate limiting")
  │     Coder iter 2: clones repo → applies iter 1 cumulative diff
  │                   → fixes → new cumulative git diff → posts result
  │
  ├── (review again... repeat up to 3 times)
  │
  ├── delegate_task(agent="coder", prompt="Push and create PR",
  │                 prior_task="final-task",
  │                 workspace={gitRepo: "user/fork", forkRepo: "upstream/repo"})
  │     Coder: clones fork → applies final diff → git push → gh pr create
  │
  └── COMPLETE: "Auth feature delivered. PR: https://..."
```

## Changes Required

### Phase 1: API Types (Foundation)

**Files:** `api/v1alpha1/task_types.go`

- Add `PriorTaskRef` field to `TaskSpec` — references a previous task whose result (containing a diff) should be applied before starting work
  ```go
  // PriorTaskRef references a previously completed task whose diff should be
  // applied to the workspace before this task begins execution.
  PriorTaskRef *PriorTaskReference `json:"priorTaskRef,omitempty"`
  ```
- Add `PriorTaskReference` type:
  ```go
  type PriorTaskReference struct {
      Name      string `json:"name"`
      Namespace string `json:"namespace,omitempty"`
  }
  ```
- Add optional fork configuration to `WorkspaceConfig`:
  ```go
  // ForkRepo is the writable fork repository URL for pushing changes
  ForkRepo string `json:"forkRepo,omitempty"`

  // PRBaseBranch is the upstream branch to target for pull requests
  PRBaseBranch string `json:"prBaseBranch,omitempty"`
  ```
- Iteration tracking labels (convention, no type changes needed):
  - `orka.ai/iteration`: "1", "2", "3"...
  - `orka.ai/iteration-group`: groups all iterations of the same logical task

**Run:** `make manifests generate` after type changes.

### Phase 2: Structured Result Format

**Files:** `workers/common/result.go`

Define a structured result envelope that workers can optionally use:
```go
type StructuredResult struct {
    Version  int               `json:"version"`           // Always 1
    Summary  string            `json:"summary"`           // Human-readable description
    BaseSHA  string            `json:"baseSHA,omitempty"`  // Commit SHA the diff was generated against
    Diff     string            `json:"diff,omitempty"`     // Cumulative git diff (--binary --full-index)
    Verdict  string            `json:"verdict,omitempty"`  // "APPROVED" | "CHANGES_NEEDED"
    Feedback string            `json:"feedback,omitempty"` // Review feedback text
    Files    []string          `json:"files,omitempty"`    // List of changed files
    Metadata map[string]string `json:"metadata,omitempty"` // Extensible metadata
}
```

Key design points:
- `BaseSHA` pins the exact commit the diff was generated against — detects upstream drift between iterations
- `Verdict` is a strict two-value enum: `APPROVED` or `CHANGES_NEEDED` (non-blocking remarks go in `Feedback` alongside `APPROVED`)
- `Diff` is always **cumulative** — full `git diff` from clean checkout, not incremental patches
- Add `FormatStructuredResult()` helper to serialize
- Add `ParseStructuredResult()` helper to deserialize (with fallback: if result doesn't parse as JSON, treat as plain text summary)
- Backward compatible: plain text results still work everywhere

### Phase 3: Worker Diff Generation

**Files:** `workers/common/workspace.go` (new shared helper), all 3 workers

Add shared `FinalizeResult()` function in `workers/common/workspace.go`:
1. Run `git rev-parse HEAD` to capture base commit SHA
2. Run `git diff --binary --full-index` to capture all changes (handles binary files, renames, permission bits)
3. Run `git diff --stat` to extract changed file list
4. Format as `StructuredResult` with agent's text output as `Summary`, diff, baseSHA, files
5. Return serialized bytes for `common.SubmitResult()`

Each worker calls `common.FinalizeResult(workDir, agentOutput)` — one line change per worker.

### Phase 4: Worker Patch Application

**Files:** `workers/common/workspace.go`, all 3 workers

Add shared `PrepareWorkspace()` function in `workers/common/workspace.go`:
1. Check for `ORKA_PRIOR_TASK` and `ORKA_PRIOR_TASK_NAMESPACE` env vars
2. If not set, return immediately (no-op for non-iterative tasks)
3. Fetch the prior task's result from controller via HTTP (`GET /api/v1/tasks/{name}/result?namespace=...`)
4. If result is missing (prior task GC'd), **fail fast** with clear error: "prior task result not found: {name}"
5. Parse structured result to extract diff and baseSHA
6. Optionally verify current HEAD matches baseSHA (warn on drift, don't block)
7. Write diff to temp file, run `git apply --check <patch>` (dry run)
8. If dry run passes: run `git apply <patch>` (all-or-nothing, no `--reject`)
9. If dry run fails: **fail the task** with clear error message including which hunks conflicted
10. Clean up temp file

Each worker calls `common.PrepareWorkspace(workDir)` after git clone, before starting LLM — one line change per worker.

### Phase 5: Extend `delegate_task` with Iteration Support

**Files:** `internal/tools/delegate_task.go`

**No new tool.** Extend existing `delegate_task` with two optional parameters:

```go
type DelegateTaskArgs struct {
    // ... existing fields (agent, prompt, namespace, priority, workspace, maxTurns, allowBash) ...

    // PriorTask references a previously completed task whose diff should be
    // applied to the workspace before this task begins. Optional.
    PriorTask string `json:"prior_task,omitempty"`

    // Feedback provides review feedback to include in the task prompt.
    // Used with prior_task for iterative code review workflows. Optional.
    Feedback  string `json:"feedback,omitempty"`
}
```

Behavior when `prior_task` is set:
- Set `Spec.PriorTaskRef` on the child Task, pointing to the prior task
- If `feedback` is also set, prepend it to the prompt: `"FEEDBACK FROM REVIEW: {feedback}\n\nTASK: {prompt}"`
- Copy workspace config from the prior task (if not explicitly provided)
- Increment iteration label: read prior task's `orka.ai/iteration` label, add 1
- Set `orka.ai/iteration-group` label (copy from prior task, or generate new UUID for first iteration)

Update tool description to mention the optional iteration parameters so the LLM knows they exist.

### Phase 6: Job Builder Wiring

**Files:** `internal/controller/job_builder.go`

When building a Job for a Task that has `Spec.PriorTaskRef`:
- Inject `ORKA_PRIOR_TASK` env var with the referenced task name
- Inject `ORKA_PRIOR_TASK_NAMESPACE` env var (from PriorTaskRef.Namespace or task's own namespace)
- `ORKA_CONTROLLER_URL` is already injected for coordination tasks; ensure it's also injected for tasks with PriorTaskRef even if coordination is not explicitly enabled

### Phase 7: `wait_for_tasks` Enhancement

**Files:** `internal/tools/wait_for_tasks.go`

Enhance the result returned to the coordinator LLM:
- Parse structured results when available
- **Strip the `diff` field** — never send raw diffs to the coordinator LLM context
- Return: `{summary, verdict, feedback, files, baseSHA, iteration}` to the coordinator
- Include iteration number from `orka.ai/iteration` label
- Fall back to raw result string for plain-text results (backward compatible)

### Phase 8: Testing

- **Unit tests** for `delegate_task` iteration extensions (prior_task, feedback params)
- **Unit tests** for `ParseStructuredResult` / `FormatStructuredResult` (including plain-text fallback)
- **Unit tests** for `PrepareWorkspace` (mock HTTP for result fetch, mock git for apply)
- **Unit tests** for `FinalizeResult` (mock git diff/rev-parse)
- **Unit tests** for job builder PriorTaskRef env var injection
- **Unit tests** for `wait_for_tasks` structured result parsing + diff stripping
- **Integration tests** for the full iteration loop (mock K8s + controller)

### Phase 9: Documentation & Examples

- Update `docs/multi-agent-coordination.md`:
  - Add "Iterative Code Review" section with the full flow diagram
  - Document `prior_task` and `feedback` params on `delegate_task`
  - Document structured result format
  - Add recommended coordinator system prompt template for review loops
  - Document iteration labels
- Add example YAMLs in `examples/`:
  - `coordinator-agent.yaml` — coordinator with review loop system prompt
  - `coder-agent.yaml` — CLI runtime specialist for coding
  - `reviewer-agent.yaml` — CLI runtime specialist for code review
  - `iterative-task.yaml` — example task that triggers the full flow
- Update `CLAUDE.md` with new env vars (`ORKA_PRIOR_TASK`, `ORKA_PRIOR_TASK_NAMESPACE`)

#### Recommended Coordinator System Prompt Template (for docs)
```
You are a coordinator agent. Follow this protocol:

1. DELEGATE implementation to the coder agent.
2. WAIT for the coder's result.
3. DELEGATE review to the reviewer agent (pass prior_task = coder's task name).
4. WAIT for the reviewer's verdict.
5. IF verdict == "CHANGES_NEEDED" AND iteration < 3:
   DELEGATE fix to the coder agent with prior_task + feedback.
   Go to step 2.
6. IF verdict == "APPROVED":
   DELEGATE PR creation to the coder agent with prior_task.
7. Report final result.
```

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Iteration approach | LLM-driven (extend `delegate_task`) | More flexible than declarative DAG; coordinator adapts based on review feedback |
| Code persistence | Cumulative diff-in-result (SQLite) | Works without git write access; no infra requirements; each diff is self-contained |
| Diff format | `git diff --binary --full-index` | Handles binary files, renames, permission bits correctly |
| Patch application | `git apply --check` then `git apply` (no `--reject`) | All-or-nothing: either patch applies cleanly or task fails with clear error |
| Base SHA tracking | Stored in StructuredResult | Detects upstream drift between iterations |
| Review model | Separate reviewer agent | Separation of concerns; reviewer sees full workspace context |
| Verdict enum | `APPROVED` / `CHANGES_NEEDED` (two values) | Simple, unambiguous; non-blocking remarks go in feedback alongside APPROVED |
| Tool consolidation | Extend `delegate_task`, no separate `send_feedback` | Less tool sprawl; LLM doesn't need to choose between "start" vs "continue" |
| PR creation | Delegated to coder agent via workspace config | CLI runtimes already have bash + gh; no new tooling needed |
| Runtimes | Coordinator=AI worker, specialists=CLI (default) | AI worker has coordination tools; CLI runtimes better at coding |
| Max iterations | Default 3, configurable | Most code converges in 1-2 rounds |
| Result format | Backward-compatible envelope | Existing plain-text results continue to work |
| Missing prior result | Fail fast with clear error | Never silently skip patch application; explicit > implicit |
| Coordinator context | Strip diffs from `wait_for_tasks` response | Prevent context bloat; coordinator only needs summaries + verdicts |

## File Change Summary

| File | Change Type |
|------|-------------|
| `api/v1alpha1/task_types.go` | Add PriorTaskRef type, PriorTaskRef field, ForkRepo/PRBaseBranch to WorkspaceConfig |
| `internal/tools/delegate_task.go` | Add optional `prior_task` and `feedback` params with iteration logic |
| `internal/tools/wait_for_tasks.go` | Parse structured results, strip diffs, return summaries |
| `internal/controller/job_builder.go` | Inject ORKA_PRIOR_TASK env vars when PriorTaskRef is set |
| `workers/common/result.go` | Add StructuredResult type, FormatStructuredResult, ParseStructuredResult |
| `workers/common/workspace.go` | New file — PrepareWorkspace (patch apply) + FinalizeResult (diff gen) |
| `workers/agent/copilot/main.go` | Call PrepareWorkspace + FinalizeResult (one line each) |
| `workers/agent/claude/main.go` | Call PrepareWorkspace + FinalizeResult (one line each) |
| `workers/ai/main.go` | Call PrepareWorkspace + FinalizeResult (one line each) |
| `docs/multi-agent-coordination.md` | Add iterative workflow section, prompt template, examples |
| `CLAUDE.md` | Add new env vars, updated tool params |
| Tests for all new code | New test files |

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Large diffs (>10MB) | HTTP fetch limit exceeded | Most features produce <1MB. For massive changes, use git branch handoff (push to fork) |
| Patch conflicts | `git apply` fails, task fails | Fail fast with clear error. Coordinator can retry from scratch or instruct agent to resolve |
| Upstream drift | Base SHA mismatch between iterations | Store baseSHA in result; warn on drift so coordinator can decide to restart |
| Infinite loops | Coordinator loops endlessly | Default max 3 iterations + `maxIterations=50` LLM turn cap |
| Context bloat | Coordinator context exhausted across iterations | Strip diffs from `wait_for_tasks`; coordinator only sees summaries + verdicts |
| Prior result GC'd | Owner ref cascade deletes prior task + result | Fail fast with clear error: "prior task result not found" |
| Coordinator wall-clock timeout | Job deadline exceeded during multi-iteration loop | Document need for adequate `activeDeadlineSeconds` on coordinator jobs (recommend 1h+) |
| Cost accumulation | Multiple LLM sessions per iteration | Each iteration = 2-3 LLM sessions; document cost implications; max iterations limit bounds cost |
