package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestStaticChartAggregatesWorkspaceParameterReadsOnlyForDispatch(t *testing.T) {
	const name = "test-orka-controller-cluster-workspace-parameter-reader"
	rendered := requireHelmRender(t,
		"--set", "controller.executionWorkspace.dispatchEnabled=true",
		"--set", "serviceAccount.create=false",
		"--set-string", "serviceAccount.name=external-controller",
		"--show-only", "templates/rbac.yaml")
	var role rbacv1.ClusterRole
	roleDocument := requireRenderedDocument(t, rendered, "kind: ClusterRole\n", "\n  name: "+name+"\n")
	if err := yaml.Unmarshal([]byte(roleDocument), &role); err != nil {
		t.Fatal(err)
	}
	shared, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", "workspace_parameter_reader_role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var sharedRole rbacv1.ClusterRole
	if err := yaml.Unmarshal(shared, &sharedRole); err != nil {
		t.Fatal(err)
	}
	if role.AggregationRule == nil ||
		!reflect.DeepEqual(role.AggregationRule, sharedRole.AggregationRule) || len(role.Rules) != 0 {
		t.Fatal("Helm parameter reader must aggregate exactly the provider opt-in roles without fixed permissions")
	}
	var binding rbacv1.RoleBinding
	bindingDocument := requireRenderedDocument(t, rendered, "kind: RoleBinding\n", "\n  name: "+name+"\n")
	if err := yaml.Unmarshal([]byte(bindingDocument), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Namespace != staticChartTestNamespace ||
		binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}) ||
		!reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: "external-controller", Namespace: staticChartTestNamespace,
		}}) {
		t.Fatal("parameter reader must bind only the selected controller ServiceAccount in its watch namespace")
	}
	for document := range strings.SplitSeq(rendered, "\n---\n") {
		if strings.Contains(document, "kind: ClusterRoleBinding\n") && strings.Contains(document, role.Name) {
			t.Fatal("provider parameter reads must not be bound cluster-wide")
		}
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
	} {
		t.Run(test.name, func(t *testing.T) {
			if rendered := requireHelmRender(t, test.args...); strings.Contains(rendered, name) {
				t.Fatal("parameter read authority rendered without dispatch and managed RBAC")
			}
		})
	}
}
