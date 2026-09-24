package v1alpha1

import (
	"encoding/json"
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

func TestTaskWorkspacePullRequestTextAdmission(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "core.orka.ai_tasks.yaml"))
	require.NoError(t, err)
	var definition apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &definition))
	workspace := definition.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["workspace"]
	for _, test := range []struct {
		field string
		limit int
	}{{"prTitle", 256}, {"prBody", 32768}} {
		t.Run(test.field, func(t *testing.T) {
			field, ok := workspace.Properties[test.field]
			require.True(t, ok, "admission must preserve the field")
			require.Equal(t, "string", field.Type)
			require.NotNil(t, field.MaxLength)
			require.EqualValues(t, test.limit, *field.MaxLength)
			require.NotContains(t, workspace.Required, test.field)
			var schema apiextensions.JSONSchemaProps
			require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&field, &schema, nil))
			validator, _, err := schemavalidation.NewSchemaValidator(&schema)
			require.NoError(t, err)
			for _, char := range []string{"a", "界", "🙂"} {
				require.Empty(t, schemavalidation.ValidateCustomResource(nil, "", validator))
				require.Empty(t, schemavalidation.ValidateCustomResource(nil, strings.Repeat(char, test.limit), validator))
				require.NotEmpty(t, schemavalidation.ValidateCustomResource(nil, strings.Repeat(char, test.limit+1), validator))
			}
			require.NotEmpty(t, schemavalidation.ValidateCustomResource(nil, 42, validator))
			if test.field == "prBody" {
				structural, err := structuralschema.NewStructural(&schema)
				require.NoError(t, err)
				celValidator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
				require.NotNil(t, celValidator)
				for _, body := range []string{"", "## Summary\n\nA regular body.", strings.Repeat("界", test.limit)} {
					errs, _ := celValidator.Validate(t.Context(), nil, structural, body, nil, celconfig.RuntimeCELCostBudget)
					require.Empty(t, errs)
				}
				for _, kind := range []string{"intent", "session"} {
					body := "Copied body\n\n<!-- orka.publisher.pr-" + kind + ".v1 key=sha256:" + strings.Repeat("a", 64) + " -->"
					errs, _ := celValidator.Validate(t.Context(), nil, structural, body, nil, celconfig.RuntimeCELCostBudget)
					require.NotEmpty(t, errs)
					require.Contains(t, errs.ToAggregate().Error(), "reserved publisher reconciliation markers")
				}
			}
		})
	}
}

func TestTaskWorkspacePullRequestTextRoundTrip(t *testing.T) {
	original := WorkspaceConfig{PRTitle: "fix: preserve 世界", PRBody: "## Summary\n\nPreserve Unicode.\n"}
	encoded, err := json.Marshal(original)
	require.NoError(t, err)
	var decoded WorkspaceConfig
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, original, decoded)
	encoded, err = json.Marshal(WorkspaceConfig{})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "prTitle")
	require.NotContains(t, string(encoded), "prBody")
}
