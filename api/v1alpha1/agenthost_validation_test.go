package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// Keyed lists compare without regard to order, but init containers run in
// order and an environment value can refer to an earlier one, so an update
// that only reorders them must be refused like any other change.
func TestAgentHostTemplateSpecRefusesReordering(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "core.orka.ai_agenthosttemplates.yaml"))
	require.NoError(t, err)
	var definition apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &definition))
	spec := definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	var schema apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&spec, &schema, nil))
	structural, err := structuralschema.NewStructural(&schema)
	require.NoError(t, err)
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	require.NotNil(t, validator)
	openAPI, _, err := schemavalidation.NewSchemaValidator(&schema)
	require.NoError(t, err)

	container := func(name string, env ...string) map[string]any {
		value := map[string]any{"name": name, "image": "registry.example/" + name + "@sha256:" + testTemplateImageDigest}
		if len(env) > 0 {
			vars := make([]any, 0, len(env))
			for _, variable := range env {
				vars = append(vars, map[string]any{"name": variable, "value": "x"})
			}
			value["env"] = vars
		}
		return value
	}
	makeSpec := func(initContainers []any, host map[string]any) map[string]any {
		value := map[string]any{
			"securityContext": map[string]any{"runAsUser": int64(1000), "runAsGroup": int64(1000)},
			"containers":      []any{host},
			"storage":         map[string]any{"size": "1Gi"},
			"listenPort":      int64(7777),
			"access":          "Standalone",
		}
		if initContainers != nil {
			value["initContainers"] = initContainers
		}
		return value
	}
	before := makeSpec([]any{container("fetch"), container("unpack", "A", "B")}, container("host", "A", "B"))
	for _, test := range []struct {
		name   string
		after  map[string]any
		reject bool
	}{
		{name: "unchanged", after: makeSpec([]any{container("fetch"), container("unpack", "A", "B")}, container("host", "A", "B"))},
		{name: "init containers reordered", reject: true,
			after: makeSpec([]any{container("unpack", "A", "B"), container("fetch")}, container("host", "A", "B"))},
		{name: "host environment reordered", reject: true,
			after: makeSpec([]any{container("fetch"), container("unpack", "A", "B")}, container("host", "B", "A"))},
		{name: "init environment reordered", reject: true,
			after: makeSpec([]any{container("fetch"), container("unpack", "B", "A")}, container("host", "A", "B"))},
		{name: "init containers removed", reject: true, after: makeSpec(nil, container("host", "A", "B"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Empty(t, schemavalidation.ValidateCustomResource(nil, test.after, openAPI))
			errs, _ := validator.Validate(t.Context(), nil, structural, test.after, before, celconfig.RuntimeCELCostBudget)
			if test.reject {
				require.NotEmpty(t, errs)
				require.Contains(t, errs.ToAggregate().Error(), "AgentHostTemplate spec is immutable")
			} else {
				require.Empty(t, errs)
			}
		})
	}
}

const testTemplateImageDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// A host takes its token from a Secret or a Connection, never both, and a
// Connection-fed host keeps its Connection for life.
func TestAgentHostSpecTokenSourceRules(t *testing.T) {
	validator, structural := loadSpecCELValidator(t, "core.orka.ai_agenthosts.yaml")
	spec := func(secret, connection string) map[string]any {
		value := map[string]any{"templateRef": map[string]any{"name": "copilotd"}}
		if secret != "" {
			value["githubTokenSecretRef"] = map[string]any{"name": secret}
		}
		if connection != "" {
			value["githubTokenFrom"] = map[string]any{"connectionRef": map[string]any{"name": connection}}
		}
		return value
	}
	for _, test := range []struct {
		name      string
		new, old  map[string]any
		wantError bool
	}{
		{name: "a Secret", new: spec("alice-github", "")},
		{name: "a Connection", new: spec("", "github-alice")},
		{name: "both", new: spec("alice-github", "github-alice"), wantError: true},
		{name: "the same Connection", new: spec("", "github-alice"), old: spec("", "github-alice")},
		{name: "another Connection", new: spec("", "github-bob"), old: spec("", "github-alice"), wantError: true},
		{name: "a Connection added", new: spec("", "github-alice"), old: spec("", ""), wantError: true},
		{name: "the Connection removed", new: spec("alice-github", ""), old: spec("", "github-alice"), wantError: true},
		{name: "a Secret changed", new: spec("bob-github", ""), old: spec("alice-github", "")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var old any
			if test.old != nil {
				old = test.old
			}
			errs, _ := validator.Validate(t.Context(), nil, structural, test.new, old, celconfig.RuntimeCELCostBudget)
			if (len(errs) != 0) != test.wantError {
				t.Fatalf("errors = %v, want rejection %t", errs, test.wantError)
			}
		})
	}
}
