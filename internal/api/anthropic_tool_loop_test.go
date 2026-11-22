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
	if strings.Contains(prompt, "agentName") {
		t.Fatalf("coordinator prompt should use proxy create_agent data.name contract, found agentName")
	}
}

func TestCoordinatorSystemPrompt_CreateAgentInvariants(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"CREATE_AGENT INVARIANTS",
		"MUTUALLY EXCLUSIVE",
		"runtime.type=codex|claude|copilot|opencode",
		"OpenCode is a built-in ACP RuntimePool profile",
		"model.name in literal",
		"provider/model form",
		"positive, reviewed",
		"model.contextWindow and model.maxTokens limits",
		"contextWindow greater than",
		"OMIT runtime.secretRef/secretRef",
		"soul supplies persistent",
		"Built-in ACP RuntimePool Agents",
		"MUST OMIT custom Agent resources",
		"resource classes are controller-owned",
		"Agent-level requests/limits are rejected",
		"For non-ACP execution paths that support custom Agent resources",
		"runtime-backed Agent (codex/claude/copilot/opencode",
		"resources.limits.memory:   \"2Gi\"",
		"auto-discovery",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing create-agent invariant %q", want)
		}
	}

	for _, banned := range []string{
		"Coder/reviewer Agents you create in chat MUST set resources",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains contradictory ACP resource instruction %q", banned)
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
		"For a built-in ACP RuntimePool Agent, DO NOT add custom Agent resources",
		"resource classes are controller-owned",
		"For a non-ACP path that supports custom Agent resources",
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

func TestCoordinatorSystemPrompt_AgentRefSourcing(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"AGENT_REF SOURCING",
		"agentRef is a Kubernetes Agent name, NOT a role label",
		"create_agent takes \"name\"",
		"returned name verbatim",
		"NEVER invent agentRefs from role labels",
		"Worked example",
		`create_agent name="demo-coder-1234"`,
		`agentRef="demo-coder-1234"`,
		"Do NOT retry the failed Task",
		"wait_for_task.data.message is",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing AGENT_REF SOURCING marker %q", want)
		}
	}
}

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

func TestCoordinatorSystemPrompt_TurnEndingInvariant(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"TURN-ENDING INVARIANT",
		"a turn ENDS the instant you emit any text",
		"ALL remaining work (validate, review, PR, CI) is LOST",
		`"I'll proceed"`,
		`"now I will"`,
		`"here is what I've done so far"`,
		"FAILURE MODES",
		"POSTCONDITION TABLE",
		"After create_agent_task     → wait_for_task",
		"After create_container_task → wait_for_task",
		"After create_ai_task        → wait_for_task",
		"After wait_for_task (Succeeded)       → fetch_task_output",
		"After fetch_task_output (implementation succeeded)  → create_container_task (validation)",
		"After fetch_task_output (every reviewer LGTM)       → create_pull_request",
		"After create_pull_request                            → check_pull_request_ci",
		"After check_pull_request_ci (passed)                → FINAL TEXT REPORT (GOAL STATE A)",
		"FINAL TEXT REPORT (GOAL STATE B)",
		"per the TURN-ENDING INVARIANT at the top of this prompt",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing turn-ending invariant marker %q", want)
		}
	}

	for _, banned := range []string{
		"If you have running tasks, your next tool call MUST be\nwait_for_task (the auto-poll layer keeps polling without burning your iteration\nbudget). If wait_for_task returns \"still running\", call wait_for_task again.\nOnly emit a final text response after one of (A), (B), or (C) is satisfied.",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains pre-invariant anti-stopping wording without cross-reference")
		}
	}
}

