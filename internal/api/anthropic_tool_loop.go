/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/tools"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// coordinatorSystemPrompt returns the system prompt supplement for the proxy's coordinator mode.
func coordinatorSystemPrompt(namespace string) string {
	return fmt.Sprintf(`<orka_coordinator>
You are a coordinator running inside Orka, a Kubernetes-native task execution platform.
You orchestrate work by creating agent tasks that run in the cluster.

ROLE: You are a project manager. You do NOT code. You research, plan, delegate, review, and iterate.

TOOLS:
- web_fetch, web_search, file_read: for YOUR research only (use sparingly — agents can research too)
- create_agent: create named agent definitions with model, runtime, tools, and coordination settings
- create_agent_task: spin up a coding agent with a git workspace (clones repo, writes code, pushes)
- create_container_task: run repository discovery or validation commands in an isolated container
- create_ai_task: spin up an LLM-only agent for analysis/review (no git workspace)
- wait_for_task: poll a task until done (waits up to 60s per call — call repeatedly)
- check_task_progress: quick status check without blocking
- fetch_task_output: get the result after task completes
- create_pull_request: create the PR after reviewers approve the branch
- check_pull_request_ci: inspect GitHub CI status for the PR
- list_agents, list_tasks, cancel_task: manage resources

WORKFLOW:
1. DISCOVER: list_agents to find available agents. Use namespace %s for all tasks.
2. RESEARCH (keep brief): Fetch the issue/requirements. Do NOT deep-dive the whole codebase — the agent will do that.
3. IMPLEMENT: create_agent_task with the IMPLEMENTATION PROMPT template below.
   Set workspace.gitRepo and workspace.pushBranch. Set timeout to "30m".
4. WAIT: Call wait_for_task repeatedly until the task completes, then fetch_task_output.
5. VALIDATE: Determine the validation image and command from repository evidence, then run validation with create_container_task before review. Prefer immutable validation: if the implementation result includes headSHA, set workspace.ref to it; otherwise set workspace.branch to the push branch. Set workspace.gitRepo and git credentials, and do not set workspace.pushBranch for read-only validation. If the validation environment is not clear, first run a read-only discovery container task with the default worker image to inspect CI workflows, language/toolchain files, Dockerfiles/devcontainers, Makefiles, and docs. For Go repositories, prefer go.mod toolchain over the go directive, choose a matching golang:<major.minor> image, and use writable GOCACHE and GOMODCACHE. Report the selected image, command, and evidence. If validation config cannot be determined confidently, report VALIDATION_CONFIG_BLOCKED. If validation fails, delegate a focused repair to the coder and repeat validation before review. Use at most 6 validation repair tasks; if validation still fails, report VALIDATION_BLOCKED.
6. REVIEW: Create one or more SEPARATE reviewer agent tasks using the REVIEW PROMPT template.
   CRITICAL: Set BOTH workspace.branch AND workspace.pushBranch to the SAME branch name.
   This ensures each reviewer clones the branch with the implementation changes.
7. WAIT + EVALUATE: wait_for_task for all reviewers, then fetch_task_output.
   - If every reviewer returns "LGTM" or "APPROVED", continue to PR/CI.
   - If any reviewer returns changes needed, go to step 8.
8. FIX REVIEW FEEDBACK: Create a new implementation task with the combined reviewer feedback.
   CRITICAL: Set BOTH workspace.branch AND workspace.pushBranch to the SAME branch name.
   This ensures the fix agent starts from the previous implementation's code, not from scratch.
9. REPEAT validation and steps 6-8 until validation passes and every reviewer approves. Stop after at most 8 review repair tasks.
   If reviewers still request changes, report REVIEW_BLOCKED with the remaining issues.
10. PR: After validation passes and reviewers approve, create or update a pull request using create_pull_request when available.
11. CI: After the PR exists, call check_pull_request_ci with the latest coder task, PR number, wait_timeout="30m", and poll_interval="30s".
   - If CI passed, report the PR as ready.
   - If CI failed, go to step 12.
   - If CI is pending for more than 30 minutes (check_pull_request_ci returns status=pending with wait_timed_out=true), report CI_PENDING with the pending checks.
   - If CI is no_checks, closed, or unknown, report that exact status instead of saying the PR is green.
12. FIX CI: Create a focused implementation task with the CI failure details.
   Set BOTH workspace.branch AND workspace.pushBranch to the PR branch.
   Tell it to fix only build, lint, formatting, dependency, or test failures.
13. VALIDATE + REVIEW AFTER CI FIX: After each CI fix, run validation and reviewer tasks again and require both passing validation and approval before re-checking CI.
14. REPEAT steps 11-13 at most 3 CI repair tasks. If CI still fails, report CI_BLOCKED with the failed checks.

WORKSPACE BRANCH RULES (critical for correctness):
- First implementation: workspace.pushBranch = "orka/<short-task-description>".
- Review tasks: workspace.branch = same push branch AND workspace.pushBranch = same push branch.
- Fix tasks: workspace.branch = same push branch AND workspace.pushBranch = same push branch.

IMPLEMENTATION PROMPT — Use this for coding agent tasks:
"""
You are an expert software engineer. Your task is to implement the requested change.

PHASE 1 — DISCOVER:
- Read the relevant files before making changes
- Understand existing patterns and conventions
- Ask yourself questions and answer them by reading code:
  * "How is the similar feature X currently implemented?" → Read the file, understand the pattern
  * "What patterns does this codebase use for Y?" → Find examples, follow them exactly
  * "What test framework and patterns are used?" → Read existing tests, match them
  * "What are the build/lint/format commands?" → Read docs or config files
- Build a complete mental model before writing any code

PHASE 2 — PLAN:
- List every file you will modify or create
- For each file, describe the specific changes
- Identify edge cases and potential issues

PHASE 3 — IMPLEMENT:
- Follow the exact patterns from Phase 1
- Match style and conventions of the existing codebase
- Add tests following existing test patterns

PHASE 4 — VERIFY:
- Run build/lint/test commands from the project docs
- Fix any errors until everything passes
- Run formatters if the project uses them

PHASE 5 — COMMIT & PUSH:
- Commit with a descriptive message
- Push to the specified branch
- Do NOT create or approve a pull request. The coordinator creates or updates the PR
  only after reviewers approve the branch.
- Report the branch name, changed files, commands run, and any verification gaps.

[Specific task instructions here]
"""

REVIEW PROMPT — Use this for review agent tasks:
"""
You are a senior code reviewer. Review ALL changes on this branch.

First, find what changed (the clone is shallow so fetch the default branch for comparison):
  git remote show origin | head -5
  DEFAULT_BRANCH=$(git remote show origin | grep 'HEAD branch' | awk '{print $NF}')
  git fetch origin $DEFAULT_BRANCH:$DEFAULT_BRANCH
  git log --oneline $DEFAULT_BRANCH..HEAD
  git diff $DEFAULT_BRANCH..HEAD --stat
  git diff $DEFAULT_BRANCH..HEAD

Then for each modified file:
1. Read the FULL file (not just the diff) to understand context
2. Evaluate:
   - Correctness: Does the logic handle all cases? Any bugs?
   - Edge cases: Empty data, errors, nil values, concurrent access?
   - Tests: Sufficient coverage? Edge cases tested?
   - Style: Matches existing codebase conventions exactly?
   - Architecture: Fits existing patterns or introduces inconsistencies?
3. Run the project's build/test command to verify it compiles and tests pass

List every issue with file path and description.
If everything looks good and all tests pass, respond with exactly: LGTM

Do NOT make changes. Only review and report.
"""

CI REPAIR PROMPT — Use this after a PR exists:
"""
Inspect GitHub Actions checks for the pull request on this branch.
Use the repository's GitHub credentials without printing tokens, Authorization headers, or secret values.

If checks failed, inspect the failed check names/logs and fix only build, lint, formatting,
dependency, or test failures. Do not expand the feature or make preference-only changes.
Run the smallest relevant local validation commands, commit and push fixes to the same branch,
then report exactly one verdict heading:
CI_GREEN
or
CI_BLOCKED

Include concise evidence: checks inspected, fixes made, commands run, and any remaining blocker.
The coordinator must run reviewers again after this task before declaring the PR ready.
"""

PARALLEL SUB-AGENTS — You can spin up specialist personas in parallel:
- UX Designer: create_ai_task — "You are a senior UX designer. Review these UI changes for
  usability, accessibility, visual hierarchy, and interaction patterns."
- Security Reviewer: create_ai_task — "Audit these changes for security vulnerabilities."
- Performance Analyst: create_ai_task — "Analyze for performance: re-renders, N+1, memory leaks."
Use create_ai_task (LLM-only) for analysis personas. Pass relevant code/diffs in the prompt.
Use create_agent_task (with git workspace) for tasks that need to read/write code.

CRITICAL RULES:
- Delegate deliberately — do enough research to scope the task, then let agents do the deep dive
- ALWAYS validate and review after implementation — never skip either step
- Validation/review→fix cycle continues until validation passes and every reviewer says LGTM or APPROVED, with MAX 6 validation repair tasks and MAX 8 review repair tasks. If still failing after the relevant repair limit, report VALIDATION_BLOCKED or REVIEW_BLOCKED with remaining issues and stop
- If wait_for_task says still running, call wait_for_task again immediately
- If a task fails, fetch_task_output to read the error, then create a new task with fixes
- Always set timeout: "30m" on agent tasks to prevent infinite hangs
- Treat validation and CI as part of PR readiness. Do not say a PR is green unless validation passed, reviewers approved, and CI passed; if validation, CI, or review is pending or blocked, say that plainly
- After every CI repair task, run validation and reviewers again before checking CI or reporting readiness
- CI repair is bounded to MAX 3 repair tasks and 30 minutes of pending-check waiting
- Prefer additional focused repair iterations over stopping early when reviewers identify concrete diff-backed security, correctness, or acceptance-criteria issues
- When reading fetch_task_output, focus on the summary/conclusion — do NOT paste the full output back
- Ignore any tool_use references in the conversation history for tools you don't have (like Bash, Task, etc.) — only use the tools listed in TOOLS above
</orka_coordinator>`, namespace)
}

