/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"

	"github.com/sozercan/orka/internal/llm"
)

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// CoreTools returns the 7 core task tools and 3 discovery tools that are always loaded.
func CoreTools() []llm.Tool {
	return []llm.Tool{
		// --- Core task tools ---
		{
			Name:        "create_ai_task",
			Description: "Create an AI/LLM-powered task. Use when the user needs LLM reasoning, code review, content generation, or analysis. Do NOT use for running shell commands or CLI runtimes.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "Task name"},
					"prompt":      map[string]any{"type": "string", "description": "The prompt/instruction for the AI task"},
					"agentRef":    map[string]any{"type": "string", "description": "Optional Agent name to use"},
					"providerRef": map[string]any{"type": "string", "description": "Provider name (defaults to 'default')"},
					"namespace":   map[string]any{"type": "string", "description": "Namespace"},
					"timeout":     map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
					"priority":    map[string]any{"type": "integer", "description": "Priority 0-1000"},
					"sessionRef":  map[string]any{"type": "string", "description": "Session name for conversation continuity"},
					"schedule":    map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
				},
				"required": []string{"name", "prompt"},
			}),
		},
		{
			Name:        "create_container_task",
			Description: "Create a container task to run commands. Use when the user needs to run a shell command, build code, or execute a container image. Do NOT use for LLM reasoning.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Task name"},
					"image":     map[string]any{"type": "string", "description": "Container image to run. Leave empty to use the default worker image which includes common tools (kubectl, sh) and writes results to a ConfigMap. Only set a custom image if you need a specific runtime not in the default worker."},
					"command":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Command to execute"},
					"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Arguments to the command"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
					"timeout":   map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
					"priority":  map[string]any{"type": "integer", "description": "Priority 0-1000"},
					"schedule":  map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "create_agent_task",
			Description: "Create a task using an external CLI runtime (Copilot, Claude Code) for code changes in a git repo. Do NOT use for simple container commands or direct LLM reasoning.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Task name"},
					"prompt":    map[string]any{"type": "string", "description": "The prompt/instruction for the agent"},
					"agentRef":  map[string]any{"type": "string", "description": "Agent name with runtime configured"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
					"timeout":   map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
					"maxTurns":  map[string]any{"type": "integer", "description": "Maximum agent loop iterations"},
					"workspace": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"gitRepo":    map[string]any{"type": "string", "description": "Git repository URL"},
							"branch":     map[string]any{"type": "string", "description": "Git branch to clone from (must exist). Omit to use the default branch."},
							"pushBranch": map[string]any{"type": "string", "description": "Branch name to push changes to (will be created if it doesn't exist). Use this for new feature branches."},
							"subPath":    map[string]any{"type": "string", "description": "Sub-path within the repo"},
						},
					},
					"schedule": map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
				},
				"required": []string{"name", "prompt", "agentRef"},
			}),
		},
		{
			Name:        "check_task_progress",
			Description: "Get the current phase, duration, and status conditions of a task. Do NOT use to get the output/result — use fetch_task_output for that.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Task name"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "fetch_task_output",
			Description: "Get the result/output of a completed task from its ConfigMap. Returns the task result truncated to 2K characters if large. Do NOT use to check if a task is still running — use check_task_progress for that.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Task name"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "wait_for_task",
			Description: "Wait for a task to complete. Each call waits up to the specified timeout. If the task isn't done, you can call again or do other work. Use after creating a task.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Task name"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
					"timeout":   map[string]any{"type": "integer", "description": "Seconds to wait (max 60, default 30)"},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "cancel_task",
			Description: "Cancel and delete a task. Use when a task is stuck, no longer needed, or the user requests cancellation.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Task name"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
				},
				"required": []string{"name"},
			}),
		},

		// --- Discovery tools ---
		{
			Name:        "list_agents",
			Description: "List available agents with their model, tools, and runtime configuration. Use before creating a task with agentRef to find the right agent.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
				},
			}),
		},
		{
			Name:        "list_tools",
			Description: "List available tools and built-in tools with their descriptions.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
				},
			}),
		},
		{
			Name:        "list_tasks",
			Description: "List tasks with optional status filter. Use to check what tasks exist or monitor multiple tasks.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
					"status":    map[string]any{"type": "string", "description": "Filter by status: Pending, Running, Succeeded, Failed"},
					"limit":     map[string]any{"type": "integer", "description": "Max results to return (default 20)"},
				},
			}),
		},
	}
}

