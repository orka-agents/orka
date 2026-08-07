package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const canonicalProductionControllerUsername = "system:serviceaccount:orka-system:orka-controller-manager"

func TestControllerWebhooksAreReleaseLocalAndModeScoped(t *testing.T) {
	digest := "sha256:" + strings.Repeat("3", 64)
	for _, mode := range []string{"harness-v1", "harness-v2"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{
				"--set-string", "controller.mode=" + mode,
				"--show-only", "templates/controller-validating-webhook.yaml",
			}
			if mode == "harness-v1" {
				args = append(args,
					"--set-string", "harnessV1.image.digest="+digest,
					"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
				)
			}

			rendered := requireHelmRender(t, args...)
			configuration := admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := yaml.Unmarshal([]byte(rendered), &configuration); err != nil {
				t.Fatalf("decode controller validating webhook configuration: %v", err)
			}
			if configuration.Name != "test-orka-controller" {
				t.Fatalf("controller webhook name = %q, want test-orka-controller", configuration.Name)
			}

			webhooks := make(map[string]admissionregistrationv1.ValidatingWebhook, len(configuration.Webhooks))
			for _, webhook := range configuration.Webhooks {
				webhooks[webhook.Name] = webhook
				if !strings.HasSuffix(webhook.Name, "."+mode+".orka.ai") {
					t.Errorf("webhook name %q is not scoped to mode %q", webhook.Name, mode)
				}
				if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionregistrationv1.Fail {
					t.Errorf("%s failurePolicy = %v, want Fail", webhook.Name, webhook.FailurePolicy)
				}
				service := webhook.ClientConfig.Service
				if service == nil || service.Name != "test-orka" || service.Namespace != "orka-test" ||
					service.Port == nil || *service.Port != 443 {
					t.Errorf("%s service = %#v, want test-orka:443 in orka-test", webhook.Name, service)
				}
				selector := webhook.NamespaceSelector
				if strings.HasPrefix(webhook.Name, "namespace-mode.") {
					selector = webhook.ObjectSelector
				}
				if selector == nil || selector.MatchLabels["orka.ai/controller-mode"] != mode {
					t.Errorf("%s execution-mode selector = %#v, want %q", webhook.Name, selector, mode)
				}
			}

			_, hasTaskWorkspace := webhooks["task-workspace-class."+mode+".orka.ai"]
			_, hasToolWorkspace := webhooks["tool-workspace-class."+mode+".orka.ai"]
			wantWorkspace := mode == "harness-v2"
			if hasTaskWorkspace != wantWorkspace || hasToolWorkspace != wantWorkspace {
				t.Fatalf("workspace webhooks present = task:%t tool:%t, want %t", hasTaskWorkspace, hasToolWorkspace, wantWorkspace)
			}
		})
	}
}

func TestControllerServiceExposesWebhookPort(t *testing.T) {
	rendered := requireHelmRender(t, "--show-only", "templates/service.yaml")
	service := corev1.Service{}
	if err := yaml.Unmarshal([]byte(rendered), &service); err != nil {
		t.Fatalf("decode controller Service: %v", err)
	}

	for _, port := range service.Spec.Ports {
		if port.Name == "webhook" {
			if port.Port != 443 || port.TargetPort.String() != "webhook" {
				t.Fatalf("webhook Service port = %#v, want 443 -> webhook", port)
			}
			return
		}
	}
	t.Fatalf("controller Service has no webhook port: %#v", service.Spec.Ports)
}

func TestControllerDeploymentEnablesReleaseLocalAdmission(t *testing.T) {
	rendered := requireHelmRender(t, "--show-only", "templates/deployment.yaml")
	deployment := appsv1.Deployment{}
	if err := yaml.Unmarshal([]byte(rendered), &deployment); err != nil {
		t.Fatalf("decode controller Deployment: %v", err)
	}

	var args []string
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "controller" {
			args = container.Args
			break
		}
	}
	for _, want := range []string{
		"--task-provenance-admission-enabled=true",
		"--workspace-class-use-admission-enabled=true",
		"--webhook-cert-path=/var/run/orka/webhook/tls",
	} {
		if !containsString(args, want) {
			t.Errorf("controller args do not contain %q: %#v", want, args)
		}
	}
}