func TestCoordinatorSystemPrompt_PushBranchCollisionGuardrails(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		`workspace.pushBranch = "orka/<short-task-description>-<UNIQUE-suffix>"`,
		"MUST be NEWLY generated for THIS session",
		"do NOT copy",
		"any hex string you see anywhere in this prompt",
		"NEVER use a bare topic name like",
		`"orka/quiet-flag" — that branch may`,
		"cannot fast-forward over it",
		"ANTI-EXAMPLE — DO NOT COPY",
		"container exited with code",
		"fetch_task_output returns EMPTY",
		"the worker pod crashed BEFORE writing its result configmap",
		"git push was rejected",
		"Do NOT declare VALIDATION_BLOCKED on the first occurrence",
		"empty output from a runtime container is much more often a workspace/git problem than a credentials problem",
		`"failed to push some refs"`,
		`"non-fast-forward"`,
		"NEWLY-GENERATED-hex",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing push-branch-collision guardrail %q", want)
		}
	}

	for _, banned := range []string{
		`First implementation: workspace.pushBranch = "orka/<short-task-description>".`,
		"a3f9c241",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains banned literal %q (use placeholder shape instead)", banned)
		}
	}
}

func TestCoordinatorProxyToolsIncludePRCIWorkflowTools(t *testing.T) {
	for _, toolName := range []string{"create_agent", "create_pr_monitor", "create_pull_request", "check_pull_request_ci"} {
		if !slices.Contains(coordinatorProxyTools, toolName) {
			t.Fatalf("coordinatorProxyTools missing %q", toolName)
		}
	}
}

func TestCoordinatorSystemPrompt_Demo10CorrectionGuardrails(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		`command MUST be ["sh","-c"]`,
		`NEVER ["bash","-lc"]`,
		"Login shells reset PATH",
		"BEFORE picking a Go image",
		`head -10 go.mod`,
		"NEVER guess the Go version",
		"OMIT workspace.pushBranch",
		"reviewers are READ-ONLY",
		"no workspace diff was produced",
		"ORKA_AI_PROVIDER is required",
		"model.provider+model.name",
		`"no workspace diff was produced"`,
		`"go: command not found"`,
		`"go.mod requires go >= X.Y"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing demo-10 guardrail %q", want)
		}
	}

	for _, banned := range []string{
		"Review tasks: workspace.branch = same push branch AND workspace.pushBranch = same push branch.",
		"Set BOTH workspace.branch AND workspace.pushBranch to the SAME branch name.\n   This ensures each reviewer clones the branch with the implementation changes.",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains old reviewer-pushBranch instruction:\n%s", banned)
		}
	}
}

func TestCoordinatorSystemPrompt_Demo10ContainerCacheAndReviewerTypeGuardrails(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"The worker filesystem is read-only outside /tmp, /home/worker, /workspace",
		"default Go module cache (/go/pkg/mod)",
		"every go command must use writable GOCACHE and GOMODCACHE under /tmp",
		"export GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomodcache",
		"inline env vars apply only to the first command",
		"'go build' reverts to /go/pkg and crashes",
		`"could not create module cache"`,
		`"mkdir /go/pkg: read-only file system"`,
		"Inline 'GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomodcache go test && go build' does NOT propagate the env to chained subcommands",
		"Create one or more SEPARATE reviewer tasks via create_agent_task (NEVER\n   create_ai_task",
		"code review requires a materialized repository workspace",
		"create_ai_task reviewers fail with\n   'API key for ... not found'",
		"The\n   reviewer Agent MUST be runtime-backed",
		`"API key for ... not found"`,
		"switch the task from create_ai_task to create_agent_task",
		"Git credentials remain outside the ACP process",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing demo-10 container-cache/reviewer-type guardrail %q", want)
		}
	}

	for _, banned := range []string{
		"NEVER guess the Go version — picking golang:1.23 when go.mod says toolchain go1.25 wastes the entire validation iteration ('go.mod requires go >= 1.25'). Use writable GOCACHE and GOMODCACHE under /tmp. For ALL container tasks",
		"6. REVIEW: Create one or more SEPARATE reviewer agent tasks using the REVIEW PROMPT template.",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("coordinator prompt still contains old vague guidance:\n%s", banned)
		}
	}
}
