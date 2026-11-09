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

type egressTestApplier = func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error

func reconcileEgressTestTool(t *testing.T, tool *corev1alpha1.Tool, apply egressTestApplier) (*recordingToolWorkspaceExecutor, corev1alpha1.Tool, int) {
	t.Helper()
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	got, probes, err := reconcileEgressTestToolWith(t, tool, executor, apply)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return executor, got, probes
}

// reconcileEgressTestToolWith reconciles with a caller-built executor, so an
// applier can observe what the executor has been asked to do so far. It returns
// the first reconcile error instead of failing the test.
func reconcileEgressTestToolWith(t *testing.T, tool *corev1alpha1.Tool, executor *recordingToolWorkspaceExecutor, apply egressTestApplier) (corev1alpha1.Tool, int, error) {
	t.Helper()
	scheme := newToolScheme()
	var probes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
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
	var reconcileErr error
	for range 3 {
		if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
			reconcileErr = err
			break
		}
	}
	var got corev1alpha1.Tool
	if err := r.Get(context.Background(), types.NamespacedName{Name: "mcp-tool", Namespace: defaultNS}, &got); err != nil {
		t.Fatalf("Get tool: %v", err)
	}
	return got, probes, reconcileErr
}

func TestToolReconcilerAppliesActorEgressBeforeResume(t *testing.T) {
	var appliedActor string
	var appliedRules []workspace.SubstrateEgressRule
	// Each reconcile must install the policy before it resumes the actor, so
	// every apply has to see exactly as many resumes as applies before it.
	applies := 0
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	got, _, err := reconcileEgressTestToolWith(t, newEgressTestMCPTool(
		corev1alpha1.SubstrateActorEgressRule{Scheme: "https", Host: "api.github.com"},
		corev1alpha1.SubstrateActorEgressRule{Scheme: "tls", Host: "*.example.com", Ports: []int32{8443}},
	), executor, func(_ context.Context, _ SubstrateConfig, actorID string, rules []workspace.SubstrateEgressRule) error {
		if resumes := len(executor.waitReadyBoots); resumes != applies {
			t.Errorf("apply %d saw %d resume(s); the actor was resumed before its egress policy was applied", applies+1, resumes)
		}
		applies++
		appliedActor, appliedRules = actorID, rules
		return nil
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if applies == 0 || len(executor.waitReadyBoots) != applies {
		t.Fatalf("applies=%d resumes=%d, want one resume after each apply", applies, len(executor.waitReadyBoots))
	}
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

// A reattached actor may be running under the policy an earlier reconcile
// installed. When a later apply fails, that policy is unverified and possibly
// broader than the Tool now allows, so the actor must be suspended.
func TestToolReconcilerSuspendsRunningActorWhenEgressPolicyApplyFails(t *testing.T) {
	fail := false
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	got, _, err := reconcileEgressTestToolWith(t, newEgressTestMCPTool(), executor, func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
		// Applies succeed until the actor has been resumed, then fail.
		if fail = fail || executor.waitReadyCalled; fail {
			return errors.New("control API unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !executor.waitReadyCalled || len(executor.waitReadyBoots) != 1 {
		t.Fatalf("resumes = %v, want the actor resumed once before the failing apply", executor.waitReadyBoots)
	}
	if len(executor.releaseReqs) == 0 {
		t.Fatal("running actor was not suspended after its egress policy failed to apply")
	}
	wantActorID := deterministicSubstrateToolActorID(defaultNS, "mcp-tool", "ate-demo", "mcp-template")
	for _, req := range executor.releaseReqs {
		if req.Ref.ID != wantActorID || !req.Retain || !req.SkipScrub {
			t.Fatalf("release request = %+v, want a retained suspend of %q without scrub", req, wantActorID)
		}
	}
	if got.Status.Available || !strings.Contains(got.Status.Error, "egress policy") {
		t.Fatalf("status = %+v, want unavailable with egress error", got.Status)
	}
}

func TestToolReconcilerSurfacesFailedSuspendAfterEgressPolicyApplyFails(t *testing.T) {
	suspendErr := errors.New("suspend refused")
	executor := &recordingToolWorkspaceExecutor{releaseErr: suspendErr}
	got, _, err := reconcileEgressTestToolWith(t, newEgressTestMCPTool(), executor, func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
		return errors.New("control API unavailable")
	})
	if !errors.Is(err, suspendErr) {
		t.Fatalf("Reconcile() error = %v, want the suspend failure so the reconcile retries", err)
	}
	if got.Status.Available || !strings.Contains(got.Status.Error, "could not be suspended") || !strings.Contains(got.Status.Error, "suspend refused") {
		t.Fatalf("status = %+v, want unavailable and naming the failed suspend", got.Status)
	}
	if executor.waitReadyCalled {
		t.Fatal("actor was resumed without a verified egress policy")
	}
}

// An actor this reconcile created has never been resumed, and one observed
// suspended has nothing running; neither needs a suspend call.
func TestToolReconcilerSkipsSuspendForActorsThatAreNotRunning(t *testing.T) {
	for name, executor := range map[string]*recordingToolWorkspaceExecutor{
		"created":   {claimCreated: true},
		"suspended": {claimPhase: workspace.PhaseRetained},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := reconcileEgressTestToolWith(t, newEgressTestMCPTool(), executor, func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
				return errors.New("control API unavailable")
			})
			if err != nil || len(executor.releaseReqs) != 0 {
				t.Fatalf("err=%v releases=%+v, want no suspend call", err, executor.releaseReqs)
			}
		})
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
