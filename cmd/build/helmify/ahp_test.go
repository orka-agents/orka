package main

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const ahpTestWorkspaceNamespace = "orka-ahp-workspaces"

var ahpTestWorkspaceImage = "registry.example.com/ahp-workspace@sha256:" + strings.Repeat("b", 64)

func ahpEnabled(args ...string) []string {
	return append([]string{
		"--set", "controller.ahp.enabled=true",
		"--set-string", "controller.oidc.issuer=https://issuer.example.com",
		"--set-string", "controller.oidc.audience=orka",
		"--set-string", "controller.oidc.allowedSubjects=*",
	}, args...)
}

func ahpWorkspacesEnabled(args ...string) []string {
	withImage := make([]string, 0, 2+len(args))
	withImage = append(withImage, "--set-string", "controller.ahp.workspaceImage="+ahpTestWorkspaceImage)
	return ahpEnabled(append(withImage, args...)...)
}

func ahpControllerArgs(t *testing.T, args ...string) []string {
	t.Helper()
	rendered := requireHelmRender(t, append(args, "--show-only", "templates/deployment.yaml")...)
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
		t.Fatalf("decode controller Deployment: %v", err)
	}
	return deployment.Spec.Template.Spec.Containers[0].Args
}

func TestStaticChartAHPIsOffByDefault(t *testing.T) {
	rendered := requireHelmRender(t)
	for _, unwanted := range []string{
		"--enable-ahp-api", "--ahp-", ahpTestWorkspaceNamespace,
	} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("default render contains %q", unwanted)
		}
	}
}

func TestStaticChartAHPControllerFlags(t *testing.T) {
	args := ahpControllerArgs(t, ahpEnabled(
		"--set-string", "controller.ahp.gitCredentialSecret=git-token",
		"--set-string", "controller.ahp.workspaceClass=warm",
		"--set-string", "controller.ahp.turnTimeout=30m",
		"--set-string", "controller.ahp.tokenMaxTTL=720h",
	)...)
	for _, want := range []string{
		"--enable-ahp-api=true",
		"--ahp-token-max-ttl=720h",
		"--ahp-turn-timeout=30m",
		"--ahp-git-credential-secret=git-token",
		"--ahp-workspace-class=warm",
	} {
		if !containsString(args, want) {
			t.Errorf("controller args are missing %s:\n%s", want, strings.Join(args, "\n"))
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--ahp-workspace-image") || strings.HasPrefix(arg, "--ahp-workspace-namespace") {
			t.Errorf("workspaces are off without an image, but the controller gets %s", arg)
		}
	}
	rendered := requireHelmRender(t, ahpEnabled()...)
	if strings.Contains(rendered, "name: "+ahpTestWorkspaceNamespace) {
		t.Fatal("the workspace namespace is rendered without a workspace image")
	}
}

func TestStaticChartAHPWorkspaceFlags(t *testing.T) {
	args := ahpControllerArgs(t, ahpWorkspacesEnabled()...)
	for _, want := range []string{
		"--ahp-workspace-namespace=" + ahpTestWorkspaceNamespace,
		"--ahp-workspace-image=" + ahpTestWorkspaceImage,
		"--ahp-workspace-idle-timeout=4h",
	} {
		if !containsString(args, want) {
			t.Errorf("controller args are missing %s:\n%s", want, strings.Join(args, "\n"))
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--ahp-workspace-connector-provider") {
			t.Errorf("private repository reads are off by default, but the controller gets %s", arg)
		}
	}
	args = ahpControllerArgs(t, ahpWorkspacesEnabled(
		"--set", "controller.connectors.enabled=true",
		"--set-string", "controller.connectors.callbackBaseUrl=https://orka.example.com",
		"--set-string", "controller.ahp.workspaceConnectorProvider=github",
		"--set-string", "controller.ahp.workspaceIdleTimeout=1h",
	)...)
	for _, want := range []string{"--ahp-workspace-connector-provider=github", "--ahp-workspace-idle-timeout=1h"} {
		if !containsString(args, want) {
			t.Errorf("controller args are missing %s:\n%s", want, strings.Join(args, "\n"))
		}
	}
	withoutConnectors := ahpWorkspacesEnabled("--set-string", "controller.ahp.workspaceConnectorProvider=github")
	if output, err := helmTemplateStaticChart(t, withoutConnectors...); err == nil ||
		!strings.Contains(output, "requires controller.connectors.enabled") {
		t.Fatalf("a connector provider without connectors rendered: %v\n%s", err, output)
	}
}

// ahpWorkspaceObjects decodes the workspace template into its objects by kind.
func ahpWorkspaceObjects(t *testing.T, rendered string) map[string][]byte {
	t.Helper()
	objects := map[string][]byte{}
	decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered), 4096)
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err == io.EOF {
			return objects
		} else if err != nil {
			t.Fatalf("decode AHP workspace objects: %v", err)
		}
		kind, _ := object["kind"].(string)
		if kind == "" {
			continue
		}
		if _, seen := objects[kind]; seen {
			t.Fatalf("AHP workspace template renders more than one %s", kind)
		}
		raw, err := yaml.Marshal(object)
		if err != nil {
			t.Fatalf("encode %s: %v", kind, err)
		}
		objects[kind] = raw
	}
}

