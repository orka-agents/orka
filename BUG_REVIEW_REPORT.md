# Orka Project — Comprehensive Bug Review Report

**Date:** 2026-02-16 (updated 2026-02-17)  
**Methodology:** 3 independent review passes (Security-focused, Correctness-focused, Reliability-focused) across 7 project subareas by Claude Opus 4.6 (fast), plus 2 additional full-codebase review passes by GPT 5.3 Codex and Gemini 3 Pro. 23 total review passes executed.  
**Reviewers:** Claude Opus 4.6 (fast) × 3 lenses, GPT 5.3 Codex × 1, Gemini 3 Pro × 1.

---

## Table of Contents

1. [Executive Summary](#executive-summary)
2. [Convergence Analysis — All 3 Reviewers Agree](#convergence-analysis)
3. [Findings by Area](#findings-by-area)
   - [Area 1: API Types & CRDs](#area-1-api-types--crds)
   - [Area 2: Controllers](#area-2-controllers)
   - [Area 3: REST API Layer](#area-3-rest-api-layer)
   - [Area 4: LLM Providers](#area-4-llm-providers)
   - [Area 5: Tools Subsystem](#area-5-tools-subsystem)
   - [Area 6: Workers](#area-6-workers)
   - [Area 7: Store & CLI](#area-7-store--cli)
4. [Full Bug Inventory](#full-bug-inventory)
5. [GPT 5.3 Codex Review](#gpt-53-codex-review)
6. [Gemini 3 Pro Review](#gemini-3-pro-review)
7. [Cross-Model Convergence Analysis](#cross-model-convergence-analysis)

---

## Executive Summary

| Severity | Count |
|----------|-------|
| **Critical** | 12 |
| **High** | 38 |
| **Medium** | 52 |
| **Low** | 40 |
| **Total distinct findings** | ~142 |

The most urgent issues cluster around:
- **SSRF vulnerabilities** (webhook URLs, tool URLs, base URLs — no validation)
- **Path traversal** in `file_read` tool (prefix check bypass + symlink bypass)
- **Double-wrapped ProviderError** in OpenAI provider (breaks all retry/fallback logic)
- **Session lock leak** when `Append=false`
- **Race conditions** in code execution (fixed filenames) and HTTP client (shared timeout mutation)
- **Missing namespace isolation** on several API endpoints and CRD references

---

## Convergence Analysis

### Issues where ALL 3 reviewers independently identified the same bug:

These represent the highest-confidence findings. When security, correctness, and reliability reviewers all flag the same issue independently, it warrants immediate attention.

#### CRITICAL CONVERGENCE (All 3 agree — Fix immediately)

| # | Bug | Area | Severity | Impact |
|---|-----|------|----------|--------|
| **C1** | **Double-wrapped `ProviderError` loses HTTP status code** | LLM Providers | Critical | Once OpenAI Responses API mode is cached, every error loses its status code. Retry logic (`IsRetryable`), fallback logic (`IsProviderDown`), cooldown marking (429), and context-too-long detection ALL break silently. The provider appears to succeed when it fails. |
| **C2** | **`file_read` path traversal via `strings.HasPrefix`** | Tools | Critical | `/tmpevil/data` passes the `/tmp` check. Any file readable by the worker process can be read from directories sharing a prefix with allowed paths. |
| **C3** | **Race condition: fixed script filenames in `code_exec`** | Tools | Critical | All Python scripts write to `script.py`, all JS to `script.js`. Concurrent tool calls overwrite each other's files — one agent executes another's code. |
| **C4** | **Shared `http.Client.Timeout` mutation in `ToolExecutor`** | Tools/Workers | Critical | `e.client.Timeout` is mutated per-request on a shared client. Sequential calls inherit stale timeouts; concurrent calls have a data race. |
| **C5** | **SSRF via webhook URL (no validation)** | Controllers | Critical | `task.Spec.WebhookURL` accepts any URL. Can probe `169.254.169.254`, `kubernetes.default.svc`, etc. from the controller. |
| **C6** | **SSRF via tool health check URL** | Controllers | Critical | Same SSRF vector as webhooks — `tool.Spec.HTTP.URL` used directly in HTTP requests. |
| **C7** | **Session lock leaked when `Append=false`** | Controllers | Critical | Lock is acquired but only released inside `if Append == true`. Tasks with `Append=false` permanently lock the session until the Task object is deleted. |
| **C8** | **Agent loop premature exit on `StopReason` after tool calls** | Workers | Critical | After executing tools, the code checks `StopReason` against the OLD response. If a provider returns `"stop"` alongside tool calls, the loop exits with pre-tool content, discarding all tool results. |

#### HIGH CONVERGENCE (All 3 agree — Fix soon)

| # | Bug | Area | Severity | Impact |
|---|-----|------|----------|--------|
| **H1** | **`context.TODO()` in `JobBuilder`** | Controllers | Medium | Two API calls in fallback provider resolution bypass context cancellation, tracing, and timeout. Calls continue even after reconciler shutdown. |
| **H2** | **Unbounded `io.ReadAll` on tool HTTP responses** | Tools/Workers | High | External tool endpoints can return multi-GB responses, causing OOM. No `io.LimitReader`. |
| **H3** | **Cross-namespace `SecretRef` not enforced** | API/Controllers | High | `TaskSpec.SecretRef.Namespace` allows cross-namespace secret access but has no `EnforceNamespaceIsolation` check, unlike `AgentRef` and `ProviderRef`. |
| **H4** | **Provider enum mismatch (`azure-openai` missing)** | API Types | Medium | `ProviderType` allows `azure-openai` but `ModelConfig.Provider` and `AISpec.Provider` enums don't include it. CRD validation rejects valid configurations. |
| **H5** | **`bool` + `omitempty` + `default=true` pattern** | API Types | Medium | `SessionReference.Append` and `AgentCLIRuntime.DefaultAllowBash` cannot be set to `false` — the value is dropped by `omitempty` and restored to `true` by CRD defaults. |
| **H6** | **Anthropic provider drops system messages** | LLM Providers | High | `buildMessages()` has no `case "system":`. The truncation note message is silently lost when using Anthropic. |
| **H7** | **No ownership check in `delete_agent`** | Tools | High | Any agent in the namespace can be deleted, not just agents created by the current task. |
| **H8** | **`cancel_task` skips parent check when env var empty** | Tools | High | When `ORKA_TASK_NAME` is empty, any task in the namespace can be cancelled. |
| **H9** | **Shallow clone (`--depth=1`) breaks `GitRef` checkout** | Workers | High | Specific commit SHAs cannot be fetched from a depth-1 single-branch clone. |
| **H10** | **Double diff after `git add -A`** | Workers | High | Both cached and unstaged diffs are collected after staging everything, producing duplicate/corrupt patches. |
| **H11** | **Missing context propagation in GitHub HTTP helpers** | Tools | Medium | All GitHub API calls use `http.NewRequest` without context — ignoring cancellation, deadlines, and tracing. |
| **H12** | **`SubPath` allows path traversal** | Workers | Medium | `workspaceDir + "/" + cfg.SubPath` with no sanitization. `../../etc` escapes the workspace. |
| **H13** | **`loadPlanContext` uses `http.DefaultClient` (no timeout)** | Workers | High | HTTP call with zero timeout can block the worker indefinitely. |
| **H14** | **No signal handling in AI/general workers** | Workers | High | `context.Background()` with no signal listener. SIGTERM doesn't propagate, causing unclean shutdown. |
| **H15** | **JSON injection in tool error handling** | REST API | High | `fmt.Sprintf('{"error":"%s"}', err.Error())` — unescaped error strings can inject arbitrary JSON. |
| **H16** | **`HandleCancelChat` missing namespace isolation** | REST API | Medium | Users can cancel chat sessions in other namespaces. |
| **H17** | **Token cache grows unboundedly** | REST API | High | `sync.Map` for token hashes has no eviction. Expired entries only removed on re-access. |
| **H18** | **`GetMessages` mark-read is not transactional** | Store | High | SELECT + UPDATE not atomic. Crash between ops causes silent message loss. |
| **H19** | **Missing HTTP client timeout in CLI** | CLI | High | `http.DefaultClient` + `context.Background()` = CLI commands can hang forever. |
| **H20** | **Token in CLI positional argument** | CLI | Medium | Visible in `ps aux` and shell history. |
| **H21** | **Copilot auto-approves all permission requests** | Workers | High | `OnPermissionRequest` returns `"approved"` unconditionally with no policy check. |
| **H22** | **Temperature not set in OpenAI streaming methods** | LLM Providers | Medium | Streaming calls ignore the `Temperature` parameter, always using the model default. |
| **H23** | **Stream goroutine leaks on context cancellation** | LLM Providers | High | Channel sends in proxy goroutines block forever when consumer stops reading. |
| **H24** | **Webhook retry has no limit or backoff** | Controllers | Medium | Failed webhooks requeue every 30s forever. No retry cap. |
| **H25** | **Semaphore released before SSE completes** | REST API | Critical | `MaxConcurrent` limiter releases as soon as `HandleChat` returns, not when the SSE closure finishes. Concurrency limit is non-functional for SSE. |
| **H26** | **CI check `total_count=0` treated as "all passed"** | Tools | High | PRs with no CI checks configured are merged without any verification. |
| **H27** | **`SaveResult` uses `INSERT OR REPLACE` resetting `created_at`** | Store | Critical | The original creation timestamp is lost on every upsert. |

---

## Findings by Area

### Area 1: API Types & CRDs

| Reviewer | Bugs Found | Critical | High | Medium | Low |
|----------|-----------|----------|------|--------|-----|
| Security (A) | 11 | 0 | 4 | 4 | 3 |
| Correctness (B) | 9 | 0 | 0 | 3 | 6 |
| Reliability (C) | 10 | 0 | 1 | 5 | 4 |

**Top issues:**
- Cross-namespace `SecretRef`/`PriorTaskRef` without isolation enforcement
- SSRF risk in `WebhookURL` and `Tool.HTTP.URL` (no validation)
- `bool` + `omitempty` + `default=true` makes `false` values unrepresentable
- Provider enum inconsistency (`azure-openai` missing from Agent/Task)
- `ConfigMapRef` system prompt silently ignored at job build time
- `ChildTasks` status has no `MaxItems` — etcd size risk
- No admission webhooks — invalid resources accepted

### Area 2: Controllers

| Reviewer | Bugs Found | Critical | High | Medium | Low |
|----------|-----------|----------|------|--------|-----|
| Security (A) | 15 | 2 | 4 | 6 | 3 |
| Correctness (B) | 15 | 2 | 4 | 6 | 3 |
| Reliability (C) | 23 | 3 | 4 | 9 | 7 |

**Top issues:**
- SSRF via webhook and tool health check URLs
- Session lock leaked when `Append=false`
- Job name reuse on retry (stale job race)
- `context.TODO()` bypasses cancellation in `JobBuilder`
- Azure OpenAI config never passed to worker
- Agent TTL never fires (no Task watch)
- `handleScheduledTask` bypasses `failTask` (incomplete failure state)
- Nil dereference in `handleAutonomousIteration`
- Webhook response body not drained (connection pool exhaustion)

### Area 3: REST API Layer

| Reviewer | Bugs Found | Critical | High | Medium | Low |
|----------|-----------|----------|------|--------|-----|
| Security (A) | 12 | 1 | 3 | 4 | 4 |
| Correctness (B) | 16 | 0 | 2 | 7 | 7 |
| Reliability (C) | 13 | 1 | 4 | 4 | 4 |

**Top issues:**
- Missing namespace isolation on internal read endpoints
- JSON injection in tool error formatting
- CORS allows all origins (`*`)
- OpenAI-compat handler ignores namespace isolation
- Semaphore released before SSE callback completes
- Detached `context.Background()` in JSON-mode tool loop
- No SSE heartbeats (proxy timeout risk)
- No graceful shutdown for SSE connections
- SystemPromptBuilder cache never hits (per-request instance)

### Area 4: LLM Providers

| Reviewer | Bugs Found | Critical | High | Medium | Low |
|----------|-----------|----------|------|--------|-----|
| Security (A) | 8 | 0 | 1 | 3 | 4 |
| Correctness (B) | 10 | 1 | 0 | 5 | 4 |
| Reliability (C) | 10 | 1 | 3 | 3 | 3 |

**Top issues:**
- Double-wrapped `ProviderError` loses HTTP status code (all error classification breaks)
- Goroutine leaks in stream proxies on context cancellation
- System messages silently dropped by Anthropic provider
- Temperature not set in streaming methods
- `FallbackProvider.Stream()` missing `ShouldFallback`/`ShouldRetry` guard
- `shortestCooldown()` loses model override
- `TracingProvider.Stream()` not instrumented
- Cooldown 5x exponential is too aggressive

### Area 5: Tools Subsystem

| Reviewer | Bugs Found | Critical | High | Medium | Low |
|----------|-----------|----------|------|--------|-----|
| Security (A) | 12 | 1 | 5 | 4 | 2 |
| Correctness (B) | 14 | 2 | 2 | 4 | 6 |
| Reliability (C) | 14 | 2 | 6 | 3 | 3 |

**Top issues:**
- Path traversal via prefix check in `file_read`
- Symlink bypass in `file_read`
- Fixed script filenames in `code_exec` (race condition)
- Shared `http.Client.Timeout` mutation (data race)
- No ownership check in `delete_agent`
- `cancel_task` parent check skipped when env var empty
- URL interpolation without encoding in `ToolExecutor`
- Unbounded stdout/stderr buffers in `code_exec`
- CI `total_count=0` treated as "all passed"

### Area 6: Workers

| Reviewer | Bugs Found | Critical | High | Medium | Low |
|----------|-----------|----------|------|--------|-----|
| Security (A) | 17 | 3 | 5 | 6 | 3 |
| Correctness (B) | 13 | 1 | 3 | 5 | 4 |
| Reliability (C) | 15 | 2 | 4 | 6 | 3 |

**Top issues:**
- Agent loop premature exit on `StopReason` after tool calls
- Shallow clone breaks `GitRef` checkout
- Double diff after `git add -A`
- No signal handling in AI/general workers
- `loadPlanContext` with no timeout
- Copilot auto-approves all permissions
- Secret leakage via environment inheritance
- SubPath path traversal
- Unbounded message/memory growth in agent loop

### Area 7: Store & CLI

| Reviewer | Bugs Found | Critical | High | Medium | Low |
|----------|-----------|----------|------|--------|-----|
| Security (A) | 9 | 0 | 1 | 4 | 4 |
| Correctness (B) | 13 | 1 | 4 | 5 | 3 |
| Reliability (C) | 14 | 1 | 3 | 5 | 5 |

**Top issues:**
- `SaveResult` resets `created_at` on upsert (`INSERT OR REPLACE`)
- Token file not whitespace-trimmed (phantom auth failures)
- `GetMessages` mark-read not transactional
- Task list `--status` filter applied after `--limit`
- `AcquireLock` conflates "not found" with "locked"
- No HTTP client timeout in CLI
- `LoadTranscript` returns oldest messages instead of newest
- SSE parser doesn't handle multi-line `data:` fields
- Tracker first render corrupts terminal output

---

## Full Bug Inventory

### Critical (12)

1. **ProviderError double-wrapping** — `internal/llm/openai/provider.go` — Breaks all retry/fallback/cooldown
2. **`file_read` path traversal** — `internal/tools/file_read.go` — `HasPrefix` without directory boundary
3. **`code_exec` fixed filenames** — `internal/tools/code_exec.go` — Race condition on concurrent exec
4. **Shared `http.Client.Timeout` mutation** — `internal/worker/tool_executor.go` — Data race
5. **SSRF via webhook URL** — `internal/controller/webhook.go` — No URL validation
6. **SSRF via tool health check** — `internal/controller/tool_controller.go` — No URL validation
7. **Session lock leak (`Append=false`)** — `internal/controller/task_controller.go` — Permanent lock
8. **Agent loop premature exit** — `workers/ai/main.go` — Discards tool results on `StopReason`
9. **Job name reuse on retry** — `internal/controller/job_builder.go` — Connects to stale job
10. **SSE semaphore released early** — `internal/api/chat.go` — Concurrency limit broken
11. **`SaveResult` resets `created_at`** — `internal/store/sqlite/result_store.go` — Data loss
12. **`GetMessages` TOCTOU race** — `internal/store/sqlite/message_store.go` — Non-atomic read

### High (38)

1. Cross-namespace `SecretRef` not enforced
2. Cross-namespace `PriorTaskRef` not enforced
3. SSRF in `WebhookURL` (no scheme validation in CRD)
4. SSRF in `Tool.HTTP.URL` (no scheme validation in CRD)
5. `ConfigMapRef` system prompt silently ignored
6. Azure OpenAI config never passed to worker
7. `handleScheduledTask` bypasses `failTask`
8. Agent TTL never fires
9. Nil dereference in `handleAutonomousIteration`
10. Shell injection in init container command
11. Full secret exposed via `EnvFrom`
12. Missing namespace isolation on internal read endpoints (REST API)
13. JSON injection in tool error handling
14. CORS allows all origins
15. OpenAI-compat handler ignores namespace isolation
16. Detached `context.Background()` in JSON-mode chat
17. Token cache unbounded growth
18. Log streaming no timeout
19. No SSE heartbeats (proxy timeout)
20. No graceful shutdown for SSE connections
21. Goroutine leaks in stream proxies
22. Stream drain blocks indefinitely
23. Anthropic drops system messages
24. `delete_agent` no ownership check
25. `cancel_task` parent check bypassed
26. URL interpolation without encoding
27. CI `total_count=0` = "all passed"
28. Copilot auto-approves all permissions
29. Path traversal via symlink in `file_read`
30. Shallow clone breaks `GitRef`
31. Double diff after `git add -A`
32. No signal handling in AI/general workers
33. `loadPlanContext` no timeout
34. Unbounded `io.ReadAll` on tool responses
35. Token file not trimmed
36. Task list `--status` after `--limit`
37. `AcquireLock` conflates errors
38. No HTTP client timeout in CLI

### Medium (52) and Low (40)

See detailed per-area sections above.

---

## GPT 5.3 Codex Review

**Model:** GPT 5.3 Codex  
**Scope:** Full independent codebase review across all 7 areas.

### Severity Summary

| Severity | Count |
|----------|-------|
| **Critical** | 2 |
| **High** | 5 |
| **Medium** | 3 |
| **Low** | 1 |
| **Total** | 11 |

### Critical Findings

| # | Bug | File(s) | Impact | Prior Review Agreement |
|---|-----|---------|--------|------------------------|
| **G1** | **`file_read` path traversal / directory breakout** | `internal/tools/file_read.go` | `filepath.Clean` + `strings.HasPrefix` is bypassable (`/workspace2/...`) and does not resolve symlinks; enables arbitrary file reads outside intended roots. | ✅ Agree (C2) |
| **G2** | **Agent workspace `subPath` traversal** | `workers/agent/claude/main.go`, `workers/agent/copilot/main.go` | `ORKA_WORKSPACE_SUBPATH` is concatenated directly into `cmd.Dir` (`"/workspace/"+subPath`) without boundary validation; `../` can escape workspace and access mounted secrets/files. | 🆕 **NEW** |

### High Findings

| # | Bug | File(s) | Impact | Prior Review Agreement |
|---|-----|---------|--------|------------------------|
| **G3** | **`code_exec` fixed shared script filenames** | `internal/tools/code_exec.go` | Always writes `script.py`/`script.js`/`script.sh` in shared workdir; concurrent calls overwrite each other. | ✅ Agree (C3) |
| **G4** | **Shared `http.Client.Timeout` mutation** | `internal/worker/tool_executor.go` | `Execute` mutates `e.client.Timeout`; timeout leaks across requests and is race-prone. | ✅ Agree (C4) |
| **G5** | **SSRF via webhook + tool health check** | `internal/controller/webhook.go`, `internal/controller/tool_controller.go` | User-controlled URLs requested by controller without private-network/egress restrictions. | ✅ Agree (C5, C6) |
| **G6** | **Session lock leak (`Append=false`)** | `internal/controller/task_controller.go` | Lock acquired for any `SessionRef` but only released when `Append` is true; session permanently locked. | ✅ Agree (C7) |
| **G7** | **OpenAI provider error double-wrapping** | `internal/llm/openai/provider.go` | `completeResponses` already returns `ProviderError`, but `Complete` wraps again with `toProviderError`, losing status code/classification. | ✅ Agree (C1) |

### New Bugs Not in Prior Review

| # | Bug | File(s) | Severity | Impact |
|---|-----|---------|----------|--------|
| **GN1** | **Agent workspace `subPath` traversal** | `workers/agent/claude/main.go`, `workers/agent/copilot/main.go` | Critical | Direct path concatenation without boundary validation allows workspace escape. |
| **GN2** | **OpenAI-compat detached context** | `internal/api/openai_compat.go` | Medium | `context.Background()` in request/stream paths ignores client cancellation/timeouts. |
| **GN3** | **Non-atomic message mark-read** | `internal/store/sqlite/message_store.go` | Medium | SELECT + per-row UPDATE outside transaction can duplicate deliveries. |
| **GN4** | **CLI port-forward signal handler leak** | `cmd/cli/helpers.go` | Low | Repeated `signal.Notify` registrations without `signal.Stop`. |

### Disagreements with Prior Review

| Prior Finding | GPT 5.3 Codex Opinion |
|---|---|
| **H3: Cross-namespace `SecretRef` not enforced** (High) | **Disagree on severity.** Current code paths mount/read secrets by name in the task namespace; `secretRef.namespace` field is effectively ignored. The issue is API/schema ambiguity (correctness), not a confirmed cross-namespace secret exfiltration path. Should be **Medium**. |
| **C8: Agent loop premature exit on `StopReason`** (Critical) | **Disagree on severity.** Logic is brittle, but with current provider mappings, tool-call responses use `tool_calls`/`tool_use` stop reasons, so this is not clearly exploitable as a live defect today. Should be **High** at most. |

---

## Gemini 3 Pro Review

**Model:** Gemini 3 Pro  
**Scope:** Full independent codebase review across all 7 areas.

### Severity Summary

| Severity | Count |
|----------|-------|
| **Critical** | 5 |
| **High** | 4 |
| **Medium** | 4 |
| **Total** | 13 |

### Critical Findings

| # | Bug | File(s) | Impact | Prior Review Agreement |
|---|-----|---------|--------|------------------------|
| **M1** | **Cross-namespace `SecretRef` allows mounting secrets from any namespace** | `api/v1alpha1/task_types.go`, `internal/controller/job_builder.go` | Privilege escalation — tasks can access secrets outside their namespace. | ✅ Agree (H3, elevated to Critical) |
| **M2** | **Agent loop checks `StopReason` on previous response** | `workers/ai/main.go` | Discards tool results if the LLM returns stop in the same turn as tool calls. | ✅ Agree (C8) |
| **M3** | **SSRF via unvalidated webhook URLs** | `internal/controller/webhook.go` | Controller makes HTTP requests to arbitrary user-supplied URLs. | ✅ Agree (C5) |
| **M4** | **SSRF via unvalidated tool health check URLs** | `internal/controller/tool_controller.go` | Same SSRF vector as webhooks. | ✅ Agree (C6) |
| **M5** | **`code_exec` fixed filenames race condition** | `internal/tools/code_exec.go` | Concurrent agents overwrite each other's scripts. | ✅ Agree (C3) |

### High Findings

| # | Bug | File(s) | Impact | Prior Review Agreement |
|---|-----|---------|--------|------------------------|
| **M6** | **`file_read` path traversal via `strings.HasPrefix`** | `internal/tools/file_read.go` | `/workspace-secret` bypasses `/workspace` check. | ✅ Agree (C2) |
| **M7** | **Session lock leak (`Append=false`)** | `internal/controller/task_controller.go` | Permanent session lock. | ✅ Agree (C7) |
| **M8** | **Shared `http.Client.Timeout` mutation** | `internal/worker/tool_executor.go` | Data race and state leak between tool executions. | ✅ Agree (C4) |
| **M9** | **OpenAI `ProviderError` double-wrapping** | `internal/llm/openai/provider.go` | Breaks retry/fallback/cooldown logic. | ✅ Agree (C1) |

### New Bugs Not in Prior Review

| # | Bug | File(s) | Severity | Impact |
|---|-----|---------|----------|--------|
| **MN1** | **SSRF via unvalidated environment variables** | `internal/tools/web_search.go`, `internal/tools/send_message.go` | High | Tools use URLs directly from env vars (e.g., `SEARCH_API_URL`) without validation; attackers who can influence env vars can redirect traffic to internal endpoints. |
| **MN2** | **Unbounded token cache growth** | `internal/api/auth.go` | Medium | `tokenCache` (`sync.Map`) never removes entries unless accessed after expiry. An attacker can flood the API with random invalid tokens to cause memory leak (DoS). |
| **MN3** | **Status update conflicts silently ignored** | `internal/controller/task_controller.go` | Medium | Controller ignores errors from `Status().Update()`, leading to lost state updates under high concurrency. |
| **MN4** | **Cross-namespace `PriorTaskRef` leak** | `internal/controller/job_builder.go` | Medium | `PriorTaskRef` allows cross-namespace references passed to workers without validation. |

### Disagreements with Prior Review

Gemini 3 Pro **agreed with all prior findings** and had no disagreements. Notably, it **elevated** H3 (Cross-namespace SecretRef) from High to **Critical**.

---

## Cross-Model Convergence Analysis

### Issues where ALL 5 reviewers agree (Claude Opus × 3 + GPT 5.3 Codex + Gemini 3 Pro):

| Bug | Claude Opus | GPT 5.3 Codex | Gemini 3 Pro | Consensus Severity |
|-----|:-----------:|:-------------:|:------------:|:------------------:|
| `file_read` path traversal | Critical | Critical | High | **Critical** |
| `code_exec` fixed filenames | Critical | High | Critical | **Critical** |
| Shared `http.Client.Timeout` mutation | Critical | High | High | **Critical/High** |
| SSRF via webhook URL | Critical | High | Critical | **Critical** |
| SSRF via tool health check | Critical | High | Critical | **Critical** |
| Session lock leak (`Append=false`) | Critical | High | High | **Critical/High** |
| OpenAI `ProviderError` double-wrapping | Critical | High | High | **Critical/High** |

### Severity disagreements across models:

| Bug | Claude Opus | GPT 5.3 Codex | Gemini 3 Pro | Notes |
|-----|:-----------:|:-------------:|:------------:|-------|
| Cross-namespace `SecretRef` | High | **Medium** (disagrees) | **Critical** (elevates) | GPT sees it as schema ambiguity; Gemini sees privilege escalation |
| Agent loop `StopReason` exit | Critical | **High** (disagrees) | Critical | GPT notes current providers don't trigger this in practice |

### New bugs found only by additional reviewers:

| Bug | Found By | Severity |
|-----|----------|----------|
| Agent workspace `subPath` traversal (agent workers) | GPT 5.3 Codex | Critical |
| SSRF via env var URLs (`web_search`, `send_message`) | Gemini 3 Pro | High |
| Unbounded token cache growth | Gemini 3 Pro | Medium |
| Status update conflicts silently ignored | Gemini 3 Pro | Medium |
| Cross-namespace `PriorTaskRef` leak | Gemini 3 Pro | Medium |
| OpenAI-compat detached context | GPT 5.3 Codex | Medium |
| Non-atomic message mark-read | GPT 5.3 Codex | Medium |
| CLI port-forward signal handler leak | GPT 5.3 Codex | Low |

---

## Recommended Prioritization

### Immediate (P0 — Fix this week)

1. **ProviderError double-wrapping** — One-line fix, unlocks entire retry/fallback chain
2. **`file_read` path traversal** — Add directory boundary check (`allowedPath + "/"`)
3. **Session lock leak** — Move `ReleaseLock` outside the `Append` check
4. **JSON injection** — Use `json.Marshal` instead of `fmt.Sprintf`
5. **`SaveResult` INSERT OR REPLACE** — Switch to `ON CONFLICT DO UPDATE`

### Short-term (P1 — Fix within 2 weeks)

6. **SSRF validation** — Add URL blocklist for webhooks and tool health checks
7. **Fixed filenames in `code_exec`** — Use `os.CreateTemp`
8. **Shared `http.Client.Timeout`** — Create per-request client
9. **Agent loop `StopReason`** — Remove dead check after tool execution
10. **Cross-namespace enforcement** — Add checks for `SecretRef` and `PriorTaskRef`
11. **Provider enum** — Add `azure-openai` to Agent/Task enums
12. **SSE semaphore** — Move acquisition into the SSE closure

### Medium-term (P2 — Fix within 1 month)

13. **Admission webhooks** — Validate CRDs at admission time
14. **Goroutine leak prevention** — Add `ctx.Done()` checks on channel sends
15. **Anthropic system messages** — Map to user messages
16. **Namespace isolation** — Add to all REST API endpoints
17. **Signal handling** — Add to AI and general workers
18. **CLI security** — Token from stdin, HTTP→HTTPS warning

---

*Report generated by automated multi-pass code review across 3 models: Claude Opus 4.6 (fast) × 3 lenses, GPT 5.3 Codex × 1, Gemini 3 Pro × 1. All findings should be validated by the development team before remediation.*
