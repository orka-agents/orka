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

	"github.com/orka-agents/orka/internal/agentcontext"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/workerenv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

type CreateAgentTool struct {
	k8sClient     client.Client
	executionMode executionmode.Mode
}

type CreateAgentArgs struct {
	Role         string                   `json:"role"`
	SystemPrompt string                   `json:"systemPrompt"`
	Soul         *corev1alpha1.SoulSource `json:"soul,omitempty"`
	Model        *ModelArgs               `json:"model,omitempty"`
	ProviderRef  string                   `json:"providerRef,omitempty"`
	Tools        []string                 `json:"tools,omitempty"`
	Skills       []string                 `json:"skills,omitempty"`
	Coordination *CoordinationArgs        `json:"coordination,omitempty"`
	Runtime      *RuntimeArgs             `json:"runtime,omitempty"`
}

type ModelArgs struct {
	Provider      string `json:"provider,omitempty"`
	Name          string `json:"name,omitempty"`
	ContextWindow *int32 `json:"contextWindow,omitempty"`
	MaxTokens     *int32 `json:"maxTokens,omitempty"`
}

type RuntimeArgs struct {
	Type                string                                    `json:"type"`
	ContractVersion     *corev1alpha1.AgentRuntimeContractVersion `json:"contractVersion,omitempty"`
	DefaultAllowedTools []string                                  `json:"defaultAllowedTools,omitempty"`
	DefaultAllowBash    *bool                                     `json:"defaultAllowBash,omitempty"`
}

func (in *RuntimeArgs) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name := range fields {
		if strings.EqualFold(name, "secretRef") {
			return fmt.Errorf("runtime.secretRef is no longer supported; provider access is controller-proxied")
		}
	}
	type plain RuntimeArgs
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*in = RuntimeArgs(value)
	return nil
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

type CoordinationArgs struct {
	Enabled               bool              `json:"enabled"`
	MaxDepth              int32             `json:"maxDepth,omitempty"`
	MaxConcurrentChildren int32             `json:"maxConcurrentChildren,omitempty"`
	AllowedAgents         []AllowedAgentArg `json:"allowedAgents,omitempty"`
}

type AllowedAgentArg struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type CreateAgentResult struct {
	AgentName string `json:"agentName"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"`
}

func NewCreateAgentTool(k8sClient client.Client, mode executionmode.Mode) *CreateAgentTool {
	return &CreateAgentTool{
		k8sClient:     k8sClient,
		executionMode: mode,
	}
}

func (t *CreateAgentTool) Name() string {
	return createAgentToolName
}

func (t *CreateAgentTool) Description() string {
	return "Dynamically create an Agent CRD. The agent is owned by the parent task and will be cleaned up automatically when the task is deleted."
}

