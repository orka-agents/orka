/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const legacyDefaultOpenCodeTemperature = 0.7

// openCodeModelSpecError is a fail-closed OpenCode model validation failure.
// message is the caller-visible error text; hint is remediation guidance for
// chat-tool surfaces.
type openCodeModelSpecError struct {
	message string
	hint    string
}

func (e *openCodeModelSpecError) Error() string { return e.message }

// validateOpenCodeModelSpec applies the fail-closed OpenCode model rules
// shared by the create-agent and chat-create-agent surfaces: substitution
// braces are rejected, the model must resolve to literal provider/model form,
// an explicit provider hint must match the provider embedded in model.name,
// temperature may only be the legacy default, fallbacks are unsupported, and
// contextWindow/maxTokens must be positive with contextWindow exceeding
// maxTokens. On success it returns the resolved providerID and modelID.
func validateOpenCodeModelSpec(model *corev1alpha1.ModelConfig) (string, string, *openCodeModelSpecError) {
	if model == nil || strings.TrimSpace(model.Name) == "" {
		return "", "", &openCodeModelSpecError{
			message: "model.name is required for opencode runtime",
			hint:    "Set model.name to a literal provider/model ID such as openai/gpt-5.4.",
		}
	}
	requested := strings.TrimSpace(model.Name)
	providerHint := strings.Trim(strings.TrimSpace(model.Provider), "/")
	if strings.ContainsAny(requested, "{}") || strings.ContainsAny(providerHint, "{}") {
		return "", "", &openCodeModelSpecError{
			message: "model.name for opencode runtime must not contain substitution braces",
			hint:    "Use the literal provider/model ID without OpenCode config substitutions.",
		}
	}
	providerID, modelID, qualified := strings.Cut(requested, "/")
	if qualified {
		providerID = strings.TrimSpace(providerID)
		modelID = strings.TrimSpace(modelID)
		if providerHint != "" && providerHint != providerID {
			return "", "", &openCodeModelSpecError{
				message: fmt.Sprintf("model.provider %q does not match provider %q in model.name for opencode runtime", providerHint, providerID),
				hint:    "Use one provider consistently, or omit model.provider when model.name already uses provider/model form.",
			}
		}
	} else {
		providerID = providerHint
		modelID = requested
	}
	if providerID == "" || modelID == "" {
		return "", "", &openCodeModelSpecError{
			message: "model.name for opencode runtime must use provider/model form",
			hint:    "Set model.name to a literal provider/model ID such as openai/gpt-5.4.",
		}
	}
	if model.Temperature != nil && *model.Temperature != legacyDefaultOpenCodeTemperature {
		return "", "", &openCodeModelSpecError{
			message: fmt.Sprintf("opencode runtime does not support model.temperature values other than the legacy default %.1f", legacyDefaultOpenCodeTemperature),
			hint:    "Omit model.temperature for OpenCode or use the legacy default 0.7.",
		}
	}
	if len(model.Fallbacks) > 0 {
		return "", "", &openCodeModelSpecError{
			message: "opencode runtime does not support model.fallbacks",
			hint:    "Remove model.fallbacks from the OpenCode Agent.",
		}
	}
	if model.ContextWindow == nil || *model.ContextWindow <= 0 {
		return "", "", &openCodeModelSpecError{
			message: "model.contextWindow is required for opencode runtime and must be positive",
			hint:    "Set model.contextWindow to the reviewed context capacity for the selected model.",
		}
	}
	if model.MaxTokens == nil || *model.MaxTokens <= 0 {
		return "", "", &openCodeModelSpecError{
			message: "model.maxTokens is required for opencode runtime and must be positive",
			hint:    "Set model.maxTokens to the reviewed output limit for the selected model.",
		}
	}
	if *model.ContextWindow <= *model.MaxTokens {
		return "", "", &openCodeModelSpecError{
			message: "model.contextWindow must exceed model.maxTokens for opencode runtime",
			hint:    "Use the reviewed context and output capacities for the selected model.",
		}
	}
	return providerID, modelID, nil
}
