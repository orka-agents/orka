/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
)

// allowSubstrateEgressForTest stands in for the native control API in tests
// that are not about egress policy.
func allowSubstrateEgressForTest(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
	return nil
}

func newEgressTestMCPTool(egress ...corev1alpha1.SubstrateActorEgressRule) *corev1alpha1.Tool {
	return &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-tool", Namespace: defaultNS, Finalizers: []string{substrateMCPToolActorFinalizer}},
		Spec: corev1alpha1.ToolSpec{
			Description: "MCP tool",
			MCP: &corev1alpha1.MCPToolServer{
				Path: testMCPPath,
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template", Namespace: "ate-demo"},
					Egress:      egress,
				},
			},
		},
	}
}

func reconcileEgressTestTool(t *testing.T, tool *corev1alpha1.Tool, apply func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error) (*recordingToolWorkspaceExecutor, corev1alpha1.Tool, int) {
	t.Helper()
	scheme := newToolScheme()
	var probes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	r := &ToolReconciler{
		Client:                       fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Tool{}).WithObjects(tool, approvedMCPActorTemplateForTest()).Build(),
		Scheme:                       scheme,
		SubstrateRouterHTTPClient:    srv.Client(),
		SubstrateMCPToolsEnabled:     true,
		SubstrateConfig:              SubstrateConfig{RouterURL: srv.URL, ClaimTimeout: 1},
		SubstrateEgressPolicyApplier: apply,
		SubstrateExecutorFactory: func(SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return executor, nil
		},
	}
	r.SubstrateTemplateValidator = substrateFixtureTemplateValidator(r.Client)
	for range 3 {
		if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	var got corev1alpha1.Tool
	if err := r.Get(context.Background(), types.NamespacedName{Name: "mcp-tool", Namespace: defaultNS}, &got); err != nil {
		t.Fatalf("Get tool: %v", err)
	}
	return executor, got, probes
}

func TestToolReconcilerAppliesActorEgressBeforeResume(t *testing.T) {
	var appliedActor string
	var appliedRules []workspace.SubstrateEgressRule
	var executor *recordingToolWorkspaceExecutor
	executor, got, _ := reconcileEgressTestTool(t, newEgressTestMCPTool(
		corev1alpha1.SubstrateActorEgressRule{Scheme: "https", Host: "api.github.com"},
		corev1alpha1.SubstrateActorEgressRule{Scheme: "tls", Host: "*.example.com", Ports: []int32{8443}},
	), func(_ context.Context, _ SubstrateConfig, actorID string, rules []workspace.SubstrateEgressRule) error {
		if executor != nil && executor.waitReadyCalled {
			t.Fatal("actor resumed before its egress policy was applied")
		}
		appliedActor, appliedRules = actorID, rules
		return nil
	})
	wantActorID := deterministicSubstrateToolActorID(defaultNS, "mcp-tool", "ate-demo", "mcp-template")
	if appliedActor != wantActorID || len(appliedRules) != 2 || appliedRules[1].Host != "*.example.com" || appliedRules[1].Ports[0] != 8443 {
		t.Fatalf("applied actor=%q rules=%+v", appliedActor, appliedRules)
	}
	if !executor.waitReadyCalled || !got.Status.Available {
		t.Fatalf("waitReady=%t status=%+v, want resumed and available", executor.waitReadyCalled, got.Status)
	}
	if condition := meta.FindStatusCondition(got.Status.Conditions, toolConditionEgressPolicyReady); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("EgressPolicyReady = %+v, want True", condition)
	}
}

func TestToolReconcilerAppliesDenyAllEgressForEmptyAllowlist(t *testing.T) {
	calls := 0
	_, got, _ := reconcileEgressTestTool(t, newEgressTestMCPTool(), func(_ context.Context, _ SubstrateConfig, _ string, rules []workspace.SubstrateEgressRule) error {
		calls++
		if len(rules) != 0 {
			t.Fatalf("rules = %+v, want deny-all", rules)
		}
		policy, err := workspace.CompileSubstrateActorEgressPolicy("ate-demo", rules)
		if err != nil || len(policy.GetRules()) != 0 {
			t.Fatalf("compiled policy = %v, %v", policy, err)
		}
		return nil
	})
	if calls == 0 || !got.Status.Available {
		t.Fatalf("calls=%d available=%t, want deny-all policy applied", calls, got.Status.Available)
	}
}

func TestToolReconcilerFailsClosedWhenEgressPolicyApplyFails(t *testing.T) {
	executor, got, probes := reconcileEgressTestTool(t, newEgressTestMCPTool(), func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
		return errors.New("control API unavailable")
	})
	if executor.waitReadyCalled || probes != 0 {
		t.Fatalf("waitReady=%t probes=%d, want actor left suspended", executor.waitReadyCalled, probes)
	}
	if got.Status.Available || got.Status.Endpoint != "" || !strings.Contains(got.Status.Error, "egress policy") {
		t.Fatalf("status = %+v, want unavailable with egress error", got.Status)
	}
	if condition := meta.FindStatusCondition(got.Status.Conditions, toolConditionEgressPolicyReady); condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "EgressPolicyFailed" {
		t.Fatalf("EgressPolicyReady = %+v, want False/EgressPolicyFailed", condition)
	}
}

func TestToolReconcilerRejectsInvalidEgressAllowlist(t *testing.T) {
	for _, rule := range []corev1alpha1.SubstrateActorEgressRule{
		{Scheme: "https", Host: "*"},
		{Scheme: "http", Host: "10.0.0.1"},
		{Scheme: "https", Host: "example.com/path"},
		{Scheme: "tls", Host: "example.com"},
	} {
		t.Run(rule.Host, func(t *testing.T) {
			applied := false
			executor, got, _ := reconcileEgressTestTool(t, newEgressTestMCPTool(rule), func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
				applied = true
				return nil
			})
			if applied || executor.claimName != "" || executor.waitReadyCalled {
				t.Fatalf("applied=%t claim=%q waitReady=%t, want admission rejection", applied, executor.claimName, executor.waitReadyCalled)
			}
			if got.Status.Available || !strings.Contains(got.Status.Error, "mcp.substrateActor.egress") {
				t.Fatalf("status = %+v, want egress validation error", got.Status)
			}
		})
	}
}

func TestToolReconcilerRejectsSubstrateMCPOutboundAccessPolicy(t *testing.T) {
	tool := newEgressTestMCPTool()
	tool.Spec.HTTP = &corev1alpha1.HTTPExecution{OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "policy"}}
	executor, got, _ := reconcileEgressTestTool(t, tool, allowSubstrateEgressForTest)
	if executor.claimName != "" || got.Status.Available || !strings.Contains(got.Status.Error, "do not support outboundAccessPolicyRef") {
		t.Fatalf("claim=%q status=%+v, want outbound access policy rejection", executor.claimName, got.Status)
	}
}

func TestToolReconcilerRejectsReservedSubstrateMCPHeaders(t *testing.T) {
	tool := newEgressTestMCPTool()
	tool.Spec.HTTP = &corev1alpha1.HTTPExecution{Headers: map[string]string{"ate-target-actor": "victims/other"}}
	executor, got, _ := reconcileEgressTestTool(t, tool, allowSubstrateEgressForTest)
	if executor.claimName != "" || got.Status.Available || !strings.Contains(got.Status.Error, "reserved header") {
		t.Fatalf("claim=%q status=%+v, want reserved header rejection", executor.claimName, got.Status)
	}
}
