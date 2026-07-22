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

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	chattools "github.com/orka-agents/orka/internal/tools"
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

	agentsSection, toolsSection, providersSection, skillsSection, err := b.buildDynamicContext(ctx)
	if err != nil {
		return "", fmt.Errorf("building dynamic context: %w", err)
	}

	hash := b.computeHash(agentsSection, toolsSection, providersSection, skillsSection)
	if hash == b.cachedHash && userSystemPrompt == "" && b.cachedPrompt != "" {
		return b.cachedPrompt, nil
	}

	var sb strings.Builder

	sb.WriteString(buildIdentitySection())
	sb.WriteString(buildCapabilitiesSection())
	sb.WriteString(buildBehaviorSection())
	sb.WriteString(buildToolCallStyleSection())
	sb.WriteString(buildTaskTypesSection(m))
	sb.WriteString(buildValidationSection())
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

	if skillsSection != "" {
		sb.WriteString("<available_skills>\n")
		sb.WriteString(skillsSection)
		sb.WriteString("</available_skills>\n\n")
	}

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

LONG-RUNNING TASKS: Agent tasks (Copilot, Claude Code, Codex) typically run for 5-20 minutes.
You MUST keep calling wait_for_task in a loop until the task reaches a terminal state
(Succeeded or Failed). Do NOT give up after a few polls — keep waiting. If wait_for_task
returns "still running", immediately call wait_for_task again. Only stop when the task
has completed or failed. After completion, call fetch_task_output to get the result.
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
- agent: Run an external CLI runtime (Copilot, Claude Code, Codex, OpenCode).
  Use create_agent_task only for Agents that have runtime listed in available_agents.
  Use for: code changes in a git repo, multi-file refactoring.
  IMPORTANT: When the user specifies an agent (via --agent or agentRef) that has a
  runtime configured, ALWAYS use create_agent_task with that agent name.
  If the specified Agent has no runtime listed, including coordinator Agents backed
  by providerRef/model only, use create_ai_task with agentRef and providerRef instead.
  When the task involves a git repository, ALWAYS include the gitRepo URL in the
  workspace config so credentials are automatically mounted:
    create_agent_task(agent: "coder", prompt: "...", gitRepo: "https://github.com/org/repo", timeout: "15m")
  When creating new agents for coding tasks, check the agent_runtimes in the Runtime line above.
  Use whichever runtime is available. If multiple runtimes are available, prefer codex, then copilot.
  If no agent runtimes are available, use create_ai_task with agentRef/providerRef for existing non-runtime agents, or create_ai_task directly for LLM-only work.
  Agent tasks need more time than AI tasks. Set timeout to at least 15m.
  Do NOT use create_container_task or create_ai_task for runtime agents.
  Do NOT use create_agent_task for non-runtime agents.
</task_types>

`)
	return sb.String()
}

func buildValidationSection() string {
	return `<validation>
When validating code changes, determine the validation environment from repository evidence rather than demo- or scenario-specific defaults.
Every validation or discovery container task that inspects repository files MUST include a workspace with workspace.gitRepo, workspace.gitSecretRef when credentials are needed, and the exact branch/ref under test. Prefer workspace.ref = the implementation headSHA; otherwise use workspace.branch = the pushed branch. Do not validate repo changes from an empty container filesystem.
Before running full validation, inspect the workspace or run a read-only discovery container task with that workspace when needed. Prefer evidence in this order: CI workflow files, language/toolchain files (go.mod, package.json, pyproject.toml, Cargo.toml, etc.), Dockerfile/devcontainer files, Makefile targets, and project documentation.
For Go repositories, read go.mod and prefer a toolchain directive when present; otherwise use the go directive. Choose a matching golang:<major.minor> image. With golang:<major.minor>, commands MUST export:
  export PATH=/usr/local/go/bin:$PATH
  export GOCACHE=/tmp/go-cache
  export GOMODCACHE=/tmp/go-mod-cache
  export CGO_ENABLED=0
