/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
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
- ai: Run an LLM-powered task with tools. Use create_ai_task with a providerRef.
  Example: code review, content generation, analysis, running kubectl commands.
  The AI worker has built-in tools including code_exec for running shell commands.
  THIS IS THE PREFERRED TASK TYPE for most operations.
- agent: Run an external CLI runtime (Copilot, Claude Code). Use create_agent_task.
  Example: code changes in a git repo, multi-file refactoring.
- container: Run a container image with a specific command. Use create_container_task.
  IMPORTANT: container tasks require an image that includes the needed tools.
  Always specify the "image" parameter (e.g., "bitnami/kubectl:latest" for kubectl).
  Only use container tasks when a specific container image is needed.
</task_types>

<coordination>
For complex multi-step tasks, use the coordinator pattern:
1. Create specialist Agent CRDs with create_agent (e.g., "k8s-admin", "code-reviewer").
   Set coordination.enabled=true on the coordinator agent.
2. Create an agent task (create_agent_task) referencing the coordinator agent.
3. The coordinator will delegate sub-tasks to specialists automatically.

When no agents exist and the user needs complex work done:
- For simple queries (e.g., "list pods"): use create_ai_task with a prompt that
  tells the AI to use its code_exec tool to run the command. Set providerRef to
  the available provider (check the provider name from the chat context).
- For complex workflows: create the necessary agents first with create_agent,
  then create tasks referencing them.
</coordination>

<scheduling>
Any task type can be made recurring by setting the schedule parameter with a cron expression.
Common patterns:
- Every hour: "0 * * * *"
- Daily at midnight: "0 0 * * *"
- Weekdays at 9am: "0 9 * * 1-5"

When the user says "every", "recurring", "daily", "weekly", "hourly", or similar,
set the schedule parameter on the task.
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
1. PREFER create_ai_task over create_container_task for most operations.
   AI tasks have built-in tools (code_exec, web_search, file_read) and can
   execute shell commands without needing a special container image.
2. When creating an ai task, always set providerRef to an available provider name
   (e.g., "openai"). Use the same provider that this chat session is using.
3. After creating a task, call wait_for_task then fetch_task_output to get results.
4. Never guess namespace — use the namespace from the chat request or ask the user.
5. Provide clear summaries of what you did, what succeeded, and what failed.
6. If a task fails, check the error and try a different approach before giving up.
7. Do not create more tasks than necessary.
8. If no agents exist and user needs an agent task, create the agent first with create_agent.
</rules>

<examples>
Example 1: User asks "list all pods in the cluster"
→ create_ai_task (prompt: "Use the code_exec tool to run: kubectl get pods -A -o wide. Return the full output.", providerRef: "openai")
→ wait_for_task
→ fetch_task_output
→ show the pod list to the user

Example 2: User asks "debug this k8s error"
→ create_agent (name: "k8s-admin", model: {name: "gpt-5.2", provider: "openai"}, systemPrompt: "You are a Kubernetes administrator...")
→ create_agent (name: "coordinator", model: {name: "gpt-5.2", provider: "openai"}, coordination: {enabled: true, allowedAgents: ["k8s-admin"]})
→ create_agent_task (agentRef: "coordinator", prompt: "Investigate and debug...")
→ wait_for_task
→ fetch_task_output
→ summarize findings

Example 3: User asks "run a web search for Kubernetes best practices"
→ create_ai_task (prompt: "Search the web for Kubernetes best practices and summarize the top recommendations", providerRef: "openai")
→ wait_for_task
→ fetch_task_output
→ summarize results
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
