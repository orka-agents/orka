/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"context"
	"maps"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

const (
	agentSessionRefImmutabilityRule   = "self.type != 'agent' || (has(self.sessionRef) == has(oldSelf.sessionRef) && (!has(self.sessionRef) || self.sessionRef == oldSelf.sessionRef))"
	agentSessionRefImmutabilityMarker = "// +kubebuilder:validation:XValidation:rule=\"" + agentSessionRefImmutabilityRule + "\",message=\"sessionRef is immutable for agent Tasks\""
)

func TestAgentSessionRefImmutabilityMarkerAdmission(t *testing.T) {
	if source := string(readTaskTypesSource(t)); !strings.Contains(source, agentSessionRefImmutabilityMarker) {
		t.Fatalf("TaskSpec is missing the complete agent sessionRef immutability marker: want %q", agentSessionRefImmutabilityMarker)
	}

	schema := apiextensions.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensions.JSONSchemaProps{
			"type": {Type: "string"},
			"sessionRef": {
				Type: "object",
				Properties: map[string]apiextensions.JSONSchemaProps{
					"name":             {Type: "string"},
					"create":           {Type: "boolean"},
					"append":           {Type: "boolean"},
					"maxMessages":      {Type: "integer", Format: "int32"},
					"throughMessageId": {Type: "string"},
					"promptIncluded":   {Type: "boolean"},
				},
			},
		},
		XValidations: apiextensions.ValidationRules{{
			Rule:    agentSessionRefImmutabilityRule,
			Message: "sessionRef is immutable for agent Tasks",
		}},
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatalf("build structural TaskSpec schema: %v", err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("compile agent sessionRef immutability admission rule: validator is nil")
	}

	fullSessionRef := map[string]any{
		"name":             "session-a",
		"create":           true,
		"append":           true,
		"maxMessages":      int64(50),
		"throughMessageId": "message-42",
		"promptIncluded":   true,
	}
	oldAgent := taskSpecForSessionRefAdmission("agent", fullSessionRef)

	tests := []struct {
		name    string
		oldSpec map[string]any
		newSpec map[string]any
		wantErr bool
	}{
		{
			name:    "unchanged complete reference",
			oldSpec: oldAgent,
			newSpec: taskSpecForSessionRefAdmission("agent", fullSessionRef),
		},
		{
			name:    "add reference",
			oldSpec: taskSpecForSessionRefAdmission("agent", nil),
			newSpec: taskSpecForSessionRefAdmission("agent", fullSessionRef),
			wantErr: true,
		},
		{
			name:    "remove reference",
			oldSpec: oldAgent,
			newSpec: taskSpecForSessionRefAdmission("agent", nil),
			wantErr: true,
		},
		{
			name:    "change name",
			oldSpec: oldAgent,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "name", "session-b")),
			wantErr: true,
		},
		{
			name:    "change create",
			oldSpec: oldAgent,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "create", false)),
			wantErr: true,
		},
		{
			name:    "change append",
			oldSpec: oldAgent,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "append", false)),
			wantErr: true,
		},
		{
			name:    "change max messages",
			oldSpec: oldAgent,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "maxMessages", int64(10))),
			wantErr: true,
		},
		{
			name:    "change transcript cutoff",
			oldSpec: oldAgent,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "throughMessageId", "message-41")),
			wantErr: true,
		},
		{
			name:    "change prompt included",
			oldSpec: oldAgent,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "promptIncluded", false)),
			wantErr: true,
		},
		{
			name:    "container reference remains mutable",
			oldSpec: taskSpecForSessionRefAdmission("container", fullSessionRef),
			newSpec: taskSpecForSessionRefAdmission("container", changedSessionRef(fullSessionRef, "name", "session-b")),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs, _ := validator.Validate(
				context.Background(),
				nil,
				structural,
				tt.newSpec,
				tt.oldSpec,
				celconfig.RuntimeCELCostBudget,
			)
			if tt.wantErr {
				if len(errs) == 0 {
					t.Fatal("agent sessionRef mutation unexpectedly passed admission")
				}
				if got := errs.ToAggregate().Error(); !strings.Contains(got, "sessionRef is immutable for agent Tasks") {
					t.Fatalf("admission error = %q, want sessionRef immutability message", got)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("admission unexpectedly rejected update: %v", errs.ToAggregate())
			}
		})
	}
}

func taskSpecForSessionRefAdmission(taskType string, sessionRef map[string]any) map[string]any {
	spec := map[string]any{"type": taskType}
	if sessionRef != nil {
		spec["sessionRef"] = changedSessionRef(sessionRef, "", nil)
	}
	return spec
}

func changedSessionRef(sessionRef map[string]any, field string, value any) map[string]any {
	changed := make(map[string]any, len(sessionRef))
	maps.Copy(changed, sessionRef)
	if field != "" {
		changed[field] = value
	}
	return changed
}
