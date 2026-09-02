/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/workerenv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// CreateAgentTool implements dynamic Agent CRD creation
type CreateAgentTool struct {
	k8sClient     client.Client
	executionMode executionmode.Mode
}

// CreateAgentArgs are the arguments for the create_agent tool
type CreateAgentArgs struct {
	Role         string            `json:"role"`
	SystemPrompt string            `json:"systemPrompt"`
	Model        *ModelArgs        `json:"model,omitempty"`
	ProviderRef  string            `json:"providerRef,omitempty"`
	Tools        []string          `json:"tools,omitempty"`
	Skills       []string          `json:"skills,omitempty"`
	Coordination *CoordinationArgs `json:"coordination,omitempty"`
	Runtime      *RuntimeArgs      `json:"runtime,omitempty"`
}

// ModelArgs specifies LLM model configuration
type ModelArgs struct {
	Provider      string `json:"provider,omitempty"`
	Name          string `json:"name,omitempty"`
	ContextWindow *int32 `json:"contextWindow,omitempty"`
	MaxTokens     *int32 `json:"maxTokens,omitempty"`
}

// RuntimeArgs specifies agent CLI runtime configuration
type RuntimeArgs struct {
	Type                string   `json:"type"`
	SecretRef           string   `json:"secretRef,omitempty"`
	DefaultAllowedTools []string `json:"defaultAllowedTools,omitempty"`
	DefaultAllowBash    *bool    `json:"defaultAllowBash,omitempty"`
}

func applyOpencodeRuntimeDefaults(runtime *corev1alpha1.AgentCLIRuntime, allowedToolsSupplied, allowBashSupplied bool) {
	if runtime == nil || runtime.Type != corev1alpha1.AgentRuntimeOpencode {
		return
	}
	if !allowedToolsSupplied {
		runtime.DefaultAllowedTools = acp.OpenCodeDefaultAllowedTools()
	}
	if !allowBashSupplied {
		allowBash := true
		runtime.DefaultAllowBash = &allowBash
	}
}

// CoordinationArgs specifies coordination configuration
type CoordinationArgs struct {
	Enabled               bool              `json:"enabled"`
	MaxDepth              int32             `json:"maxDepth,omitempty"`
	MaxConcurrentChildren int32             `json:"maxConcurrentChildren,omitempty"`
	AllowedAgents         []AllowedAgentArg `json:"allowedAgents,omitempty"`
}

// AllowedAgentArg specifies an allowed agent for coordination
type AllowedAgentArg struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// CreateAgentResult is the result of creating an agent
type CreateAgentResult struct {
	AgentName string `json:"agentName"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"`
}

// NewCreateAgentTool creates a new create_agent tool
func NewCreateAgentTool(k8sClient client.Client, mode executionmode.Mode) *CreateAgentTool {
	return &CreateAgentTool{
		k8sClient:     k8sClient,
		executionMode: mode,
	}
}

// Name returns the tool name
func (t *CreateAgentTool) Name() string {
	return createAgentToolName
}

// Description returns the tool description
func (t *CreateAgentTool) Description() string {
	return "Dynamically create an Agent CRD. The agent is owned by the parent task and will be cleaned up automatically when the task is deleted."
}

