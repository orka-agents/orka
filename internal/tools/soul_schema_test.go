package tools

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/executionmode"
)

func TestSoulSchemasEnforceSourceExclusivity(t *testing.T) {
	ref := map[string]any{"name": "persona", "key": "SOUL.md"}
	digest := agentcontext.Digest("persona")
	for _, tool := range []struct {
		name string
		tool Tool
	}{
		{"native create", NewCreateAgentTool(nil, executionmode.HarnessV2)},
		{"chat create", &ChatCreateAgentTool{}},
		{"update", &UpdateAgentTool{}},
	} {
		t.Run(tool.name, func(t *testing.T) {
			var schema jsonschema.Schema
			if err := json.Unmarshal(tool.tool.Parameters(), &schema); err != nil {
				t.Fatal(err)
			}
			source := schema.Properties["soul"]
			if source == nil {
				t.Fatal("tool does not expose the soul schema")
			}
			resolved, err := source.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name  string
				value map[string]any
				valid bool
			}{
				{"inline", map[string]any{"inline": "persona"}, true},
				{"inline with digest", map[string]any{"inline": "persona", "digest": digest}, true},
				{"pinned configmap", map[string]any{"configMapRef": ref, "digest": digest}, true},
				{"empty", map[string]any{}, false},
				{"empty inline", map[string]any{"inline": ""}, false},
				{"digest without source", map[string]any{"digest": digest}, false},
				{"unpinned configmap", map[string]any{"configMapRef": ref}, false},
				{"both sources without digest", map[string]any{"inline": "persona", "configMapRef": ref}, false},
				{"both sources with digest", map[string]any{"inline": "persona", "configMapRef": ref, "digest": digest}, false},
				{"configmap with empty inline", map[string]any{"inline": "", "configMapRef": ref, "digest": digest}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := resolved.Validate(tc.value); (err == nil) != tc.valid {
						t.Fatalf("schema valid=%v, error=%v", tc.valid, err)
					}
				})
			}
		})
	}
}

func TestSoulRuntimeValidationRejectsMixedSources(t *testing.T) {
	for _, withDigest := range []bool{false, true} {
		value := map[string]any{
			"inline":       "persona",
			"configMapRef": map[string]any{"name": "persona", "key": "SOUL.md"},
		}
		if withDigest {
			value["digest"] = agentcontext.Digest("persona")
		}
		if _, err := soulArgument(value); err == nil {
			t.Fatalf("runtime accepted mixed sources: withDigest=%v", withDigest)
		}
	}
}
