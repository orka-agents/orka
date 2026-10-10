package tools

import (
	"encoding/json"
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/agentcontext"
)

const (
	soulPatternField      = "pattern"
	soulMinLengthField    = "minLength"
	soulInlineField       = "inline"
	soulConfigMapRefField = "configMapRef"
	// Match Go TrimSpace (Unicode White_Space), not the regex engine-specific \s.
	soulNonWhitespacePattern = "[^\\t-\\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]"
)

func soulParameterSchema() map[string]any {
	return map[string]any{
		jsonSchemaTypeField: jsonSchemaTypeObject,
		jsonSchemaDescriptionField: fmt.Sprintf("Persistent Agent persona, separate from systemPrompt. "+
			"Supported only for AI worker Agents and built-in codex, claude, copilot, and opencode Agents using orka.harness.v2. "+
			"External runtimeRef and orka.harness.v1 Agents do not support souls. "+
			"Use inline Markdown or an Agent-namespace ConfigMap with its exact SHA-256 digest. "+
			"Soul content is limited to %d UTF-8 bytes (not characters). "+
			"Inline maxLength counts Unicode characters only; runtime validation enforces the byte limit. "+
			"Copilot instructions cannot contain @ characters because its native loader processes file imports.", agentcontext.MaxSoulBytes),
		jsonSchemaPropertiesField: map[string]any{
			soulInlineField:       map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, soulMinLengthField: 1, soulPatternField: soulNonWhitespacePattern, "maxLength": agentcontext.MaxSoulBytes},
			soulConfigMapRefField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{"name": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, soulMinLengthField: 1, soulPatternField: soulNonWhitespacePattern}, "key": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, soulMinLengthField: 1, soulPatternField: soulNonWhitespacePattern}}, jsonSchemaRequiredField: []string{"name", "key"}},
			"digest":              map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, soulPatternField: "^sha256:[0-9a-f]{64}$"},
		},
		"oneOf": []any{
			map[string]any{
				jsonSchemaRequiredField: []string{soulInlineField},
				"not":                   map[string]any{jsonSchemaRequiredField: []string{soulConfigMapRefField}},
			},
			map[string]any{
				jsonSchemaRequiredField: []string{soulConfigMapRefField, "digest"},
				"not":                   map[string]any{jsonSchemaRequiredField: []string{soulInlineField}},
			},
		},
	}
}

func marshalAgentSchema(schema map[string]any) json.RawMessage {
	schema[jsonSchemaPropertiesField].(map[string]any)["soul"] = soulParameterSchema()
	return mustMarshalSchema(schema)
}

func withSoulParameter(raw json.RawMessage) json.RawMessage {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		panic("invalid static Agent schema")
	}
	return marshalAgentSchema(schema)
}

func soulArgument(value any) (*corev1alpha1.SoulSource, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("soul must be an object containing inline or configMapRef and digest")
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("invalid soul object")
	}
	var source corev1alpha1.SoulSource
	if err := json.Unmarshal(encoded, &source); err != nil {
		return nil, fmt.Errorf("invalid soul source: %w", err)
	}
	if err := agentcontext.ValidateSource(&source); err != nil {
		return nil, err
	}
	return &source, nil
}

// validateInlineCopilotInstructions checks only known inline portions of the
// completed Agent. It does not resolve ConfigMaps; the controller must still
// validate the resolved configuration before dispatch.
func validateInlineCopilotInstructions(agent *corev1alpha1.Agent) error {
	if agent == nil || agent.Spec.Runtime == nil {
		return nil
	}
	runtime := agent.Spec.Runtime
	if runtime.Type != corev1alpha1.AgentRuntimeCopilot || runtime.RuntimeRef != nil ||
		runtime.ContractVersion == nil || *runtime.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return nil
	}
	if role := agent.Spec.SystemPrompt; role != nil {
		if err := acp.ValidateCopilotInstructions(role.Inline); err != nil {
			return fmt.Errorf("agent.spec.systemPrompt.inline: %w", err)
		}
	}
	if soul := agent.Spec.Soul; soul != nil {
		if err := acp.ValidateCopilotInstructions(soul.Inline); err != nil {
			return fmt.Errorf("agent.spec.soul.inline: %w", err)
		}
	}
	return nil
}
