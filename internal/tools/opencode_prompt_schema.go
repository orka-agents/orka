package tools

import (
	"encoding/json"
	"fmt"

	"github.com/orka-agents/orka/internal/acp"
)

// Keep the model-visible bound specific to OpenCode. Other Agent runtimes do
// not share this encoded-byte limit, and update_agent selects its runtime from
// the stored Agent rather than an input field.
func withOpenCodePromptLimits(parameters json.RawMessage) json.RawMessage {
	var schema map[string]any
	if err := json.Unmarshal(parameters, &schema); err != nil {
		panic(fmt.Sprintf("invalid static Agent tool schema: %v", err))
	}
	properties := schema[jsonSchemaPropertiesField].(map[string]any)
	prompt := properties[systemPromptField].(map[string]any)
	// Every character occupies at least one encoded byte, plus the two JSON
	// quotes. Escaping/non-ASCII text can need more; Execute enforces the exact
	// encoded-byte limit rather than treating maxLength as a sufficient check.
	maxCharacters := acp.MaxOpenCodeSystemPromptEncodedBytes - 2
	prompt[jsonSchemaDescriptionField] = fmt.Sprintf(
		"%s OpenCode permits at most %d characters and %d JSON-encoded bytes, including quotes and escapes; the exact encoded-byte limit is validated at execution. Keeping to %d characters fits that byte budget even with six-byte JSON escapes; longer prompts are allowed when their encoding fits.",
		prompt[jsonSchemaDescriptionField], maxCharacters, acp.MaxOpenCodeSystemPromptEncodedBytes, maxCharacters/6,
	)
	if _, hasRuntime := properties["runtime"]; hasRuntime {
		conditions, _ := schema["allOf"].([]any)
		schema["allOf"] = append(conditions, map[string]any{
			"if": map[string]any{
				"required": []string{"runtime"},
				jsonSchemaPropertiesField: map[string]any{
					"runtime": map[string]any{
						"required":                []string{"type"},
						jsonSchemaPropertiesField: map[string]any{"type": map[string]any{"const": "opencode"}},
					},
				},
			},
			"then": map[string]any{
				jsonSchemaPropertiesField: map[string]any{systemPromptField: map[string]any{"maxLength": maxCharacters}},
			},
		})
	} else {
		prompt[jsonSchemaDescriptionField] = fmt.Sprintf("%s This OpenCode-only limit is determined by the existing Agent's runtime.", prompt[jsonSchemaDescriptionField])
	}
	return mustMarshalSchema(schema)
}
