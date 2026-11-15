package main

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestStaticChartMountsRotatableSubstrateCredentials(t *testing.T) {
	for _, tc := range []struct {
		auth    string
		enabled bool
	}{
		{"mtls", true},
		{"mtls", false},
		// Bearer control credentials remain valid for cleanup while MCP Tools
		// are disabled; enabled Tools also need the router's client certificate.
		{"bearer", false},
	} {
		t.Run(tc.auth+"/enabled="+strconv.FormatBool(tc.enabled), func(t *testing.T) {
			args := []string{
				"--set", "controller.substrate.mcpToolsEnabled=" + strconv.FormatBool(tc.enabled),
				"--set-string", "controller.substrate.apiCredentials.existingSecret=substrate-control",
				"--set-string", "controller.substrate.apiCredentials.caKey=server-ca",
				"--show-only", "templates/deployment.yaml",
			}
			wantKeys := map[string]string{"server-ca": "ca.crt"}
			wantArgs := []string{"--substrate-api-ca-file=/var/run/orka/substrate-api/ca.crt"}
			if tc.auth == "mtls" {
				args = append(args, "--set-string", "controller.substrate.apiCredentials.certKey=identity",
					"--set-string", "controller.substrate.apiCredentials.privateKeyKey=private-key")
				wantKeys["identity"], wantKeys["private-key"] = "client.crt", "client.key"
				wantArgs = append(wantArgs, "--substrate-api-cert-file=/var/run/orka/substrate-api/client.crt",
					"--substrate-api-key-file=/var/run/orka/substrate-api/client.key")
			} else {
				args = append(args, "--set-string", "controller.substrate.apiCredentials.bearerTokenKey=access-token")
				wantKeys["access-token"] = "bearer-token"
				wantArgs = append(wantArgs, "--substrate-api-bearer-token-file=/var/run/orka/substrate-api/bearer-token")
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal([]byte(requireHelmRender(t, args...)), &deployment); err != nil {
				t.Fatal(err)
			}
			container := deployment.Spec.Template.Spec.Containers[0]
			for _, arg := range wantArgs {
				if !slices.Contains(container.Args, arg) {
					t.Errorf("controller is missing %s", arg)
				}
			}
			if slices.Contains(container.Args, "--substrate-mcp-tools-enabled=true") != tc.enabled {
				t.Fatal("retaining cleanup credentials changed Substrate admission")
			}
			mountIndex := slices.IndexFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
				return m.Name == "substrate-api-credentials"
			})
			if mountIndex < 0 {
				t.Fatal("controller has no Substrate credential mount")
			}
			mount := container.VolumeMounts[mountIndex]
			if mount.MountPath != "/var/run/orka/substrate-api" || !mount.ReadOnly || mount.SubPath != "" {
				t.Fatal("credential mount is writable or prevents projected Secret rotation")
			}
			volumeIndex := slices.IndexFunc(deployment.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
				return v.Name == mount.Name
			})
			if volumeIndex < 0 {
				t.Fatal("credential mount has no volume")
			}
			secret := deployment.Spec.Template.Spec.Volumes[volumeIndex].Secret
			if secret == nil || secret.SecretName != "substrate-control" || len(secret.Items) != len(wantKeys) ||
				secret.DefaultMode == nil || *secret.DefaultMode != 0400 {
				t.Fatal("controller does not project exactly the configured credential keys")
			}
			for _, item := range secret.Items {
				if wantKeys[item.Key] != item.Path {
					t.Errorf("credential key %s has unexpected mounted path %s", item.Key, item.Path)
				}
			}
		})
	}
}

func TestStaticChartRejectsIncompleteSubstrateCredentials(t *testing.T) {
	for _, selection := range []string{
		"",
		"apiCredentials.existingSecret=client,apiCredentials.certKey=cert",
		"apiCredentials.bearerTokenKey=token",
		"apiCredentials.existingSecret=client,apiCredentials.certKey=cert," +
			"apiCredentials.privateKeyKey=key,apiCredentials.bearerTokenKey=token",
		"apiCredentials.existingSecret=client,apiCredentials.bearerTokenKey=token,apiBearerTokenFile=/custom/token",
	} {
		t.Run(selection, func(t *testing.T) {
			args := []string{"--set", "controller.substrate.mcpToolsEnabled=true", "--show-only", "templates/deployment.yaml"}
			for value := range strings.SplitSeq(selection, ",") {
				if value != "" {
					args = append(args, "--set-string", "controller.substrate."+value)
				}
			}
			output, err := helmTemplateStaticChart(t, args...)
			if err == nil || !strings.Contains(output, "controller.substrate") {
				t.Fatal("Helm accepted incomplete or conflicting Substrate authentication")
			}
		})
	}
}

