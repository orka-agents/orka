package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentHostDisableUpgradeChecksEffectiveWatchedNamespace(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for chart upgrade tests")
	}
	for _, helper := range []string{
		"static/templates/_helpers.tpl",
		"../../../manifest_staging/charts/orka/templates/_helpers.tpl",
	} {
		for _, tc := range []struct {
			name, watchNamespace, hostNamespace string
			crd, blocked                        bool
		}{
			{"default live host", "", "orka-test", true, true},
			{"default empty", "", "", true, false},
			{"split watched host", "orka-tasks", "orka-tasks", true, true},
			{"split unrelated release host", "orka-tasks", "orka-test", true, false},
			{"split empty", "orka-tasks", "", true, false},
			{"pre AgentHost release", "orka-tasks", "", false, false},
		} {
			t.Run(helper+"/"+tc.name, func(t *testing.T) {
				config := agentHostDisableLookupConfig(t, tc.hostNamespace, tc.crd)
				chart := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(chart, "templates"), 0700))
				body, err := os.ReadFile(helper)
				require.NoError(t, err)
				for name, contents := range map[string][]byte{
					"Chart.yaml":             []byte("apiVersion: v2\nname: audit\nversion: 0.1.0\n"),
					"values.yaml":            []byte("controller:\n  agentHosts:\n    enabled: false\n"),
					"templates/_helpers.tpl": body,
					"templates/probe.yaml": []byte(
						"{{ include \"orka.validateAgentHosts\" . }}\n" +
							"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: guard-passed\n"),
				} {
					require.NoError(t, os.WriteFile(filepath.Join(chart, name), contents, 0600))
				}
				args := []string{"template", "audit", chart, "--namespace", "orka-test",
					"--is-upgrade", "--dry-run=server", "--kubeconfig", config}
				if tc.watchNamespace != "" {
					args = append(args, "--set-string", "controller.watchNamespace="+tc.watchNamespace)
				}
				output, err := exec.Command(helm, args...).CombinedOutput()
				if tc.blocked {
					require.Error(t, err, "%s", output)
					require.Contains(t, string(output), "would leave 1 AgentHost(s) in "+tc.hostNamespace)
					return
				}
				require.NoError(t, err, "%s", output)
				require.Contains(t, string(output), "name: guard-passed")
			})
		}
	}
}

func agentHostDisableLookupConfig(t *testing.T, hostNamespace string, crd bool) string {
	t.Helper()
	apiGroup := func(name, version string) map[string]any {
		groupVersion := name + "/" + version
		identity := map[string]string{"groupVersion": groupVersion, "version": version}
		return map[string]any{
			"name": name, "versions": []any{identity}, "preferredVersion": identity,
		}
	}
	objects := map[string]any{
		"/apis": map[string]any{
			"apiVersion": "v1", "kind": "APIGroupList", "groups": []any{
				apiGroup("apiextensions.k8s.io", "v1"), apiGroup("core.orka.ai", "v1alpha1"),
			},
		},
		"/apis/core.orka.ai/v1alpha1": map[string]any{
			"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "core.orka.ai/v1alpha1",
			"resources": []any{map[string]any{
				"name": "agenthosts", "kind": "AgentHost", "namespaced": true,
				"verbs": []string{"get", "list"},
			}},
		},
	}
	if crd {
		objects["/apis/apiextensions.k8s.io/v1/customresourcedefinitions/agenthosts.core.orka.ai"] = map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]string{"name": "agenthosts.core.orka.ai"},
		}
	}
	for _, namespace := range []string{"orka-test", "orka-tasks"} {
		items := []any{}
		if namespace == hostNamespace {
			items = append(items, map[string]any{
				"apiVersion": "core.orka.ai/v1alpha1", "kind": "AgentHost",
				"metadata": map[string]string{"name": "live-host", "namespace": namespace},
			})
		}
		objects["/apis/core.orka.ai/v1alpha1/namespaces/"+namespace+"/agenthosts"] = map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1", "kind": "AgentHostList", "items": items,
		}
	}
	return helmLookupConfig(t, objects)
}
