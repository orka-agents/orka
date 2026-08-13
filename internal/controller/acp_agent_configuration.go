package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type permanentACPAgentConfigurationError struct{ err error }

func (e *permanentACPAgentConfigurationError) Error() string { return e.err.Error() }
func (e *permanentACPAgentConfigurationError) Unwrap() error { return e.err }

func permanentACPAgentConfiguration(err error) error {
	if err == nil {
		return nil
	}
	return &permanentACPAgentConfigurationError{err: err}
}

func isPermanentACPAgentConfigurationError(err error) bool {
	var permanent *permanentACPAgentConfigurationError
	return errors.As(err, &permanent)
}

func resolveACPAgentSessionConfiguration(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (harnessv2.AgentSessionConfiguration, error) {
	if err := validateACPAgentSkills(agent); err != nil {
		return harnessv2.AgentSessionConfiguration{}, permanentACPAgentConfiguration(err)
	}
	if err := validateACPAgentTools(agent); err != nil {
		return harnessv2.AgentSessionConfiguration{}, permanentACPAgentConfiguration(err)
	}
	if reader == nil {
		return harnessv2.AgentSessionConfiguration{}, fmt.Errorf("API reader is required to resolve ACP Agent configuration")
	}
	systemPrompt, err := resolveACPSystemPrompt(ctx, reader, agent)
	if err != nil {
		return harnessv2.AgentSessionConfiguration{}, err
	}
	return buildACPAgentSessionConfiguration(task, agent, systemPrompt)
}

func buildACPAgentSessionConfiguration(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	systemPrompt string,
) (harnessv2.AgentSessionConfiguration, error) {
	if task == nil || agent == nil || agent.Spec.Runtime == nil {
		return harnessv2.AgentSessionConfiguration{}, fmt.Errorf("task and built-in runtime Agent are required")
	}
	if err := validateACPAgentSkills(agent); err != nil {
		return harnessv2.AgentSessionConfiguration{}, permanentACPAgentConfiguration(err)
	}
	if err := validateACPAgentTools(agent); err != nil {
		return harnessv2.AgentSessionConfiguration{}, permanentACPAgentConfiguration(err)
	}
	if err := validateACPAgentModelControls(agent.Spec.Runtime, agent.Spec.Model); err != nil {
		return harnessv2.AgentSessionConfiguration{}, permanentACPAgentConfiguration(err)
	}
	model := ""
	if agent.Spec.Model != nil {
		model = strings.TrimSpace(agent.Spec.Model.Name)
	}
	configuration := harnessv2.AgentSessionConfiguration{
		AgentUID:        string(agent.UID),
		AgentGeneration: agent.Generation,
		ProviderKind:    string(agent.Spec.Runtime.Type),
		Model:           model,
		MaxTurns:        effectiveACPMaxTurns(task, agent),
		ReasoningEffort: effectiveACPReasoningEffort(agent),
		SystemPrompt:    systemPrompt,
	}
	if err := configuration.Validate(); err != nil {
		return harnessv2.AgentSessionConfiguration{}, permanentACPAgentConfiguration(fmt.Errorf("ACP Agent session configuration: %w", err))
	}
	return configuration, nil
}

func validateACPAgentSkills(agent *corev1alpha1.Agent) error {
	if agent == nil || len(agent.Spec.Skills) == 0 {
		return nil
	}
	return errors.New("built-in ACP runtimes do not support Agent.spec.skills; refusing to omit declared skills")
}

func validateACPAgentTools(agent *corev1alpha1.Agent) error {
	if agent == nil {
		return nil
	}
	for _, toolRef := range agent.Spec.Tools {
		if toolRef.Enabled != nil && !*toolRef.Enabled {
			continue
		}
		// Agent.spec.tools is additive for the managed AI worker, while a non-empty
		// ACP allowlist replaces the provider-native default set. Merging enabled
		// references would therefore silently remove native tools, with no defined
		// precedence against runtime defaults or Task overrides. Explicitly disabled
		// references are inert and safe to ignore.
		return errors.New("built-in ACP runtimes do not support enabled Agent.spec.tools; refusing to omit declared tools")
	}
	return nil
}

func resolveACPSystemPrompt(ctx context.Context, reader client.Reader, agent *corev1alpha1.Agent) (string, error) {
	if agent == nil || agent.Spec.SystemPrompt == nil {
		return "", nil
	}
	source := agent.Spec.SystemPrompt
	inline := source.Inline
	if inline != "" && source.ConfigMapRef != nil {
		return "", permanentACPAgentConfiguration(fmt.Errorf("agent systemPrompt must use exactly one of inline or configMapRef"))
	}
	if source.ConfigMapRef == nil {
		return inline, nil
	}
	name := strings.TrimSpace(source.ConfigMapRef.Name)
	key := strings.TrimSpace(source.ConfigMapRef.Key)
	if name == "" || key == "" {
		return "", permanentACPAgentConfiguration(fmt.Errorf("agent systemPrompt ConfigMap name and key are required"))
	}
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, configMap); err != nil {
		wrapped := fmt.Errorf("resolve Agent systemPrompt ConfigMap %q: %w", name, err)
		if apierrors.IsNotFound(err) {
			return "", permanentACPAgentConfiguration(wrapped)
		}
		return "", wrapped
	}
	value, ok := configMap.Data[key]
	if !ok {
		return "", permanentACPAgentConfiguration(fmt.Errorf("agent systemPrompt ConfigMap %q does not contain key %q", name, key))
	}
	return value, nil
}

