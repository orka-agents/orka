package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

func TestTaskWebhooksBypassExactControllerCleanupAcrossInstallPaths(t *testing.T) {
	t.Run("Helm", func(t *testing.T) {
		digest := "sha256:" + strings.Repeat("3", 64)
		rendered := requireHelmRender(t,
			"--set", "admission.enabled=true",
			"--set", "admission.webhooks.enabled=true",
			"--set-string", "admission.tls.existingSecret=orka-admission-tls",
			"--set-string", "admission.webhooks.caBundle=Y2E=",
			"--set-string", "controller.image.digest="+digest,
			"--show-only", "templates/admission-validating-webhook.yaml",
		)
		assertTaskWebhooksBypassExactControllerCleanup(t, []byte(rendered))
	})

	t.Run("standalone", func(t *testing.T) {
		path := filepath.Join("..", "..", "..", "config", "orka-admission-webhooks", "validating_webhook.yaml")
		manifest, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read standalone admission webhooks: %v", err)
		}
		assertTaskWebhooksBypassExactControllerCleanup(t, manifest)
	})
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
		"request.userInfo.username ==",
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
