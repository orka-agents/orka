package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"

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

func TestSoulSchemasEnforceNonWhitespace(t *testing.T) {
	values := []string{"", " \t\n\v\f\r", "persona", "\tpersona\u3000", "\u200b", "\ufeff", "\u180e", "\x1c"}
	// Enumerate Go's whitespace set rather than duplicating the schema's class.
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) {
			values = append(values, string(r))
		}
	}
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
			for _, value := range values {
				t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
					for _, field := range []struct {
						name  string
						value map[string]any
					}{
						{"inline", map[string]any{"inline": value}},
						{"name", map[string]any{"configMapRef": map[string]any{"name": value, "key": "SOUL.md"}, "digest": agentcontext.Digest("persona")}},
						{"key", map[string]any{"configMapRef": map[string]any{"name": "persona", "key": value}, "digest": agentcontext.Digest("persona")}},
					} {
						t.Run(field.name, func(t *testing.T) {
							wantValid := strings.TrimSpace(value) != ""
							if err := resolved.Validate(field.value); (err == nil) != wantValid {
								t.Errorf("schema valid=%v, error=%v", wantValid, err)
							}
							if _, err := soulArgument(field.value); (err == nil) != wantValid {
								t.Errorf("runtime valid=%v, error=%v", wantValid, err)
							}
						})
					}
				})
			}
		})
	}
}

func TestSoulSchemasUTF8ByteLimit(t *testing.T) {
	cases := []struct {
		name         string
		text         string
		schemaValid  bool
		runtimeValid bool
	}{
		{"ASCII at 8192 bytes", strings.Repeat("a", 8192), true, true},
		{"ASCII at 8193 bytes", strings.Repeat("a", 8193), false, false},
		{"two-byte text at 8192 bytes", strings.Repeat("é", 4096), true, true},
		{"two-byte text at 8193 bytes", strings.Repeat("é", 4096) + "a", true, false},
		{"CJK at 8191 bytes", strings.Repeat("界", 2730) + "a", true, true},
		{"CJK at 8192 bytes", strings.Repeat("界", 2730) + "ab", true, true},
		{"CJK at 8193 bytes", strings.Repeat("界", 2731), true, false},
		{"3000 CJK characters", strings.Repeat("界", 3000), true, false},
		{"four-byte text at 8192 bytes", strings.Repeat("🙂", 2048), true, true},
		{"four-byte text at 8193 bytes", strings.Repeat("🙂", 2048) + "a", true, false},
	}
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
			for _, guidance := range []string{"8192 UTF-8 bytes", "not characters", "maxLength", "Unicode characters", "runtime validation"} {
				if !strings.Contains(source.Description, guidance) {
					t.Errorf("soul schema does not explain %q", guidance)
				}
			}
			inline := source.Properties["inline"]
			if inline == nil || inline.MaxLength == nil || *inline.MaxLength != 8192 {
				t.Fatal("inline maxLength must retain the full 8192-character ASCII capacity")
			}
			resolved, err := source.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					value := map[string]any{"inline": tc.text}
					// Standard maxLength counts Unicode characters, so multibyte
					// overflow can pass the schema while runtime validation rejects it.
					if err := resolved.Validate(value); (err == nil) != tc.schemaValid {
						t.Errorf("schema valid=%v, error=%v", tc.schemaValid, err)
					}
					parsed, err := soulArgument(value)
					if !tc.runtimeValid {
						if err == nil || !strings.Contains(err.Error(), "8192-byte limit") {
							t.Errorf("expected runtime byte-limit rejection, got %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if parsed.Inline != tc.text {
						t.Fatal("runtime validation changed the valid inline content")
					}
				})
			}
		})
	}
}