func TestStaticChartAHPWorkspaceNamespaceRBACAndPolicy(t *testing.T) {
	rendered := requireHelmRender(t, append(ahpWorkspacesEnabled(), "--show-only", "templates/ahp-workspaces.yaml")...)
	objects := ahpWorkspaceObjects(t, rendered)

	var namespace corev1.Namespace
	if err := yaml.Unmarshal(objects["Namespace"], &namespace); err != nil {
		t.Fatalf("decode workspace Namespace: %v", err)
	}
	if namespace.Name != ahpTestWorkspaceNamespace {
		t.Fatalf("workspace namespace = %q, want %s", namespace.Name, ahpTestWorkspaceNamespace)
	}
	for _, mode := range []string{"enforce", "audit", "warn"} {
		if got := namespace.Labels["pod-security.kubernetes.io/"+mode]; got != "restricted" {
			t.Errorf("pod-security %s = %q, want restricted", mode, got)
		}
	}
	if _, kept := namespace.Annotations["helm.sh/resource-policy"]; kept {
		t.Error("workspace files are ephemeral; the namespace must go with the release")
	}

	docs := decodeWorkerRBACDocuments(t, string(objects["Role"])+"\n---\n"+string(objects["RoleBinding"]))
	kustomize, err := os.ReadFile("../../../config/ahp-workspaces/rbac.yaml")
	if err != nil {
		t.Fatalf("read Kustomize AHP workspace RBAC: %v", err)
	}
	var want map[workerPermission]bool
	for _, doc := range decodeWorkerRBACDocuments(t, string(kustomize)) {
		if doc.Kind == "Role" {
			want = workerPermissions(doc.Rules)
		}
	}
	role, binding := docs[0], docs[1]
	if role.Metadata.Namespace != ahpTestWorkspaceNamespace || binding.Metadata.Namespace != ahpTestWorkspaceNamespace {
		t.Fatalf("Role and RoleBinding namespaces = %q, %q", role.Metadata.Namespace, binding.Metadata.Namespace)
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

	var policy, kustomizePolicy networkingv1.NetworkPolicy
	if err := yaml.Unmarshal(objects["NetworkPolicy"], &policy); err != nil {
		t.Fatalf("decode workspace NetworkPolicy: %v", err)
	}
	raw, err := os.ReadFile("../../../config/ahp-workspaces/networkpolicy.yaml")
	if err != nil {
		t.Fatalf("read Kustomize AHP workspace NetworkPolicy: %v", err)
	}
	if err := yaml.Unmarshal(raw, &kustomizePolicy); err != nil {
		t.Fatalf("decode Kustomize AHP workspace NetworkPolicy: %v", err)
	}
	if policy.Namespace != ahpTestWorkspaceNamespace || !reflect.DeepEqual(policy.Spec, kustomizePolicy.Spec) {
		t.Fatalf("chart NetworkPolicy %s spec = %#v, want the Kustomize policy's %#v",
			policy.Namespace, policy.Spec, kustomizePolicy.Spec)
	}
	if len(policy.Spec.Ingress) != 0 {
		t.Fatal("workspaces must accept no inbound traffic")
	}
}

func TestStaticChartRejectsInvalidAHPValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "tagged workspace image",
			args: ahpEnabled("--set-string", "controller.ahp.workspaceImage=registry.example.com/ahp-workspace:1"),
			want: "controller.ahp.workspaceImage must be pinned by digest",
		},
		{
			name: "no identity that can mint tokens",
			args: []string{"--set", "controller.ahp.enabled=true"},
			want: "controller.ahp.enabled requires controller.oidc.issuer and controller.oidc.audience",
		},
		{
			name: "release namespace",
			args: ahpWorkspacesEnabled("--set-string", "controller.ahp.workspaceNamespace="+staticChartTestNamespace),
			want: "must differ from the Helm release namespace",
		},
		{
			name: "ACP runtime namespace",
			args: ahpWorkspacesEnabled("--set-string", "controller.ahp.workspaceNamespace=orka-runtimes"),
			want: "must differ from the Helm release namespace, controller.watchNamespace and controller.acpRuntime.namespace",
		},
		{
			name: "empty namespace",
			args: ahpWorkspacesEnabled("--set-string", "controller.ahp.workspaceNamespace="),
			want: "controller.ahp.workspaceNamespace is required",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, tt.args...)
			if err == nil || !strings.Contains(output, tt.want) {
				t.Fatalf("helm render error = %v, want %q:\n%s", err, tt.want, output)
			}
		})
	}
}
