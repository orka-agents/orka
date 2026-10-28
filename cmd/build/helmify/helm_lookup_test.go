package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func helmLookupConfig(t *testing.T, objects map[string]any) string {
	t.Helper()
	resources := func(group string, entries ...map[string]any) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": group, "resources": entries}
	}
	resource := func(name, kind string, namespaced bool) map[string]any {
		return map[string]any{"name": name, "kind": kind, "namespaced": namespaced, "verbs": []string{"get", "list"}}
	}
	discovery := map[string]any{
		"/version": map[string]string{"major": "1", "minor": "35", "gitVersion": "v1.35.0"},
		"/api":     map[string]any{"apiVersion": "v1", "kind": "APIVersions", "versions": []string{"v1"}},
		"/apis": map[string]any{"apiVersion": "v1", "kind": "APIGroupList", "groups": []any{
			map[string]any{
				"name": "apps", "versions": []any{map[string]string{"groupVersion": "apps/v1", "version": "v1"}},
				"preferredVersion": map[string]string{"groupVersion": "apps/v1", "version": "v1"},
			},
			map[string]any{
				"name":             "apiextensions.k8s.io",
				"versions":         []any{map[string]string{"groupVersion": "apiextensions.k8s.io/v1", "version": "v1"}},
				"preferredVersion": map[string]string{"groupVersion": "apiextensions.k8s.io/v1", "version": "v1"},
			},
		}},
		"/api/v1": resources("v1",
			resource("namespaces", "Namespace", false),
			resource("configmaps", "ConfigMap", true),
			resource("secrets", "Secret", true),
			resource("services", "Service", true)),
		"/apis/apps/v1": resources("apps/v1", resource("deployments", "Deployment", true)),
		"/apis/apiextensions.k8s.io/v1": resources("apiextensions.k8s.io/v1",
			resource("customresourcedefinitions", "CustomResourceDefinition", false)),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet {
			t.Errorf("Helm lookup attempted a write: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		value, exists := objects[request.URL.Path]
		if !exists {
			value, exists = discovery[request.URL.Path]
		}
		if !exists {
			writer.WriteHeader(http.StatusNotFound)
			value = map[string]any{
				"apiVersion": "v1", "kind": "Status", "status": "Failure",
				"reason": "NotFound", "code": http.StatusNotFound,
			}
		}
		if err := json.NewEncoder(writer).Encode(value); err != nil {
			t.Errorf("write Helm lookup fixture: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fixture
  cluster:
    server: %s
contexts:
- name: fixture
  context:
    cluster: fixture
    user: fixture
current-context: fixture
users:
- name: fixture
  user: {}
`, server.URL)
	configPath := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0600))
	return configPath
}
