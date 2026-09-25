package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestCheckpointSourceWebhookMatchesSharedAdmission(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "config", "orka-admission-webhooks", "validating_webhook.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	chart := requireHelmRender(t, "--show-only", "templates/controller-validating-webhook.yaml")
	wantScope := admissionv1.NamespacedScope
	wantRules := []admissionv1.RuleWithOperations{{
		Operations: []admissionv1.OperationType{admissionv1.Create},
		Rule: admissionv1.Rule{
			APIGroups: []string{"workspace.orka.ai"}, APIVersions: []string{"v1alpha1"},
			Resources: []string{"executionworkspacecheckpoints"}, Scope: &wantScope,
		},
	}}
	for name, manifest := range map[string]string{"shared": string(shared), "helm": chart} {
		t.Run(name, func(t *testing.T) {
			var config admissionv1.ValidatingWebhookConfiguration
			if err := yaml.Unmarshal([]byte(manifest), &config); err != nil {
				t.Fatal(err)
			}
			for _, webhook := range config.Webhooks {
				service := webhook.ClientConfig.Service
				if service == nil || service.Path == nil ||
					*service.Path != "/validate-workspace-orka-ai-v1alpha1-checkpoint-source-use" {
					continue
				}
				if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionv1.Fail ||
					!reflect.DeepEqual(webhook.Rules, wantRules) {
					t.Fatal("checkpoint source webhook does not fail closed on exactly checkpoint creation")
				}
				return
			}
			t.Fatal("checkpoint source authorization webhook is missing")
		})
	}
}

func TestWorkspaceUsePoliciesMatchSharedAdmission(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "policy", "workspace_class_use_policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	chart := requireHelmRender(t, "--show-only", "templates/workspace-class-use-policy.yaml")
	for _, name := range []string{"task-workspace-class-use", "tool-workspace-class-use", "checkpoint-source-use"} {
		t.Run(name, func(t *testing.T) {
			sharedDoc := requireRenderedDocument(t, string(shared),
				"kind: ValidatingAdmissionPolicy\n", "name: "+name+".orka.ai")
			chartDoc := requireRenderedDocument(t, chart, "kind: ValidatingAdmissionPolicy\n", "name: test-orka-"+name)
			var sharedPolicy, chartPolicy admissionv1.ValidatingAdmissionPolicy
			if err := yaml.Unmarshal([]byte(sharedDoc), &sharedPolicy); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(chartDoc), &chartPolicy); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sharedPolicy.Spec, chartPolicy.Spec) {
				t.Fatal("Helm and Kustomize enforce different workspace use authorization")
			}
			bindingDoc := requireRenderedDocument(t, chart, "kind: ValidatingAdmissionPolicyBinding\n", "name: test-orka-"+name)
			var binding admissionv1.ValidatingAdmissionPolicyBinding
			if err := yaml.Unmarshal([]byte(bindingDoc), &binding); err != nil {
				t.Fatal(err)
			}
			if binding.Spec.PolicyName != chartPolicy.Name ||
				!slices.Equal(binding.Spec.ValidationActions, []admissionv1.ValidationAction{admissionv1.Deny}) {
				t.Fatal("Helm authorization policy has no enforcing binding")
			}
		})
	}
}

func TestStaticChartGrantsNativeSubstrateCheckpointRBAC(t *testing.T) {
	rendered := requireHelmRender(t, "--show-only", "templates/rbac.yaml")
	var role rbacv1.Role
	roleDoc := requireRenderedDocument(t, rendered, "# Controller tenant Role.", "kind: Role\n")
	if err := yaml.Unmarshal([]byte(roleDoc), &role); err != nil {
		t.Fatal(err)
	}
	if role.Namespace != staticChartTestNamespace {
		t.Fatal("native Substrate permissions escaped the controller namespace")
	}
	for _, test := range []struct {
		group, resource string
		verbs           []string
	}{
		{"workspace.orka.ai", "executionworkspacecheckpoints", []string{"get", "list", "watch", "update", "patch", "use"}},
		{"workspace.orka.ai", "executionworkspacecheckpoints/status", []string{"get", "update", "patch"}},
		{"workspace.orka.ai", "executionworkspacecheckpoints/finalizers", []string{"update"}},
	} {
		for _, verb := range test.verbs {
			if !testSubstrateRuleAllows(role.Rules, test.group, test.resource, verb) {
				t.Errorf("controller lacks %s on %s/%s", verb, test.group, test.resource)
			}
		}
	}
	var clusterRole rbacv1.ClusterRole
	clusterDoc := requireRenderedDocument(t, rendered, "kind: ClusterRole\n", `resources: ["tokenreviews"]`)
	if err := yaml.Unmarshal([]byte(clusterDoc), &clusterRole); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"get", "list", "watch"} {
		if !testSubstrateRuleAllows(clusterRole.Rules, "ate.dev", "workerpools", verb) {
			t.Errorf("controller cannot %s WorkerPools outside its tenant namespace", verb)
		}
	}
	allRules := append(slices.Clone(role.Rules), clusterRole.Rules...)
	for _, verb := range []string{"create", "update", "patch", "delete"} {
		if testSubstrateRuleAllows(allRules, "ate.dev", "workerpools", verb) {
			t.Errorf("controller unexpectedly has %s on provider WorkerPools", verb)
		}
	}
	if testSubstrateRuleAllows(allRules, "ate.dev", "actortemplates", "get") {
		t.Fatal("controller still requests obsolete Kubernetes ActorTemplate access")
	}
}

func testSubstrateRuleAllows(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if (slices.Contains(rule.APIGroups, group) || slices.Contains(rule.APIGroups, "*")) &&
			(slices.Contains(rule.Resources, resource) || slices.Contains(rule.Resources, "*")) &&
			(slices.Contains(rule.Verbs, verb) || slices.Contains(rule.Verbs, "*")) {
			return true
		}
	}
	return false
}
