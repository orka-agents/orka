package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

func TestControllerStartupProbe(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		args       []string
		timing     [3]int32
		healthPort int32
	}{
		{name: "kustomize", source: "../../../config/manager/manager.yaml", timing: [3]int32{60, 10, 1}, healthPort: 8081},
		{name: "helm default", timing: [3]int32{60, 10, 1}, healthPort: 8082},
		{name: "helm override", timing: [3]int32{120, 15, 3}, healthPort: 18082, args: []string{
			"--set", "controller.startupProbe.failureThreshold=120",
			"--set", "controller.startupProbe.periodSeconds=15",
			"--set", "controller.startupProbe.timeoutSeconds=3",
			"--set", "controller.healthPort=18082",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rendered string
			if tc.source != "" {
				data, err := os.ReadFile(tc.source)
				require.NoError(t, err)
				rendered = string(data)
			} else {
				rendered = requireHelmRender(t, append([]string{"--show-only", "templates/deployment.yaml"}, tc.args...)...)
			}
			var deployment appsv1.Deployment
			for document := range strings.SplitSeq(rendered, "\n---\n") {
				var candidate appsv1.Deployment
				require.NoError(t, yaml.Unmarshal([]byte(document), &candidate))
				if candidate.Kind == "Deployment" {
					deployment = candidate
					break
				}
			}
			require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
			controller := deployment.Spec.Template.Spec.Containers[0]
			require.NotNil(t, controller.StartupProbe, "initialization must have its own startup allowance")
			require.NotNil(t, controller.StartupProbe.HTTPGet)
			require.Equal(t, "/healthz", controller.StartupProbe.HTTPGet.Path)
			require.Equal(t, tc.timing, [3]int32{
				controller.StartupProbe.FailureThreshold,
				controller.StartupProbe.PeriodSeconds,
				controller.StartupProbe.TimeoutSeconds,
			})
			require.Equal(t, controller.LivenessProbe.HTTPGet, controller.StartupProbe.HTTPGet)
			require.Contains(t, controller.Args, fmt.Sprintf("--health-probe-bind-address=:%d", tc.healthPort))
			if tc.source == "" {
				require.Equal(t, intstr.FromString("health"), controller.StartupProbe.HTTPGet.Port)
				var healthPort int32
				for _, port := range controller.Ports {
					if port.Name == "health" {
						healthPort = port.ContainerPort
					}
				}
				require.Equal(t, tc.healthPort, healthPort)
			} else {
				require.Equal(t, intstr.FromInt32(tc.healthPort), controller.StartupProbe.HTTPGet.Port)
			}
			require.Equal(t, int32(15), controller.LivenessProbe.InitialDelaySeconds)
			require.Equal(t, int32(20), controller.LivenessProbe.PeriodSeconds)
			require.Equal(t, int32(5), controller.ReadinessProbe.InitialDelaySeconds)
			require.Equal(t, int32(10), controller.ReadinessProbe.PeriodSeconds)
		})
	}
}

func TestControllerStartupProbeReusedValues(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for startup probe render tests")
	}
	defaults, err := os.ReadFile("static/values.yaml")
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		settings map[string]any
		timing   [3]int32
	}{
		{name: "absent settings", timing: [3]int32{60, 10, 1}},
		{name: "empty settings", settings: map[string]any{}, timing: [3]int32{60, 10, 1}},
		{name: "partial settings", settings: map[string]any{"periodSeconds": 15}, timing: [3]int32{60, 15, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chartDir := filepath.Join(t.TempDir(), "chart")
			require.NoError(t, os.CopyFS(chartDir, os.DirFS("static")))
			forceStaticChartNamespaceMode(t, chartDir, "harness-v2")
			var values map[string]any
			require.NoError(t, yaml.Unmarshal(defaults, &values))
			controller, ok := values["controller"].(map[string]any)
			require.True(t, ok, "controller values must be a map")
			delete(controller, "startupProbe")
			if tc.settings != nil {
				controller["startupProbe"] = tc.settings
			}
			// --reuse-values replaces the new chart defaults with the old
			// release's coalesced values; a values overlay would merge them.
			retained, err := yaml.Marshal(values)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(chartDir, "values.yaml"), retained, 0o600))
			args := append([]string{
				"template", "test", chartDir, "--namespace", "orka-test", "--is-upgrade",
			}, staticChartDefaultArgs()...)
			args = append(args, "--show-only", "templates/deployment.yaml")
			rendered, err := exec.Command(helm, args...).CombinedOutput()
			require.NoError(t, err, "%s", rendered)
			var deployment appsv1.Deployment
			require.NoError(t, yaml.Unmarshal(rendered, &deployment))
			require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
			probe := deployment.Spec.Template.Spec.Containers[0].StartupProbe
			require.NotNil(t, probe)
			require.Equal(t, tc.timing, [3]int32{probe.FailureThreshold, probe.PeriodSeconds, probe.TimeoutSeconds})
		})
	}
}