For other ecosystems, choose the image and command that match the repo's declared runtime and standard test command.
Report the selected validation image, command, workspace ref/branch, and evidence. If validation fails because the container is missing the repo, a tool is not on PATH, caches are unwritable, or the validation environment cannot be determined confidently, report VALIDATION_CONFIG_BLOCKED or retry validation with corrected configuration instead of treating it as a code failure.
</validation>

`
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

INTER-AGENT MESSAGING: Child tasks delegated by a coordinator can communicate with
each other using send_message and check_messages. This is useful when:
- One task produces results that a sibling task needs before it can start
- Tasks need to coordinate or exchange intermediate findings
- A pipeline of tasks where each step builds on the previous

The coordinator should instruct child tasks to use send_message (with to_task="*" to
broadcast to all siblings) and check_messages in their prompts. For reliable delivery,
delegate the sender first, wait for it to complete, then delegate the receiver.

CANCEL: The coordinator can use cancel_task to cancel running child tasks. This is
useful for race patterns (start multiple tasks, keep the first result, cancel the rest)
or when a child task's result makes other tasks unnecessary.

MANUAL (multi-step): For more control, create agents separately:
1. Create specialist agents with create_agent
2. Create a coordinator agent with coordination.enabled=true
3. Create a task referencing the coordinator: use create_agent_task only when the
   coordinator has runtime listed; otherwise use create_ai_task with agentRef and providerRef.

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
   When an agent has no "runtime" listed, including coordinator agents, use create_ai_task
   with agentRef and providerRef — never create_agent_task.
11. For multi-step workflows involving coding + review + PR creation:
    - Prefer available coordinator candidates: agents whose names contain "coordinator";
      if "dev-coordinator" exists, prefer it.
    - If the chosen coordinator has "runtime" listed in available_agents, start it with create_agent_task.
    - If the chosen coordinator has no "runtime" listed, start it with create_ai_task using agentRef and providerRef.
      A coordinator handles the full workflow: code → validate → review repair loop → PR → CI repair loop → validate + re-review → approve.
    - If no coordinator candidate exists, use the coordinator pattern: create a coordinator agent that delegates to
      specialist agents (coder, reviewer) sequentially.
      In the coordinator prompt, tell it to:
      * run coder → validation → reviewers → coder repairs → validation → reviewers until validation passes and all reviewers approve,
        with at most 8 review repair tasks;
      * determine validation image/command from repository evidence before running validation, and allow up to 6 validation repair tasks before reporting VALIDATION_BLOCKED;
      * create or update the PR only after validation passes and reviewer approval;
      * call check_pull_request_ci after PR creation with wait_timeout="30m" and poll_interval="30s";
      * when CI fails, delegate a focused CI repair to the coder on the PR branch,
        run validation and reviewers again, then re-check CI;
      * bound CI repair to at most 3 repair tasks; if checks are still pending after 30 minutes of pending-check waiting (status=pending with wait_timed_out=true), report CI_PENDING;
      * prefer additional focused repair iterations over stopping early when reviewers identify concrete diff-backed security, correctness, or acceptance-criteria issues;
      * avoid merge tools unless the user explicitly asks to merge.
    - Do NOT bundle all steps into a single agent task prompt.
12. Agent tasks run for 5-20 minutes. NEVER stop polling wait_for_task while a task
    is still running. Keep calling wait_for_task until the task reaches Succeeded or Failed.
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

Example 5: "refactor the auth module" (with runtime agent available)
→ create_agent_task (agent: "coder", prompt: "Refactor the auth module...", gitRepo: "https://github.com/org/repo")
→ wait_for_task → fetch_task_output

Example 6: User specifies --agent my-agent (which has runtime: copilot)
→ create_agent_task (agent: "my-agent", prompt: "...", gitRepo: "https://github.com/org/repo")
  (Use create_agent_task because the agent has a runtime; include gitRepo for credentials)
→ wait_for_task → fetch_task_output

Example 7: "Research X and then write a guide based on findings" (multi-step pipeline)
→ Use the coordinator pattern. The coordinator should:
  1. delegate_task to a researcher agent with prompt to research X and use send_message to broadcast findings
  2. wait_for_tasks for the researcher to complete
  3. delegate_task to a writer agent with prompt to check_messages for research findings and write the guide
  4. wait_for_tasks and synthesize the final result

Example 8: "Get me an answer as fast as possible" (race pattern)
→ Use the coordinator pattern. The coordinator should:
  1. delegate_task multiple agents with the same prompt in parallel
  2. wait_for_tasks — as soon as one completes, use cancel_task on the others
  3. Return the winning result
</examples>
`
}

