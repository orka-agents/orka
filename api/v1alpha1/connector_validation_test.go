package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func loadCRD(t *testing.T, crdFile string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", crdFile))
	if err != nil {
		t.Fatal(err)
	}
	var definition apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &definition); err != nil {
		t.Fatal(err)
	}
	return definition
}

func loadRootValidator(t *testing.T, crdFile string) schemavalidation.SchemaValidator {
	t.Helper()
	definition := loadCRD(t, crdFile)
	var rootSchema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		definition.Spec.Versions[0].Schema.OpenAPIV3Schema, &rootSchema, nil); err != nil {
		t.Fatal(err)
	}
	rootValidator, _, err := schemavalidation.NewSchemaValidator(&rootSchema)
	if err != nil {
		t.Fatal(err)
	}
	return rootValidator
}

// requireSpecAtRoot proves the root schema refuses to drop spec, which is the
// only way a transition rule scoped to spec could be bypassed.
func requireSpecAtRoot(t *testing.T, crdFile string, spec map[string]any) {
	t.Helper()
	rootValidator := loadRootValidator(t, crdFile)
	withSpec := map[string]any{"spec": spec}
	if errs := schemavalidation.ValidateCustomResource(nil, withSpec, rootValidator); len(errs) != 0 {
		t.Fatalf("valid object rejected by its root schema: %v", errs)
	}
	if errs := schemavalidation.ValidateCustomResource(nil, map[string]any{}, rootValidator); len(errs) == 0 {
		t.Fatalf("%s accepts an object without spec, which bypasses spec-scoped immutability", crdFile)
	}
	if errs := schemavalidation.ValidateCustomResourceUpdate(nil, map[string]any{}, withSpec, rootValidator); len(errs) == 0 {
		t.Fatalf("%s allows removing spec on update", crdFile)
	}
}

func loadSpecCELValidator(t *testing.T, crdFile string) (*cel.Validator, *structuralschema.Structural) {
	t.Helper()
	definition := loadCRD(t, crdFile)
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
		t.Fatalf("%s has no admission validation", crdFile)
	}
	return validator, structural
}

func TestConnectionSpecImmutability(t *testing.T) {
	validator, structural := loadSpecCELValidator(t, "core.orka.ai_connections.yaml")
	makeSpec := func(issuer, subject, provider, mode string) map[string]any {
		return map[string]any{
			"subject":     map[string]any{"issuer": issuer, "subject": subject},
			"providerRef": map[string]any{"name": provider},
			"mode":        mode,
		}
	}
	base := makeSpec("https://issuer.example.test", "alice", "github", "readOnly")
	requireSpecAtRoot(t, "core.orka.ai_connections.yaml", base)
	for _, test := range []struct {
		name      string
		old, new  map[string]any
		wantError bool
	}{
		{name: "create", old: nil, new: base},
		{name: "mode change", old: base, new: makeSpec("https://issuer.example.test", "alice", "github", "readWrite")},
		{name: "subject change", old: base, new: makeSpec("https://issuer.example.test", "bob", "github", "readOnly"), wantError: true},
		{name: "issuer change", old: base, new: makeSpec("https://other.example.test", "alice", "github", "readOnly"), wantError: true},
		{name: "provider change", old: base, new: makeSpec("https://issuer.example.test", "alice", "gmail", "readOnly"), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var oldSpec any
			if test.old != nil {
				oldSpec = test.old
			}
			errs, _ := validator.Validate(t.Context(), nil, structural, test.new, oldSpec, celconfig.RuntimeCELCostBudget)
			if (len(errs) != 0) != test.wantError {
				t.Fatalf("errors = %v, want rejection %t", errs, test.wantError)
			}
		})
	}
}

