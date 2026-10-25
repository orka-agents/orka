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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agenthost"
	"github.com/orka-agents/orka/internal/connectors"
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

func agentHostGitHubValues(extra ...string) []string {
	return append(append(agentHostsEnabled(),
		"--set", "controller.connectors.enabled=true",
		"--set", "controller.connectors.callbackBaseUrl=https://orka.example.com",
		"--set", "controller.agentHosts.github.enabled=true",
		"--set", "controller.agentHosts.github.clientID=Iv23example",
	), extra...)
}

// People's Connections outlive an uninstall and hold the provider, so Helm
// keeps the provider, its template and the client Secret the chart created.
func TestStaticChartAgentHostGitHubOutlivesAnUninstall(t *testing.T) {
	rendered := requireHelmRender(t, append(agentHostGitHubValues(
		"--set", "controller.agentHosts.github.clientSecret.value=not-a-real-secret"),
		"--show-only", "templates/agent-host-github.yaml")...)
	for _, marker := range []string{"kind: Secret\n", "kind: AgentHostTemplate\n", "kind: ConnectorProvider\n"} {
		var object metav1.PartialObjectMetadata
		if err := yaml.Unmarshal([]byte(requireRenderedDocument(t, rendered, marker)), &object); err != nil {
			t.Fatal(err)
		}
		if got := object.Annotations["helm.sh/resource-policy"]; got != "keep" {
			t.Fatalf("%s: resource-policy = %q, want keep", strings.TrimSpace(marker), got)
		}
	}
}

