package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

const serviceAccountAutomountDisabledMarker = "automountServiceAccountToken: false"

func TestSCMEgressProxyStandaloneWorkloadHasNoKubernetesCredential(t *testing.T) {
	root := filepath.Join("..", "..", "..", "config", "scm-egress-proxy")
	deploymentManifest, err := os.ReadFile(filepath.Join(root, "deployment.yaml"))
	if err != nil {
		t.Fatalf("read SCM proxy Deployment: %v", err)
	}
	serviceAccountManifest, err := os.ReadFile(filepath.Join(root, "serviceaccount.yaml"))
	if err != nil {
		t.Fatalf("read SCM proxy ServiceAccount: %v", err)
	}

	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(deploymentManifest, &deployment); err != nil {
		t.Fatalf("decode SCM proxy Deployment: %v", err)
	}
	assertSCMProxyPodCredentialIsolation(t, deployment.Spec.Template.Spec)
	assertSCMProxyPodSecurityContext(t, deployment.Spec.Template.Spec)

	var serviceAccount corev1.ServiceAccount
	if err := yaml.Unmarshal(serviceAccountManifest, &serviceAccount); err != nil {
		t.Fatalf("decode SCM proxy ServiceAccount: %v", err)
	}
	if serviceAccount.AutomountServiceAccountToken == nil || *serviceAccount.AutomountServiceAccountToken {
		t.Fatal("SCM proxy ServiceAccount must disable token mounting")
	}
}

func assertSCMProxyPodCredentialIsolation(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("SCM proxy Pod must disable service-account token mounting")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Fatal("SCM proxy Pod must disable unnecessary service links")
	}
	for _, volume := range pod.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				t.Fatalf("SCM proxy Pod contains a projected Kubernetes credential volume %q", volume.Name)
			}
		}
	}
}

func assertSCMProxyPodSecurityContext(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil ||
		!*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.RunAsUser == nil ||
		*pod.SecurityContext.RunAsUser != 65532 || pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("SCM proxy Pod must run as the non-root distroless user with runtime-default seccomp")
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("SCM proxy container count = %d, want 1", len(pod.Containers))
	}
	container := pod.Containers[0]
	if container.SecurityContext == nil ||
		container.SecurityContext.AllowPrivilegeEscalation == nil ||
		*container.SecurityContext.AllowPrivilegeEscalation ||
		container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("SCM proxy container must forbid privilege escalation and use a read-only root filesystem")
	}
	if container.SecurityContext.Capabilities == nil ||
		len(container.SecurityContext.Capabilities.Drop) != 1 ||
		container.SecurityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("SCM proxy dropped capabilities = %v, want [ALL]", container.SecurityContext.Capabilities)
	}
}

func TestSCMEgressProxyStandaloneNetworkPolicyRetainsDirectPrivateAddressDefense(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "..", "config", "scm-egress-proxy", "networkpolicy.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read SCM proxy NetworkPolicy: %v", err)
	}
	var policy networkingv1.NetworkPolicy
	if err := yaml.Unmarshal(manifest, &policy); err != nil {
		t.Fatalf("decode SCM proxy NetworkPolicy: %v", err)
	}

	for _, test := range []struct {
		name     string
		cidr     string
		required []string
	}{
		{
			name: "IPv4",
			cidr: "0.0.0.0/0",
			required: []string{
				"10.0.0.0/8",
				"127.0.0.0/8",
				"169.254.0.0/16",
				"172.16.0.0/12",
				"192.88.99.0/24",
				"192.168.0.0/16",
			},
		},
		{
			name: "IPv6",
			cidr: "::/0",
			required: []string{
				"100:0:0:1::/64",
				"2001:2::/48",
				"2001:10::/28",
				"2001:20::/28",
				"3fff::/20",
				"5f00::/16",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, rule := range policy.Spec.Egress {
				for _, peer := range rule.To {
					if peer.IPBlock == nil || peer.IPBlock.CIDR != test.cidr {
						continue
					}
					exclusions := make(map[string]bool, len(peer.IPBlock.Except))
					for _, exclusion := range peer.IPBlock.Except {
						exclusions[exclusion] = true
					}
					for _, required := range test.required {
						if !exclusions[required] {
							t.Errorf("SCM proxy %s egress exclusions are missing %s", test.name, required)
						}
					}
					return
				}
			}
			t.Fatalf("SCM proxy NetworkPolicy is missing its public %s egress block", test.name)
		})
	}
}

func TestStaticChartSCMEgressProxyNetworkPolicyRejectsSpecialUseRanges(t *testing.T) {
	rendered := requireHelmRender(t, "--show-only", "templates/scm-egress-proxy-networkpolicy.yaml")
	for _, required := range []string{
		"192.88.99.0/24",
		"100:0:0:1::/64",
		"2001:2::/48",
		"2001:10::/28",
		"2001:20::/28",
		"3fff::/20",
		"5f00::/16",
	} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("SCM proxy chart NetworkPolicy is missing special-use exclusion %q:\n%s", required, rendered)
		}
	}
}

func TestStaticChartSCMEgressProxyWorkloadHasNoKubernetesCredential(t *testing.T) {
	rendered := requireHelmRender(t, "--show-only", "templates/scm-egress-proxy-deployment.yaml")
	for _, required := range []string{
		serviceAccountAutomountDisabledMarker,
		"enableServiceLinks: false",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"capabilities: {drop: [ALL]}",
	} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("SCM proxy Deployment is missing hardening marker %q:\n%s", required, rendered)
		}
	}
	for _, forbidden := range []string{
		"serviceAccountToken:",
		"/var/run/secrets/kubernetes.io/serviceaccount",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("SCM proxy Deployment contains Kubernetes credential surface %q:\n%s", forbidden, rendered)
		}
	}
	serviceAccount := requireHelmRender(t, "--show-only", "templates/scm-egress-proxy-serviceaccount.yaml")
	if !strings.Contains(serviceAccount, serviceAccountAutomountDisabledMarker) {
		t.Fatalf("SCM proxy ServiceAccount permits token mounting:\n%s", serviceAccount)
	}
}
