package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const agentHostTestNamespace = "orka-agent-hosts"

func TestStaticChartAgentHostsAreOffByDefault(t *testing.T) {
	rendered := requireHelmRender(t)
	for _, unwanted := range []string{
		"--enable-agent-hosts",
		"--agent-host-",
		"agenthost.harness-v2.orka.ai",
		"namespace: " + agentHostTestNamespace,
		"name: " + agentHostTestNamespace,
	} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("default render contains %q", unwanted)
		}
	}
}

func agentHostsEnabled() []string {
	return []string{"--set", "controller.agentHosts.enabled=true"}
}

func TestStaticChartAgentHostControllerFlags(t *testing.T) {
	rendered := requireHelmRender(t, append(agentHostsEnabled(), "--show-only", "templates/deployment.yaml")...)
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
		t.Fatalf("decode controller Deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	for _, want := range []string{
		"--enable-agent-hosts=true",
		"--agent-host-namespace=" + agentHostTestNamespace,
		"--agent-host-agent-image=" + container.Image,
		"--agent-host-controller-pod-labels=orka.ai/network-role=controller",
	} {
		if !containsString(container.Args, want) {
			t.Errorf("controller args are missing %s:\n%s", want, strings.Join(container.Args, "\n"))
		}
	}
	// Each host's NetworkPolicy admits only Pods with this label.
	if got := deployment.Spec.Template.Labels["orka.ai/network-role"]; got != "controller" {
		t.Fatalf("controller Pod label orka.ai/network-role = %q, want controller", got)
	}
}

func TestStaticChartAgentHostExplicitAgentImage(t *testing.T) {
	image := "registry.example.com/orka@sha256:" + strings.Repeat("a", 64)
	rendered := requireHelmRender(t, append(agentHostsEnabled(),
		"--set-string", "controller.agentHosts.agentImage="+image,
		"--show-only", "templates/deployment.yaml")...)
	if !strings.Contains(rendered, `"--agent-host-agent-image=`+image+`"`) {
		t.Fatalf("render does not use the explicit agent image:\n%s", rendered)
	}
}

func TestStaticChartAgentHostNamespaceIsRestricted(t *testing.T) {
	rendered := requireHelmRender(t, append(agentHostsEnabled(), "--show-only", "templates/agent-host-namespace.yaml")...)
	var namespace corev1.Namespace
	if err := yaml.Unmarshal([]byte(rendered), &namespace); err != nil {
		t.Fatalf("decode host Namespace: %v", err)
	}
	if namespace.Name != agentHostTestNamespace {
		t.Fatalf("host namespace = %q, want %s", namespace.Name, agentHostTestNamespace)
	}
	if got := namespace.Annotations["helm.sh/resource-policy"]; got != "keep" {
		t.Errorf("host namespace helm.sh/resource-policy = %q, want keep so host volumes outlive the release", got)
	}
	for _, mode := range []string{"enforce", "audit", "warn"} {
		if got := namespace.Labels["pod-security.kubernetes.io/"+mode]; got != "restricted" {
			t.Errorf("pod-security %s = %q, want restricted", mode, got)
		}
	}
}

