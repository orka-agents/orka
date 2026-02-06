/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ConversationMessage represents a single message in a chat conversation.
type ConversationMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SystemPromptBuilder constructs the orchestrator system prompt with
// dynamic context from the cluster (agents, tools).
type SystemPromptBuilder struct {
	client    client.Client
	namespace string

	// Cache fields
	cachedPrompt string
	cachedHash   string
}

// NewSystemPromptBuilder creates a new SystemPromptBuilder.
func NewSystemPromptBuilder(c client.Client, namespace string) *SystemPromptBuilder {
	return &SystemPromptBuilder{
		client:    c,
		namespace: namespace,
	}
}

// BuildSystemPrompt assembles the full system prompt with dynamic context.
func (b *SystemPromptBuilder) BuildSystemPrompt(ctx context.Context, userSystemPrompt string) (string, error) {
	agentsSection, toolsSection, err := b.buildDynamicContext(ctx)
	if err != nil {
		return "", fmt.Errorf("building dynamic context: %w", err)
	}

	hash := b.computeHash(agentsSection, toolsSection)
	if hash == b.cachedHash && userSystemPrompt == "" && b.cachedPrompt != "" {
		return b.cachedPrompt, nil
	}

	var sb strings.Builder

	sb.WriteString(`<identity>
You are the Mercan orchestrator — an AI assistant that manages Kubernetes-native
task execution. You help users create, monitor, and manage tasks by interacting
with the Mercan platform on their behalf.
</identity>

<capabilities>
You can create and monitor three types of tasks, discover available agents and
tools, and manage platform resources. You operate autonomously — create tasks,
wait for results, and report back.
</capabilities>

<task_types>
- container: Run a shell command or container image. Use create_container_task.
  Example: building code, running scripts, data processing.
- ai: Run an LLM-powered task with tools. Use create_ai_task.
  Example: code review, content generation, analysis with web search.
- agent: Run an external CLI runtime (Copilot, Claude Code). Use create_agent_task.
  Example: code changes in a git repo, multi-file refactoring.
</task_types>

<scheduling>
Any task type can be made recurring by setting the schedule parameter with a cron expression.
Common patterns:
- Every hour: "0 * * * *"
- Every 6 hours: "0 */6 * * *"
- Daily at midnight: "0 0 * * *"
- Weekdays at 9am: "0 9 * * 1-5"
- Every 5 minutes: "*/5 * * * *"
- Weekly on Monday: "0 0 * * 1"

When the user says "every", "recurring", "daily", "weekly", "hourly", or similar time-based
repetition, set the schedule parameter on the task. Each scheduled run creates a child task
that appears in list_tasks with the label mercan.ai/parent-task.
</scheduling>

<available_agents>
`)
	sb.WriteString(agentsSection)
	sb.WriteString(`</available_agents>

<available_tools>
`)
	sb.WriteString(toolsSection)
	sb.WriteString(`</available_tools>

<rules>
1. Always use list_agents before creating a task with agentRef to verify the agent exists.
2. After creating a task, call wait_for_task then fetch_task_output to get results.
3. Use the task type that best matches the user's intent — don't default to ai for everything.
4. Never guess namespace — use the namespace from the chat request or ask the user.
5. Provide clear summaries of what you did, what succeeded, and what failed.
6. If a task fails, check the error and try a different approach before giving up.
7. Do not create more tasks than necessary — prefer fewer, well-configured tasks.
</rules>

<examples>
Example 1: User asks "run a web search for Kubernetes best practices"
→ list_agents (find agent with web_search tool)
→ create_ai_task (with agentRef, prompt)
→ wait_for_task
→ fetch_task_output
→ summarize results to user

Example 2: User asks "build and test my Go project"
→ create_container_task (go build)
→ wait_for_task
→ check_task_progress (verify success)
→ create_container_task (go test)
→ wait_for_task
→ fetch_task_output
→ report results
</examples>
`)

	if userSystemPrompt != "" {
		sb.WriteString("\n<user_instructions>\n")
		sb.WriteString(userSystemPrompt)
		sb.WriteString("\n</user_instructions>\n")
	}

	prompt := sb.String()
	b.cachedPrompt = prompt
	b.cachedHash = hash

	return prompt, nil
}

