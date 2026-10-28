package main

import (
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func requireRenderedObject(t *testing.T, rendered, kind, name, namespace string) string {
	t.Helper()
	for document := range strings.SplitSeq(rendered, "\n---") {
		var metadata metav1.PartialObjectMetadata
		if err := yaml.Unmarshal([]byte(document), &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata.Kind == kind && metadata.Name == name && metadata.Namespace == namespace {
			return document
		}
	}
	t.Fatalf("rendered %s %s/%s is missing", kind, namespace, name)
	return ""
}

func requireRenderedRole(t *testing.T, rendered, name string) rbacv1.Role {
	t.Helper()
	var role rbacv1.Role
	document := requireRenderedObject(t, rendered, "Role", name, staticChartTestNamespace)
	if err := yaml.Unmarshal([]byte(document), &role); err != nil {
		t.Fatal(err)
	}
	return role
}

func requireResourceVerbs(t *testing.T, rules []rbacv1.PolicyRule, group, resource string, expected []string) {
	t.Helper()
	var verbs []string
	for _, rule := range rules {
		if (slices.Contains(rule.APIGroups, group) || slices.Contains(rule.APIGroups, "*")) &&
			(slices.Contains(rule.Resources, resource) || slices.Contains(rule.Resources, "*")) {
			if len(rule.ResourceNames) != 0 {
				t.Fatalf("%s/%s unexpectedly restricts resource names", group, resource)
			}
			verbs = append(verbs, rule.Verbs...)
		}
	}
	slices.Sort(verbs)
	verbs = slices.Compact(verbs)
	expected = slices.Clone(expected)
	slices.Sort(expected)
	if !slices.Equal(verbs, expected) {
		t.Fatalf("%s/%s verbs = %v, want %v", group, resource, verbs, expected)
	}
}
