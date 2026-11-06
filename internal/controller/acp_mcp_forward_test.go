package controller

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func forwardingDescriptorTool() *corev1alpha1.Tool {
	return &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-arm", Namespace: "tenant", UID: "tool-uid", Generation: 1},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Azure Resource Manager",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP: &corev1alpha1.HTTPExecution{
				URL: "https://management.azure.com/", Method: http.MethodPost,
				OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "azure"},
				Forward:                 &corev1alpha1.HTTPForward{Protocol: corev1alpha1.HTTPForwardProtocolAzureResourceManager},
			},
		},
	}
}

func TestCustomACPMCPToolDescriptorMarksForwardingTools(t *testing.T) {
	descriptor, err := customACPMCPToolDescriptor(forwardingDescriptorTool())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Forward == nil || descriptor.Forward.Origin != "https://management.azure.com" ||
		descriptor.Effect != harnessv2.MCPToolEffectReadOnly || !bytes.Equal(descriptor.InputSchema, harnessv2.MCPForwardInputSchema) {
		t.Fatalf("descriptor = %#v", descriptor)
	}
}

func TestCustomACPMCPToolDescriptorRefusesUnsafeForwardingTools(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*corev1alpha1.Tool)
		want   string
	}{
		"write class": {func(tool *corev1alpha1.Tool) {
			tool.Spec.BrokeredToolClass = corev1alpha1.AgentRuntimeBrokeredToolClassWrite
		}, "brokeredToolClass read"},
		"no policy": {func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.OutboundAccessPolicyRef = nil }, "outbound access policy"},
		"auth secret": {func(tool *corev1alpha1.Tool) {
			tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "s", Key: "k"}
		}, "auth Secret"},
		"headers": {func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.Headers = map[string]string{"X-A": "b"} }, "headers"},
		"parameters": {func(tool *corev1alpha1.Tool) {
			tool.Spec.Parameters = &apiextensionsv1.JSON{Raw: []byte(`{"type":"object"}`)}
		}, "parameters"},
		"path":           {func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.URL = "https://management.azure.com/subscriptions" }, "no path"},
		"plain http":     {func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.URL = "http://management.azure.com" }, "https origin"},
		"other protocol": {func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.Forward.Protocol = "Other" }, "unsupported protocol"},
	} {
		tool := forwardingDescriptorTool()
		tc.mutate(tool)
		if _, err := customACPMCPToolDescriptor(tool); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// The Tool CRD refuses forwarding Tools named like a runtime's built-in tool,
// which the descriptor builder would treat as provider-native and never
// forward. Its name list must cover every built-in name a Tool can take.
func TestForwardingToolCRDCoversEveryBuiltInToolName(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/core.orka.ai_tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	rule := ""
	for _, check := range crd.Spec.Versions[0].Schema.OpenAPIV3Schema.XValidations {
		if strings.Contains(check.Rule, "self.metadata.name in") {
			rule = check.Rule
		}
	}
	if rule == "" {
		t.Fatal("the Tool CRD has no forwarding name rule")
	}
	for provider, names := range providerNativeTools {
		for lower := range names {
			if len(validation.IsDNS1123Subdomain(lower)) > 0 {
				continue
			}
			if !strings.Contains(rule, "'"+lower+"'") {
				t.Errorf("forwarding name rule misses %s built-in tool %q", provider, lower)
			}
		}
	}
}
