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
	sessionRefImmutabilityRule   = "has(self.sessionRef) == has(oldSelf.sessionRef) && (!has(self.sessionRef) || self.sessionRef == oldSelf.sessionRef)"
	sessionRefImmutabilityMarker = "// +kubebuilder:validation:XValidation:rule=\"" + sessionRefImmutabilityRule + "\",message=\"sessionRef is immutable\""
)

func TestTaskSessionRefImmutabilityMarkerAdmission(t *testing.T) {
	if source := string(readTaskTypesSource(t)); !strings.Contains(source, sessionRefImmutabilityMarker) {
		t.Fatalf("TaskSpec is missing the complete sessionRef immutability marker: want %q", sessionRefImmutabilityMarker)
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
			Rule:    sessionRefImmutabilityRule,
			Message: "sessionRef is immutable",
		}},
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatalf("build structural TaskSpec schema: %v", err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("compile sessionRef immutability admission rule: validator is nil")
	}

	fullSessionRef := map[string]any{
		"name":             "session-a",
		"create":           true,
		"append":           true,
		"maxMessages":      int64(50),
		"throughMessageId": "message-42",
		"promptIncluded":   true,
	}
	oldTask := taskSpecForSessionRefAdmission("agent", fullSessionRef)

	tests := []struct {
		name    string
		oldSpec map[string]any
		newSpec map[string]any
		wantErr bool
	}{
		{
			name:    "create with reference",
			newSpec: taskSpecForSessionRefAdmission("agent", fullSessionRef),
		},
		{
			name:    "unchanged absent reference",
			oldSpec: taskSpecForSessionRefAdmission("agent", nil),
			newSpec: taskSpecForSessionRefAdmission("agent", nil),
		},
		{
			name:    "unchanged complete reference",
			oldSpec: oldTask,
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
			oldSpec: oldTask,
			newSpec: taskSpecForSessionRefAdmission("agent", nil),
			wantErr: true,
		},
		{
			name:    "change name",
			oldSpec: oldTask,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "name", "session-b")),
			wantErr: true,
		},
		{
			name:    "change create",
			oldSpec: oldTask,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "create", false)),
			wantErr: true,
		},
		{
			name:    "change append",
			oldSpec: oldTask,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "append", false)),
			wantErr: true,
		},
		{
			name:    "change max messages",
			oldSpec: oldTask,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "maxMessages", int64(10))),
			wantErr: true,
		},
		{
			name:    "change transcript cutoff",
			oldSpec: oldTask,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "throughMessageId", "message-41")),
			wantErr: true,
		},
		{
			name:    "change prompt included",
			oldSpec: oldTask,
			newSpec: taskSpecForSessionRefAdmission("agent", changedSessionRef(fullSessionRef, "promptIncluded", false)),
			wantErr: true,
		},
	}

	for _, taskType := range []string{"agent", "ai", "container"} {
		for _, tt := range tests {
			t.Run(taskType+"/"+tt.name, func(t *testing.T) {
				oldSpec := maps.Clone(tt.oldSpec)
				newSpec := maps.Clone(tt.newSpec)
				if oldSpec != nil {
					oldSpec["type"] = taskType
				}
				newSpec["type"] = taskType
				errs, _ := validator.Validate(
					context.Background(),
					nil,
					structural,
					newSpec,
					oldSpec,
					celconfig.RuntimeCELCostBudget,
				)
				if tt.wantErr {
					if len(errs) == 0 {
						t.Fatal("sessionRef mutation unexpectedly passed admission")
					}
					if got := errs.ToAggregate().Error(); !strings.Contains(got, "sessionRef is immutable") {
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