func TestStaticChartRequiresSubstrateServerTrust(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selection  string
		caSetting  string
		caArgument string
	}{
		{"Secret mTLS", "apiCredentials.existingSecret=client,apiCredentials.certKey=cert,apiCredentials.privateKeyKey=key",
			"apiCredentials.caKey=server-ca", "--substrate-api-ca-file=/var/run/orka/substrate-api/ca.crt"},
		{"Secret bearer", "apiCredentials.existingSecret=client,apiCredentials.bearerTokenKey=token",
			"apiCredentials.caKey=server-ca", "--substrate-api-ca-file=/var/run/orka/substrate-api/ca.crt"},
		{"mounted mTLS", "apiCertFile=/custom/client.crt,apiKeyFile=/custom/client.key",
			"apiCAFile=/custom/ca.crt", "--substrate-api-ca-file=/custom/ca.crt"},
		{"mounted bearer", "apiBearerTokenFile=/custom/token",
			"apiCAFile=/custom/ca.crt", "--substrate-api-ca-file=/custom/ca.crt"},
	} {
		for _, enabled := range []bool{true, false} {
			if enabled && strings.Contains(tc.name, "bearer") {
				continue // covered by TestStaticChartMCPToolsRequireRouterClientCertificate
			}
			t.Run(tc.name+" enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				args := []string{"--set", "controller.substrate.mcpToolsEnabled=" + strconv.FormatBool(enabled),
					"--show-only", "templates/deployment.yaml"}
				for value := range strings.SplitSeq(tc.selection, ",") {
					args = append(args, "--set-string", "controller.substrate."+value)
				}
				output, err := helmTemplateStaticChart(t, args...)
				if err == nil || !strings.Contains(output, "required for verified Substrate API TLS") {
					t.Fatal("Helm accepted Substrate authentication without server trust")
				}
				for _, trust := range []struct{ flag, value, argument string }{
					{"--set-string", tc.caSetting, tc.caArgument},
					{"--set", "apiInsecureSkipVerify=true", "--substrate-api-insecure-skip-verify=true"},
				} {
					var deployment appsv1.Deployment
					rendered := requireHelmRender(t, append(slices.Clone(args), trust.flag, "controller.substrate."+trust.value)...)
					if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
						t.Fatal(err)
					}
					if !slices.Contains(deployment.Spec.Template.Spec.Containers[0].Args, trust.argument) {
						t.Fatalf("controller is missing selected trust argument %s", trust.argument)
					}
				}
			})
		}
	}
}