// Names YAML would read as numbers stay strings, as Kubernetes requires. The
// render is decoded untyped: decoding into the typed objects would convert
// them by the field's type, while the API server receives the YAML's.
func TestStaticChartAgentHostGitHubQuotesItsNames(t *testing.T) {
	rendered := requireHelmRender(t, append(agentHostGitHubValues(
		"--set", "controller.agentHosts.github.providerName=123",
		"--set", "controller.agentHosts.template.name=456",
		"--set", "controller.agentHosts.github.clientSecret.existingSecret=789"),
		"--show-only", "templates/agent-host-github.yaml")...)
	field := func(doc map[string]any, path ...string) any {
		var current any = doc
		for _, key := range path {
			next, ok := current.(map[string]any)
			if !ok {
				return nil
			}
			current = next[key]
		}
		return current
	}
	decode := func(marker string) map[string]any {
		t.Helper()
		doc := map[string]any{}
		if err := yaml.Unmarshal([]byte(requireRenderedDocument(t, rendered, marker)), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	provider, template := decode("kind: ConnectorProvider\n"), decode("kind: AgentHostTemplate\n")
	for name, got := range map[string]any{
		"provider name":                 field(provider, "metadata", "name"),
		"provider namespace":            field(provider, "metadata", "namespace"),
		"provider clientSecretRef name": field(provider, "spec", "oauth", "clientSecretRef", "name"),
		"provider templateRef name":     field(provider, "spec", "agentHost", "templateRef", "name"),
		"template name":                 field(template, "metadata", "name"),
	} {
		if _, ok := got.(string); !ok {
			t.Errorf("%s = %#v, want a string", name, got)
		}
	}
	if field(provider, "metadata", "name") != "123" || field(template, "metadata", "name") != "456" ||
		field(provider, "spec", "oauth", "clientSecretRef", "name") != "789" {
		t.Fatalf("names changed:\n%s", rendered)
	}
}

// One helm install gives people hosts from GitHub: the chart renders the
// provider and the copilotd template, which is the sample's, unchanged.
func TestStaticChartAgentHostGitHubRendersTheProviderAndTemplate(t *testing.T) {
	rendered := requireHelmRender(t, append(agentHostGitHubValues(
		"--set", "controller.agentHosts.github.clientSecret.value=not-a-real-secret"),
		"--show-only", "templates/agent-host-github.yaml")...)

	var provider corev1alpha1.ConnectorProvider
	providerDoc := requireRenderedDocument(t, rendered, "kind: ConnectorProvider\n")
	if err := yaml.Unmarshal([]byte(providerDoc), &provider); err != nil {
		t.Fatal(err)
	}
	if provider.Name != "github" || provider.Spec.AgentHost == nil ||
		provider.Spec.AgentHost.TemplateRef.Name != "copilotd" ||
		provider.Spec.OAuth.ClientID != "Iv23example" || provider.Spec.OAuth.ClientSecretRef.Name != "test-orka-github-app" ||
		provider.Spec.OAuth.TokenURL != "https://github.com/login/oauth/access_token" || len(provider.Spec.Tools) != 0 {
		t.Fatalf("provider = %+v", provider)
	}
	if issue := connectors.ValidateProviderSpec(&provider, nil); issue != nil {
		t.Fatalf("the rendered provider is invalid: %v", issue)
	}
	var secret corev1.Secret
	if err := yaml.Unmarshal([]byte(requireRenderedDocument(t, rendered, "kind: Secret\n")), &secret); err != nil {
		t.Fatal(err)
	}
	if secret.Name != "test-orka-github-app" || secret.StringData["clientSecret"] != "not-a-real-secret" {
		t.Fatalf("secret %s keys = %v", secret.Name, len(secret.StringData))
	}

	var template corev1alpha1.AgentHostTemplate
	templateDoc := requireRenderedDocument(t, rendered, "kind: AgentHostTemplate\n")
	if err := yaml.Unmarshal([]byte(templateDoc), &template); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "samples", "core_v1alpha1_agenthosttemplate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var sample corev1alpha1.AgentHostTemplate
	if err := yaml.Unmarshal(raw, &sample); err != nil {
		t.Fatal(err)
	}
	if template.Name != "copilotd" || !reflect.DeepEqual(template.Spec, sample.Spec) {
		t.Fatal("the chart's copilotd template differs from config/samples/core_v1alpha1_agenthosttemplate.yaml")
	}
	if template.Spec.Access != corev1alpha1.AgentHostAccessRelay {
		t.Fatal("hosts fed by a Connection need a Relay template")
	}

	// An existing Secret is named instead of rendered.
	existing := requireHelmRender(t, append(agentHostGitHubValues(
		"--set", "controller.agentHosts.github.clientSecret.existingSecret=github-app"),
		"--show-only", "templates/agent-host-github.yaml")...)
	if strings.Contains(existing, "kind: Secret\n") || !strings.Contains(existing, `name: "github-app"`) {
		t.Fatalf("existing Secret render:\n%s", existing)
	}

	// A Secret key YAML would read as another type stays a string. Decoding
	// into the typed objects would hide this: it converts by the field's type,
	// while the API server receives the YAML's.
	for _, source := range []string{"existingSecret=github-app", "value=not-a-real-secret"} {
		rendered := requireHelmRender(t, append(agentHostGitHubValues(
			"--set", "controller.agentHosts.github.clientSecret."+source,
			"--set", "controller.agentHosts.github.clientSecret.key=true"),
			"--show-only", "templates/agent-host-github.yaml")...)
		if !strings.Contains(requireRenderedDocument(t, rendered, "kind: ConnectorProvider\n"), `key: "true"`) {
			t.Fatalf("%s: the provider's clientSecretRef key is not a string:\n%s", source, rendered)
		}
		if source == "value=not-a-real-secret" &&
			!strings.Contains(requireRenderedDocument(t, rendered, "kind: Secret\n"), `"true": "not-a-real-secret"`) {
			t.Fatalf("the rendered Secret's key is not a string:\n%s", rendered)
		}
	}
}

func TestStaticChartAgentHostTemplateImagesMatchRuntimePolicy(t *testing.T) {
	args := append(agentHostGitHubValues(
		"--set", "controller.agentHosts.github.clientSecret.existingSecret=github-app"),
		"--show-only", "templates/agent-host-github.yaml")
	rendered := requireHelmRender(t, args...)
	var defaults corev1alpha1.AgentHostTemplate
	document := requireRenderedDocument(t, rendered, "kind: AgentHostTemplate\n")
	if err := yaml.Unmarshal([]byte(document), &defaults); err != nil {
		t.Fatal(err)
	}
	if err := agenthost.ValidateTemplate(&defaults); err != nil {
		t.Fatalf("the default template is invalid: %v", err)
	}

	const repository = "registry.example.com/agent-host"
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, field := range []string{"copilotdImage", "hostImage"} {
		t.Run(field, func(t *testing.T) {
			for _, tt := range []struct {
				name  string
				image string
				valid bool
			}{
				{"digest only", repository + "@" + digest, true},
				{"tag and digest", repository + ":v1@" + digest, true},
				{"tag only", repository + ":v1", false},
				{"no tag or digest", repository, false},
				{"empty", "", false},
				{"missing repository", "@" + digest, false},
				{"short digest", repository + "@sha256:" + strings.Repeat("a", 63), false},
				{"uppercase digest", repository + "@sha256:" + strings.Repeat("A", 64), false},
				{"wrong algorithm", repository + "@sha512:" + strings.Repeat("a", 128), false},
				{"leading whitespace", " " + repository + "@" + digest, false},
				{"trailing whitespace", repository + "@" + digest + " ", false},
			} {
				t.Run(tt.name, func(t *testing.T) {
					want := defaults.DeepCopy()
					if field == "copilotdImage" {
						want.Spec.InitContainers[0].Image = tt.image
					} else {
						want.Spec.Containers[0].Image = tt.image
					}
					if err := agenthost.ValidateTemplate(want); (err == nil) != tt.valid {
						t.Fatalf("runtime validation = %v, want valid=%t", err, tt.valid)
					}
					setting := "controller.agentHosts.template." + field
					output, err := helmTemplateStaticChart(t, append(args, "--set-string", setting+"="+tt.image)...)
					if !tt.valid {
						message := setting + " must be pinned by digest"
						if err == nil || !strings.Contains(output, message) {
							t.Fatalf("helm render error = %v, want %q:\n%s", err, message, output)
						}
						return
					}
					if err != nil {
						t.Fatalf("valid image override does not render: %v\n%s", err, output)
					}
					var got corev1alpha1.AgentHostTemplate
					document := requireRenderedDocument(t, output, "kind: AgentHostTemplate\n")
					if err := yaml.Unmarshal([]byte(document), &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got.Spec, want.Spec) {
						t.Fatal("the image override must change only the selected container image")
					}
				})
			}
		})
	}
}

func TestStaticChartAgentHostTemplateImagesIgnoredWhenDisabled(t *testing.T) {
	for _, enabled := range []string{"false", "true"} {
		t.Run("agent hosts enabled="+enabled, func(t *testing.T) {
			// Without the GitHub provider, no chart-managed template uses these images.
			rendered := requireHelmRender(t,
				"--set", "controller.agentHosts.enabled="+enabled,
				"--set-string", "controller.agentHosts.template.copilotdImage=registry.example.com/copilotd:latest",
				"--set-string", "controller.agentHosts.template.hostImage=registry.example.com/host:latest")
			if strings.Contains(rendered, "kind: AgentHostTemplate\n") {
				t.Fatal("a disabled GitHub provider must not render an agent-host template")
			}
		})
	}
}

func TestStaticChartAgentHostGitHubRefusesIncompleteSettings(t *testing.T) {
	for name, args := range map[string][]string{
		"no client secret": agentHostGitHubValues(),
		"both client secrets": agentHostGitHubValues(
			"--set", "controller.agentHosts.github.clientSecret.value=x",
			"--set", "controller.agentHosts.github.clientSecret.existingSecret=y"),
		"no client ID": agentHostGitHubValues(
			"--set", "controller.agentHosts.github.clientID=",
			"--set", "controller.agentHosts.github.clientSecret.value=x"),
		"no connectors": append(agentHostGitHubValues("--set", "controller.agentHosts.github.clientSecret.value=x"),
			"--set", "controller.connectors.enabled=false"),
		"an invalid Secret name": agentHostGitHubValues(
			"--set", "controller.agentHosts.github.clientSecret.existingSecret=Not_A_Name"),
		"an empty Secret key": agentHostGitHubValues("--set", "controller.agentHosts.github.clientSecret.value=x",
			"--set", "controller.agentHosts.github.clientSecret.key="),
		"an invalid provider name": agentHostGitHubValues("--set", "controller.agentHosts.github.clientSecret.value=x",
			"--set", "controller.agentHosts.github.providerName=GitHub_App"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := helmTemplateStaticChart(t, args...); err == nil {
				t.Fatal("the chart rendered an agent-host provider it cannot run")
			}
		})
	}
	rendered := requireHelmRender(t)
	if strings.Contains(rendered, "kind: ConnectorProvider") || strings.Contains(rendered, "kind: AgentHostTemplate") {
		t.Fatal("the default render must not create an agent-host provider or template")
	}
}

// With the Tailscale operator, the API is served on the tailnet.
func TestStaticChartIngressForTailscale(t *testing.T) {
	rendered := requireHelmRender(t, "--set", "ingress.enabled=true", "--set", "ingress.className=tailscale",
		"--set", "ingress.tls[0].hosts[0]=orka", "--show-only", "templates/ingress.yaml")
	var ingress networkingv1.Ingress
	if err := yaml.Unmarshal([]byte(rendered), &ingress); err != nil {
		t.Fatal(err)
	}
	backend := ingress.Spec.DefaultBackend
	if ingress.Spec.IngressClassName == nil || *ingress.Spec.IngressClassName != "tailscale" || backend == nil ||
		backend.Service.Name != "test-orka" || backend.Service.Port.Name != "api" ||
		len(ingress.Spec.TLS) != 1 || ingress.Spec.TLS[0].Hosts[0] != "orka" {
		t.Fatalf("ingress = %+v", ingress.Spec)
	}
	if _, err := helmTemplateStaticChart(t, "--show-only", "templates/ingress.yaml"); err == nil {
		t.Fatal("the default render must not create an Ingress")
	}
	// A class name YAML would read as a number or a boolean stays a string.
	for _, className := range []string{"123", "true"} {
		rendered := requireHelmRender(t, "--set", "ingress.enabled=true", "--set", "ingress.className="+className,
			"--show-only", "templates/ingress.yaml")
		doc := map[string]any{}
		if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
			t.Fatal(err)
		}
		if spec, _ := doc["spec"].(map[string]any); spec["ingressClassName"] != className {
			t.Fatalf("ingressClassName = %#v, want the string %q", spec["ingressClassName"], className)
		}
	}
}