func (t *CreateAgentTool) Parameters() json.RawMessage {
	return withSoulParameter(json.RawMessage(`{
		"type": "object",
		"properties": {
			"role": {
				"type": "string",
				"description": "Role name for the agent (e.g. coder, reviewer)"
			},
			"systemPrompt": {
				"type": "string",
				"description": "Role instructions for the agent; persona defaults may be supplied separately through soul. Built-in harness v2 Copilot instructions cannot contain @ characters."
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
								"name": {"type": "string", "minLength": 1, "pattern": "\\S"},
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
					"contractVersion": {
						"type": "string",
						"enum": ["orka.harness.v2"],
						"description": "Orka runtime protocol. Defaults to orka.harness.v2."
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
	}`))
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
		if isBuiltInACPRuntime(runtimeType) && requested == "" {
			return "", fmt.Errorf("model.name is required for %s runtime Agents: the ACP runtime session has no default model", runtimeType)
		}
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

func configureCreatedAgentRuntime(agent *corev1alpha1.Agent, runtimeArgs *RuntimeArgs) {
	if runtimeArgs == nil {
		return
	}
	runtimeType := corev1alpha1.AgentRuntimeType(strings.TrimSpace(runtimeArgs.Type))
	runtime := &corev1alpha1.AgentCLIRuntime{Type: runtimeType, ContractVersion: runtimeArgs.ContractVersion}
	if runtimeArgs.DefaultAllowedTools != nil {
		runtime.DefaultAllowedTools = append([]string{}, runtimeArgs.DefaultAllowedTools...)
	}
	if runtimeArgs.DefaultAllowBash != nil {
		allowBash := *runtimeArgs.DefaultAllowBash
		runtime.DefaultAllowBash = &allowBash
	}
	if runtimeType == corev1alpha1.AgentRuntimeOpencode {
		applyOpencodeRuntimeDefaults(runtime, runtimeArgs.DefaultAllowedTools != nil, runtimeArgs.DefaultAllowBash != nil)
		if agent.Spec.Model != nil {
			agent.Spec.Model.Provider = ""
		}
	}
	agent.Spec.Runtime = runtime
	agent.Spec.ProviderRef = nil
	agent.Spec.SecretRef = nil
}

//nolint:gocyclo // Keep Agent construction and coordination-policy validation in one flow.
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
	if err := agentcontext.ValidateSource(a.Soul); err != nil {
		return "", err
	}
	if runtimeType != string(corev1alpha1.AgentRuntimeOpencode) && strings.TrimSpace(a.SystemPrompt) == "" && a.Soul == nil {
		return "", fmt.Errorf("systemPrompt is required")
	}
	if a.Model != nil {
		if a.Model.ContextWindow != nil && *a.Model.ContextWindow < 1 {
			return "", fmt.Errorf("model.contextWindow must be at least 1")
		}
		if a.Model.MaxTokens != nil && *a.Model.MaxTokens < 1 {
			return "", fmt.Errorf("model.maxTokens must be at least 1")
		}
	}
	effectiveModel := a.Model
	if runtimeType != string(corev1alpha1.AgentRuntimeOpencode) &&
		(effectiveModel == nil || strings.TrimSpace(effectiveModel.Name) == "") {
		if inheritedModel := strings.TrimSpace(os.Getenv(workerenv.AIModel)); inheritedModel != "" {
			modelCopy := ModelArgs{Name: inheritedModel}
			if effectiveModel != nil {
				modelCopy = *effectiveModel
				modelCopy.Name = inheritedModel
			}
			effectiveModel = &modelCopy
		}
	}
	requestedModel, err := normalizedCreateAgentModel(a.Runtime, effectiveModel)
	if err != nil {
		return "", err
	}

	parentName := os.Getenv(envOrkaTaskName)
	parentNamespace := os.Getenv(envOrkaTaskNamespace)

	ns := parentNamespace
	if ns == "" {
		ns = defaultNamespace
	}

	hash := sha256.Sum256([]byte(parentName + a.Role + time.Now().UTC().String()))
	shortHash := fmt.Sprintf("%x", hash[:3])
	agentName := fmt.Sprintf("%s-%s-%s", parentName, a.Role, shortHash)

	parentTask := &corev1alpha1.Task{}
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: parentName, Namespace: ns}, parentTask); err != nil {
		return "", fmt.Errorf("failed to get parent task: %w", err)
	}

	model := &corev1alpha1.ModelConfig{}
	if effectiveModel != nil {
		model.Name = requestedModel
		model.ContextWindow = effectiveModel.ContextWindow
		model.MaxTokens = effectiveModel.MaxTokens
	}
	if model.Name == "" {
		model.Name = os.Getenv(workerenv.AIModel)
	}

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

	toolRefs := make([]corev1alpha1.ToolReference, 0, len(a.Tools))
	for _, name := range a.Tools {
		toolRefs = append(toolRefs, corev1alpha1.ToolReference{Name: name})
	}

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
	agent.Spec.Soul = a.Soul
	if strings.TrimSpace(a.SystemPrompt) != "" {
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: a.SystemPrompt}
	}

	if a.Coordination != nil {
		coord := &corev1alpha1.CoordinationConfig{
			Enabled:               a.Coordination.Enabled,
			MaxDepth:              a.Coordination.MaxDepth,
			MaxConcurrentChildren: a.Coordination.MaxConcurrentChildren,
		}
		for i, aa := range a.Coordination.AllowedAgents {
			if strings.TrimSpace(aa.Name) == "" {
				return "", fmt.Errorf("coordination.allowedAgents[%d].name is required", i)
			}
			coord.AllowedAgents = append(coord.AllowedAgents, corev1alpha1.AllowedAgent{
				Name:      aa.Name,
				Namespace: aa.Namespace,
			})
		}
		agent.Spec.Coordination = coord
	}

	configureCreatedAgentRuntime(agent, a.Runtime)
	if err := executionmode.DefaultBuiltInAgentContract(agent, t.executionMode); err != nil {
		return "", err
	}
	if err := agentcontext.ValidateSoulRuntime(agent); err != nil {
		return "", err
	}
	if err := agentcontext.ValidateInlineCopilotInstructions(agent); err != nil {
		return "", err
	}

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

var _ Tool = (*CreateAgentTool)(nil)
