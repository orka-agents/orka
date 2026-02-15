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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
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

// PromptMode controls how much of the system prompt is included.
type PromptMode string

const (
	// PromptModeFull includes all sections (default for top-level chat).
	PromptModeFull PromptMode = "full"
	// PromptModeMinimal omits scheduling, examples, image catalog, and coordination (for sub-agents).
	PromptModeMinimal PromptMode = "minimal"
)

// BuildSystemPrompt assembles the full system prompt with dynamic context.
// mode is optional; defaults to PromptModeFull.
func (b *SystemPromptBuilder) BuildSystemPrompt(ctx context.Context, userSystemPrompt string, mode ...PromptMode) (string, error) {
	m := PromptModeFull
	if len(mode) > 0 {
		m = mode[0]
	}

	agentsSection, toolsSection, providersSection, err := b.buildDynamicContext(ctx)
	if err != nil {
		return "", fmt.Errorf("building dynamic context: %w", err)
	}

	hash := b.computeHash(agentsSection, toolsSection, providersSection)
	if hash == b.cachedHash && userSystemPrompt == "" && b.cachedPrompt != "" {
		return b.cachedPrompt, nil
	}

	var sb strings.Builder

	sb.WriteString(buildIdentitySection())
	sb.WriteString(buildCapabilitiesSection())
	sb.WriteString(buildBehaviorSection())
	sb.WriteString(buildToolCallStyleSection())
	sb.WriteString(buildTaskTypesSection(m))
	sb.WriteString(buildCoordinationSection(m))
	sb.WriteString(buildSchedulingSection(m))

	// Dynamic context
	sb.WriteString("<available_agents>\n")
	sb.WriteString("Skills are loaded on-demand. Read skill content only when relevant to the current task.\n")
	sb.WriteString(agentsSection)
	sb.WriteString("</available_agents>\n\n")

	sb.WriteString("<available_tools>\n")
	sb.WriteString(toolsSection)
	sb.WriteString("</available_tools>\n")
	sb.WriteString(providersSection)
	sb.WriteString("\n")

	sb.WriteString(buildRulesSection())
	sb.WriteString(buildExamplesSection(m))

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

func buildIdentitySection() string {
	return `<identity>
You are the Orka orchestrator — an AI assistant that manages task execution.
You help users create, monitor, and manage tasks by interacting with the Orka
platform on their behalf.
</identity>

`
}

func buildCapabilitiesSection() string {
	return `<capabilities>
You can create and monitor three types of tasks, discover available agents and
tools, and manage platform resources. You operate autonomously — create tasks,
wait for results, and report back.
</capabilities>

`
}

func buildBehaviorSection() string {
	return `<behavior>
CRITICAL RULE: When the user asks you to run, create, or execute something,
you MUST call the appropriate tool in your response. NEVER respond with only text
like "I'll create that task" or "Let me run that" — you MUST include the tool
call in the SAME response. If you need to create a task AND fetch its result,
call create_*_task, then wait_for_task, then fetch_task_output all in sequence
without stopping to narrate between steps. Act first, summarize after.
</behavior>

`
}

func buildToolCallStyleSection() string {
	return `<tool_call_style>
Default: do not narrate routine, low-risk tool calls — just call the tool.
Narrate only when it helps: multi-step work, complex problems, sensitive actions, or when asked.
Keep narration brief and value-dense; avoid repeating obvious steps.
</tool_call_style>

`
}

func buildTaskTypesSection(mode PromptMode) string {
	var sb strings.Builder
	sb.WriteString(`<task_types>
- container: Run a command in a container. Use create_container_task.
  PREFERRED for: shell commands, CLI tools, scripts, data processing.
`)
	if mode == PromptModeFull {
		sb.WriteString(`  Common images (Chainguard, hardened, non-root):
    • bash/shell: "cgr.dev/chainguard/bash:latest"
    • python: "cgr.dev/chainguard/python:latest-dev" (includes pip)
    • node: "cgr.dev/chainguard/node:latest-dev" (includes npm)
    • go: "cgr.dev/chainguard/go:latest"
    • curl: "cgr.dev/chainguard/curl:latest"
    • git: "cgr.dev/chainguard/git:latest-dev"
`)
	}
	sb.WriteString(`  All containers run as non-root with read-only root filesystem.
  Writable paths: /tmp, /home/nonroot. Do NOT assume root access.
- ai: Run an LLM-powered task. Use create_ai_task with a providerRef.
  Use for: reasoning, analysis, content generation, code review, summarization,
  answering questions about data. The AI worker has built-in tools (code_exec,
  web_search, file_read, web_fetch, file_write) but runs in a minimal container without CLI tools.
  Do NOT use for infrastructure commands.
- agent: Run an external CLI runtime (Copilot, Claude Code). Use create_agent_task.
  Use for: code changes in a git repo, multi-file refactoring.
  IMPORTANT: When the user specifies an agent (via --agent or agentRef) that has a
  runtime configured, ALWAYS use create_agent_task with that agent name.
  Do NOT use create_container_task or create_ai_task for runtime agents.
</task_types>

`)
	return sb.String()
}

func buildCoordinationSection(mode PromptMode) string {
	if mode == PromptModeMinimal {
		return ""
	}
	return `<coordination>
For complex multi-step tasks, use the self-bootstrapping coordinator pattern:

PREFERRED (one-shot): Create a coordinator agent with initialPrompt to instantly start:
  create_agent(name="coordinator", coordination={enabled: true}, 
    systemPrompt="You are a coordinator. Analyze the task, create specialist agents 
    with create_agent, then delegate work with delegate_task.",
    initialPrompt="Build a REST API with tests")
  → This creates the agent AND starts a task in one call.

The coordinator agent will then:
1. Use create_agent to create specialist sub-agents (coder, reviewer, tester)
2. Use delegate_task to assign work to specialists
3. Use wait_for_tasks to collect results
4. Synthesize results and iterate if needed
5. Specialist agents are auto-cleaned up when the coordinator task is deleted

MANUAL (multi-step): For more control, create agents separately:
1. Create specialist agents with create_agent
2. Create a coordinator agent with coordination.enabled=true
3. Create a task with create_agent_task or create_ai_task referencing the coordinator

When no agents exist and the user needs complex work done:
- For simple commands (e.g., "list pods", "check disk"): use create_container_task
  with an appropriate image. No LLM needed.
- For questions needing reasoning (e.g., "explain this error"): use create_ai_task.
- For complex workflows: use the one-shot coordinator pattern above.
</coordination>

`
}

func buildSchedulingSection(mode PromptMode) string {
	if mode == PromptModeMinimal {
		return ""
	}
	return `<scheduling>
Any task type can be made recurring by setting the schedule parameter with a cron expression.
Common patterns:
- Every hour: "0 * * * *"
- Daily at midnight: "0 0 * * *"
- Weekdays at 9am: "0 9 * * 1-5"

When the user says "every", "recurring", "daily", "weekly", "hourly", or similar,
set the schedule parameter on the task.
</scheduling>

`
}

func buildRulesSection() string {
	return `<rules>
1. PREFER create_container_task for shell commands, CLI tools, scripts.
   Container tasks are fast, reliable, and run the exact command the user wants.
2. Use create_ai_task ONLY for work that requires a SEPARATE long-running LLM job
   (e.g., code generation, detailed code review, multi-file analysis). Do NOT
   create an AI task just to answer a question — answer it yourself directly.
3. When creating an ai task, always set providerRef to an available provider name
   (e.g., "openai"). Use the same provider that this chat session is using.
4. After creating a task, IMMEDIATELY call wait_for_task then fetch_task_output
   in the SAME turn — do not stop to narrate between tool calls.
5. Never guess namespace — use the namespace from the request or ask the user.
6. Provide clear summaries of what you did, what succeeded, and what failed.
7. If a task fails, check the error and try a different approach before giving up.
8. Do not create more tasks than necessary.
9. If no agents exist and user needs an agent task, create the agent first with create_agent.
10. When the user specifies an agent (agentRef) that has a "runtime" listed in the
   available_agents section, ALWAYS use create_agent_task — never create_container_task
   or create_ai_task. Runtime agents have their own CLI environment with full tool access.
</rules>

`
}

func buildExamplesSection(mode PromptMode) string {
	if mode == PromptModeMinimal {
		return ""
	}
	return `<examples>
Example 1: "list all pods in the cluster"
→ create_container_task (image: "cgr.dev/chainguard/kubectl:latest", command: ["kubectl","get","pods","-A","-o","wide"])
→ wait_for_task → fetch_task_output → show results

Example 2: "what are the top 5 largest images in the cluster?"
→ create_container_task (image: "cgr.dev/chainguard/kubectl:latest", command: ["sh","-c","kubectl get pods -A -o jsonpath='{...}' | sort | uniq -c | sort -rn | head -5"])
→ wait_for_task → fetch_task_output → summarize

Example 3: "process this CSV with Python"
→ create_container_task (image: "cgr.dev/chainguard/python:latest-dev", command: ["python3","-c","import csv; ..."])
→ wait_for_task → fetch_task_output

Example 4: "review this code for security issues"
→ create_ai_task (prompt: "Review for security vulnerabilities: ...", providerRef: "openai")
→ wait_for_task → fetch_task_output → summarize findings

Example 5: "refactor the auth module" (with agent available)
→ create_agent_task (agent: "coder", prompt: "Refactor the auth module...")
→ wait_for_task → fetch_task_output

Example 6: User specifies --agent my-agent (which has runtime: copilot)
→ create_agent_task (agent: "my-agent", prompt: "...")
  (Use create_agent_task because the agent has a runtime)
→ wait_for_task → fetch_task_output
</examples>
`
}

// buildDynamicContext fetches agents, tools, and providers from the cluster and formats them.
func (b *SystemPromptBuilder) buildDynamicContext(ctx context.Context) (agentsSection, toolsSection, providersSection string, err error) {
	// Fetch agents
	var agentList corev1alpha1.AgentList
	if err := b.client.List(ctx, &agentList, client.InNamespace(b.namespace)); err != nil {
		return "", "", "", fmt.Errorf("listing agents: %w", err)
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
		return "", "", "", fmt.Errorf("listing tools: %w", err)
	}

	toolLines := make([]string, 0, len(toolList.Items)+5)
	// Built-in tools
	toolLines = append(toolLines,
		"web_search - Search the web for information (built-in)",
		"code_exec - Execute code snippets (built-in)",
		"file_read - Read file contents (built-in)",
		"web_fetch - Fetch and extract content from URLs (built-in)",
		"file_write - Write files to workspace (built-in)",
	)

	for i := range toolList.Items {
		tool := &toolList.Items[i]
		toolLines = append(toolLines, fmt.Sprintf("%s - %s", tool.Name, tool.Spec.Description))
	}

	toolsSection = strings.Join(toolLines, "\n") + "\n"

	// Fetch providers
	var providerList corev1alpha1.ProviderList
	if err := b.client.List(ctx, &providerList, client.InNamespace(b.namespace)); err != nil {
		return "", "", "", fmt.Errorf("listing providers: %w", err)
	}

	providerNames := make([]string, 0, len(providerList.Items))
	for i := range providerList.Items {
		providerNames = append(providerNames, providerList.Items[i].Name)
	}

	providersSection = fmt.Sprintf("Runtime: providers=[%s] | agents=%d | tools=%d | container=yes\n",
		strings.Join(providerNames, ", "), len(agentList.Items), len(toolList.Items)+5)

	return agentsSection, toolsSection, providersSection, nil
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

// computeHash returns a truncated SHA-256 hash of the dynamic sections.
func (b *SystemPromptBuilder) computeHash(agents, tools, providers string) string {
	h := sha256.New()
	h.Write([]byte(agents))
	h.Write([]byte(tools))
	h.Write([]byte(providers))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}
