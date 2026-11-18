package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestSubstrateActorPoolTemplateAdmission(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "core.orka.ai_substrateactorpools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var definition apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &definition); err != nil {
		t.Fatal(err)
	}
	external := definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	var schema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&external, &schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("SubstrateActorPool has no admission validation")
	}
	makeSpec := func(namespace string, actors int64) map[string]any {
		ref := map[string]any{"name": "native-template"}
		if namespace != "" {
			ref["namespace"] = namespace
		}
		return map[string]any{"templateRef": ref, "targetActors": actors}
	}
	var rootSchema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		definition.Spec.Versions[0].Schema.OpenAPIV3Schema, &rootSchema, nil); err != nil {
		t.Fatal(err)
	}
	rootValidator, _, err := schemavalidation.NewSchemaValidator(&rootSchema)
	if err != nil {
		t.Fatal(err)
	}
	withSpec := map[string]any{"spec": makeSpec("team-a", 1)}
	if errs := schemavalidation.ValidateCustomResource(nil, withSpec, rootValidator); len(errs) != 0 {
		t.Fatalf("valid pool rejected by its root schema: %v", errs)
	}
	if errs := schemavalidation.ValidateCustomResourceUpdate(nil, map[string]any{}, withSpec, rootValidator); len(errs) == 0 {
		t.Fatal("removing spec bypasses the Atespace immutability rule")
	}
	for _, test := range []struct {
		name, before, after string
		afterTemplate       string
		create, wantError   bool
	}{
		{name: "create", after: "team-a", create: true},
		{name: "scale explicit Atespace", before: "team-a", after: "team-a"},
		{name: "scale implicit Atespace"},
		{name: "move Atespace", before: "team-a", after: "team-b", wantError: true},
		{name: "remove explicit Atespace", before: "team-a", wantError: true},
		{name: "replace implicit Atespace", after: "team-b", wantError: true},
		{name: "change template in same Atespace", before: "team-a", after: "team-a", afterTemplate: "another-template", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var oldSpec any = makeSpec(test.before, 1)
			if test.create {
				oldSpec = nil
			}
			newSpec := makeSpec(test.after, 2)
			if test.afterTemplate != "" {
				newSpec["templateRef"].(map[string]any)["name"] = test.afterTemplate
			}
			errs, _ := validator.Validate(t.Context(), nil, structural, newSpec, oldSpec,
				celconfig.RuntimeCELCostBudget)
			if (len(errs) != 0) != test.wantError {
				t.Fatalf("Atespace admission errors = %v, want rejection %t", errs, test.wantError)
			}
		})
	}
}