const legacyDefaultACPTemperature = 0.7

func validateACPAgentModelControls(runtimeCfg *corev1alpha1.AgentCLIRuntime, model *corev1alpha1.ModelConfig) error {
	if runtimeCfg == nil || runtimeCfg.RuntimeRef != nil || model == nil {
		return nil
	}
	switch runtimeCfg.Type {
	case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode:
	default:
		return nil
	}
	if model.Temperature != nil && *model.Temperature != legacyDefaultACPTemperature {
		return fmt.Errorf("built-in ACP runtime %q does not support spec.model.temperature values other than the legacy default %.1f", runtimeCfg.Type, legacyDefaultACPTemperature)
	}
	if runtimeCfg.Type != corev1alpha1.AgentRuntimeOpencode {
		if model.ContextWindow != nil {
			return fmt.Errorf("built-in ACP runtime %q does not support spec.model.contextWindow", runtimeCfg.Type)
		}
		if model.MaxTokens != nil {
			return fmt.Errorf("built-in ACP runtime %q does not support spec.model.maxTokens", runtimeCfg.Type)
		}
	}
	if len(model.Fallbacks) > 0 {
		return fmt.Errorf("built-in ACP runtime %q does not support spec.model.fallbacks", runtimeCfg.Type)
	}
	return nil
}

func effectiveACPAllowBash(task *corev1alpha1.Task, agent *corev1alpha1.Agent) bool {
	allowBash := true
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultAllowBash != nil {
		allowBash = *agent.Spec.Runtime.DefaultAllowBash
	}
	if task != nil && task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowBash != nil {
		allowBash = *task.Spec.AgentRuntime.AllowBash
	}
	return allowBash
}

func effectiveACPReasoningEffort(agent *corev1alpha1.Agent) string {
	if agent == nil || agent.Spec.Runtime == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(agent.Spec.Runtime.DefaultReasoningEffort))
}

func validateACPProviderNativePolicy(provider string, intent corev1alpha1.WorkspaceIntent, allowed, disallowed []string, allowBash bool) error {
	unrestricted := allowed == nil && len(disallowed) == 0 && allowBash
	switch provider {
	case string(corev1alpha1.AgentRuntimeClaude):
		return nil
	case string(corev1alpha1.AgentRuntimeCodex):
		if unrestricted {
			return nil
		}
		if intent == corev1alpha1.WorkspaceIntentRead && codexReadOnlyNativePolicy(allowed) {
			return nil
		}
		return fmt.Errorf("codex ACP runtime cannot exactly enforce provider-native tool restrictions")
	case string(corev1alpha1.AgentRuntimeCopilot):
		for _, name := range allowed {
			if strings.EqualFold(strings.TrimSpace(name), "WebSearch") {
				return fmt.Errorf("copilot ACP runtime cannot exactly enforce the WebSearch provider-native tool")
			}
		}
		return nil
	case string(corev1alpha1.AgentRuntimeOpencode):
		return nil
	default:
		return fmt.Errorf("unsupported ACP provider %q", provider)
	}
}

// codexReadOnlyNativePolicy reports whether the provider-native slice of the
// allowed tools is exactly the {Glob, Grep, Read} read-only surface. That is
// the single restricted policy codex sessions support: instead of per-tool
// switches, the RuntimeSession boundary rejects every elevation request,
// mediates file writes through the supervisor, and fails any read-intent turn
// whose workspace delta shows a modification. Brokered and custom tool names
// are ignored here because the MCP broker enforces them, not codex.
func codexReadOnlyNativePolicy(allowed []string) bool {
	native := providerNativeTools[strings.ToLower(string(corev1alpha1.AgentRuntimeCodex))]
	surface := make(map[string]struct{}, 3)
	for _, name := range allowed {
		canonical, ok := native[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			continue
		}
		switch canonical {
		case providerNativeToolGlob, providerNativeToolGrep, providerNativeToolRead:
			surface[canonical] = struct{}{}
		default:
			return false
		}
	}
	return len(surface) == 3
}

func validateACPProviderSystemPrompt(provider string, configuration harnessv2.AgentSessionConfiguration) error {
	if configuration.SystemPrompt == "" {
		return nil
	}
	switch provider {
	case string(corev1alpha1.AgentRuntimeCodex):
		const maxCodexConfigEnvironmentBytes = 96 << 10
		encoded, err := json.Marshal(map[string]any{
			"model": configuration.Model, "openai_base_url": strings.Repeat("x", 2048),
			"check_for_update_on_startup": false, "developer_instructions": configuration.SystemPrompt,
			"model_reasoning_effort": configuration.ReasoningEffort,
		})
		if err != nil {
			return err
		}
		if len(encoded) > maxCodexConfigEnvironmentBytes {
			return fmt.Errorf("codex session configuration exceeds the safe environment limit")
		}
	case string(corev1alpha1.AgentRuntimeCopilot):
		return fmt.Errorf("copilot ACP runtime cannot enforce Agent systemPrompt")
	case string(corev1alpha1.AgentRuntimeOpencode):
		return fmt.Errorf("opencode ACP runtime cannot enforce Agent systemPrompt")
	}
	return nil
}
