/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"slices"
	"strings"
	"testing"
)

func TestCoordinatorSystemPrompt_PRReviewCIRepairLoop(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"REPEAT validation and steps 6-8 until validation passes and every reviewer approves",
		"at most 8 review repair tasks",
		"create or update a pull request using create_pull_request",
		"check_pull_request_ci",
		"pending for more than 30 minutes",
		"After each CI fix, run validation and reviewer tasks again",
		"at most 3 CI repair tasks",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing %q", want)
		}
	}
}

func TestCoordinatorSystemPrompt_CreateAgentInvariants(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"CREATE_AGENT INVARIANTS",
		"MUTUALLY EXCLUSIVE",
		"runtime.type=codex|claude|copilot",
		"resources.limits.memory:   \"2Gi\"",
		"auto-discovery",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing create-agent invariant %q", want)
		}
	}
}

func TestCoordinatorSystemPrompt_GoalState(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"GOAL STATE",
		"VALIDATION_BLOCKED",
		"REVIEW_BLOCKED",
		"CI_BLOCKED",
		"CI_PENDING",
		"VALIDATION_CONFIG_BLOCKED",
		"DO NOT stop with",
		"your VERY NEXT\nresponse must be a tool_use=wait_for_task",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing goal-state directive %q", want)
		}
	}
}

func TestCoordinatorSystemPrompt_FailureSignalHandling(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"\"OOMKilled\" or \"memory limit ... exceeded\"",
		"\"container exited with code\"",
		"\"agent ... has both runtime and model.provider set\"",
		"\"git secret ... not found\"",
		"recreate the Agent",
		"\"failed to get agent\"",
		"missing-Agent error is permanent",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing failure-signal handler %q", want)
		}
	}
}

// AGENT_REF on create_agent_task / create_ai_task MUST be a real Agent name,
// not a role label. create_agent takes a "role" and generates the Agent name
// as {parent-task}-{role}-{hash} — the model must copy the returned agentName
// back into agentRef. The prompt's AGENT_REF SOURCING section pins this rule
// + a worked example + a recovery path so that Opus 4.7 (which has been
// observed inventing agentRefs from role labels) actually sees and follows it.
func TestCoordinatorSystemPrompt_AgentRefSourcing(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"AGENT_REF SOURCING",
		"agentRef is a Kubernetes Agent name, NOT a role label",
		"create_agent takes \"role\"",
		"{parent-task}-{role}-{hash}",
		"copy that returned agentName verbatim",
		"NEVER invent agentRefs from role labels",
		"Worked example",
		`create_agent role="coder"`,
		`agentRef="proxy-abc-coder-de4f56"`,
		"Do NOT retry the failed Task",
		"wait_for_task.data.message is",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing AGENT_REF SOURCING marker %q", want)
		}
	}
}

// The Codex/Claude/Copilot worker stages, commits, and pushes the agent's
// uncommitted changes to ORKA_PUSH_BRANCH after PHASE 4. If the IMPLEMENTATION
// PROMPT tells the agent to push itself, the agent runs `git push origin HEAD`,
// the commit lands on whatever branch is checked out (often main), and the
// worker fails with "no workspace diff was produced". This test pins the
// prompt's anti-push contract so a future edit can't silently regress it.
func TestCoordinatorSystemPrompt_ImplementationPromptForbidsAgentSidePush(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"DO NOT run \"git commit\" or \"git push\"",
		"worker stages, commits,",
		"Leave changes UNCOMMITTED",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing anti-push directive %q", want)
		}
	}
	for _, banned := range []string{
		"- Commit with a descriptive message",
		"- Push to the specified branch",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains old commit/push instruction %q", banned)
		}
	}
}

