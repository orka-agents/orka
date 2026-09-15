package tools

import (
	"encoding/json"
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
)

const soulMinLengthField = "minLength"

func soulParameterSchema() map[string]any {
	return map[string]any{
		jsonSchemaTypeField: jsonSchemaTypeObject,
		jsonSchemaDescriptionField: "Persistent Agent persona, separate from systemPrompt. " +
			"Supported only for AI worker Agents and built-in codex, claude, copilot, and opencode Agents using orka.harness.v2. " +
			"External runtimeRef and orka.harness.v1 Agents do not support souls. " +
			"Use inline Markdown or an Agent-namespace ConfigMap with its exact SHA-256 digest. " +
			"Copilot instructions cannot contain @ characters because its native loader processes file imports.",
		jsonSchemaPropertiesField: map[string]any{
			"inline":       map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, soulMinLengthField: 1, "maxLength": agentcontext.MaxSoulBytes},
			"configMapRef": map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{"name": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, soulMinLengthField: 1}, "key": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, soulMinLengthField: 1}}, jsonSchemaRequiredField: []string{"name", "key"}},
			"digest":       map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, "pattern": "^sha256:[0-9a-f]{64}$"},
		},
		"oneOf": []any{
			map[string]any{
				jsonSchemaRequiredField: []string{"inline"},
				"not":                   map[string]any{jsonSchemaRequiredField: []string{"configMapRef"}},
			},
			map[string]any{
				jsonSchemaRequiredField: []string{"configMapRef", "digest"},
				"not":                   map[string]any{jsonSchemaRequiredField: []string{"inline"}},
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