func TestSharedAdmissionAuthorizesCanonicalProductionController(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "orka-admission", "deployment.yaml")
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read standalone admission Deployment: %v", err)
	}
	deployment := appsv1.Deployment{}
	if err := yaml.Unmarshal(manifest, &deployment); err != nil {
		t.Fatalf("decode standalone admission Deployment: %v", err)
	}

	var args []string
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "admission" {
			args = container.Args
			break
		}
	}
	for _, prefix := range []string{"--controller-usernames=", "--task-provenance-trusted-users="} {
		if !commaListArgumentContains(args, prefix, canonicalProductionControllerUsername) {
			t.Errorf("admission args do not authorize %q in %s: %#v",
				canonicalProductionControllerUsername, prefix, args)
		}
	}
}

func TestSharedTaskWebhooksBypassExactControllerCleanup(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "orka-admission-webhooks", "validating_webhook.yaml")
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read standalone admission webhooks: %v", err)
	}
	assertTaskWebhooksBypassExactControllerCleanup(t, manifest)
}

func assertTaskWebhooksBypassExactControllerCleanup(t *testing.T, manifest []byte) {
	t.Helper()

	configuration := admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := yaml.Unmarshal(manifest, &configuration); err != nil {
		t.Fatalf("decode validating webhook configuration: %v", err)
	}
	webhooks := make(map[string]admissionregistrationv1.ValidatingWebhook, len(configuration.Webhooks))
	for _, webhook := range configuration.Webhooks {
		webhooks[webhook.Name] = webhook
	}

	authority, ok := webhooks["taskexecutionauthority.core.orka.ai"]
	if !ok {
		t.Fatal("task execution authority webhook is missing")
	}
	if len(authority.MatchConditions) != 1 ||
		authority.MatchConditions[0].Name != "route-unless-controller-cleanup-safe" {
		t.Fatalf("task execution authority cleanup-safe condition = %#v", authority.MatchConditions)
	}
	condition := authority.MatchConditions[0]
	for _, marker := range []string{
		"request.userInfo.username == '" + canonicalProductionControllerUsername + "'",
		"request.operation == 'UPDATE'",
		"has(oldObject.metadata.deletionTimestamp)",
		"oldObject.metadata.?finalizers.orValue([]).exists(f, f == 'orka.ai/cleanup')",
		"oldObject.metadata.?finalizers.orValue([]).filter(f, f != 'orka.ai/cleanup')",
		"object.spec == oldObject.spec",
		"object.?status.orValue({}) == oldObject.?status.orValue({})",
	} {
		if !strings.Contains(condition.Expression, marker) {
			t.Fatalf("cleanup-safe condition is missing %q:\n%s", marker, condition.Expression)
		}
	}

	for _, name := range []string{
		"taskprovenance.core.orka.ai",
		"taskworkspaceclassuse.core.orka.ai",
	} {
		webhook, ok := webhooks[name]
		if !ok {
			t.Fatalf("%s webhook is missing", name)
		}
		if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionregistrationv1.Fail {
			t.Fatalf("%s failurePolicy = %v, want Fail", name, webhook.FailurePolicy)
		}
		if !reflect.DeepEqual(webhook.MatchConditions, authority.MatchConditions) {
			t.Fatalf("%s cleanup-safe conditions = %#v, want %#v", name, webhook.MatchConditions, authority.MatchConditions)
		}
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func commaListArgumentContains(args []string, prefix, want string) bool {
	for _, arg := range args {
		value, ok := strings.CutPrefix(arg, prefix)
		if !ok {
			continue
		}
		if slices.Contains(strings.Split(value, ","), want) {
			return true
		}
	}
	return false
}
