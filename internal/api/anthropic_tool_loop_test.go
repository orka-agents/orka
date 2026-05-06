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
