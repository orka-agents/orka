/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

const (
	soulAutonomyRule    = "!has(self.soul) || !has(self.coordination) || !has(self.coordination.autonomous) || !self.coordination.autonomous"
	soulAutonomyMessage = "soul is not yet supported with coordination.autonomous: soul-bound Tasks cannot be suspended"
)

// The CRD rule keeps a soul off autonomous Agents even where no Agent
// admission webhook runs, such as a direct kubectl apply.
func TestAgentSoulAutonomyMarkerAdmission(t *testing.T) {
	_, testFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	// -trimpath builds report a module path; go test runs in the package directory.
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "agent_types.go"))
	if err != nil {
		source, err = os.ReadFile("agent_types.go")
	}
	if err != nil {
		t.Fatal(err)
	}
	marker := "// +kubebuilder:validation:XValidation:rule=\"" + soulAutonomyRule + "\",message=\"" + soulAutonomyMessage + "\""
	if !strings.Contains(string(source), marker) {
		t.Fatalf("AgentSpec is missing the soul autonomy marker: want %q", marker)
	}

	schema := apiextensions.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensions.JSONSchemaProps{
			"soul": {Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{"inline": {Type: "string"}}},
			"coordination": {Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{
				"enabled":    {Type: "boolean"},
				"autonomous": {Type: "boolean"},
			}},
		},
		XValidations: apiextensions.ValidationRules{{Rule: soulAutonomyRule, Message: soulAutonomyMessage}},
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatalf("build structural AgentSpec schema: %v", err)
	}
	validator := cel.NewValidator(structural, false, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("compile soul autonomy rule: validator is nil")
	}
	soul := map[string]any{"inline": "persona"}
	for _, tc := range []struct {
		name    string
		spec    map[string]any
		wantErr bool
	}{
		{"soul only", map[string]any{"soul": soul}, false},
		{"soul with coordination", map[string]any{"soul": soul, "coordination": map[string]any{"enabled": true}}, false},
		{"soul with autonomy off", map[string]any{"soul": soul, "coordination": map[string]any{"autonomous": false}}, false},
		{"autonomy without soul", map[string]any{"coordination": map[string]any{"enabled": true, "autonomous": true}}, false},
		{"soul with autonomy", map[string]any{"soul": soul, "coordination": map[string]any{"enabled": true, "autonomous": true}}, true},
		// The controller iterates on autonomous alone.
		{"soul with disabled autonomy", map[string]any{"soul": soul, "coordination": map[string]any{"enabled": false, "autonomous": true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs, _ := validator.Validate(context.Background(), nil, structural, tc.spec, nil, celconfig.RuntimeCELCostBudget)
			if !tc.wantErr {
				if len(errs) != 0 {
					t.Fatalf("rule unexpectedly rejected the Agent: %v", errs.ToAggregate())
				}
				return
			}
			if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), soulAutonomyMessage) {
				t.Fatalf("soul with autonomy was not rejected: %v", errs.ToAggregate())
			}
		})
	}
}