// Demo 10 run 2026-05-31 21:00 PT regressed: the coordinator stopped after only
// 4 tool iterations (38s, $0.024, 318 output tokens) by emitting a "Progress
// summary" text response after create_agent_task. In the Anthropic streaming
// protocol, any text response outside a tool_use ENDS the turn — the chat
// client disconnects, the orka-api tool loop dies (the auto-poll resume can't
// reach a dead SSE stream), and validation/review/PR are never executed even
// though the implementation Task itself succeeded asynchronously.
// The existing "DO NOT stop with progress summary" line at the bottom of
// CRITICAL RULES was too easy for the model to ignore. This test pins:
//
//	(a) The new TURN-ENDING INVARIANT block at the top of the prompt.
//	(b) The POSTCONDITION TABLE so the model has explicit per-tool guidance.
//	(c) The cross-reference from the CRITICAL RULES anti-stopping line back to
//	    the invariant (so a future edit can't silently remove the invariant
//	    without also removing the cross-reference).
func TestCoordinatorSystemPrompt_TurnEndingInvariant(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		// (a) top-of-prompt invariant
		"TURN-ENDING INVARIANT",
		"a turn ENDS the instant you emit any text",
		"ALL remaining work (validate, review, PR, CI) is LOST",
		`"I'll proceed"`,
		`"now I will"`,
		`"here is what I've done so far"`,
		"FAILURE MODES",
		// (b) postcondition table — explicit per-tool next-step rules
		"POSTCONDITION TABLE",
		"After create_agent_task     → wait_for_task",
		"After create_container_task → wait_for_task",
		"After create_ai_task        → wait_for_task",
		"After wait_for_task (Succeeded)       → fetch_task_output",
		"After fetch_task_output (implementation succeeded)  → create_container_task (validation)",
		"After fetch_task_output (every reviewer LGTM)       → create_pull_request",
		"After create_pull_request                            → check_pull_request_ci",
		"After check_pull_request_ci (success)               → FINAL TEXT REPORT (GOAL STATE A)",
		"FINAL TEXT REPORT (GOAL STATE B)",
		// (c) cross-reference from old CRITICAL RULES line
		"per the TURN-ENDING INVARIANT at the top of this prompt",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing turn-ending invariant marker %q", want)
		}
	}

	// The bare "If you have running tasks, your next tool call MUST be wait_for_task"
	// sentence (without the invariant cross-reference) was too easy for the model
	// to skim past. The new wording must replace it, not duplicate it.
	for _, banned := range []string{
		"If you have running tasks, your next tool call MUST be\nwait_for_task (the auto-poll layer keeps polling without burning your iteration\nbudget). If wait_for_task returns \"still running\", call wait_for_task again.\nOnly emit a final text response after one of (A), (B), or (C) is satisfied.",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains pre-invariant anti-stopping wording without cross-reference")
		}
	}
}

// Demo 10 run 2026-05-31 21:40 PT regressed in a new way: the TURN-ENDING
// INVARIANT prompt fix worked perfectly (14 iterations, $0.063, followed
// postcondition table), but BOTH coder Tasks (proxy-9072de74, proxy-2abe472e)
// failed because the coordinator picked a bare pushBranch ("orka/quiet-flag")
// that already existed on the remote from a prior demo run. The codex worker
// built the code, ran tests successfully, then died on
// `failed to push some refs to 'https://github.com/sozercan/vekil'
//
//	! [rejected]        orka/quiet-flag -> orka/quiet-flag (fetch first)`.
//
// Because the push happens AFTER PHASE 5 — i.e., after the result configmap
// would have been written — fetch_task_output returned empty and the
// coordinator misdiagnosed it as `codex runtime container is failing
// (likely missing credentials)` and reported VALIDATION_BLOCKED.
// Two prompt fixes:
//
//	(a) WORKSPACE BRANCH RULES requires a unique 8-char suffix on pushBranch.
//	    Eliminates the collision class entirely.
//	(b) CRITICAL RULES table interprets `container exit 1 + empty output` as a
//	    likely workspace/git problem first (with explicit recovery action),
//	    not a credentials problem.
//
// Plus a brand-new explicit "failed to push some refs" signal handler.
//
// Demo 10 run 2026-06-01 10:50 PT regressed AGAIN: the model copied the
// literal hex suffix `a3f9c241` from the original prompt's example —
// it matched a branch from PR #162 that already existed on the remote,
// so the coder's push got rejected. The fix is to:
//   - rename the WORKSPACE BRANCH RULES placeholder from <8-char-suffix>
//     to <UNIQUE-suffix> + an explicit "do NOT copy any hex string you
//     see anywhere in this prompt" ANTI-EXAMPLE warning.
//   - rewrite the "failed to push some refs" recovery handler to use
//     <NEWLY-GENERATED-hex> instead of a concrete-looking hex like
//     a3f9c241 (which the model interpreted as "use exactly this string").
//
// This test pins both shape changes — banning the old `a3f9c241` literal
// so a future edit can't reintroduce the same poisonous example.
func TestCoordinatorSystemPrompt_PushBranchCollisionGuardrails(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		// (a) unique suffix requirement (revised)
		`workspace.pushBranch = "orka/<short-task-description>-<UNIQUE-suffix>"`,
		"MUST be NEWLY generated for THIS session",
		"do NOT copy",
		"any hex string you see anywhere in this prompt",
		"NEVER use a bare topic name like",
		`"orka/quiet-flag" — that branch may`,
		"cannot fast-forward over it",
		"ANTI-EXAMPLE — DO NOT COPY",
		// (b) revised "container exit 1" interpretation
		"container exited with code",
		"fetch_task_output returns EMPTY",
		"the worker pod crashed BEFORE writing its result configmap",
		"git push was rejected",
		"Do NOT declare VALIDATION_BLOCKED on the first occurrence",
		"empty output from a runtime container is much more often a workspace/git problem than a credentials problem",
		// (c) revised push-rejection handler — no concrete hex
		`"failed to push some refs"`,
		`"non-fast-forward"`,
		"NEWLY-GENERATED-hex",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing push-branch-collision guardrail %q", want)
		}
	}

	// The old bare instruction must NOT come back.
	for _, banned := range []string{
		`First implementation: workspace.pushBranch = "orka/<short-task-description>".`,
		// Literal hex suffixes are banned anywhere in the prompt — the
		// model will copy them verbatim. Use only obvious placeholders.
		"a3f9c241",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains banned literal %q (use placeholder shape instead)", banned)
		}
	}
}

