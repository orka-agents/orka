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
	webhookEventsRule   = "!has(self.webhookEvents) || size(self.webhookEvents) == 0 || (has(self.webhookURL) && size(self.webhookURL) > 0)"
	webhookEventsMarker = "// +kubebuilder:validation:XValidation:rule=\"" + webhookEventsRule + "\",message=\"webhookEvents requires webhookURL\""
)

func TestTaskWebhookEventsRequireWebhookURL(t *testing.T) {
	if source := string(readTaskTypesSource(t)); !strings.Contains(source, webhookEventsMarker) {
		t.Fatalf("TaskSpec is missing the webhookEvents marker: want %q", webhookEventsMarker)
	}
	schema := apiextensions.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensions.JSONSchemaProps{
			"webhookURL": {Type: "string"},
			"webhookEvents": {
				Type:  "array",
				Items: &apiextensions.JSONSchemaPropsOrArray{Schema: &apiextensions.JSONSchemaProps{Type: "string"}},
			},
		},
		XValidations: apiextensions.ValidationRules{{Rule: webhookEventsRule, Message: "webhookEvents requires webhookURL"}},
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatal(err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("compile webhookEvents rule: validator is nil")
	}
	events := []any{string(TaskWebhookEventApprovalRequested)}
	for _, tc := range []struct {
		name    string
		spec    map[string]any
		wantErr bool
	}{
		{name: "neither", spec: map[string]any{}},
		{name: "terminal only", spec: map[string]any{"webhookURL": "https://example.com/hook"}},
		{name: "empty events", spec: map[string]any{"webhookEvents": []any{}}},
		{name: "events with URL", spec: map[string]any{"webhookURL": "https://example.com/hook", "webhookEvents": events}},
		{name: "events without URL", spec: map[string]any{"webhookEvents": events}, wantErr: true},
		{name: "events with empty URL", spec: map[string]any{"webhookURL": "", "webhookEvents": events}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs, _ := validator.Validate(context.Background(), nil, structural, tc.spec, nil, celconfig.RuntimeCELCostBudget)
			if tc.wantErr != (len(errs) != 0) {
				t.Fatalf("errors = %v, want error %v", errs.ToAggregate(), tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(errs.ToAggregate().Error(), "webhookEvents requires webhookURL") {
				t.Fatalf("error = %v", errs.ToAggregate())
			}
		})
	}
}