// Parameters returns the JSON Schema for parameters
func (t *CreateAgentTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"role": {
				"type": "string",
				"description": "Role name for the agent (e.g. coder, reviewer)"
			},
			"systemPrompt": {
				"type": "string",
				"description": "System prompt for the agent. Required unless runtime.type is opencode; OpenCode cannot enforce Agent system prompts, so omit this field and use task prompts instead."
			},
			"model": {
				"type": "object",
				"description": "LLM model config; model.name is inherited from the coordinator when omitted except for OpenCode, which requires provider/model form",
				"properties": {
					"provider": {
						"type": "string",
						"description": "LLM provider (e.g. anthropic, openai). For OpenCode, use only with a provider-relative bare model name; omit it when model.name already contains the full provider/model ID."
					},
					"name": {
						"type": "string",
						"description": "Model identifier"
					},
					"contextWindow": {
						"type": "integer",
						"minimum": 1,
						"description": "Reviewed model context capacity; required for OpenCode"
					},
					"maxTokens": {
						"type": "integer",
						"minimum": 1,
						"description": "Reviewed maximum output tokens; required for OpenCode"
					}
				}
			},
			"providerRef": {
				"type": "string",
				"description": "Provider CRD reference name; inherited from coordinator if not set"
			},
			"tools": {
				"type": "` + jsonSchemaTypeArray + `",
				"items": {"type": "string"},
				"description": "Tool names to attach to the agent"
			},
			"skills": {
				"type": "` + jsonSchemaTypeArray + `",
				"items": {"type": "string"},
				"description": "Skill names to attach to the agent"
			},
			"coordination": {
				"type": "object",
				"description": "Enable sub-delegation for the agent",
				"properties": {
					"enabled": {
						"type": "` + jsonSchemaTypeBoolean + `",
						"description": "Whether coordination is enabled"
					},
					"maxDepth": {
						"type": "integer",
						"description": "Maximum delegation depth"
					},
					"maxConcurrentChildren": {
						"type": "integer",
						"description": "Maximum concurrent child tasks"
					},
					"allowedAgents": {
						"type": "` + jsonSchemaTypeArray + `",
						"items": {
							"type": "object",
							"properties": {
								"name": {"type": "string"},
								"namespace": {"type": "string"}
							},
							"required": ["name"]
						},
						"description": "Agents this agent can delegate to"
					}
				}
			},
			"runtime": {
				"type": "object",
					"description": "Set to make this a built-in ACP runtime agent (copilot, claude, codex, or opencode). Runtime agents run code against a materialized workspace. Do NOT set runtime on coordinator agents.",
				"properties": {
					"type": {
						"type": "string",
							"description": "Runtime type: copilot, claude, codex, or opencode"
					},
					"secretRef": {
						"type": "string",
						"description": "Deprecated legacy field. Built-in ACP runtimes are credential-free at the Agent boundary; OpenCode rejects it."
					},
					"defaultAllowedTools": {
						"type": "array",
						"items": {"type": "string"},
						"description": "Default CLI tools allowed for tasks using this runtime agent. OpenCode defaults to Read, Write, Edit, Bash, Glob, and Grep when omitted."
					},
					"defaultAllowBash": {
						"type": "boolean",
						"description": "Whether bash is allowed by default. OpenCode defaults to true when omitted."
					}
				}
			}
		},
		"required": ["role"]
	}`)
}

func isBuiltInACPRuntime(runtimeType corev1alpha1.AgentRuntimeType) bool {
	switch runtimeType {
	case corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeOpencode:
		return true
	default:
		return false
	}
}

func normalizedCreateAgentModel(runtime *RuntimeArgs, model *ModelArgs) (string, error) {
	requested := ""
	if model != nil {
		requested = strings.TrimSpace(model.Name)
	}
	if runtime == nil {
		return requested, nil
	}
	runtimeType := corev1alpha1.AgentRuntimeType(strings.TrimSpace(runtime.Type))
	if model != nil {
		if err := validateBuiltInRuntimeModelLimits(runtimeType, &corev1alpha1.ModelConfig{
			ContextWindow: model.ContextWindow,
			MaxTokens:     model.MaxTokens,
		}); err != nil {
			return "", err
		}
	}
	if runtimeType != corev1alpha1.AgentRuntimeOpencode {
		return requested, nil
	}
	var spec *corev1alpha1.ModelConfig
	if model != nil {
		spec = &corev1alpha1.ModelConfig{
			Provider:      model.Provider,
			Name:          model.Name,
			ContextWindow: model.ContextWindow,
			MaxTokens:     model.MaxTokens,
		}
	}
	providerID, modelID, specErr := validateOpenCodeModelSpec(spec)
	if specErr != nil {
		return "", specErr
	}
	return providerID + "/" + modelID, nil
}

func configureCreatedAgentRuntime(agent *corev1alpha1.Agent, runtimeArgs *RuntimeArgs) error {
	if runtimeArgs == nil {
		return nil
	}
	runtimeType := corev1alpha1.AgentRuntimeType(strings.TrimSpace(runtimeArgs.Type))
	runtime := &corev1alpha1.AgentCLIRuntime{Type: runtimeType}
	if runtimeArgs.DefaultAllowedTools != nil {
		runtime.DefaultAllowedTools = append([]string{}, runtimeArgs.DefaultAllowedTools...)
	}
	if runtimeArgs.DefaultAllowBash != nil {
		allowBash := *runtimeArgs.DefaultAllowBash
		runtime.DefaultAllowBash = &allowBash
	}
	if runtimeType == corev1alpha1.AgentRuntimeOpencode {
		if strings.TrimSpace(runtimeArgs.SecretRef) != "" {
			return fmt.Errorf("opencode runtime does not accept runtime.secretRef; provider access is controller-proxied")
		}
		applyOpencodeRuntimeDefaults(runtime, runtimeArgs.DefaultAllowedTools != nil, runtimeArgs.DefaultAllowBash != nil)
		if agent.Spec.Model != nil {
			agent.Spec.Model.Provider = ""
		}
	}
	agent.Spec.Runtime = runtime
	agent.Spec.ProviderRef = nil
	agent.Spec.SecretRef = nil
	return nil
}

// Execute creates an Agent CRD dynamically
func (t *CreateAgentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a CreateAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if a.Role == "" {
		return "", fmt.Errorf("role is required")
	}
	runtimeType := ""
	if a.Runtime != nil {
		runtimeType = strings.TrimSpace(a.Runtime.Type)
		if runtimeType == "" {
			return "", fmt.Errorf("runtime.type is required when runtime is provided")
		}
		if !isBuiltInACPRuntime(corev1alpha1.AgentRuntimeType(runtimeType)) {
			return "", fmt.Errorf("unsupported runtime type %q; supported built-in runtimes are copilot, claude, codex, and opencode", runtimeType)
		}
	}
	if runtimeType == string(corev1alpha1.AgentRuntimeOpencode) {
		if strings.TrimSpace(a.SystemPrompt) != "" {
			return "", fmt.Errorf("opencode runtime does not support systemPrompt; omit it and use task prompts for instructions")
		}
	} else if strings.TrimSpace(a.SystemPrompt) == "" {
		return "", fmt.Errorf("systemPrompt is required")
	}
	requestedModel, err := normalizedCreateAgentModel(a.Runtime, a.Model)
	if err != nil {
		return "", err
	}

	parentName := os.Getenv(envOrkaTaskName)
	parentNamespace := os.Getenv(envOrkaTaskNamespace)

	ns := parentNamespace
	if ns == "" {
		ns = defaultNamespace
	}

	// Generate agent name: {parentTaskName}-{role}-{shortHash}
	hash := sha256.Sum256([]byte(parentName + a.Role + time.Now().UTC().String()))
	shortHash := fmt.Sprintf("%x", hash[:3])
	agentName := fmt.Sprintf("%s-%s-%s", parentName, a.Role, shortHash)

	// Fetch parent task for owner reference
	parentTask := &corev1alpha1.Task{}
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: parentName, Namespace: ns}, parentTask); err != nil {
		return "", fmt.Errorf("failed to get parent task: %w", err)
	}

	// Build model config — clear provider to avoid mismatch with providerRef
	model := &corev1alpha1.ModelConfig{}
	if a.Model != nil {
		model.Name = requestedModel
		model.ContextWindow = a.Model.ContextWindow
		model.MaxTokens = a.Model.MaxTokens
	}
	if model.Name == "" {
		model.Name = os.Getenv(workerenv.AIModel)
	}

	// Build provider ref — inherit from parent agent's providerRef for reliability,
	// since the LLM often guesses wrong provider names (e.g., "anthropic" instead of "default")
	providerRefName := ""
	if parentTask.Spec.AgentRef != nil {
		parentAgent := &corev1alpha1.Agent{}
		agentNS := parentTask.Spec.AgentRef.Namespace
		if agentNS == "" {
			agentNS = ns
		}
		if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: parentTask.Spec.AgentRef.Name, Namespace: agentNS}, parentAgent); err == nil {
			if parentAgent.Spec.ProviderRef != nil {
				providerRefName = parentAgent.Spec.ProviderRef.Name
			}
		}
	}
	if providerRefName == "" {
		providerRefName = a.ProviderRef
	}
	if providerRefName == "" {
		providerRefName = os.Getenv(workerenv.AIProvider)
	}
	if providerRefName == "" {
		providerRefName = defaultNamespace
	}

	// Build tools
	toolRefs := make([]corev1alpha1.ToolReference, 0, len(a.Tools))
	for _, name := range a.Tools {
		toolRefs = append(toolRefs, corev1alpha1.ToolReference{Name: name})
	}

	// Build skills
	skillRefs := make([]corev1alpha1.SkillReference, 0, len(a.Skills))
	for _, name := range a.Skills {
		skillRefs = append(skillRefs, corev1alpha1.SkillReference{
			Name: name,
		})
	}

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName,
			Namespace: ns,
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(parentName),
				labels.LabelCreatedBy:  createAgentToolName,
				labels.LabelAgentRole:  a.Role,
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: parentName,
			},
		},
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: providerRefName},
			Model:       model,
			Tools:       toolRefs,
			Skills:      skillRefs,
		},
	}
	if strings.TrimSpace(a.SystemPrompt) != "" {
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: a.SystemPrompt}
	}

	// Set coordination config if provided
	if a.Coordination != nil {
		coord := &corev1alpha1.CoordinationConfig{
			Enabled:               a.Coordination.Enabled,
			MaxDepth:              a.Coordination.MaxDepth,
			MaxConcurrentChildren: a.Coordination.MaxConcurrentChildren,
		}
		for _, aa := range a.Coordination.AllowedAgents {
			coord.AllowedAgents = append(coord.AllowedAgents, corev1alpha1.AllowedAgent{
				Name:      aa.Name,
				Namespace: aa.Namespace,
			})
		}
		agent.Spec.Coordination = coord
	}

	// Set the normalized built-in runtime when provided.
	if err := configureCreatedAgentRuntime(agent, a.Runtime); err != nil {
		return "", err
	}
	if err := executionmode.DefaultBuiltInAgentContract(agent, t.executionMode); err != nil {
		return "", err
	}

	// Set a non-blocking owner reference for auto-cleanup. Worker RBAC allows
	// creating child resources but not updating the parent Task's finalizers.
	isController := true
	agent.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       taskKindString,
			Name:       parentTask.Name,
			UID:        parentTask.UID,
			Controller: &isController,
		},
	}

	if err := t.k8sClient.Create(ctx, agent); err != nil {
		return "", fmt.Errorf("failed to create agent: %w", err)
	}

	result := CreateAgentResult{
		AgentName: agent.Name,
		Namespace: agent.Namespace,
		Status:    GitHubPullRequestStatusCreated,
	}
	output, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// Ensure CreateAgentTool implements Tool
var _ Tool = (*CreateAgentTool)(nil)
