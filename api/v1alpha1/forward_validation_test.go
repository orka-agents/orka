package v1alpha1

import (
	"os"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func admitSpec(t *testing.T, crdPath, property string) func(map[string]any) bool {
	t.Helper()
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	external := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties[property]
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
	return func(spec map[string]any) bool {
		errs := schemavalidation.ValidateCustomResource(nil, spec, openAPI)
		celErrs, _ := validator.Validate(t.Context(), nil, structural, spec, nil, celconfig.RuntimeCELCostBudget)
		return len(errs)+len(celErrs) == 0
	}
}

func TestForwardingToolAdmission(t *testing.T) {
	admit := admitSpec(t, "../../config/crd/bases/core.orka.ai_tools.yaml", "spec")
	base := func() map[string]any {
		return map[string]any{
			"description": "Azure Resource Manager", "brokeredToolClass": "read",
			"http": map[string]any{
				"url": "https://management.azure.com", "forward": map[string]any{"protocol": "AzureResourceManager"},
				"outboundAccessPolicyRef": map[string]any{"name": "azure"},
			},
		}
	}
	http := func(s map[string]any) map[string]any { return s["http"].(map[string]any) }
	for _, tc := range []struct {
		name   string
		patch  func(map[string]any)
		reject bool
	}{
		{name: "valid"},
		{name: "trailing slash", patch: func(s map[string]any) { http(s)["url"] = "https://management.azure.com/" }},
		{name: "write class", patch: func(s map[string]any) { s["brokeredToolClass"] = "write" }, reject: true},
		{name: "no class", patch: func(s map[string]any) { delete(s, "brokeredToolClass") }, reject: true},
		{name: "no policy", patch: func(s map[string]any) { delete(http(s), "outboundAccessPolicyRef") }, reject: true},
		{name: "path", patch: func(s map[string]any) { http(s)["url"] = "https://management.azure.com/subscriptions" }, reject: true},
		{name: "plain http", patch: func(s map[string]any) { http(s)["url"] = "http://management.azure.com" }, reject: true},
		{name: "userinfo", patch: func(s map[string]any) { http(s)["url"] = "https://user@management.azure.com" }, reject: true},
		{name: "template", patch: func(s map[string]any) { http(s)["url"] = "https://{{host}}" }, reject: true},
		{name: "auth secret", patch: func(s map[string]any) { http(s)["authSecretRef"] = map[string]any{"name": "a", "key": "b"} }, reject: true},
		{name: "headers", patch: func(s map[string]any) { http(s)["headers"] = map[string]any{"X-A": "b"} }, reject: true},
		{name: "body auth", patch: func(s map[string]any) { http(s)["authInject"] = "body" }, reject: true},
		{name: "unknown protocol", patch: func(s map[string]any) { http(s)["forward"] = map[string]any{"protocol": "Other"} }, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := base()
			if tc.patch != nil {
				tc.patch(spec)
			}
			if admit(spec) == tc.reject {
				t.Fatalf("admitted = %t, want reject = %t", !tc.reject, tc.reject)
			}
		})
	}
}

func TestForwardingToolNameMustNotShadowABuiltInTool(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/core.orka.ai_tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	external := *crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	var schema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&external, &schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	tool := func(name string, forward bool) map[string]any {
		http := map[string]any{"url": "https://management.azure.com", "outboundAccessPolicyRef": map[string]any{"name": "azure"}}
		if forward {
			http["forward"] = map[string]any{"protocol": "AzureResourceManager"}
		}
		return map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1", "kind": "Tool",
			"metadata": map[string]any{"name": name, "namespace": "team-a"},
			"spec":     map[string]any{"description": "Azure", "brokeredToolClass": "read", "http": http},
		}
	}
	for _, tc := range []struct {
		name    string
		forward bool
		reject  bool
	}{
		{"azurearm", true, false},
		{"read", true, true},
		{"bash", true, true},
		{"remember", true, true},
		{"web_fetch", true, true},
		{"remember", false, false},
		{"read", false, false},
	} {
		errs, _ := validator.Validate(t.Context(), nil, structural, tool(tc.name, tc.forward), nil, celconfig.RuntimeCELCostBudget)
		if (len(errs) > 0) != tc.reject {
			t.Errorf("%s (forward=%t): errors = %v, want reject = %t", tc.name, tc.forward, errs, tc.reject)
		}
	}
}