func TestStaticChartExternalWorkerPermissionsAreReadOnly(t *testing.T) {
	rendered := requireHelmRender(t, "--set", "controller.substrate.mcpToolsEnabled=false",
		"--set-string", "controller.executionWorkspace.workerNamespaces[0]=ate-workers",
		"--show-only", "templates/external-runtime-worker-rbac.yaml")
	var role rbacv1.Role
	if err := yaml.Unmarshal([]byte(requireRenderedDocument(t, rendered, "kind: Role\n")), &role); err != nil {
		t.Fatal(err)
	}
	if role.Namespace != "ate-workers" {
		t.Fatal("Substrate worker authority escaped the selected provider namespace")
	}
	for _, verb := range []string{"get", "list", "watch"} {
		if !testSubstrateRuleAllows(role.Rules, "", "pods", verb) {
			t.Errorf("worker cleanup lacks Pod %s permission", verb)
		}
	}
	for _, verb := range []string{"create", "update", "patch", "delete"} {
		if testSubstrateRuleAllows(role.Rules, "", "pods", verb) {
			t.Errorf("worker observation permits compute mutation: %s", verb)
		}
	}
	for _, verb := range []string{"get", "list", "create", "patch", "delete"} {
		if testSubstrateRuleAllows(role.Rules, "networking.k8s.io", "networkpolicies", verb) {
			t.Errorf("worker reader can mutate or inspect network policies: %s", verb)
		}
	}
	for _, resource := range []string{"secrets", "pods/exec", "serviceaccounts/token"} {
		for _, verb := range []string{"get", "list", "create", "delete"} {
			if testSubstrateRuleAllows(role.Rules, "", resource, verb) {
				t.Errorf("worker role unexpectedly grants %s on %s", verb, resource)
			}
		}
	}
	var binding rbacv1.RoleBinding
	if err := yaml.Unmarshal([]byte(requireRenderedDocument(t, rendered, "kind: RoleBinding\n")), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Namespace != role.Namespace || binding.RoleRef.Name != role.Name || binding.RoleRef.Kind != "Role" ||
		len(binding.Subjects) != 1 || binding.Subjects[0].Namespace != staticChartTestNamespace {
		t.Fatal("worker role is not bound to the release controller")
	}
}

func TestStaticChartMCPToolsRequireRouterClientCertificate(t *testing.T) {
	for name, selection := range map[string][]string{
		"Secret bearer": {
			"apiCredentials.existingSecret=client", "apiCredentials.bearerTokenKey=token", "apiCredentials.caKey=ca",
		},
		"mounted bearer": {"apiBearerTokenFile=/custom/token", "apiCAFile=/custom/ca.crt"},
	} {
		t.Run(name, func(t *testing.T) {
			args := make([]string, 0, 4+2*len(selection))
			args = append(args, "--set", "controller.substrate.mcpToolsEnabled=true", "--show-only", "templates/deployment.yaml")
			for _, value := range selection {
				args = append(args, "--set-string", "controller.substrate."+value)
			}
			output, err := helmTemplateStaticChart(t, args...)
			if err == nil || !strings.Contains(output, "pod-certificate client identity") {
				t.Fatal("Helm enabled MCP Tools without a router client certificate")
			}
		})
	}
}

func TestStaticChartProjectsSubstratePodCertificate(t *testing.T) {
	var deployment appsv1.Deployment
	rendered := requireHelmRender(t, "--set", "controller.substrate.mcpToolsEnabled=true",
		"--set", "controller.substrate.podCertificate.enabled=true", "--show-only", "templates/deployment.yaml")
	if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	for _, arg := range []string{
		"--substrate-api-cert-file=/run/substrate-client/credential-bundle.pem",
		"--substrate-api-key-file=/run/substrate-client/credential-bundle.pem",
		"--substrate-api-ca-file=/run/substrate-server/trust-bundle.pem",
		"--substrate-router-url=https://atenet-router.ate-system.svc:443",
	} {
		if !slices.Contains(container.Args, arg) {
			t.Errorf("controller is missing %s", arg)
		}
	}
	for _, arg := range container.Args {
		if strings.Contains(arg, "actor-dns-suffix") || strings.Contains(arg, "session-identity") {
			t.Errorf("controller renders retired Substrate v0.1.0 flag %s", arg)
		}
	}
	volumes := map[string]corev1.Volume{}
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	client := volumes["substrate-client"].Projected
	if client == nil || len(client.Sources) != 1 || client.Sources[0].PodCertificate == nil ||
		client.Sources[0].PodCertificate.SignerName != "podidentity.podcert.ate.dev/identity" {
		t.Fatal("controller does not project the Substrate pod identity")
	}
	server := volumes["substrate-server"].Projected
	if server == nil || len(server.Sources) != 1 || server.Sources[0].ClusterTrustBundle == nil ||
		server.Sources[0].ClusterTrustBundle.SignerName == nil ||
		*server.Sources[0].ClusterTrustBundle.SignerName != "servicedns.podcert.ate.dev/identity" {
		t.Fatal("controller does not project the Substrate service trust bundle")
	}
	for _, mount := range container.VolumeMounts {
		if (mount.Name == "substrate-client" || mount.Name == "substrate-server") && !mount.ReadOnly {
			t.Fatalf("%s mount is writable", mount.Name)
		}
	}

	output, err := helmTemplateStaticChart(t, "--set", "controller.substrate.podCertificate.enabled=true",
		"--set-string", "controller.substrate.apiBearerTokenFile=/custom/token", "--show-only", "templates/deployment.yaml")
	if err == nil || !strings.Contains(output, "cannot be combined") {
		t.Fatal("Helm combined the pod certificate with another control identity")
	}
}

func TestStaticChartAdmitsSubstrateEgressGatewayOnlyWhenEnabled(t *testing.T) {
	base := []string{"--set", "providerProxy.enabled=true",
		"--set-string", "providerProxy.upstreamBaseURL=https://gateway.example.test",
		"--set-json",
		`providerProxy.egress=[{"to":[{"ipBlock":{"cidr":"203.0.113.0/24"}}],` +
			`"ports":[{"protocol":"TCP","port":443}]}]`,
		"--show-only", "templates/provider-proxy-networkpolicy.yaml"}
	if rendered := requireHelmRender(t, base...); strings.Contains(rendered, "atenet-egress") {
		t.Fatal("provider proxy admits the Substrate egress gateway by default")
	}
	enabled := append(slices.Clone(base), "--set", "providerProxy.substrateEgressGateway.enabled=true")
	rendered := requireHelmRender(t, enabled...)
	for _, want := range []string{"kubernetes.io/metadata.name: \"ate-system\"", "app: atenet-egress"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("provider proxy ingress is missing %s:\n%s", want, rendered)
		}
	}
}
