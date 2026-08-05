/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"fmt"
	"math"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type agentModelUpdate struct {
	provider       string
	providerSet    bool
	name           string
	nameSet        bool
	legacyString   bool
	temperature    float64
	temperatureSet bool
	contextWindow  int32
	contextSet     bool
	maxTokens      int32
	maxTokensSet   bool
}

func parseAgentModelUpdate(value any) (agentModelUpdate, error) {
	var update agentModelUpdate
	switch model := value.(type) {
	case map[string]any:
		if err := parseAgentModelStringField(model, "provider", &update.provider, &update.providerSet); err != nil {
			return agentModelUpdate{}, err
		}
		if err := parseAgentModelStringField(model, nameField, &update.name, &update.nameSet); err != nil {
			return agentModelUpdate{}, err
		}
		if raw, ok := model["temperature"]; ok {
			temperature, ok := raw.(float64)
			if !ok || math.IsNaN(temperature) || math.IsInf(temperature, 0) {
				return agentModelUpdate{}, fmt.Errorf("model.temperature must be a number")
			}
			if temperature < 0 || temperature > 2 {
				return agentModelUpdate{}, fmt.Errorf("model.temperature must be between 0 and 2")
			}
			update.temperature = temperature
			update.temperatureSet = true
		}
		if raw, ok := model["contextWindow"]; ok {
			contextWindow, err := parsePositiveAgentModelInt32("model.contextWindow", raw)
			if err != nil {
				return agentModelUpdate{}, err
			}
			update.contextWindow = contextWindow
			update.contextSet = true
		}
		if raw, ok := model["maxTokens"]; ok {
			maxTokens, err := parsePositiveAgentModelInt32("model.maxTokens", raw)
			if err != nil {
				return agentModelUpdate{}, err
			}
			update.maxTokens = maxTokens
			update.maxTokensSet = true
		}
		return update, nil
	case string:
		legacy := strings.TrimSpace(model)
		if legacy == "" {
			return update, nil
		}
		update.name = legacy
		update.nameSet = true
		update.legacyString = true
		return update, nil
	default:
		return agentModelUpdate{}, fmt.Errorf("model must be an object")
	}
}

func parseAgentModelStringField(model map[string]any, field string, destination *string, supplied *bool) error {
	raw, ok := model[field]
	if !ok {
		return nil
	}
	value, ok := raw.(string)
	if !ok {
		return fmt.Errorf("model.%s must be a string", field)
	}
	*destination = strings.TrimSpace(value)
	*supplied = true
	return nil
}

func parsePositiveAgentModelInt32(field string, raw any) (int32, error) {
	value, ok := raw.(float64)
	const maxInt32 = float64(1<<31 - 1)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return 0, fmt.Errorf("%s must be an integer", field)
	}
	if value < 1 || value > maxInt32 {
		return 0, fmt.Errorf("%s must be a positive 32-bit integer", field)
	}
	return int32(value), nil
}

func validateBuiltInRuntimeModelLimits(runtimeType corev1alpha1.AgentRuntimeType, model *corev1alpha1.ModelConfig) error {
	if !isBuiltInACPRuntime(runtimeType) || runtimeType == corev1alpha1.AgentRuntimeOpencode || model == nil {
		return nil
	}
	if model.ContextWindow != nil {
		return fmt.Errorf("built-in ACP runtime %q does not support model.contextWindow", runtimeType)
	}
	if model.MaxTokens != nil {
		return fmt.Errorf("built-in ACP runtime %q does not support model.maxTokens", runtimeType)
	}
	return nil
}

func validateAgentModelUpdateForRuntime(agent *corev1alpha1.Agent, update agentModelUpdate) error {
	if agent == nil || agent.Spec.Runtime == nil || !isBuiltInACPRuntime(agent.Spec.Runtime.Type) ||
		agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeOpencode {
		return nil
	}
	if update.contextSet {
		return fmt.Errorf("built-in ACP runtime %q does not support model.contextWindow", agent.Spec.Runtime.Type)
	}
	if update.maxTokensSet {
		return fmt.Errorf("built-in ACP runtime %q does not support model.maxTokens", agent.Spec.Runtime.Type)
	}
	return nil
}