func TestCoordinatorProxyToolsIncludePRCIWorkflowTools(t *testing.T) {
	for _, toolName := range []string{"create_agent", "create_pull_request", "check_pull_request_ci"} {
		if !slices.Contains(coordinatorProxyTools, toolName) {
			t.Fatalf("coordinatorProxyTools missing %q", toolName)
		}
	}
}

// Demo 10 run 2026-05-31 surfaced four distinct, fixable failure modes during a
// single chat-to-PR cycle. The prompt should preempt each one so future runs
// don't burn validation/review-repair budget on them:
//  1. proxy-7c28e58b: coordinator used ["bash","-lc"] on golang:1.23 → exit 127
//     (login shell reset PATH; 'go: command not found').
//  2. proxy-b654a51d: coordinator used golang:1.23 without reading go.mod →
//     'go.mod requires go >= 1.25'.
//  3. proxy-09377574: reviewer Task had pushBranch set → review content was
//     correct but worker failed with 'no workspace diff was produced'.
//  4. proxy-49e462ef: create_ai_task with empty agentRef → 'ORKA_AI_PROVIDER
//     is required' (worker pod started and died).
//
// This test pins all four guard-rails so a future edit can't silently regress
// them and so the next demo run shrinks toward zero corrections.
func TestCoordinatorSystemPrompt_Demo10CorrectionGuardrails(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		// (1) sh -c, never bash -lc
		`command MUST be ["sh","-c"]`,
		`NEVER ["bash","-lc"]`,
		"Login shells reset PATH",
		// (2) mandatory go.mod read before picking a Go image
		"BEFORE picking a Go image",
		`head -10 go.mod`,
		"NEVER guess the Go version",
		// (3) reviewers are read-only; no pushBranch
		"OMIT workspace.pushBranch",
		"reviewers are READ-ONLY",
		"no workspace diff was produced",
		// (4) create_ai_task precondition — agent must have model.provider+name
		"ORKA_AI_PROVIDER is required",
		"model.provider+model.name",
		// failure-signal recovery handlers for the above
		`"no workspace diff was produced"`,
		`"go: command not found"`,
		`"go.mod requires go >= X.Y"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing demo-10 guardrail %q", want)
		}
	}

	// Old reviewer-pushBranch instruction must NOT come back. The previous wording
	// caused failure #3 above.
	for _, banned := range []string{
		"Review tasks: workspace.branch = same push branch AND workspace.pushBranch = same push branch.",
		"Set BOTH workspace.branch AND workspace.pushBranch to the SAME branch name.\n   This ensures each reviewer clones the branch with the implementation changes.",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains old reviewer-pushBranch instruction:\n%s", banned)
		}
	}
}
