/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"context"
	"regexp"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

func TestTaskGitHubAppCredentialsOnlyOnAgentTasks(t *testing.T) {
	const message = "GitHubApp workspace credentials are only supported for agent Tasks"
	match := regexp.MustCompile(`XValidation:rule="(self\.type == 'agent' \|\| !has\(self\.workspace\) \|\| [^"]*GitHubApp[^"]*)",message="` + message + `"`).
		FindSubmatch(readTaskTypesSource(t))
	if match == nil {
		t.Fatal("TaskSpec is missing the GitHubApp credential marker")
	}
	const signingMessage = "commitSigningCredentialRef does not support GitHubApp"
	signing := regexp.MustCompile(`XValidation:rule="([^"]*commitSigningCredentialRef[^"]*)",message="` + signingMessage + `"`).
		FindSubmatch(readTaskTypesSource(t))
	if signing == nil {
		t.Fatal("TaskSpec is missing the commit signing GitHubApp marker")
	}
	ref := apiextensions.JSONSchemaProps{Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{"type": {Type: "string"}}}
	refs := map[string]apiextensions.JSONSchemaProps{}
	for _, name := range []string{"readCredentialRef", "publicationReadCredentialRef", "publicationCredentialRef", "forgeCredentialRef", "commitSigningCredentialRef"} {
		refs[name] = ref
	}
	structural, err := structuralschema.NewStructural(&apiextensions.JSONSchemaProps{
		Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{
			"type": {Type: "string"}, "workspace": {Type: "object", Properties: refs},
		},
		XValidations: apiextensions.ValidationRules{{Rule: string(match[1]), Message: message}, {Rule: string(signing[1]), Message: signingMessage}},
	})
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	app, token := map[string]any{"type": "GitHubApp"}, map[string]any{"type": "Token"}
	for name, tc := range map[string]struct {
		spec    map[string]any
		wantErr bool
	}{
		"agent with App on every role": {spec: map[string]any{"type": "agent", "workspace": map[string]any{
			"readCredentialRef": app, "publicationReadCredentialRef": app, "publicationCredentialRef": app, "forgeCredentialRef": app,
			"commitSigningCredentialRef": token,
		}}},
		"container with Token and untyped refs": {spec: map[string]any{"type": "container", "workspace": map[string]any{
			"readCredentialRef": map[string]any{}, "publicationCredentialRef": token,
		}}},
		"container with App read":             {spec: map[string]any{"type": "container", "workspace": map[string]any{"readCredentialRef": app}}, wantErr: true},
		"container with App publication read": {spec: map[string]any{"type": "container", "workspace": map[string]any{"publicationReadCredentialRef": app}}, wantErr: true},
		"container with App publication":      {spec: map[string]any{"type": "container", "workspace": map[string]any{"publicationCredentialRef": app}}, wantErr: true},
		"ai with App forge":                   {spec: map[string]any{"type": "ai", "workspace": map[string]any{"forgeCredentialRef": app}}, wantErr: true},
		"agent with App commit signing":       {spec: map[string]any{"type": "agent", "workspace": map[string]any{"commitSigningCredentialRef": app}}, wantErr: true},
	} {
		errs, _ := validator.Validate(context.Background(), nil, structural, tc.spec, nil, celconfig.RuntimeCELCostBudget)
		if tc.wantErr != (len(errs) != 0) {
			t.Errorf("%s: errors = %v, want error %v", name, errs.ToAggregate(), tc.wantErr)
		}
	}
}
