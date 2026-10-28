package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentHostNamespacesMatchControllerReservations(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for chart namespace tests")
	}
	for _, helper := range []struct {
		name, path string
	}{
		{"source", "static/templates/_helpers.tpl"},
		{"staged", "../../../manifest_staging/charts/orka/templates/_helpers.tpl"},
	} {
		chart := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(chart, "templates"), 0700))
		body, err := os.ReadFile(helper.path)
		require.NoError(t, err)
		for name, contents := range map[string][]byte{
			"Chart.yaml": []byte("apiVersion: v2\nname: namespace-test\nversion: 0.1.0\n"),
			"values.yaml": []byte("controller:\n  watchNamespace: \"\"\n" +
				"  acpRuntime:\n    namespace: orka-runtimes\n" +
				"  agentHosts:\n    enabled: true\n    namespace: orka-hosts\n" +
				"    agentImage: registry.example.invalid/orka@sha256:" + strings.Repeat("0", 64) + "\n"),
			"templates/_helpers.tpl": body,
			"templates/probe.yaml": []byte("{{ include \"orka.validateAgentHosts\" . }}\n" +
				"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: namespace-test\n"),
		} {
			require.NoError(t, os.WriteFile(filepath.Join(chart, name), contents, 0600))
		}
		for _, tc := range []struct {
			name, watch, host string
			enabled, blocked  bool
		}{
			{"split watched collision", "orka-tasks", "orka-tasks", true, true},
			{"split distinct", "orka-tasks", "orka-hosts", true, false},
			{"default watched collision", "", "orka-test", true, true},
			{"default distinct", "", "orka-hosts", true, false},
			{"disabled split collision", "orka-tasks", "orka-tasks", false, false},
			{"release collision", "orka-tasks", "orka-test", true, true},
			{"ACP collision", "orka-tasks", "orka-runtimes", true, true},
		} {
			t.Run(helper.name+"/"+tc.name, func(t *testing.T) {
				output, err := exec.Command(helm, "template", "namespace-test", chart,
					"--namespace", "orka-test",
					"--set-string", "controller.watchNamespace="+tc.watch,
					"--set-string", "controller.agentHosts.namespace="+tc.host,
					"--set", "controller.agentHosts.enabled="+strconv.FormatBool(tc.enabled),
				).CombinedOutput()
				if tc.blocked {
					require.Error(t, err, "%s", output)
				} else {
					require.NoError(t, err, "%s", output)
				}
			})
		}
	}
}
