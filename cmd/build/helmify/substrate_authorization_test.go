package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

func TestStaticChartGrantsGenericWorkspaceCheckpointAndObservationRBAC(t *testing.T) {
	rendered := requireHelmRender(t, "--show-only", "templates/rbac.yaml")
	var role rbacv1.Role
	roleDoc := requireRenderedDocument(t, rendered, "# Controller tenant Role.", "kind: Role\n")
	if err := yaml.Unmarshal([]byte(roleDoc), &role); err != nil {
		t.Fatal(err)
	}
	if role.Namespace != staticChartTestNamespace {
		t.Fatal("workspace checkpoint permissions escaped the controller namespace")
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
	for _, verb := range []string{"get", "list"} {
		if !testSubstrateRuleAllows(clusterRole.Rules, "", "persistentvolumes", verb) {
			t.Errorf("Core cannot independently verify PV %s", verb)
		}
	}
	for _, resource := range []string{"runtimepools"} {
		if !testSubstrateRuleAllows(clusterRole.Rules, "core.orka.ai", resource, "list") {
			t.Errorf("upgrade preflight cannot list %s", resource)
		}
	}

	// Provider finalization scans checkpoint references outside the controller
	// namespace. Both production installers must grant only the cluster list.
	kustomize, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "config", "acp-workload", "v2_controller_cluster_role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kustomizeRole rbacv1.ClusterRole
	if err := yaml.Unmarshal([]byte(strings.SplitN(string(kustomize), "\n---", 2)[0]), &kustomizeRole); err != nil {
		t.Fatal(err)
	}
	for name, rules := range map[string][]rbacv1.PolicyRule{"helm": clusterRole.Rules, "kustomize": kustomizeRole.Rules} {
		if !testSubstrateRuleAllows(rules, "workspace.orka.ai", "executionworkspacecheckpoints", "list") {
			t.Errorf("%s provider deletion protection cannot list checkpoints cluster-wide", name)
		}
		for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
			if testSubstrateRuleAllows(rules, "workspace.orka.ai", "executionworkspacecheckpoints", verb) {
				t.Errorf("%s grants cluster-wide checkpoint %s", name, verb)
			}
		}
	}
	if !testSubstrateRuleAllows(clusterRole.Rules, "networking.k8s.io", "networkpolicies", "list") {
		t.Error("generic retirement cannot discover core policies")
	}

	allRules := append(slices.Clone(role.Rules), clusterRole.Rules...)
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
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
