package v1alpha1

import (
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

func TestTaskSoulBindingFreezesExecutionInputs(t *testing.T) {
	const definition = "core.orka.ai_tasks.yaml"
	crd := loadCRD(t, definition)
	var schema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("Task root has no admission validation")
	}
	rootValidator := loadRootValidator(t, definition)
	for _, bound := range []bool{false, true} {
		for _, change := range []string{"unchanged", "prompt", "suspend"} {
			name := map[bool]string{false: "unbound/", true: "bound/"}[bound] + change
			t.Run(name, func(t *testing.T) {
				old := map[string]any{
					"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
					"metadata": map[string]any{"name": "task", "namespace": "default"},
					"spec": map[string]any{
						"type": "ai", "ai": map[string]any{"provider": "openai", "model": "test", "prompt": "hello"},
					},
				}
				if bound {
					digest := "sha256:" + strings.Repeat("a", 64)
					old["status"] = map[string]any{"soulBinding": map[string]any{
						"taskGeneration": int64(1), "agentUID": "agent", "agentGeneration": int64(1),
						"soulDigest": digest, "promptDigest": digest, "inputsDigest": digest,
					}}
				}
				current := runtime.DeepCopyJSON(old)
				spec := current["spec"].(map[string]any)
				switch change {
				case "prompt":
					spec["ai"].(map[string]any)["prompt"] = "changed"
				case "suspend":
					spec["suspend"] = true
				}
				for _, object := range []map[string]any{old, current} {
					if errs := schemavalidation.ValidateCustomResource(nil, object, rootValidator); len(errs) != 0 {
						t.Fatalf("invalid admission fixture: %v", errs)
					}
				}
				errs, _ := validator.Validate(t.Context(), nil, structural, current, old, celconfig.RuntimeCELCostBudget)
				reject := bound && change == "prompt"
				if (len(errs) != 0) != reject {
					t.Fatalf("execution mutation rejected=%t, want %t: %v", len(errs) != 0, reject, errs)
				}
			})
		}
	}
}