// buildDynamicContext fetches agents, tools, and providers from the cluster and formats them.
func (b *SystemPromptBuilder) buildDynamicContext(ctx context.Context) (agentsSection, toolsSection, providersSection, skillsSection string, err error) {
	// Fetch agents
	var agentList corev1alpha1.AgentList
	if err := b.client.List(ctx, &agentList, client.InNamespace(b.namespace)); err != nil {
		return "", "", "", "", fmt.Errorf("listing agents: %w", err)
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
		return "", "", "", "", fmt.Errorf("listing tools: %w", err)
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
		return "", "", "", "", fmt.Errorf("listing providers: %w", err)
	}

	providerNames := make([]string, 0, len(providerList.Items))
	for i := range providerList.Items {
		providerNames = append(providerNames, providerList.Items[i].Name)
	}

	// Detect available agent runtimes by checking for well-known secrets
	var availableRuntimes []string
	var secretList corev1.SecretList
	if err := b.client.List(ctx, &secretList, client.InNamespace(b.namespace)); err == nil {
		secretsByName := make(map[string]*corev1.Secret, len(secretList.Items))
		for i := range secretList.Items {
			secret := &secretList.Items[i]
			secretsByName[secret.Name] = secret
		}
		if chattools.FirstDiscoverableRuntimeSecretName(secretsByName, corev1alpha1.AgentRuntimeCodex) != "" {
			availableRuntimes = append(availableRuntimes, "codex")
		}
		if chattools.FirstDiscoverableRuntimeSecretName(secretsByName, corev1alpha1.AgentRuntimeCopilot) != "" {
			availableRuntimes = append(availableRuntimes, "copilot")
		}
		if chattools.FirstDiscoverableRuntimeSecretName(secretsByName, corev1alpha1.AgentRuntimeClaude) != "" {
			availableRuntimes = append(availableRuntimes, "claude")
		}
		if chattools.FirstDiscoverableRuntimeSecretName(secretsByName, corev1alpha1.AgentRuntimeOpencode) != "" {
			availableRuntimes = append(availableRuntimes, "opencode")
		}
	}

	runtimeInfo := "none"
	if len(availableRuntimes) > 0 {
		runtimeInfo = strings.Join(availableRuntimes, ", ")
	}

	providersSection = fmt.Sprintf("Runtime: providers=[%s] | agents=%d | tools=%d | container=yes | agent_runtimes=[%s]\n",
		strings.Join(providerNames, ", "), len(agentList.Items), len(toolList.Items)+5, runtimeInfo)

	// Fetch skills
	var skillList corev1alpha1.SkillList
	if err := b.client.List(ctx, &skillList, client.InNamespace(b.namespace)); err != nil {
		return "", "", "", "", fmt.Errorf("listing skills: %w", err)
	}
	if len(skillList.Items) > 0 {
		skillLines := make([]string, 0, len(skillList.Items))
		for i := range skillList.Items {
			skill := &skillList.Items[i]
			name := skill.Name
			if skill.Spec.DisplayName != "" {
				name = skill.Spec.DisplayName
			}
			if skill.Spec.Description == "" {
				skillLines = append(skillLines, fmt.Sprintf("%s - %s", skill.Name, name))
				continue
			}
			skillLines = append(skillLines, fmt.Sprintf("%s - %s: %s", skill.Name, name, skill.Spec.Description))
		}
		skillsSection = strings.Join(skillLines, "\n") + "\n"
	}

	return agentsSection, toolsSection, providersSection, skillsSection, nil
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
func (b *SystemPromptBuilder) computeHash(agents, tools, providers, skills string) string {
	h := sha256.New()
	h.Write([]byte(agents))
	h.Write([]byte(tools))
	h.Write([]byte(providers))
	h.Write([]byte(skills))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}
