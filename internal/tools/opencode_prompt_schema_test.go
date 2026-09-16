package tools

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/executionmode"
)

func TestAgentToolSchemasExposeRuntimeScopedOpenCodePromptLimits(t *testing.T) {
	maxCharacters := acp.MaxOpenCodeSystemPromptEncodedBytes - 2
	for _, tool := range []struct {
		name       string
		parameters func() json.RawMessage
		runtimeArg bool
	}{
		{"create_agent", NewCreateAgentTool(nil, executionmode.HarnessV2).Parameters, true},
		{"chat_create_agent", (&ChatCreateAgentTool{}).Parameters, true},
		{"update_agent", (&UpdateAgentTool{}).Parameters, false},
	} {
		t.Run(tool.name, func(t *testing.T) {
			var schema struct {
				Properties map[string]struct {
					Description string
					MaxLength   *int
				}
				AllOf []struct {
					If struct {
						Required   []string
						Properties map[string]struct {
							Required   []string
							Properties map[string]struct{ Const string }
						}
					}
					Then struct {
						Properties map[string]struct{ MaxLength int }
					}
				}
			}
			if err := json.Unmarshal(tool.parameters(), &schema); err != nil {
				t.Fatal(err)
			}
			prompt := schema.Properties[systemPromptField]
			for _, text := range []string{"OpenCode", strconv.Itoa(maxCharacters), strconv.Itoa(acp.MaxOpenCodeSystemPromptEncodedBytes), strconv.Itoa(maxCharacters / 6), "JSON-encoded bytes"} {
				if !strings.Contains(prompt.Description, text) {
					t.Errorf("schema does not advertise %q", text)
				}
			}
			if prompt.MaxLength != nil {
				t.Fatal("OpenCode's limit must not restrict other Agent runtimes")
			}
			found := false
			for _, condition := range schema.AllOf {
				runtime := condition.If.Properties["runtime"]
				if runtime.Properties["type"].Const != "opencode" {
					continue
				}
				found = true
				if len(condition.If.Required) != 1 || condition.If.Required[0] != "runtime" || len(runtime.Required) != 1 || runtime.Required[0] != "type" {
					t.Fatal("conditional bound would also apply when runtime/type is omitted")
				}
				if condition.Then.Properties[systemPromptField].MaxLength != maxCharacters {
					t.Fatal("conditional character limit differs from the encoded-byte budget")
				}
			}
			if found != tool.runtimeArg {
				t.Fatal("runtime condition does not match the public tool's input fields")
			}
		})
	}
	if err := acp.ValidateOpenCodeSystemPrompt(strings.Repeat("a", maxCharacters)); err != nil {
		t.Fatal("schema limit rejects the largest valid ASCII prompt")
	}
	if err := acp.ValidateOpenCodeSystemPrompt(strings.Repeat("<", maxCharacters)); err == nil {
		t.Fatal("character guidance must not replace exact escaped-byte validation")
	}
}

func TestOpenCodeConservativePromptGuidanceRetainsLargerValidInputs(t *testing.T) {
	conservativeCharacters := (acp.MaxOpenCodeSystemPromptEncodedBytes - 2) / 6
	if err := acp.ValidateOpenCodeSystemPrompt(strings.Repeat("<", conservativeCharacters)); err != nil {
		t.Fatal("conservative guidance must fit even with six-byte escapes")
	}
	if err := acp.ValidateOpenCodeSystemPrompt(strings.Repeat("<", conservativeCharacters+1)); err == nil {
		t.Fatal("exact encoded-byte validation must still reject the next escaped character")
	}
	if err := acp.ValidateOpenCodeSystemPrompt(strings.Repeat("a", conservativeCharacters+1)); err != nil {
		t.Fatal("conservative guidance must not become a new character restriction")
	}
}
