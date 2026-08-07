package executionmode

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNamespaceMode(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant", Labels: map[string]string{NamespaceLabel: string(HarnessV1)},
	}}
	if mode, err := FromNamespace(namespace); err != nil || mode != HarnessV1 {
		t.Fatalf("FromNamespace() = %q, %v", mode, err)
	}
	if err := ValidateNamespace(namespace, HarnessV2); err == nil {
		t.Fatal("mode mismatch must fail")
	}
	for _, raw := range []string{"", "auto", "dual", "harness-v3"} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded", raw)
		}
	}
}