func applyAgentModelUpdate(agent *corev1alpha1.Agent, update agentModelUpdate) error {
	if !update.hasChanges() {
		return nil
	}
	if err := validateAgentModelUpdateForRuntime(agent, update); err != nil {
		return err
	}
	if agent.Spec.Model == nil {
		agent.Spec.Model = &corev1alpha1.ModelConfig{}
	}
	if isOpenCodeAgent(agent) {
		return applyOpenCodeModelUpdate(agent.Spec.Model, update)
	}
	applyCommonAgentModelUpdate(agent.Spec.Model, update)
	return nil
}

func (u agentModelUpdate) hasChanges() bool {
	return u.providerSet || u.nameSet || u.temperatureSet || u.contextSet || u.maxTokensSet
}

func applyCommonAgentModelUpdate(model *corev1alpha1.ModelConfig, update agentModelUpdate) {
	if update.legacyString {
		provider, modelName := splitModelString(update.name)
		if provider != "" {
			model.Provider = strings.TrimSpace(provider)
		}
		model.Name = strings.TrimSpace(modelName)
		applyAgentModelControlUpdate(model, update)
		return
	}
	if update.providerSet {
		model.Provider = update.provider
	}
	if update.nameSet {
		model.Name = update.name
	}
	applyAgentModelControlUpdate(model, update)
}

func applyOpenCodeModelUpdate(model *corev1alpha1.ModelConfig, update agentModelUpdate) error {
	provider, modelName := currentOpenCodeModelIdentity(model)
	nameWasQualified := false
	if update.nameSet {
		requestedName := strings.TrimSpace(update.name)
		if requestedProvider, requestedModel, qualified := strings.Cut(requestedName, "/"); qualified {
			provider = strings.TrimSpace(requestedProvider)
			modelName = strings.TrimSpace(requestedModel)
			nameWasQualified = true
		} else {
			modelName = requestedName
		}
	}
	if update.providerSet {
		requestedProvider := strings.TrimSpace(update.provider)
		if requestedProvider != "" && nameWasQualified && provider != "" && requestedProvider != provider {
			return fmt.Errorf("model.provider %q does not match provider %q in model.name", requestedProvider, provider)
		}
		if requestedProvider != "" || !nameWasQualified {
			provider = requestedProvider
		}
	}

	model.Name = modelName
	model.Provider = provider
	if nameWasQualified {
		// Preserve every qualified OpenCode ID through final validation so nested
		// model paths are not reinterpreted as a conflicting provider hint.
		model.Provider = ""
		if provider != "" {
			model.Name = provider + "/" + modelName
		}
		if update.providerSet {
			model.Provider = strings.TrimSpace(update.provider)
		}
	}
	applyAgentModelControlUpdate(model, update)
	return nil
}

func currentOpenCodeModelIdentity(model *corev1alpha1.ModelConfig) (string, string) {
	if model == nil {
		return "", ""
	}
	requested := strings.TrimSpace(model.Name)
	if provider, modelName, qualified := strings.Cut(requested, "/"); qualified {
		return strings.TrimSpace(provider), strings.TrimSpace(modelName)
	}
	return strings.TrimSpace(model.Provider), requested
}

func applyAgentModelControlUpdate(model *corev1alpha1.ModelConfig, update agentModelUpdate) {
	if update.temperatureSet {
		temperature := update.temperature
		model.Temperature = &temperature
	}
	if update.contextSet {
		contextWindow := update.contextWindow
		model.ContextWindow = &contextWindow
	}
	if update.maxTokensSet {
		maxTokens := update.maxTokens
		model.MaxTokens = &maxTokens
	}
}

func isOpenCodeAgent(agent *corev1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeOpencode
}
