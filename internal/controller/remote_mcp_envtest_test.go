package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Remote MCP admission", func() {
	It("accepts remote configuration, rejects transport/backend conflicts and preserves legacy parameter admission", func() {
		ctx := context.Background()
		for _, test := range []struct {
			name   string
			mutate func(*corev1alpha1.Tool)
			reject bool
		}{
			{name: "valid"},
			{name: "missing-params-consumer-validated", mutate: func(tool *corev1alpha1.Tool) { tool.Spec.Parameters = nil }},
			{name: "missing-secret", mutate: func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.AuthSecretRef = nil }, reject: true},
			{name: "missing-policy", mutate: func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.OutboundAccessPolicyRef = nil }, reject: true},
			{name: "missing-name", mutate: func(tool *corev1alpha1.Tool) { tool.Spec.MCP.Remote.ToolName = "" }, reject: true},
			{name: "actor-conflict", mutate: func(tool *corev1alpha1.Tool) {
				tool.Spec.MCP.SubstrateActor = &corev1alpha1.SubstrateMCPActor{TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "host"}}
			}, reject: true},
			{name: "url-conflict", mutate: func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.URL = "https://other.example.com" }, reject: true},
			{name: "session-override", mutate: func(tool *corev1alpha1.Tool) { tool.Spec.HTTP.Headers = map[string]string{"Mcp-Session-Id": "other"} }, reject: true},
			{name: "legacy-boolean-schema", mutate: func(tool *corev1alpha1.Tool) {
				tool.Spec.MCP = nil
				tool.Spec.HTTP.URL = "https://example.com"
				tool.Spec.Parameters.Raw = []byte(`true`)
			}},
		} {
			tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("remote-%s-%d", test.name, time.Now().UnixNano()), Namespace: "default"}, Spec: corev1alpha1.ToolSpec{Description: "Reviewed", Parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object"}`)}, MCP: &corev1alpha1.MCPToolServer{Remote: &corev1alpha1.RemoteMCPServer{URL: "https://example.com/mcp", ToolName: "read"}}, HTTP: &corev1alpha1.HTTPExecution{AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "auth", Key: "token"}, OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "egress"}}}}
			if test.mutate != nil {
				test.mutate(tool)
			}
			err := k8sClient.Create(ctx, tool)
			if test.reject {
				Expect(err).To(HaveOccurred(), test.name)
			} else {
				Expect(err).NotTo(HaveOccurred(), test.name)
				Expect(k8sClient.Delete(ctx, tool)).To(Succeed())
			}
		}
	})
})
