/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"maps"
	"os"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// TestTaskMemoryConsolidationAdmission runs the generated Task spec schema,
// with its CEL rules, against memory consolidation Tasks as the API server
// would, including updates.
func TestTaskMemoryConsolidationAdmission(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/core.orka.ai_tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	external := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	var schema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&external, &schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	openAPI, _, err := schemavalidation.NewSchemaValidator(&schema)
	if err != nil {
		t.Fatal(err)
	}
	admit := func(spec, old map[string]any) []string {
		schemaErrs := schemavalidation.ValidateCustomResource(nil, spec, openAPI)
		messages := make([]string, 0, len(schemaErrs))
		for _, err := range schemaErrs {
			messages = append(messages, err.Error())
		}
		var oldObject any
		if old != nil {
			oldObject = old
		}
		errs, _ := validator.Validate(t.Context(), nil, structural, spec, oldObject, celconfig.RuntimeCELCostBudget)
		for _, err := range errs {
			messages = append(messages, err.Error())
		}
		return messages
	}
	task := func(taskType string, consolidation map[string]any, extra map[string]any) map[string]any {
		ai := map[string]any{"prompt": "Consolidate memory"}
		if consolidation != nil {
			ai["memoryConsolidation"] = consolidation
		}
		spec := map[string]any{"type": taskType, "ai": ai}
		maps.Copy(spec, extra)
		return spec
	}
	run := map[string]any{"mode": "Propose", "minNewSessions": int64(3), "maxOperations": int64(5), "lookback": "48h"}

	requireMessages := func(name string, got []string, want string) {
		t.Helper()
		if want == "" {
			if len(got) != 0 {
				t.Fatalf("%s: rejected: %v", name, got)
			}
			return
		}
		for _, message := range got {
			if strings.Contains(message, want) {
				return
			}
		}
		t.Fatalf("%s: want a rejection containing %q, got %v", name, want, got)
	}
	requireMessages("an ai run", admit(task("ai", run, nil), nil), "")
	requireMessages("an empty run takes defaults", admit(task("ai", map[string]any{}, nil), nil), "")
	requireMessages("an agent Task", admit(task("agent", run, nil), nil), "ai.memoryConsolidation requires an ai Task without sessionRef")
	requireMessages("a run with a Session", admit(task("ai", run, map[string]any{"sessionRef": map[string]any{"name": "s"}}), nil),
		"ai.memoryConsolidation requires an ai Task without sessionRef")
	requireMessages("an unknown mode", admit(task("ai", map[string]any{"mode": "Apply"}, nil), nil), "Unsupported value")
	requireMessages("too many operations", admit(task("ai", map[string]any{"maxOperations": int64(21)}, nil), nil), "maxOperations")
	requireMessages("a long lookback", admit(task("ai", map[string]any{"lookback": "800h"}, nil), nil), "lookback must be between 1h and 720h")

	requireMessages("an unchanged run", admit(task("ai", run, nil), task("ai", run, nil)), "")
	requireMessages("adding the field", admit(task("ai", run, nil), task("ai", nil, nil)), "ai.memoryConsolidation is immutable")
	requireMessages("removing the field", admit(task("ai", nil, nil), task("ai", run, nil)), "ai.memoryConsolidation is immutable")
	changed := map[string]any{"mode": "DryRun"}
	requireMessages("changing the mode", admit(task("ai", changed, nil), task("ai", run, nil)), "ai.memoryConsolidation is immutable")
	requireMessages("an ordinary Task update", admit(task("ai", nil, nil), task("ai", nil, nil)), "")
}
