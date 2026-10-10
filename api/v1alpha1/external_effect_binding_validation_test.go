package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
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

func TestExternalEffectApprovalTaskBindingAdmission(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "core.orka.ai_externaleffects.yaml"))
	require.NoError(t, err)
	var definition apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &definition))
	external := definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	require.Contains(t, external.Properties, "approvalTaskUID", "the API must retain the immutable Task binding")
	var schema apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&external, &schema, nil))
	structural, err := structuralschema.NewStructural(&schema)
	require.NoError(t, err)
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	require.NotNil(t, validator)
	openAPI, _, err := schemavalidation.NewSchemaValidator(&schema)
	require.NoError(t, err)
	makeSpec := func(uid string) map[string]any {
		value := map[string]any{
			"id": "effect", "kind": "acp-mcp-tool", "identityNamespace": "tenant-a",
			"aggregateId": "shared-runtime-session", "operationId": "call",
			"requestDigest": "sha256:" + strings.Repeat("a", 64),
		}
		if uid != "" {
			value["approvalTaskUID"] = uid
		}
		return value
	}
	for _, test := range []struct {
		name, before, after string
		create, reject      bool
	}{
		{name: "create bound effect", after: "task-a", create: true},
		{name: "create legacy effect", create: true},
		{name: "preserve bound effect", before: "task-a", after: "task-a"},
		{name: "preserve legacy effect"},
		{name: "change binding", before: "task-a", after: "task-b", reject: true},
		{name: "remove binding", before: "task-a", reject: true},
		{name: "backfill legacy binding", after: "task-a", reject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var oldSpec any
			if !test.create {
				oldSpec = makeSpec(test.before)
			}
			newSpec := makeSpec(test.after)
			require.Empty(t, schemavalidation.ValidateCustomResource(nil, newSpec, openAPI))
			errs, _ := validator.Validate(t.Context(), nil, structural, newSpec, oldSpec, celconfig.RuntimeCELCostBudget)
			if test.reject {
				require.NotEmpty(t, errs)
				require.Contains(t, errs.ToAggregate().Error(), "external effect spec is immutable")
			} else {
				require.Empty(t, errs)
			}
		})
	}
}
