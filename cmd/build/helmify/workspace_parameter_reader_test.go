package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func requireWorkspaceParameterReaders(t *testing.T, rendered, profileName, profileBindingName,
	configName, configBindingName, namespace, serviceAccount string,
) {
	t.Helper()
	for _, reader := range []struct {
		name, label string
	}{
		{profileName, "workspace.orka.ai/aggregate-to-parameter-reader"},
		{configName, "workspace.orka.ai/aggregate-to-provider-config-reader"},
	} {
		var role rbacv1.ClusterRole
		document := requireRenderedDocument(t, rendered, "kind: ClusterRole\n", "\n  name: "+reader.name+"\n")
		if err := yaml.Unmarshal([]byte(document), &role); err != nil {
			t.Fatal(err)
		}
		wantAggregation := &rbacv1.AggregationRule{ClusterRoleSelectors: []metav1.LabelSelector{{
			MatchLabels: map[string]string{reader.label: "true"},
		}}}
		if !reflect.DeepEqual(role.AggregationRule, wantAggregation) || len(role.Rules) != 0 || role.Namespace != "" {
			t.Fatalf("%s must aggregate only its distinct opt-in label without fixed permissions: %#v", reader.name, role)
		}
	}

	wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: serviceAccount, Namespace: namespace}}
	var profileBinding rbacv1.RoleBinding
	profileDocument := requireRenderedDocument(t, rendered, "kind: RoleBinding\n", "\n  name: "+profileBindingName+"\n")
	if err := yaml.Unmarshal([]byte(profileDocument), &profileBinding); err != nil {
		t.Fatal(err)
	}
	if profileBinding.Namespace != namespace ||
		profileBinding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: profileName}) ||
		!reflect.DeepEqual(profileBinding.Subjects, wantSubjects) {
		t.Fatal("profile reader must bind only the exact controller ServiceAccount in its watch namespace")
	}

	var configBinding rbacv1.ClusterRoleBinding
	configDocument := requireRenderedDocument(t, rendered,
		"kind: ClusterRoleBinding\n", "\n  name: "+configBindingName+"\n")
	if err := yaml.Unmarshal([]byte(configDocument), &configBinding); err != nil {
		t.Fatal(err)
	}
	if configBinding.Namespace != "" ||
		configBinding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: configName}) ||
		!reflect.DeepEqual(configBinding.Subjects, wantSubjects) {
		t.Fatal("provider-config reader must bind only the exact controller ServiceAccount cluster-wide")
	}

	for document := range strings.SplitSeq(rendered, "\n---\n") {
		if !strings.Contains(document, "kind: ClusterRoleBinding\n") {
			continue
		}
		var binding rbacv1.ClusterRoleBinding
		if err := yaml.Unmarshal([]byte(document), &binding); err != nil {
			t.Fatal(err)
		}
		if binding.RoleRef.Kind == "ClusterRole" && binding.RoleRef.Name == profileName {
			t.Fatal("namespaced profile permissions must never be bound cluster-wide")
		}
	}
}

func TestStaticChartAggregatesWorkspaceParameterReadsOnlyForDispatch(t *testing.T) {
	const profileName = "test-orka-controller-cluster-workspace-parameter-reader"
	const configName = "test-orka-controller-cluster-workspace-provider-config-reader"
	for _, test := range []struct {
		name, serviceAccount string
		args                 []string
	}{
		{name: "managed controller", serviceAccount: "test-orka"},
		{name: "external controller", serviceAccount: "external-controller", args: []string{
			"--set", "serviceAccount.create=false",
			"--set-string", "serviceAccount.name=external-controller",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{
				"--set", "controller.executionWorkspace.dispatchEnabled=true",
				"--show-only", "templates/rbac.yaml",
			}, test.args...)
			rendered := requireHelmRender(t, args...)
			requireWorkspaceParameterReaders(t, rendered, profileName, profileName, configName, configName,
				staticChartTestNamespace, test.serviceAccount)
		})
	}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "dispatch disabled"},
		{name: "MCP only", args: []string{
			"--set", "controller.substrate.mcpToolsEnabled=true",
			"--set-string", "controller.substrate.apiCredentials.existingSecret=substrate-control",
			"--set-string", "controller.substrate.apiCredentials.bearerTokenKey=token",
			"--set-string", "controller.substrate.apiCredentials.caKey=ca",
		}},
		{name: "RBAC disabled", args: []string{
			"--set", "controller.executionWorkspace.dispatchEnabled=true",
			"--set", "rbac.create=false",
		}},
		{name: "harness v1", args: []string{
			"--set-string", "controller.mode=harness-v1",
			"--set-string", "harnessV1.image.digest=sha256:" + strings.Repeat("1", 64),
			"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
			"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := requireHelmRender(t, test.args...)
			for _, marker := range []string{
				"workspace-parameter-reader", "workspace-provider-config-reader",
				"workspace.orka.ai/aggregate-to-parameter-reader", "workspace.orka.ai/aggregate-to-provider-config-reader",
			} {
				if strings.Contains(rendered, marker) {
					t.Fatalf("%s rendered without managed RBAC, harness-v2 and workspace dispatch", marker)
				}
			}
		})
	}
}

func TestWorkspaceParameterReadersCanonicalRBAC(t *testing.T) {
	root := filepath.Join("..", "..", "..", "config", "rbac")
	contents, err := os.ReadFile(filepath.Join(root, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(contents, &kustomization); err != nil {
		t.Fatal(err)
	}
	documents := make([]string, 0, len(kustomization.Resources))
	for _, file := range []string{
		"workspace_parameter_reader_role.yaml", "workspace_parameter_reader_role_binding.yaml",
		"workspace_parameter_reader_config_role.yaml", "workspace_parameter_reader_config_role_binding.yaml",
	} {
		if !slices.Contains(kustomization.Resources, file) {
			t.Fatalf("canonical RBAC kustomization omits %s", file)
		}
	}
	// Include all canonical RBAC documents to reject any cluster binding of the profile reader.
	for _, file := range kustomization.Resources {
		contents, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, string(contents))
	}
	requireWorkspaceParameterReaders(t, strings.Join(documents, "\n---\n"),
		"workspace-parameter-reader", "workspace-parameter-reader-rolebinding",
		"workspace-provider-config-reader", "workspace-provider-config-reader-rolebinding",
		"system", "controller-manager")
}