func TestStaticChartAgentHostRoleMatchesKustomize(t *testing.T) {
	rendered := requireHelmRender(t, append(agentHostsEnabled(), "--show-only", "templates/rbac.yaml")...)
	var role, binding *workerRBACDocument
	docs := decodeWorkerRBACDocuments(t, rendered)
	for i := range docs {
		if docs[i].Metadata.Namespace != agentHostTestNamespace {
			continue
		}
		switch docs[i].Kind {
		case "Role":
			role = &docs[i]
		case "RoleBinding":
			binding = &docs[i]
		}
	}
	if role == nil || binding == nil {
		t.Fatalf("render has no Role and RoleBinding in %s", agentHostTestNamespace)
	}
	// Like the namespace, they outlive a change of host namespace, so the
	// controller can still clean up the hosts left in the old one.
	for _, doc := range []*workerRBACDocument{role, binding} {
		if got := doc.Metadata.Annotations["helm.sh/resource-policy"]; got != "keep" {
			t.Errorf("%s helm.sh/resource-policy = %q, want keep", doc.Kind, got)
		}
	}
	kustomize, err := os.ReadFile("../../../config/agent-hosts/rbac.yaml")
	if err != nil {
		t.Fatalf("read Kustomize agent-host RBAC: %v", err)
	}
	var want map[workerPermission]bool
	for _, doc := range decodeWorkerRBACDocuments(t, string(kustomize)) {
		if doc.Kind == "Role" {
			want = workerPermissions(doc.Rules)
		}
	}
	if got := workerPermissions(role.Rules); !reflect.DeepEqual(got, want) {
		t.Fatalf("chart Role permissions = %v, want the Kustomize Role's %v", got, want)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Metadata.Name {
		t.Fatalf("RoleBinding roleRef = %#v, want Role %s", binding.RoleRef, role.Metadata.Name)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
		binding.Subjects[0].Namespace != staticChartTestNamespace {
		t.Fatalf("RoleBinding subjects = %#v, want the controller ServiceAccount in %s",
			binding.Subjects, staticChartTestNamespace)
	}
}

func TestStaticChartAgentHostWebhookIsReleaseLocal(t *testing.T) {
	rendered := requireHelmRender(t, append(agentHostsEnabled(),
		"--show-only", "templates/controller-validating-webhook.yaml")...)
	var configuration admissionregistrationv1.ValidatingWebhookConfiguration
	if err := yaml.Unmarshal([]byte(rendered), &configuration); err != nil {
		t.Fatalf("decode controller validating webhook configuration: %v", err)
	}
	var webhook *admissionregistrationv1.ValidatingWebhook
	for i := range configuration.Webhooks {
		if configuration.Webhooks[i].Name == "agenthost.harness-v2.orka.ai" {
			webhook = &configuration.Webhooks[i]
		}
	}
	if webhook == nil {
		t.Fatalf("render has no AgentHost webhook:\n%s", rendered)
	}
	if webhook.ClientConfig.Service == nil ||
		webhook.ClientConfig.Service.Path == nil ||
		*webhook.ClientConfig.Service.Path != "/validate-core-orka-ai-v1alpha1-agenthost" {
		t.Fatalf("AgentHost webhook service = %#v", webhook.ClientConfig.Service)
	}
	if len(webhook.Rules) != 1 || !reflect.DeepEqual(webhook.Rules[0].Resources, []string{"agenthosts"}) ||
		!reflect.DeepEqual(webhook.Rules[0].Operations, []admissionregistrationv1.OperationType{
			admissionregistrationv1.Create, admissionregistrationv1.Update,
		}) {
		t.Fatalf("AgentHost webhook rules = %#v", webhook.Rules)
	}
	if webhook.NamespaceSelector == nil ||
		webhook.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != staticChartTestNamespace {
		t.Fatalf("AgentHost webhook namespace selector = %#v", webhook.NamespaceSelector)
	}
}

func TestStaticChartRejectsInvalidAgentHostValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "tagged agent image",
			args: []string{"--set-string", "controller.agentHosts.agentImage=registry.example.com/orka:1"},
			want: "controller.agentHosts.agentImage must be pinned by digest",
		},
		{
			name: "tagged controller image as the default",
			args: []string{"--set-string", "controller.image.digest="},
			want: "controller.image.digest must pin the controller image",
		},
		{
			name: "release namespace",
			args: []string{"--set-string", "controller.agentHosts.namespace=" + staticChartTestNamespace},
			want: "must differ from the Helm release namespace",
		},
		{
			name: "ACP runtime namespace",
			args: []string{"--set-string", "controller.agentHosts.namespace=orka-runtimes"},
			want: "must differ from the Helm release namespace and controller.acpRuntime.namespace",
		},
		{
			name: "empty namespace",
			args: []string{"--set-string", "controller.agentHosts.namespace="},
			want: "controller.agentHosts.namespace is required",
		},
		{
			name: "namespace that is no DNS label",
			args: []string{"--set-string", "controller.agentHosts.namespace=host_ns"},
			want: "must be a DNS-1123 label",
		},
		{
			name: "namespace too long",
			args: []string{"--set-string", "controller.agentHosts.namespace=" + strings.Repeat("a", 64)},
			want: "must be a DNS-1123 label",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append(agentHostsEnabled(), tt.args...)
			output, err := helmTemplateStaticChart(t, args...)
			if err == nil || !strings.Contains(output, tt.want) {
				t.Fatalf("helm render error = %v, want %q:\n%s", err, tt.want, output)
			}
		})
	}
}

// Helm and Kustomize enforce the same AgentHost admission, and only the
// controller whose identity the Deployment names may create a host fed by a
// Connection.
func TestStaticChartAgentHostPolicyMatchesSharedAdmission(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "policy", "workspace_class_use_policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const policyKind = "kind: ValidatingAdmissionPolicy\n"
	sharedDoc := requireRenderedDocument(t, string(shared), policyKind, "name: agenthost-template-use.orka.ai")
	chart := requireHelmRender(t, "--show-only", "templates/workspace-class-use-policy.yaml")
	chartDoc := requireRenderedDocument(t, chart, policyKind, "name: test-orka-agenthost-template-use")

	rendered := requireHelmRender(t, "--show-only", "templates/deployment.yaml")
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
		t.Fatal(err)
	}
	controller := ""
	for _, arg := range deployment.Spec.Template.Spec.Containers[0].Args {
		if value, ok := strings.CutPrefix(arg, "--execution-mode-controller-usernames="); ok {
			controller = value
		}
	}
	if controller == "" || !strings.Contains(chartDoc, "request.userInfo.username == '"+controller+"'") {
		t.Fatalf("the chart's AgentHost policy does not reserve Connection hosts to the controller %q", controller)
	}
	const kustomizeController = "system:serviceaccount:orka-system:orka-controller-manager"
	var sharedPolicy, chartPolicy admissionregistrationv1.ValidatingAdmissionPolicy
	sharedDoc = strings.ReplaceAll(sharedDoc, kustomizeController, controller)
	if err := yaml.Unmarshal([]byte(sharedDoc), &sharedPolicy); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(chartDoc), &chartPolicy); err != nil {
		t.Fatal(err)
	}
	// The chart's controller check names one release's controller, so its
	// policy covers only that release's namespace; with Kustomize there is
	// one controller, whose watch namespace can change.
	selector := chartPolicy.Spec.MatchConstraints.NamespaceSelector
	releaseOnly := map[string]string{"kubernetes.io/metadata.name": "orka-test"}
	if selector == nil || !reflect.DeepEqual(selector.MatchLabels, releaseOnly) || len(selector.MatchExpressions) != 0 {
		t.Fatalf("the chart's AgentHost policy must cover only the release namespace: %+v", selector)
	}
	chartPolicy.Spec.MatchConstraints.NamespaceSelector = nil
	if !reflect.DeepEqual(sharedPolicy.Spec, chartPolicy.Spec) {
		t.Fatal("Helm and Kustomize enforce different AgentHost admission")
	}
}
