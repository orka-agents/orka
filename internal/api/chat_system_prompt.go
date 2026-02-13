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
- container: Run a command in a container. Use create_container_task.
  PREFERRED for: shell commands, kubectl, system tools, scripts, data processing.
  Use a base image that has the tools needed:
    • General CLI / shell / scripting: "cgr.dev/chainguard/bash:latest" (minimal shell)
    • Kubernetes / kubectl commands: "cgr.dev/chainguard/kubectl:latest"
    • Python scripts: "cgr.dev/chainguard/python:latest-dev" (includes pip and shell)
    • Node.js scripts: "cgr.dev/chainguard/node:latest-dev" (includes npm and shell)
    • Go programs: "cgr.dev/chainguard/go:latest" (includes go toolchain)
    • Curl / HTTP tools: "cgr.dev/chainguard/curl:latest"
  These are hardened, minimal, zero/low-CVE Chainguard images rebuilt daily.
  The "-dev" variants include package managers (pip, npm, apk) and shells.
  The command runs directly — no LLM involved. Fast and reliable.
  If a tool isn't available in the base image, use a -dev variant and install it
  (e.g., command: ["sh","-c","apk add --no-cache jq && jq ..."]).
- ai: Run an LLM-powered task. Use create_ai_task with a providerRef.
  Use for: reasoning, analysis, content generation, code review, summarization,
  answering questions about data. The AI worker has built-in tools (code_exec,
  web_search, file_read) but runs in a minimal container without cluster access
  or CLI tools like kubectl. Do NOT use for infrastructure commands.
- agent: Run an external CLI runtime (Copilot, Claude Code). Use create_agent_task.
  Use for: code changes in a git repo, multi-file refactoring.
</task_types>

<coordination>
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
1. Create specialist Agent CRDs with create_agent
2. Create a coordinator agent with coordination.enabled=true
3. Create a task with create_agent_task or create_ai_task referencing the coordinator

When no agents exist and the user needs complex work done:
- For simple commands (e.g., "list pods", "check disk"): use create_container_task
  with an appropriate image. No LLM needed.
- For questions needing reasoning (e.g., "explain this error"): use create_ai_task.
- For complex workflows: use the one-shot coordinator pattern above.
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
1. PREFER create_container_task for shell commands, kubectl, CLI tools, scripts.
   Container tasks are fast, reliable, and run the exact command the user wants.
2. Use create_ai_task ONLY when LLM reasoning is needed (analysis, generation,
   summarization, answering questions). Do NOT use AI tasks for kubectl or shell.
3. When creating an ai task, always set providerRef to an available provider name
   (e.g., "openai"). Use the same provider that this chat session is using.
4. After creating a task, call wait_for_task then fetch_task_output to get results.
5. Never guess namespace — use the namespace from the chat request or ask the user.
6. Provide clear summaries of what you did, what succeeded, and what failed.
7. If a task fails, check the error and try a different approach before giving up.
8. Do not create more tasks than necessary.
9. If no agents exist and user needs an agent task, create the agent first with create_agent.
</rules>

<examples>
Example 1: User asks "list all pods in the cluster"
→ create_container_task (image: "cgr.dev/chainguard/kubectl:latest", command: ["kubectl","get","pods","-A","-o","wide"])
→ wait_for_task
→ fetch_task_output
→ show the pod list to the user

Example 2: User asks "what are the top 5 largest images in the cluster?"
→ create_container_task (image: "cgr.dev/chainguard/kubectl:latest", command: ["sh","-c","kubectl get pods -A -o jsonpath='{range .items[*]}{.spec.containers[*].image}{\"\\n\"}{end}' | sort | uniq -c | sort -rn | head -5"])
→ wait_for_task
→ fetch_task_output
→ summarize the results

Example 3: User asks "review this code for security issues" (with code provided)
→ create_ai_task (prompt: "Review the following code for security vulnerabilities: ...", providerRef: "openai")
→ wait_for_task
→ fetch_task_output
→ summarize findings

Example 4: User asks "run a Python script that processes data"
→ create_container_task (image: "cgr.dev/chainguard/python:latest-dev", command: ["python3","-c","import json; ..."])
→ wait_for_task
→ fetch_task_output

Example 5: User asks "debug this k8s error"
→ create_agent (name: "k8s-debugger", systemPrompt: "You are a Kubernetes debugging coordinator...", 
   coordination: {enabled: true}, initialPrompt: "Investigate and debug the k8s error: ...")
→ wait_for_task (the auto-created task)
→ fetch_task_output
→ summarize findings
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
