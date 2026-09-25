package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestStaticChartArtifactLimitIsDecimalInteger(t *testing.T) {
	valuesPath := filepath.Join(t.TempDir(), "values.yaml")
	values := []byte("controller:\n  acpArtifact:\n    maxBytes: 1073741824\n")
	if err := os.WriteFile(valuesPath, values, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []string
		want int64
	}{
		{name: "default", want: 536870912},
		{name: "values file", args: []string{"--values", valuesPath}, want: 1073741824},
		{name: "integer override", args: []string{"--set", "controller.acpArtifact.maxBytes=2147483648"}, want: 2147483648},
		{
			name: "string override",
			args: []string{"--set-string", "controller.acpArtifact.maxBytes=2147483648"},
			want: 2147483648,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--show-only", "templates/deployment.yaml"}, test.args...)
			rendered := requireHelmRender(t, args...)
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
				t.Fatalf("decode rendered controller: %v", err)
			}
			for _, container := range deployment.Spec.Template.Spec.Containers {
				for _, env := range container.Env {
					if env.Name != "ORKA_ACP_ARTIFACT_MAX_BYTES" {
						continue
					}
					limit, err := strconv.ParseInt(env.Value, 10, 64)
					if err != nil || limit != test.want {
						t.Fatalf("artifact limit %q must parse as decimal %d: %v", env.Value, test.want, err)
					}
					return
				}
			}
			t.Fatal("controller artifact limit environment variable is missing")
		})
	}
}
