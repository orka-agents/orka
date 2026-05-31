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

func TestCoordinatorProxyToolsIncludePRCIWorkflowTools(t *testing.T) {
	for _, toolName := range []string{"create_agent", "create_pull_request", "check_pull_request_ci"} {
		if !slices.Contains(coordinatorProxyTools, toolName) {
			t.Fatalf("coordinatorProxyTools missing %q", toolName)
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

// The CRITICAL RULES "failed to get agent" handler tells the coordinator how
// to recover from the most common AGENT_REF SOURCING violation: a Task whose
// agentRef was never registered. Pin the recovery wording so a future edit
// can't silently drop it.
func TestCoordinatorSystemPrompt_AgentNotFoundFailureSignal(t *testing.T) {
	prompt := coordinatorSystemPrompt("default")

	for _, want := range []string{
		"\"failed to get agent\"",
		"Agent.core.orka.ai",
		"agentRef on that Task was never registered",
		"missing-Agent error is permanent",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing agent-not-found handler %q", want)
		}
	}
}
