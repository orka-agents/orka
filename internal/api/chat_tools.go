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
	"encoding/json"
	"strings"

	"github.com/sozercan/mercan/internal/llm"
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
					"name":       map[string]any{"type": "string", "description": "Task name"},
					"prompt":     map[string]any{"type": "string", "description": "The prompt/instruction for the AI task"},
					"agentRef":     map[string]any{"type": "string", "description": "Optional Agent CRD name to use"},
					"providerRef":  map[string]any{"type": "string", "description": "Provider CRD name (defaults to 'default')"},
					"namespace":    map[string]any{"type": "string", "description": "Kubernetes namespace"},
					"timeout":    map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
					"priority":   map[string]any{"type": "integer", "description": "Priority 0-1000"},
					"sessionRef": map[string]any{"type": "string", "description": "Session name for conversation continuity"},
					"schedule":   map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
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
					"image":     map[string]any{"type": "string", "description": "Container image to run"},
					"command":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Command to execute"},
					"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Arguments to the command"},
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
					"timeout":   map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
					"priority":  map[string]any{"type": "integer", "description": "Priority 0-1000"},
					"schedule":  map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
				},
				"required": []string{"name", "image"},
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
					"agentRef":  map[string]any{"type": "string", "description": "Agent CRD name with runtime configured"},
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
					"timeout":   map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
					"maxTurns":  map[string]any{"type": "integer", "description": "Maximum agent loop iterations"},
					"workspace": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"gitRepo": map[string]any{"type": "string", "description": "Git repository URL"},
							"branch":  map[string]any{"type": "string", "description": "Git branch"},
							"subPath": map[string]any{"type": "string", "description": "Sub-path within the repo"},
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
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
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
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
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
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
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
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
				},
				"required": []string{"name"},
			}),
		},

		// --- Discovery tools ---
		{
			Name:        "list_agents",
			Description: "List available Agent CRDs with their model, tools, and runtime configuration. Use before creating a task with agentRef to find the right agent.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
				},
			}),
		},
		{
			Name:        "list_tools",
			Description: "List available Tool CRDs and built-in tools with their descriptions.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
				},
			}),
		},
		{
			Name:        "list_tasks",
			Description: "List tasks with optional status filter. Use to check what tasks exist or monitor multiple tasks.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
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
			Description: "Create an Agent CRD with model, tools, and optional runtime configuration.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":         map[string]any{"type": "string", "description": "Agent name"},
					"namespace":    map[string]any{"type": "string", "description": "Kubernetes namespace"},
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
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "update_agent",
			Description: "Update an existing Agent CRD.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":         map[string]any{"type": "string", "description": "Agent name"},
					"namespace":    map[string]any{"type": "string", "description": "Kubernetes namespace"},
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
			Description: "Delete an Agent CRD.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Agent name"},
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
				},
				"required": []string{"name"},
			}),
		},
		{
			Name:        "create_tool",
			Description: "Create a Tool CRD with an HTTP endpoint.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "Tool name"},
					"namespace":   map[string]any{"type": "string", "description": "Kubernetes namespace"},
					"description": map[string]any{"type": "string", "description": "Tool description"},
					"url":         map[string]any{"type": "string", "description": "HTTP endpoint URL"},
					"method":      map[string]any{"type": "string", "description": "HTTP method (default POST)"},
				},
				"required": []string{"name", "description", "url"},
			}),
		},
		{
			Name:        "delete_tool",
			Description: "Delete a Tool CRD.",
			Parameters: mustMarshal(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Tool name"},
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
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
					"namespace": map[string]any{"type": "string", "description": "Kubernetes namespace"},
				},
				"required": []string{"sessionId"},
			}),
		},
	}
}

// ShouldLoadManagementTools checks if the user message contains intent signals
// for management operations (create/update/delete agents, tools, or sessions).
func ShouldLoadManagementTools(message string) bool {
	lower := strings.ToLower(message)
	signals := []string{
		"create an agent",
		"create agent",
		"delete the tool",
		"delete tool",
		"update agent",
		"remove tool",
		"new agent",
		"make a tool",
		"delete session",
		"create tool",
	}
	for _, s := range signals {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}
