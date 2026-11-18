package tools

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestForwardingToolNamesCoverTheBrokeredBuiltIns keeps the Tool type's
// forwarding-name rule in step with the ACP broker: a forwarding Tool named
// after a brokered built-in would be served as that built-in instead.
func TestForwardingToolNamesCoverTheBrokeredBuiltIns(t *testing.T) {
	raw, err := os.ReadFile("../../api/v1alpha1/tool_types.go")
	if err != nil {
		t.Fatal(err)
	}
	var rule string
	for line := range strings.Lines(string(raw)) {
		if strings.HasPrefix(line, "// +kubebuilder:validation:XValidation:") && strings.Contains(line, "self.metadata.name in") {
			rule = line
		}
	}
	if rule == "" {
		t.Fatal("the Tool type has no forwarding-name rule")
	}
	registry := NewRegistry()
	k8sClient := fake.NewClientBuilder().Build()
	registry.Register(NewReplyInConversationTool())
	for _, register := range []func() error{
		func() error { return RegisterBrokeredWebTools(registry) },
		func() error { return RegisterBrokeredCoordinationTools(registry, k8sClient) },
		func() error { return RegisterBrokeredConnectionTools(registry) },
		func() error { return RegisterBrokeredGitHubTools(registry, k8sClient) },
	} {
		if err := register(); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range registry.Names() {
		if !strings.Contains(rule, "'"+name+"'") {
			t.Errorf("the forwarding-name rule does not reserve brokered built-in %q", name)
		}
	}
}