// buildDynamicContext fetches agents and tools from the cluster and formats them.
func (b *SystemPromptBuilder) buildDynamicContext(ctx context.Context) (agentsSection, toolsSection string, err error) {
	// Fetch agents
	var agentList corev1alpha1.AgentList
	if err := b.client.List(ctx, &agentList, client.InNamespace(b.namespace)); err != nil {
		return "", "", fmt.Errorf("listing agents: %w", err)
	}

	agentLines := make([]string, 0, len(agentList.Items))
	for i := range agentList.Items {
		agent := &agentList.Items[i]
		line := formatAgent(agent)
		agentLines = append(agentLines, line)
	}

	if len(agentLines) == 0 {
		agentsSection = "No agents configured.\n"
	} else {
		agentsSection = strings.Join(agentLines, "\n") + "\n"
	}

	// Fetch custom tools
	var toolList corev1alpha1.ToolList
	if err := b.client.List(ctx, &toolList, client.InNamespace(b.namespace)); err != nil {
		return "", "", fmt.Errorf("listing tools: %w", err)
	}

	toolLines := make([]string, 0, len(toolList.Items)+3)
	// Built-in tools
	toolLines = append(toolLines,
		"web_search - Search the web for information (built-in)",
		"code_exec - Execute code snippets (built-in)",
		"file_read - Read file contents (built-in)",
	)

	for i := range toolList.Items {
		tool := &toolList.Items[i]
		toolLines = append(toolLines, fmt.Sprintf("%s - %s", tool.Name, tool.Spec.Description))
	}

	toolsSection = strings.Join(toolLines, "\n") + "\n"

	return agentsSection, toolsSection, nil
}

// formatAgent produces a single-line summary for an agent.
func formatAgent(agent *corev1alpha1.Agent) string {
	var parts []string

	if agent.Spec.Model != nil {
		modelInfo := fmt.Sprintf("model: %s", agent.Spec.Model.Name)
		if agent.Spec.Model.Provider != "" {
			modelInfo = fmt.Sprintf("model: %s, provider: %s", agent.Spec.Model.Name, agent.Spec.Model.Provider)
		}
		parts = append(parts, modelInfo)
	}

	if len(agent.Spec.Tools) > 0 {
		var toolNames []string
		for _, t := range agent.Spec.Tools {
			if t.Enabled == nil || *t.Enabled {
				toolNames = append(toolNames, t.Name)
			}
		}
		if len(toolNames) > 0 {
			parts = append(parts, fmt.Sprintf("tools: [%s]", strings.Join(toolNames, ", ")))
		}
	}

	if agent.Spec.Runtime != nil {
		parts = append(parts, fmt.Sprintf("runtime: %s", agent.Spec.Runtime.Type))
	}

	detail := strings.Join(parts, ", ")
	if detail != "" {
		return fmt.Sprintf("%s - %s", agent.Name, detail)
	}
	return agent.Name
}

// computeHash returns a truncated SHA-256 hash of the agent and tool sections.
func (b *SystemPromptBuilder) computeHash(agents, tools string) string {
	h := sha256.New()
	h.Write([]byte(agents))
	h.Write([]byte(tools))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// EstimateTokens returns an approximate token count (~4 chars per token).
func EstimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// TruncateConversation keeps the first message and the newest messages that
// fit within the token budget. If truncation occurs, a context note is inserted.
func TruncateConversation(messages []ConversationMessage, tokenBudget int) []ConversationMessage {
	if len(messages) == 0 {
		return messages
	}

	totalTokens := 0
	for _, m := range messages {
		totalTokens += EstimateTokens(m.Content)
	}
	if totalTokens <= tokenBudget {
		return messages
	}

	// Always keep the first message
	first := messages[0]
	firstTokens := EstimateTokens(first.Content)
	remaining := tokenBudget - firstTokens
	if remaining <= 0 {
		return []ConversationMessage{first}
	}

	// From the tail, collect messages that fit
	rest := messages[1:]
	var kept []ConversationMessage
	for i := len(rest) - 1; i >= 0; i-- {
		cost := EstimateTokens(rest[i].Content)
		if remaining-cost < 0 {
			break
		}
		remaining -= cost
		kept = append([]ConversationMessage{rest[i]}, kept...)
	}

	// If we dropped any messages, insert a truncation note
	if len(kept) < len(rest) {
		summary := first.Content
		if len(summary) > 100 {
			summary = summary[:100]
		}
		note := ConversationMessage{
			Role:    "system",
			Content: fmt.Sprintf("[Earlier messages truncated. The conversation began with: '%s']", summary),
		}
		result := make([]ConversationMessage, 0, len(kept)+2)
		result = append(result, first, note)
		result = append(result, kept...)
		return result
	}

	result := make([]ConversationMessage, 0, len(kept)+1)
	result = append(result, first)
	result = append(result, kept...)
	return result
}
