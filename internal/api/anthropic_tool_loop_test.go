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
		"your next tool call MUST be",
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
