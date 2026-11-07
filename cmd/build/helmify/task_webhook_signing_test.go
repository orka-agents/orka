package main

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const taskWebhookSigningTestNamespace = "orka-task-webhook-signing"

func renderedControllerDeployment(t *testing.T, args ...string) appsv1.Deployment {
	t.Helper()
	rendered := requireHelmRender(t, append(args, "--show-only", "templates/deployment.yaml")...)
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
		t.Fatalf("decode controller Deployment: %v", err)
	}
	return deployment
}

func TestStaticChartTaskWebhookSigningKeyLivesInItsOwnNamespace(t *testing.T) {
	deployment := renderedControllerDeployment(t)
	args := deployment.Spec.Template.Spec.Containers[0].Args
	for _, want := range []string{
		"--task-webhook-signing-secret=test-orka-task-webhook-signing",
		"--task-webhook-signing-namespace=" + taskWebhookSigningTestNamespace,
	} {
		if !containsString(args, want) {
			t.Errorf("controller args are missing %s:\n%s", want, strings.Join(args, "\n"))
		}
	}

	rendered := requireHelmRender(t, "--show-only", "templates/task-webhook-signing-namespace.yaml")
	var namespace corev1.Namespace
	if err := yaml.Unmarshal([]byte(rendered), &namespace); err != nil {
		t.Fatalf("decode signing Namespace: %v", err)
	}
	if namespace.Name != taskWebhookSigningTestNamespace {
		t.Fatalf("signing namespace = %q, want %s", namespace.Name, taskWebhookSigningTestNamespace)
	}
	if got := namespace.Annotations["helm.sh/resource-policy"]; got != "keep" {
		t.Errorf("signing namespace helm.sh/resource-policy = %q, want keep so the key outlives an uninstall", got)
	}
	for _, mode := range []string{"enforce", "audit", "warn"} {
		if got := namespace.Labels["pod-security.kubernetes.io/"+mode]; got != "restricted" {
			t.Errorf("pod-security %s = %q, want restricted", mode, got)
		}
	}
}

// Only the controller may touch Secrets in the signing namespace, whatever the
// AI worker profile: no worker, client or proxy binding reaches the key.
func TestStaticChartOnlyTheControllerReachesTheTaskWebhookSigningKey(t *testing.T) {
	for _, profile := range []string{"default", "restricted"} {
		t.Run(profile, func(t *testing.T) {
			values := []string{"--set-string", "workers.ai.rbacProfile=" + profile}
			controllerSA := renderedControllerDeployment(t, values...).Spec.Template.Spec.ServiceAccountName
			docs := decodeWorkerRBACDocuments(t, requireHelmRender(t, append(values, "--show-only", "templates/rbac.yaml")...))
			roles := map[string]workerRBACDocument{}
			for _, doc := range docs {
				if doc.Kind == "Role" || doc.Kind == "ClusterRole" {
					roles[doc.Kind+"/"+doc.Metadata.Namespace+"/"+doc.Metadata.Name] = doc
				}
			}
			var controllerBound bool
			for _, binding := range docs {
				inSigningNamespace := binding.Kind == "RoleBinding" && binding.Metadata.Namespace == taskWebhookSigningTestNamespace
				if !inSigningNamespace && binding.Kind != "ClusterRoleBinding" {
					continue
				}
				roleNamespace := ""
				if binding.RoleRef.Kind == "Role" {
					roleNamespace = binding.Metadata.Namespace
				}
				role, found := roles[binding.RoleRef.Kind+"/"+roleNamespace+"/"+binding.RoleRef.Name]
				if !found {
					t.Fatalf("%s %s binds %s %s, which the chart does not render", binding.Kind, binding.Metadata.Name, binding.RoleRef.Kind, binding.RoleRef.Name)
				}
				if !grantsSecrets(role) {
					continue
				}
				for _, subject := range binding.Subjects {
					if subject.Kind != "ServiceAccount" || subject.Name != controllerSA || subject.Namespace != staticChartTestNamespace {
						t.Errorf("%s %s grants Secrets in the signing namespace to %#v", binding.Kind, binding.Metadata.Name, subject)
					}
				}
				if binding.Kind == "RoleBinding" {
					controllerBound = true
					want := map[workerPermission]bool{
						{resource: "secrets", resourceName: "test-orka-task-webhook-signing", verb: "get"}:    true,
						{resource: "secrets", resourceName: "test-orka-task-webhook-signing", verb: "update"}: true,
						{resource: "secrets", verb: "create"}:                                                 true,
					}
					if got := workerPermissions(role.Rules); len(got) != len(want) {
						t.Errorf("signing Role permissions = %v, want %v", got, want)
					} else {
						for permission := range want {
							if !got[permission] {
								t.Errorf("signing Role permissions = %v, want %v", got, want)
							}
						}
					}
				}
			}
			if !controllerBound {
				t.Fatalf("no RoleBinding grants the controller its signing Secret in %s", taskWebhookSigningTestNamespace)
			}
		})
	}
}

func grantsSecrets(role workerRBACDocument) bool {
	for permission := range workerPermissions(role.Rules) {
		if permission.group == "" && (permission.resource == "secrets" || permission.resource == "*") {
			return true
		}
	}
	return false
}

func TestStaticChartTaskWebhookSigningRendersFromReusedValues(t *testing.T) {
	args := renderedControllerDeployment(t, "--set", "controller.taskWebhooks=null").Spec.Template.Spec.Containers[0].Args
	if !containsString(args, "--task-webhook-signing-namespace="+taskWebhookSigningTestNamespace) {
		t.Fatalf("a release without controller.taskWebhooks must default the signing namespace:\n%s", strings.Join(args, "\n"))
	}
}

func TestStaticChartRejectsTaskWebhookSigningBesideTasks(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "release namespace",
			args: []string{"--set-string", "controller.taskWebhooks.signingNamespace=" + staticChartTestNamespace},
			want: "must differ from the Helm release, client, ACP runtime and agent-host namespaces",
		},
		{
			name: "client namespace",
			args: []string{
				"--set-string", "client.namespace=orka-clients",
				"--set-string", "controller.taskWebhooks.signingNamespace=orka-clients",
			},
			want: "must differ from the Helm release, client, ACP runtime and agent-host namespaces",
		},
		{
			name: "ACP runtime namespace",
			args: []string{"--set-string", "controller.taskWebhooks.signingNamespace=orka-runtimes"},
			want: "must differ from the Helm release, client, ACP runtime and agent-host namespaces",
		},
		{
			name: "agent-host namespace",
			args: append(agentHostsEnabled(), "--set-string", "controller.taskWebhooks.signingNamespace="+agentHostTestNamespace),
			want: "must differ from the Helm release, client, ACP runtime and agent-host namespaces",
		},
		{
			name: "namespace that is no DNS label",
			args: []string{"--set-string", "controller.taskWebhooks.signingNamespace=signing_ns"},
			want: "must be a DNS-1123 label",
		},
		{
			name: "secret that is no Secret name",
			args: []string{"--set-string", "controller.taskWebhooks.signingSecretName=Bad_Name"},
			want: "must be a Secret name",
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