// An upgrade that reuses an older release's values has no ingress map and no
// agent-host template or GitHub defaults; it renders as a fresh install would.
func TestStaticChartAgentHostGitHubRendersFromReusedValues(t *testing.T) {
	if _, err := helmTemplateStaticChart(t, "--set", "ingress=null"); err != nil {
		t.Fatalf("a release without an ingress map must render: %v", err)
	}
	rendered := requireHelmRender(t, append(agentHostGitHubValues(
		"--set", "controller.agentHosts.template=null",
		"--set", "controller.agentHosts.github.providerName=null",
		"--set", "controller.agentHosts.github.displayName=null",
		"--set", "controller.agentHosts.github.clientSecret.key=null",
		"--set", "controller.agentHosts.github.clientSecret.value=not-a-real-secret"),
		"--show-only", "templates/agent-host-github.yaml")...)
	var template corev1alpha1.AgentHostTemplate
	reusedTemplate := requireRenderedDocument(t, rendered, "kind: AgentHostTemplate\n")
	if err := yaml.Unmarshal([]byte(reusedTemplate), &template); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "samples", "core_v1alpha1_agenthosttemplate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var sample corev1alpha1.AgentHostTemplate
	if err := yaml.Unmarshal(raw, &sample); err != nil {
		t.Fatal(err)
	}
	if template.Name != "copilotd" || !reflect.DeepEqual(template.Spec, sample.Spec) {
		t.Fatal("without template values the chart must render the default copilotd template")
	}
	if err := agenthost.ValidateTemplate(&template); err != nil {
		t.Fatalf("the template from reused values is invalid: %v", err)
	}
	var provider corev1alpha1.ConnectorProvider
	reusedProvider := requireRenderedDocument(t, rendered, "kind: ConnectorProvider\n")
	if err := yaml.Unmarshal([]byte(reusedProvider), &provider); err != nil {
		t.Fatal(err)
	}
	if provider.Name != "github" || provider.Spec.DisplayName != "GitHub" ||
		provider.Spec.OAuth.ClientSecretRef.Key != "clientSecret" {
		t.Fatalf("provider = %+v", provider)
	}
}