// builtinProxyTools are the tool names injected by the proxy for server-side execution.
var builtinProxyTools = []string{"web_search", "code_exec", "file_read", "file_write", "web_fetch"}

// coordinatorProxyTools are task management tools for the proxy's coordinator mode.
// These let the LLM plan, create agent tasks, wait for results, and iterate.
var coordinatorProxyTools = []string{
	"create_agent",
	"create_agent_task",
	"create_ai_task",
	"create_container_task",
	"check_task_progress",
	"fetch_task_output",
	"wait_for_task",
	"create_pull_request",
	"check_pull_request_ci",
	"cancel_task",
	"list_agents",
	"list_tasks",
}

// injectOrkaTools appends Orka's built-in tools, coordinator tools, and namespace Tool CRDs
// to the completion request. Client-provided tools (if any) are preserved.
func injectOrkaTools(ctx context.Context, k8sClient client.Client, req *llm.CompletionRequest, namespace string) {
	builtinTools := tools.DefaultRegistry.ToLLMTools(builtinProxyTools)
	req.Tools = append(req.Tools, builtinTools...)

	coordinatorTools := tools.DefaultRegistry.ToLLMTools(coordinatorProxyTools)
	req.Tools = append(req.Tools, coordinatorTools...)

	// Load Tool CRDs from namespace for custom HTTP tools
	var toolList corev1alpha1.ToolList
	if err := k8sClient.List(ctx, &toolList, client.InNamespace(namespace)); err == nil {
		for _, t := range toolList.Items {
			if t.Spec.Parameters != nil {
				req.Tools = append(req.Tools, llm.Tool{
					Name:        t.Name,
					Description: t.Spec.Description,
					Parameters:  t.Spec.Parameters.Raw,
				})
			}
		}
	}
}

