package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// grantsClassOperatorAccess reports whether rules let their subject pass the
// operator-access admission check on ExecutionWorkspaceClasses, including
// through a wildcard.
func grantsClassOperatorAccess(rules []rbacv1.PolicyRule) bool {
	matches := func(values []string, want string) bool {
		return slices.Contains(values, want) || slices.Contains(values, rbacv1.ResourceAll)
	}
	for _, rule := range rules {
		if matches(rule.APIGroups, "workspace.orka.ai") &&
			matches(rule.Resources, "executionworkspaceclasses") &&
			matches(rule.Verbs, "operator-access") {
			return true
		}
	}
	return false
}

func rolesGrantingClassOperatorAccess(t *testing.T, manifest string) []string {
	t.Helper()
	var roles []string
	for _, document := range decodeWorkerRBACDocuments(t, manifest) {
		if (document.Kind == "ClusterRole" || document.Kind == "Role") && grantsClassOperatorAccess(document.Rules) {
			roles = append(roles, document.Kind+"/"+document.Metadata.Name)
		}
	}
	return roles
}

// Opting a class into orka task exec and orka task cp requires the
// operator-access verb, and the feature's promise is that nobody holds it
// until a cluster administrator grants it deliberately. A bundled role that
// carries the verb, most easily through a '*' on the class resource, would
// hand it to everyone already bound to that role.
func TestNoBundledRoleGrantsClassOperatorAccess(t *testing.T) {
	installer, err := os.ReadFile(filepath.Join("..", "..", "..", "manifest_staging", "deploy", "orka.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, manifest := range map[string]string{
		"Kustomize installer": string(installer),
		"Helm chart":          requireHelmRender(t),
	} {
		documents := decodeWorkerRBACDocuments(t, manifest)
		if !slices.ContainsFunc(documents, func(document workerRBACDocument) bool { return document.Kind == "ClusterRole" }) {
			t.Fatalf("%s: found no ClusterRole to inspect", name)
		}
		if roles := rolesGrantingClassOperatorAccess(t, manifest); len(roles) != 0 {
			t.Errorf("%s: bundled roles grant operator-access on executionworkspaceclasses: %v", name, roles)
		}
	}
}

func TestGrantsClassOperatorAccessSeesWildcards(t *testing.T) {
	const group, classes = "workspace.orka.ai", "executionworkspaceclasses"
	rule := func(group, resource string, verbs ...string) []rbacv1.PolicyRule {
		return []rbacv1.PolicyRule{{APIGroups: []string{group}, Resources: []string{resource}, Verbs: verbs}}
	}
	for name, tc := range map[string]struct {
		rules []rbacv1.PolicyRule
		want  bool
	}{
		"explicit verb":     {rule(group, classes, "get", "operator-access"), true},
		"verb wildcard":     {rule(group, classes, "*"), true},
		"resource wildcard": {rule(group, "*", "*"), true},
		"group wildcard":    {rule("*", "*", "*"), true},
		"class management": {rule(group, classes,
			"create", "delete", "deletecollection", "get", "list", "patch", "update", "use", "watch"), false},
		"another resource":   {rule(group, "executionworkspacepools", "*"), false},
		"another API group":  {rule("core.orka.ai", "*", "*"), false},
		"status subresource": {rule(group, classes+"/status", "*"), false},
	} {
		if got := grantsClassOperatorAccess(tc.rules); got != tc.want {
			t.Errorf("%s: grantsClassOperatorAccess = %v, want %v", name, got, tc.want)
		}
	}
}
