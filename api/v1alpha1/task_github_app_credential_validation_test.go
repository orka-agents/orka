/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

const githubAppCredentialMessage = "GitHubApp workspace credentials are only supported for agent Tasks"

func TestTaskGitHubAppCredentialsOnlyOnAgentTasks(t *testing.T) {
	source := string(readTaskTypesSource(t))
	const prefix = "// +kubebuilder:validation:XValidation:rule=\"self.type == 'agent' || !has(self.workspace) || ((!has(self.workspace.readCredentialRef)"
	start := strings.Index(source, prefix)
	if start < 0 {
		t.Fatal("TaskSpec is missing the GitHubApp credential marker")
	}
	line := source[start : start+strings.Index(source[start:], "\n")]
	rule := strings.TrimPrefix(line, "// +kubebuilder:validation:XValidation:rule=\"")
	rule = rule[:strings.Index(rule, "\",message=")]
	if !strings.HasSuffix(line, "message=\""+githubAppCredentialMessage+"\"") {
		t.Fatalf("GitHubApp credential marker message = %q", line)
	}
	reference := apiextensions.JSONSchemaProps{Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{
		"name": {Type: "string"}, "key": {Type: "string"}, "type": {Type: "string"},
	}}
	schema := apiextensions.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensions.JSONSchemaProps{
			"type": {Type: "string"},
			"workspace": {Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{
				"readCredentialRef":            reference,
				"publicationReadCredentialRef": reference,
				"publicationCredentialRef":     reference,
				"forgeCredentialRef":           reference,
			}},
		},
		XValidations: apiextensions.ValidationRules{{Rule: rule, Message: githubAppCredentialMessage}},
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("compile GitHubApp credential rule: validator is nil")
	}
	app := map[string]any{"name": "publisher-app", "key": "privateKey", "type": string(WorkspaceCredentialGitHubApp)}
	token := map[string]any{"name": "token", "key": "token", "type": string(WorkspaceCredentialToken)}
	plain := map[string]any{"name": "token", "key": "token"}
	for _, tc := range []struct {
		name    string
		spec    map[string]any
		wantErr bool
	}{
		{name: "agent with App on every role", spec: map[string]any{"type": "agent", "workspace": map[string]any{
			"readCredentialRef": app, "publicationReadCredentialRef": app, "publicationCredentialRef": app, "forgeCredentialRef": app,
		}}},
		{name: "container without workspace", spec: map[string]any{"type": "container"}},
		{name: "container with plain and Token refs", spec: map[string]any{"type": "container", "workspace": map[string]any{
			"readCredentialRef": plain, "publicationCredentialRef": token,
		}}},
		{name: "container with App read", spec: map[string]any{"type": "container", "workspace": map[string]any{"readCredentialRef": app}}, wantErr: true},
		{name: "container with App publication read", spec: map[string]any{"type": "container", "workspace": map[string]any{"publicationReadCredentialRef": app}}, wantErr: true},
		{name: "container with App publication", spec: map[string]any{"type": "container", "workspace": map[string]any{"publicationCredentialRef": app}}, wantErr: true},
		{name: "ai with App forge", spec: map[string]any{"type": "ai", "workspace": map[string]any{"forgeCredentialRef": app}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs, _ := validator.Validate(context.Background(), nil, structural, tc.spec, nil, celconfig.RuntimeCELCostBudget)
			if tc.wantErr != (len(errs) != 0) {
				t.Fatalf("errors = %v, want error %v", errs.ToAggregate(), tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(errs.ToAggregate().Error(), githubAppCredentialMessage) {
				t.Fatalf("error = %v", errs.ToAggregate())
			}
		})
	}
}