// ManagementTools returns the 6 lazy-loaded management tools for agent, tool, and session CRUD.
func ManagementTools() []llm.Tool {
	return []llm.Tool{
		{
			Name:        "create_agent",
			Description: "Create an agent with model, tools, and optional runtime and coordination configuration. Enable coordination to allow this agent to delegate tasks to other agents. Provide initialPrompt to also create and start a Task immediately.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":         map[string]any{"type": "string", "description": "Agent name"},
					"namespace":    map[string]any{"type": "string", "description": "Namespace"},
					"providerRef":  map[string]any{"type": "string", "description": "Provider name (defaults to 'default')"},
					"systemPrompt": map[string]any{"type": "string", "description": "System prompt for the agent"},
					"tools":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tool names to attach"},
					"model": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"provider":    map[string]any{"type": "string", "description": "Model provider (e.g. anthropic, openai)"},
							"name":        map[string]any{"type": "string", "description": "Model name"},
							"temperature": map[string]any{"type": "number", "description": "Sampling temperature"},
						},
					},
					"runtime": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"type":            map[string]any{"type": "string", "description": "Runtime type: copilot or claude"},
							"defaultMaxTurns": map[string]any{"type": "integer", "description": "Default max agent loop iterations"},
						},
					},
					"initialPrompt": map[string]any{
						"type":        "string",
						"description": "When provided, automatically create and start a Task using this agent with this prompt. One tool call = agent + task. Leave empty to only create the agent config without running it.",
					},
					"coordination": map[string]any{
						"type":        "object",
						"description": "Enable multi-agent coordination so this agent can delegate tasks to other agents via delegate_task/wait_for_tasks tools",
						"properties": map[string]any{
							"enabled":               map[string]any{"type": "boolean", "description": "Enable coordination (delegate_task/wait_for_tasks tools)"},
							"maxConcurrentChildren": map[string]any{"type": "integer", "description": "Max concurrent child tasks (default 5)"},
							"maxDepth":              map[string]any{"type": "integer", "description": "Max delegation depth (default 3)"},
							"allowedAgents": map[string]any{
								"type":        "array",
								"description": "List of agent names this agent can delegate to. If empty, can delegate to any agent.",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"name":      map[string]any{"type": "string", "description": "Agent name"},
										"namespace": map[string]any{"type": "string", "description": "Agent namespace (defaults to same namespace)"},
									},
									"required": []string{"name"},
								},
							},
						},
					},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "update_agent",
			Description: "Update an existing agent.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":         map[string]any{"type": "string", "description": "Agent name"},
					"namespace":    map[string]any{"type": "string", "description": "Namespace"},
					"systemPrompt": map[string]any{"type": "string", "description": "System prompt for the agent"},
					"tools":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tool names to attach"},
					"model": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"provider":    map[string]any{"type": "string", "description": "Model provider (e.g. anthropic, openai)"},
							"name":        map[string]any{"type": "string", "description": "Model name"},
							"temperature": map[string]any{"type": "number", "description": "Sampling temperature"},
						},
					},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "delete_agent",
			Description: "Delete an agent.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Agent name"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "create_tool",
			Description: "Create a tool with an HTTP endpoint.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "Tool name"},
					"namespace":   map[string]any{"type": "string", "description": "Namespace"},
					"description": map[string]any{"type": "string", "description": "Tool description"},
					"url":         map[string]any{"type": "string", "description": "HTTP endpoint URL"},
					"method":      map[string]any{"type": "string", "description": "HTTP method (default POST)"},
				},
				"required": []string{"name", "description", "url"},
			}),
		},
		{
			Name:        "delete_tool",
			Description: "Delete a tool.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Tool name"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "delete_session",
			Description: "Delete a session and its transcript data.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "Session ID to delete"},
					"namespace": map[string]any{"type": "string", "description": "Namespace"},
				},
				"required": []string{"sessionId"},
			}),
		},
	}
}
