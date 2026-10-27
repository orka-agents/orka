package v1alpha1

import (
	"testing"

	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

func TestTaskEnforcementAdmissionPreservesOrderedRequirements(t *testing.T) {
	const definition = "core.orka.ai_tasks.yaml"
	validator, structural := loadSpecCELValidator(t, definition)
	makeSpec := func() map[string]any {
		return map[string]any{
			"type": "ai",
			"ai":   map[string]any{"provider": "openai", "model": "test", "prompt": "hello"},
			"enforcement": map[string]any{
				"format": "test/v1", "policy": "cG9saWN5", "mode": "enforce",
				"requiredCapabilities": []any{"filesystem", "network"},
			},
		}
	}
	requireSpecAtRoot(t, definition, makeSpec())
	for _, test := range []struct {
		name        string
		create      bool
		unprotected bool
		mutate      func(map[string]any)
		reject      bool
	}{
		{name: "create", create: true},
		{name: "unchanged"},
		{name: "suspend", mutate: func(spec map[string]any) { spec["suspend"] = true }},
		{name: "reorder", reject: true, mutate: func(spec map[string]any) {
			spec["enforcement"].(map[string]any)["requiredCapabilities"] = []any{"network", "filesystem"}
		}},
		{name: "change format", reject: true, mutate: func(spec map[string]any) {
			spec["enforcement"].(map[string]any)["format"] = "other/v1"
		}},
		{name: "change policy", reject: true, mutate: func(spec map[string]any) {
			spec["enforcement"].(map[string]any)["policy"] = "b3RoZXI="
		}},
		{name: "add capability", reject: true, mutate: func(spec map[string]any) {
			spec["enforcement"].(map[string]any)["requiredCapabilities"] = []any{"filesystem", "network", "process"}
		}},
		{name: "remove capability", reject: true, mutate: func(spec map[string]any) {
			spec["enforcement"].(map[string]any)["requiredCapabilities"] = []any{"filesystem"}
		}},
		{name: "remove requirement", reject: true, mutate: func(spec map[string]any) { delete(spec, "enforcement") }},
		{name: "add requirement", unprotected: true, reject: true},
		{name: "duplicate capabilities on create", create: true, reject: true, mutate: func(spec map[string]any) {
			spec["enforcement"].(map[string]any)["requiredCapabilities"] = []any{"filesystem", "filesystem"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var previous any
			if !test.create {
				old := makeSpec()
				if test.unprotected {
					delete(old, "enforcement")
				}
				previous = old
			}
			current := makeSpec()
			if test.mutate != nil {
				test.mutate(current)
			}
			errs, _ := validator.Validate(t.Context(), nil, structural, current, previous, celconfig.RuntimeCELCostBudget)
			if (len(errs) != 0) != test.reject {
				t.Fatalf("admission rejected=%t, want %t: %v", len(errs) != 0, test.reject, errs)
			}
		})
	}
}
