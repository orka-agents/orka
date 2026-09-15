package v1alpha1

import (
	"os"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestRemoteMCPAdmissionRulesFitCELCostLimits(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/core.orka.ai_tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var external apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &external); err != nil {
		t.Fatal(err)
	}
	apiextensionsv1.SetDefaults_CustomResourceDefinition(&external)
	external.Status.StoredVersions = []string{external.Spec.Versions[0].Name}
	var crd apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&external, &crd, nil); err != nil {
		t.Fatal(err)
	}
	if errs := apiextensionsvalidation.ValidateCustomResourceDefinition(t.Context(), &crd); len(errs) > 0 {
		t.Fatalf("generated Tool CRD cannot be admitted: %v", errs)
	}
}

func TestRemoteMCPAdmission(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/core.orka.ai_tools.yaml")
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
	type admissionCase struct {
		name   string
		patch  func(map[string]any)
		reject bool
	}
	baseCases := []admissionCase{
		{name: "reviewed remote"},
		// Parameters remains schemaless for legacy compatibility; consumers enforce it for remote.
		{name: "parameters presence enforced at consumers", patch: func(s map[string]any) { delete(s, "parameters") }},
		{name: "missing credential", patch: func(s map[string]any) { delete(s["http"].(map[string]any), "authSecretRef") }, reject: true},
		{name: "missing policy", patch: func(s map[string]any) { delete(s["http"].(map[string]any), "outboundAccessPolicyRef") }, reject: true},
		{name: "missing name", patch: func(s map[string]any) { delete(s["mcp"].(map[string]any)["remote"].(map[string]any), "toolName") }, reject: true},
		{name: "empty name", patch: func(s map[string]any) { s["mcp"].(map[string]any)["remote"].(map[string]any)["toolName"] = "" }, reject: true},
		{name: "workspace conflict", patch: func(s map[string]any) {
			s["mcp"].(map[string]any)["workspace"] = map[string]any{"classRef": map[string]any{"name": "host"}, "port": int64(80)}
		}, reject: true},
		{name: "actor conflict", patch: func(s map[string]any) {
			s["mcp"].(map[string]any)["substrateActor"] = map[string]any{"templateRef": map[string]any{"name": "host"}}
		}, reject: true},
		{name: "http url conflict", patch: func(s map[string]any) { s["http"].(map[string]any)["url"] = "https://example.com" }, reject: true},
		{name: "path conflict", patch: func(s map[string]any) { s["mcp"].(map[string]any)["path"] = "/other" }, reject: true},
		{name: "body authentication", patch: func(s map[string]any) { s["http"].(map[string]any)["authInject"] = "body" }, reject: true},
		{name: "interpolation", patch: func(s map[string]any) {
			s["mcp"].(map[string]any)["remote"].(map[string]any)["url"] = "https://example.com/{path}"
		}, reject: true},
		{name: "protocol override", patch: func(s map[string]any) {
			s["http"].(map[string]any)["headers"] = map[string]any{"mCp-PrOtOcOl-VeRsIoN": "bad"}
		}, reject: true},
		{name: "session override", patch: func(s map[string]any) {
			s["http"].(map[string]any)["headers"] = map[string]any{"Mcp-Session-Id": "other"}
		}, reject: true},
		{name: "authority override", patch: func(s map[string]any) { s["http"].(map[string]any)["headers"] = map[string]any{"Host": "other"} }, reject: true},
	}
	headers := []struct {
		name   string
		reject bool
	}{
		{"X-Tenant", false}, {"x-TeNaNt", false}, {"X-ReQuEsT-ID", false},
		{"X-Api-Key", true}, {"api_key", true}, {"API-KEY", true}, {"ApiKey", true}, {"X_API_KEY", true},
		{"X-Auth-Token", true}, {"X-Access-Token", true}, {"X_AUTH_TOKEN", true},
		{"X-Client-Secret", true}, {"X-Password", true}, {"X-Credential", true}, {"X-Authorization", true},
	}
	backends := []string{"remote", "HTTP", "managed MCP"}
	cases := make([]admissionCase, 0, len(baseCases)+len(headers)*len(backends))
	cases = append(cases, baseCases...)
	for _, header := range headers {
		for _, backend := range backends {
			cases = append(cases, admissionCase{
				name: backend + " header " + header.name, reject: backend == "remote" && header.reject,
				patch: func(s map[string]any) {
					s["http"].(map[string]any)["headers"] = map[string]any{header.name: "fixture-value"}
					switch backend {
					case "HTTP":
						delete(s, "mcp")
						s["http"].(map[string]any)["url"] = "https://example.com"
					case "managed MCP":
						s["mcp"] = map[string]any{"workspace": map[string]any{"classRef": map[string]any{"name": "host"}, "port": int64(80)}}
					}
				},
			})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]any{"description": "Reviewed description", "parameters": map[string]any{"type": "object"}, "mcp": map[string]any{"remote": map[string]any{"url": "https://example.com/mcp", "toolName": "remote_name"}}, "http": map[string]any{"authSecretRef": map[string]any{"name": "auth", "key": "token"}, "outboundAccessPolicyRef": map[string]any{"name": "egress"}}}
			if tc.patch != nil {
				tc.patch(spec)
			}
			errs := schemavalidation.ValidateCustomResource(nil, spec, openAPI)
			celErrs, _ := validator.Validate(t.Context(), nil, structural, spec, nil, celconfig.RuntimeCELCostBudget)
			errs = append(errs, celErrs...)
			if (len(errs) > 0) != tc.reject {
				t.Fatalf("admission errors = %v; reject = %t", errs, tc.reject)
			}
		})
	}
}