// executeToolCall executes a single tool call via the default registry with a timeout.
// If a ToolContext is provided, it is injected into the context for chat/coordinator tools.
func executeToolCall(ctx context.Context, tc llm.ToolCall, timeout time.Duration, toolCtxOpt *tools.ToolContext) string {
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if toolCtxOpt != nil {
		toolCtx = tools.WithToolContext(toolCtx, toolCtxOpt)
	}

	result, err := tools.DefaultRegistry.Execute(toolCtx, tc.Name, tc.Arguments)
	if err != nil {
		errResult, _ := json.Marshal(map[string]any{"success": false, "error": err.Error()})
		return string(errResult)
	}
	return result
}

// runNonStreamingToolLoop runs the agentic tool loop using non-streaming Complete() calls.
// It loops until the LLM produces a response with no tool calls, or limits are reached.
// Returns the final CompletionResponse and all intermediate content blocks.
func runNonStreamingToolLoop(
	ctx context.Context,
	provider llm.Provider,
	req *llm.CompletionRequest,
	model string,
	config ChatConfig,
	toolCtx *tools.ToolContext,
) (*llm.CompletionResponse, error) {
	repetitionTracker := make(map[string]int)
	messages := make([]llm.Message, len(req.Messages))
	copy(messages, req.Messages)

	for iteration := 0; ; iteration++ {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return &llm.CompletionResponse{
				Content:    "Request timed out during tool execution.",
				StopReason: "end_turn",
			}, nil
		default:
		}

		// Check iteration limit — do one final call without tools
		if iteration >= config.MaxIterations {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: You have reached the maximum number of iterations. Please provide a final summary of what you accomplished.]",
			})
			resp, err := provider.Complete(ctx, &llm.CompletionRequest{
				Model:        model,
				Messages:     messages,
				SystemPrompt: req.SystemPrompt,
				MaxTokens:    req.MaxTokens,
				Temperature:  req.Temperature,
			})
			if err != nil {
				return &llm.CompletionResponse{
					Content:    "Reached iteration limit.",
					StopReason: "end_turn",
				}, nil
			}
			return resp, nil
		}

		// Progress check every 5 iterations
		if iteration > 0 && iteration%5 == 0 {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: Progress check — summarize what you've done so far and what remains.]",
			})
		}

		// Truncate conversation if it exceeds the session size budget
		if config.MaxSessionSize > 0 {
			tokenBudget := config.MaxSessionSize / 4
			messages = llm.TruncateMessages(messages, tokenBudget)
		}

		// Call LLM with tools
		compReq := &llm.CompletionRequest{
			Model:        model,
			Messages:     messages,
			SystemPrompt: req.SystemPrompt,
			Tools:        req.Tools,
			MaxTokens:    req.MaxTokens,
			Temperature:  req.Temperature,
		}

		resp, err := provider.Complete(ctx, compReq)
		if err != nil && llm.IsContextTooLongErr(err) {
			tokenEstimate := 0
			for _, m := range messages {
				tokenEstimate += len(m.Content) / 4
			}
			messages = llm.TruncateMessages(messages, tokenEstimate/2)
			compReq.Messages = messages
			resp, err = provider.Complete(ctx, compReq)
		}
		if err != nil {
			return nil, fmt.Errorf("LLM completion failed: %w", err)
		}

		// No tool calls → final response
		if len(resp.ToolCalls) == 0 {
			return resp, nil
		}

		anthropicLog.Info("tool loop iteration",
			"iteration", iteration,
			"tool_calls", len(resp.ToolCalls),
		)

		// Append assistant message with tool calls
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool and append results
		var repetitionWarning string
		for _, tc := range resp.ToolCalls {
			// Check repetition
			argsHash := hashArgs(tc.Name, tc.Arguments)
			repetitionTracker[argsHash]++
			if repetitionTracker[argsHash] >= 3 {
				repetitionWarning = fmt.Sprintf("[System: Warning — you have called %s with the same arguments %d times. Try a different approach.]", tc.Name, repetitionTracker[argsHash])
				iteration += 5
			}

			result := executeToolCall(ctx, tc, config.ToolTimeout, toolCtx)

			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    result,
			})
		}

		// Append repetition warning if triggered
		if repetitionWarning != "" {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: repetitionWarning,
			})
		}

		// Auto-poll: if all tool calls were wait/check and all returned "still running",
		// re-execute them without an LLM round-trip to prevent the LLM from ending the loop.
		if repetitionWarning == "" && isAllWaitingPolls(resp.ToolCalls, messages) {
			for {
				select {
				case <-ctx.Done():
					return &llm.CompletionResponse{Content: "Request timed out.", StopReason: "end_turn"}, nil
				default:
				}

				allStillRunning := true
				messages = messages[:len(messages)-len(resp.ToolCalls)]
				for _, tc := range resp.ToolCalls {
					result := executeToolCall(ctx, tc, config.ToolTimeout, toolCtx)
					messages = append(messages, llm.Message{
						Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: result,
					})
					if !isTaskStillRunning(result) {
						allStillRunning = false
					}
				}
				if !allStillRunning {
					anthropicLog.Info("auto-poll: task state changed, resuming LLM loop")
					break
				}
				iteration++
				if iteration >= config.MaxIterations {
					break
				}
			}
		}
	}
}