func TestConnectorProviderSpecRules(t *testing.T) {
	validator, structural := loadSpecCELValidator(t, "core.orka.ai_connectorproviders.yaml")
	makeSpec := func(mutate func(map[string]any)) map[string]any {
		spec := map[string]any{
			"oauth": map[string]any{
				"authorizeURL":    "https://github.com/login/oauth/authorize",
				"tokenURL":        "https://github.com/login/oauth/access_token",
				"clientID":        "client",
				"clientSecretRef": map[string]any{"name": "oauth", "key": "clientSecret"},
			},
			"tools": []any{
				map[string]any{"name": "list_pull_requests", "class": "read", "source": "Builtin"},
			},
		}
		if mutate != nil {
			mutate(spec)
		}
		return spec
	}
	requireSpecAtRoot(t, "core.orka.ai_connectorproviders.yaml", makeSpec(nil))
	t.Run("http timeout must decode as a bounded duration", func(t *testing.T) {
		rootValidator := loadRootValidator(t, "core.orka.ai_connectorproviders.yaml")
		type timeoutCase struct {
			timeout   string
			wantError bool
		}
		cases := []timeoutCase{
			{timeout: "30s"}, {timeout: "10m"}, {timeout: "0.5s"}, {timeout: "600s"}, {timeout: "250ms"},
			{timeout: "30 seconds", wantError: true}, {timeout: "", wantError: true}, {timeout: "-5s", wantError: true},
			{timeout: "1m30s"}, {timeout: "500µs"}, {timeout: "1m0.5s"}, {timeout: "10m0.000000001s", wantError: true},
			{timeout: "1h0m0s", wantError: true}, {timeout: "1m30", wantError: true}, {timeout: "30sm", wantError: true},
			{timeout: "11m", wantError: true}, {timeout: "1h", wantError: true}, {timeout: "0s", wantError: true},
			// time.ParseDuration overflows on these even though they look well-formed.
			{timeout: "9223372036854775808ns", wantError: true}, {timeout: "9223372037s", wantError: true},
			{timeout: "999999h999999h999999h", wantError: true},
		}
		// The forms a typed client sends: metav1.Duration serializes through
		// time.Duration.String, so 10m arrives as "10m0s" and 0.5ms as "500µs".
		durations := []time.Duration{10 * time.Minute, 150 * time.Second, 90 * time.Second, 30 * time.Second, 1500 * time.Millisecond, 500 * time.Microsecond, time.Nanosecond}
		typed := make([]timeoutCase, 0, len(durations))
		for _, d := range durations {
			raw, err := json.Marshal(metav1.Duration{Duration: d})
			if err != nil {
				t.Fatal(err)
			}
			var serialized string
			if err := json.Unmarshal(raw, &serialized); err != nil {
				t.Fatal(err)
			}
			typed = append(typed, timeoutCase{timeout: serialized})
		}
		for _, test := range append(cases, typed...) {
			spec := makeSpec(func(s map[string]any) {
				tool := s["tools"].([]any)[0].(map[string]any)
				tool["source"] = "HTTP"
				tool["description"] = "x"
				tool["http"] = map[string]any{"url": "https://api.github.com/x", "timeout": test.timeout}
			})
			schemaErrs := schemavalidation.ValidateCustomResource(nil, map[string]any{"spec": spec}, rootValidator)
			var celErrs []error
			if len(schemaErrs) == 0 {
				// CEL runs only on schema-valid objects, as in the API server.
				errs, _ := validator.Validate(t.Context(), nil, structural, spec, nil, celconfig.RuntimeCELCostBudget)
				for _, err := range errs {
					celErrs = append(celErrs, err)
				}
			}
			if (len(schemaErrs)+len(celErrs) != 0) != test.wantError {
				t.Fatalf("timeout %q: schema errors = %v, cel errors = %v, want rejection %t", test.timeout, schemaErrs, celErrs, test.wantError)
			}
		}
	})
	oauth := func(spec map[string]any) map[string]any { return spec["oauth"].(map[string]any) }
	tool := func(spec map[string]any) map[string]any { return spec["tools"].([]any)[0].(map[string]any) }
	for _, test := range []struct {
		name      string
		mutate    func(map[string]any)
		wantError bool
	}{
		{name: "valid"},
		{name: "http authorize", mutate: func(s map[string]any) { oauth(s)["authorizeURL"] = "http://github.com/authorize" }, wantError: true},
		{name: "http token", mutate: func(s map[string]any) { oauth(s)["tokenURL"] = "http://github.com/token" }, wantError: true},
		{name: "http revocation", mutate: func(s map[string]any) { oauth(s)["revocationURL"] = "http://github.com/revoke" }, wantError: true},
		{name: "https revocation", mutate: func(s map[string]any) { oauth(s)["revocationURL"] = "https://github.com/revoke" }},
		{name: "reserved authorize parameter", mutate: func(s map[string]any) {
			oauth(s)["additionalAuthorizeParameters"] = map[string]any{"Redirect_URI": "https://evil.example"}
		}, wantError: true},
		{name: "allowed authorize parameter", mutate: func(s map[string]any) {
			oauth(s)["additionalAuthorizeParameters"] = map[string]any{"prompt": "consent"}
		}},
		{name: "http tool without http", mutate: func(s map[string]any) { tool(s)["source"] = "HTTP"; tool(s)["description"] = "x" }, wantError: true},
		{name: "http tool plain url", mutate: func(s map[string]any) {
			tool(s)["source"] = "HTTP"
			tool(s)["description"] = "x"
			tool(s)["http"] = map[string]any{"url": "http://api.github.com/x"}
		}, wantError: true},
		{name: "http tool valid", mutate: func(s map[string]any) {
			tool(s)["source"] = "HTTP"
			tool(s)["description"] = "x"
			tool(s)["http"] = map[string]any{"url": "https://api.github.com/x"}
		}},
		{name: "builtin with http", mutate: func(s map[string]any) {
			tool(s)["http"] = map[string]any{"url": "https://api.github.com/x"}
		}, wantError: true},
		{name: "builtin with description", mutate: func(s map[string]any) { tool(s)["description"] = "x" }, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			errs, _ := validator.Validate(t.Context(), nil, structural, makeSpec(test.mutate), nil, celconfig.RuntimeCELCostBudget)
			if (len(errs) != 0) != test.wantError {
				t.Fatalf("errors = %v, want rejection %t", errs, test.wantError)
			}
		})
	}
}
