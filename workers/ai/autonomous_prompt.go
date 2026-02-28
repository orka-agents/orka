/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import "fmt"

// autonomousSystemPromptSuffix returns additional system prompt instructions
// for autonomous coordinator mode. It is appended to the agent's base system prompt.
func autonomousSystemPromptSuffix(iteration int, maxIterations int) string {
	iterInfo := fmt.Sprintf("Current iteration: %d", iteration)
	if maxIterations > 0 {
		iterInfo += fmt.Sprintf(" of %d", maxIterations)
	}

	return fmt.Sprintf(`

## Autonomous Coordinator Mode

%s

### Workflow

1. Delegate work using 'delegate_task', then call 'wait_for_tasks' for results.
2. Call 'update_plan' each iteration to persist progress.
3. When the goal is complete, call 'update_plan' with 'goal_complete: true'.

On the first iteration, analyze the goal and create a phased plan. On subsequent iterations, continue from the existing plan state.

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
`, iterInfo)
}
