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

const (
	requesterDecisionsRule    = "self.type == 'agent' || !has(self.agentRuntime) || !has(self.agentRuntime.permissionDecisions) || self.agentRuntime.permissionDecisions != 'Requester'"
	requesterDecisionsMessage = "agentRuntime.permissionDecisions Requester is only supported for agent Tasks"
	requesterDecisionsMarker  = "// +kubebuilder:validation:XValidation:rule=\"" + requesterDecisionsRule + "\",message=\"" + requesterDecisionsMessage + "\""
)

// Only agent Tasks are planned onto a runtime that can ask before each tool;
// AI and container Tasks would run their tools without the requester deciding.
func TestTaskRequesterPermissionDecisionsAreForAgentTasksOnly(t *testing.T) {
	if source := string(readTaskTypesSource(t)); !strings.Contains(source, requesterDecisionsMarker) {
		t.Fatalf("TaskSpec is missing the requester decisions marker: want %q", requesterDecisionsMarker)
	}
	schema := apiextensions.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensions.JSONSchemaProps{
			"type": {Type: "string"},
			"agentRuntime": {
				Type:       "object",
				Properties: map[string]apiextensions.JSONSchemaProps{"permissionDecisions": {Type: "string"}},
			},
		},
		XValidations: apiextensions.ValidationRules{{Rule: requesterDecisionsRule, Message: requesterDecisionsMessage}},
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatalf("build structural TaskSpec schema: %v", err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("compile the requester decisions rule: validator is nil")
	}
	for _, tc := range []struct {
		taskType, decisions string
		wantErr             bool
	}{
		{taskType: "agent", decisions: "Requester"},
		{taskType: "agent", decisions: "Policy"},
		{taskType: "ai"},
		{taskType: "ai", decisions: "Policy"},
		{taskType: "ai", decisions: "Requester", wantErr: true},
		{taskType: "container", decisions: "Requester", wantErr: true},
	} {
		spec := map[string]any{"type": tc.taskType}
		if tc.decisions != "" {
			spec["agentRuntime"] = map[string]any{"permissionDecisions": tc.decisions}
		}
		errs, _ := validator.Validate(context.Background(), nil, structural, spec, nil, celconfig.RuntimeCELCostBudget)
		if got := len(errs) > 0; got != tc.wantErr {
			t.Errorf("%s Task with permissionDecisions %q: rejected=%v, want %v (%v)", tc.taskType, tc.decisions, got, tc.wantErr, errs)
		}
		if tc.wantErr && len(errs) > 0 && !strings.Contains(errs.ToAggregate().Error(), requesterDecisionsMessage) {
			t.Errorf("%s Task rejected for another reason: %v", tc.taskType, errs)
		}
	}
}
