package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestStaticChartOptionalEnforcementValues(t *testing.T) {
	helm, err := exec.LookPath("helm")
	require.NoError(t, err)
	for _, setting := range []string{"missing", "null", "disabled"} {
		t.Run(setting, func(t *testing.T) {
			destination := t.TempDir()
			oldOutput := *outputDir
			*outputDir = destination
			t.Cleanup(func() { *outputDir = oldOutput })
			require.NoError(t, copyStaticFiles("static"))
			path := filepath.Join(destination, "values.yaml")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			var values map[string]any
			require.NoError(t, yaml.Unmarshal(raw, &values))
			delete(values, "enforcement")
			switch setting {
			case "null":
				values["enforcement"] = nil
			case "disabled":
				values["enforcement"] = map[string]any{"enabled": false}
			}
			raw, err = yaml.Marshal(values)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, raw, 0600))
			args := append([]string{"template", "test", destination, "--namespace", "orka-test"}, staticChartDefaultArgs()...)
			output, err := exec.Command(helm, args...).CombinedOutput()
			require.NoError(t, err, string(output))
		})
	}
}
