/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"os"
	"strings"
)

// autonomousSystemPromptSuffix returns additional system prompt instructions
// for autonomous coordinator mode. It is appended to the agent's base system prompt.
func autonomousSystemPromptSuffix(iteration int, maxIterations int) string {
	iterInfo := fmt.Sprintf("Current iteration: %d", iteration)
	if maxIterations > 0 {
		iterInfo += fmt.Sprintf(" of %d", maxIterations)
	}

	triageSection := githubTriageSection()

	return fmt.Sprintf(`

## Autonomous Coordinator Mode

%s

### Workflow

1. Delegate work using 'delegate_task', then call 'wait_for_tasks' for results.
2. Call 'update_plan' each iteration to persist progress.
3. When the goal is complete, call 'update_plan' with 'goal_complete: true'.

On the first iteration, analyze the goal and create a phased plan.
On subsequent iterations, continue from the existing plan state.

### Plan Document Format

Use 'update_plan' to maintain a markdown plan:

`+"```"+`markdown
# Goal
<one-line description>

# Completed
- [x] Phase 1: <description> — <outcome>

# In Progress
- [ ] Phase 2: <description> — <status>

# Remaining
- [ ] Phase 3: <description>

# Issues
- <blockers or failed approaches>
`+"```"+`

If no further progress is possible, set 'goal_complete: true' and explain why.
%s`, iterInfo, triageSection)
}

// githubTriageSection returns the GitHub triage workflow guidance when GitHub
// tools (list_issues, list_pull_requests) are available. It returns an empty
// string when the tools are not present.
func githubTriageSection() string {
	if !hasGitHubTools() {
		return ""
	}

	return `
## GitHub Triage Workflow

When you have access to list_issues and list_pull_requests tools, follow this workflow:

### Scanning Phase
1. Call list_issues() to discover open, unassigned issues
2. Call list_pull_requests() to discover open PRs needing review
3. Use get_issue(issue_number) to read full details of promising issues

### Selection Phase (pick ONE issue + ONE PR per iteration)
4. Pick the most impactful unassigned issue to work on
5. Pick one open PR that needs review
6. Skip issues that already have a comment from you (to avoid duplicate work)

### Execution Phase
7. Call comment_on_issue() on your chosen issue to signal you're working on it
8. Delegate a coder agent for the issue: delegate_task(agent="coder", prompt="...", workspace={...})
9. Delegate a reviewer agent for the PR: delegate_task(agent="reviewer", prompt="Review PR #N...", workspace={...})
10. Call wait_for_tasks() to wait for both to complete

### Follow-up Phase
11. If the coder created a new PR, delegate a reviewer agent to review it
12. If the reviewer requested changes on an existing PR, delegate a coder with feedback
13. Call update_plan() to record what was done and what remains

### Important Guidelines
- Only pick ONE issue and ONE PR per iteration to keep work manageable
- Always comment on an issue before starting work to prevent duplicate effort
- When delegating, pass the full workspace config including gitRepo and pushBranch
- Approve PRs only — do not auto-merge
`
}

// hasGitHubTools checks whether GitHub issue/PR tools are available by
// inspecting the ORKA_AI_TOOLS env var for known tool names, or by checking
// if ORKA_GIT_REPO is set (indicating a workspace tied to a GitHub repo).
func hasGitHubTools() bool {
	if os.Getenv("ORKA_GIT_REPO") != "" {
		return true
	}

	toolsStr := os.Getenv("ORKA_AI_TOOLS")
	if toolsStr == "" {
		return false
	}

	for t := range strings.SplitSeq(toolsStr, ",") {
		name := strings.TrimSpace(t)
		if name == "list_issues" || name == "list_pull_requests" {
			return true
		}
	}

	return false
}
