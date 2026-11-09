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

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
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

// runningEgressTestTool reconciles a Tool until its Actor is running under a
// verified egress policy, and returns the reconciler for the next change.
func runningEgressTestTool(t *testing.T, executor *recordingToolWorkspaceExecutor, apply egressTestApplier, objects ...client.Object) (*ToolReconciler, corev1alpha1.Tool) {
	t.Helper()
	scheme := newToolScheme()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	tool := newEgressTestMCPTool(corev1alpha1.SubstrateActorEgressRule{Scheme: "https", Host: "api.github.com"})
	tool.Generation = 1
	validator := substrateFixtureTemplateValidator
	for _, object := range objects {
		// A pool in another namespace keeps its Actor leases there.
		if pool, ok := object.(*corev1alpha1.SubstrateActorPool); ok {
			tool.Spec.MCP.SubstrateActor.PoolRef = &corev1alpha1.SubstrateActorPoolReference{Name: pool.Name, Namespace: pool.Namespace}
			validator = substrateActorPoolFixtureValidator
		}
	}
	objects = append(objects, tool, approvedMCPActorTemplateForTest())
	r := &ToolReconciler{
		Client:                       fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Tool{}).WithObjects(objects...).Build(),
		Scheme:                       scheme,
		SubstrateRouterHTTPClient:    srv.Client(),
		SubstrateMCPToolsEnabled:     true,
		SubstrateConfig:              SubstrateConfig{RouterURL: srv.URL, ClaimTimeout: 1},
		SubstrateEgressPolicyApplier: apply,
		SubstrateExecutorFactory: func(SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return executor, nil
		},
	}
	r.SubstrateTemplateValidator = validator(r.Client)
	for range 3 {
		if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	got := getEgressTestTool(t, r)
	if !got.Status.Available || got.Status.Actor == nil || !substrateMCPEgressVerified(&got) || len(executor.waitReadyBoots) == 0 {
		t.Fatalf("status = %+v, want a running actor with a verified egress policy", got.Status)
	}
	if len(executor.releaseReqs) != 0 || len(executor.deleteReqs) != 0 {
		t.Fatalf("releases=%v deletes=%v, want the actor left running", executor.releaseReqs, executor.deleteReqs)
	}
	return r, got
}

func getEgressTestTool(t *testing.T, r *ToolReconciler) corev1alpha1.Tool {
	t.Helper()
	var got corev1alpha1.Tool
	if err := r.Get(context.Background(), types.NamespacedName{Name: "mcp-tool", Namespace: defaultNS}, &got); err != nil {
		t.Fatalf("Get tool: %v", err)
	}
	return got
}

// editEgressTestTool changes the running Tool's specification, as an API
// server would, to a new generation.
func editEgressTestTool(t *testing.T, r *ToolReconciler, edit func(*corev1alpha1.Tool)) {
	t.Helper()
	tool := getEgressTestTool(t, r)
	edit(&tool)
	tool.Generation++
	if err := r.Update(context.Background(), &tool); err != nil {
		t.Fatalf("Update tool: %v", err)
	}
	if got := getEgressTestTool(t, r); got.Generation != tool.Generation {
		t.Fatalf("generation = %d, want %d", got.Generation, tool.Generation)
	}
}

func requireEgressUnverified(t *testing.T, tool corev1alpha1.Tool) {
	t.Helper()
	condition := meta.FindStatusCondition(tool.Status.Conditions, toolConditionEgressPolicyReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.ObservedGeneration != tool.Generation {
		t.Fatalf("EgressPolicyReady = %+v, want False for generation %d", condition, tool.Generation)
	}
	if tool.Status.Available {
		t.Fatalf("status = %+v, want unavailable", tool.Status)
	}
}

// An edit that the controller rejects never reaches the egress policy step.
// The Actor must not keep running under the allowlist of the previous
// specification, which can be broader than the one the edit asked for.
func TestToolReconcilerStopsRunningActorWhenAnEditFailsBeforeItsEgressPolicy(t *testing.T) {
	invalidEgress := func(tool *corev1alpha1.Tool) {
		// Passes the schema, fails the controller: tls needs explicit ports.
		tool.Spec.MCP.SubstrateActor.Egress = []corev1alpha1.SubstrateActorEgressRule{{Scheme: "tls", Host: "example.com"}}
	}
	missingSecret := func(tool *corev1alpha1.Tool) {
		tool.Spec.HTTP = &corev1alpha1.HTTPExecution{AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "missing", Key: "token"}}
	}
	for name, edit := range map[string]func(*corev1alpha1.Tool){"invalid egress": invalidEgress, "missing auth secret": missingSecret} {
		t.Run(name, func(t *testing.T) {
			executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
			r, running := runningEgressTestTool(t, executor, allowSubstrateEgressForTest)
			editEgressTestTool(t, r, edit)
			for range 2 {
				if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}
			if len(executor.releaseReqs) == 0 || len(executor.deleteReqs) != 0 {
				t.Fatalf("releases=%v deletes=%v, want the running actor suspended", executor.releaseReqs, executor.deleteReqs)
			}
			for _, req := range executor.releaseReqs {
				if req.Ref.ID != running.Status.Actor.ActorID || !req.Retain || !req.SkipScrub {
					t.Fatalf("release request = %+v, want a retained suspend of %q without scrub", req, running.Status.Actor.ActorID)
				}
			}
			requireEgressUnverified(t, getEgressTestTool(t, r))
		})
	}
}

// A pooled Actor's ownership lease lives in the pool's namespace, on both
// paths that stop one.
func TestToolReconcilerStopsItsPooledActorWithoutAVerifiedEgressPolicy(t *testing.T) {
	for _, name := range []string{"edit rejected before the policy", "policy apply fails"} {
		t.Run(name, func(t *testing.T) {
			pool := &corev1alpha1.SubstrateActorPool{
				ObjectMeta: metav1.ObjectMeta{Name: testMCPPoolName, Namespace: "pools"},
				Status:     corev1alpha1.SubstrateActorPoolStatus{TemplateUID: "validated-native-template-uid"},
				Spec: corev1alpha1.SubstrateActorPoolSpec{
					TemplateRef:  corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template", Namespace: "ate-demo"},
					TargetActors: 2,
				},
			}
			fail := false
			executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
			r, running := runningEgressTestTool(t, executor, func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
				if fail {
					return errors.New("control API unavailable")
				}
				return nil
			}, pool)
			if running.Status.Actor.PoolRef == nil || running.Status.Actor.PoolRef.Namespace != "pools" {
				t.Fatalf("actor status = %+v, want a pooled actor from the pools namespace", running.Status.Actor)
			}
			if name == "policy apply fails" {
				fail = true
			} else {
				editEgressTestTool(t, r, func(tool *corev1alpha1.Tool) {
					tool.Spec.MCP.SubstrateActor.Egress = []corev1alpha1.SubstrateActorEgressRule{{Scheme: "tls", Host: "example.com"}}
				})
			}
			if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if len(executor.releaseReqs) != 1 || executor.releaseReqs[0].Ref.ID != running.Status.Actor.ActorID {
				t.Fatalf("releases = %+v, want the pooled actor %q suspended", executor.releaseReqs, running.Status.Actor.ActorID)
			}
		})
	}
}

// A failure after admission, such as no executor, is handled the same way.
func TestToolReconcilerRetriesWhenAnEditedToolHasNoExecutorToStopItsActor(t *testing.T) {
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	r, _ := runningEgressTestTool(t, executor, allowSubstrateEgressForTest)
	editEgressTestTool(t, r, func(tool *corev1alpha1.Tool) { tool.Spec.MCP.SubstrateActor.Egress = nil })
	factoryErr := errors.New("substrate API is not configured")
	r.SubstrateExecutorFactory = func(SubstrateConfig) (workspace.WorkspaceExecutor, error) { return nil, factoryErr }
	if _, err := r.Reconcile(context.Background(), mcpToolRequest()); !errors.Is(err, factoryErr) {
		t.Fatalf("Reconcile() error = %v, want the failed stop so the reconcile retries", err)
	}
	got := getEgressTestTool(t, r)
	if !strings.Contains(got.Status.Error, "could not be stopped") {
		t.Fatalf("status = %+v, want the failed stop named", got.Status)
	}
	requireEgressUnverified(t, got)
}

// A failure while the installed policy is the one the current specification
// asked for leaves nothing stale, so the Actor keeps running.
func TestToolReconcilerKeepsRunningActorWhenItsEgressPolicyIsCurrent(t *testing.T) {
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	r, _ := runningEgressTestTool(t, executor, allowSubstrateEgressForTest)
	if err := r.Delete(context.Background(), approvedMCPActorTemplateForTest()); err != nil {
		t.Fatalf("Delete template: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := getEgressTestTool(t, r)
	if got.Status.Available || len(executor.releaseReqs) != 0 || len(executor.deleteReqs) != 0 || !substrateMCPEgressVerified(&got) {
		t.Fatalf("status=%+v releases=%v deletes=%v, want unavailable with the actor untouched", got.Status, executor.releaseReqs, executor.deleteReqs)
	}
}

// Suspend or delete depends on the template the Actor runs. When it cannot be
// read the controller must not guess, and must keep retrying.
func TestToolReconcilerRetriesWhenItCannotTellHowToStopTheActor(t *testing.T) {
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	r, _ := runningEgressTestTool(t, executor, allowSubstrateEgressForTest)
	if err := r.Delete(context.Background(), approvedMCPActorTemplateForTest()); err != nil {
		t.Fatalf("Delete template: %v", err)
	}
	editEgressTestTool(t, r, func(tool *corev1alpha1.Tool) { tool.Spec.MCP.SubstrateActor.Egress = nil })
	if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err == nil || !strings.Contains(err.Error(), "read the actor's template") {
		t.Fatalf("Reconcile() error = %v, want the unreadable template so the reconcile retries", err)
	}
	got := getEgressTestTool(t, r)
	if len(executor.releaseReqs) != 0 || len(executor.deleteReqs) != 0 || !strings.Contains(got.Status.Error, "could not be stopped") {
		t.Fatalf("status=%+v releases=%v deletes=%v, want no guess and the failure named", got.Status, executor.releaseReqs, executor.deleteReqs)
	}
	requireEgressUnverified(t, got)
}

// The lease is what makes an Actor this Tool's. Without it the Tool stops nothing.
func TestToolReconcilerDoesNotStopAnActorItHoldsNoLeaseFor(t *testing.T) {
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	r, _ := runningEgressTestTool(t, executor, allowSubstrateEgressForTest)
	if err := r.DeleteAllOf(context.Background(), &coordinationv1.Lease{}, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("Delete leases: %v", err)
	}
	editEgressTestTool(t, r, func(tool *corev1alpha1.Tool) {
		tool.Spec.MCP.SubstrateActor.Egress = []corev1alpha1.SubstrateActorEgressRule{{Scheme: "tls", Host: "example.com"}}
	})
	if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(executor.releaseReqs) != 0 || len(executor.deleteReqs) != 0 {
		t.Fatalf("releases=%v deletes=%v, want an unowned actor left alone", executor.releaseReqs, executor.deleteReqs)
	}
	requireEgressUnverified(t, getEgressTestTool(t, r))
}

// Substrate cannot suspend an Actor whose template snapshots volumes only and
// mounts no durable directory: the attempt fails before the workload stops.
// Such an Actor is deleted under its UID instead, on both paths that stop one.
func TestToolReconcilerDeletesAnActorThatCannotBeSuspended(t *testing.T) {
	stateless := func(r *ToolReconciler) {
		validate := r.SubstrateTemplateValidator
		r.SubstrateTemplateValidator = func(ctx context.Context, request *ExecutionWorkspaceRequest) error {
			request.TemplateSuspendUnsupported = true
			return validate(ctx, request)
		}
	}
	requireDeleted := func(t *testing.T, executor *recordingToolWorkspaceExecutor, actorID string) {
		t.Helper()
		if len(executor.releaseReqs) != 0 || len(executor.deleteReqs) == 0 {
			t.Fatalf("releases=%v deletes=%v, want the actor deleted and never suspended", executor.releaseReqs, executor.deleteReqs)
		}
		for _, req := range executor.deleteReqs {
			if req.Ref.ID != actorID || !req.SkipScrub {
				t.Fatalf("delete request = %+v, want %q deleted without scrub", req, actorID)
			}
		}
	}
	t.Run("edit rejected before the policy", func(t *testing.T) {
		executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
		r, running := runningEgressTestTool(t, executor, allowSubstrateEgressForTest)
		stateless(r)
		editEgressTestTool(t, r, func(tool *corev1alpha1.Tool) {
			tool.Spec.MCP.SubstrateActor.Egress = []corev1alpha1.SubstrateActorEgressRule{{Scheme: "tls", Host: "example.com"}}
		})
		if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		requireDeleted(t, executor, running.Status.Actor.ActorID)
	})
	t.Run("policy apply fails", func(t *testing.T) {
		fail := false
		executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
		r, running := runningEgressTestTool(t, executor, func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
			if fail {
				return errors.New("control API unavailable")
			}
			return nil
		})
		stateless(r)
		fail = true
		if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		requireDeleted(t, executor, running.Status.Actor.ActorID)
	})
	t.Run("delete fails", func(t *testing.T) {
		deleteErr := errors.New("delete refused")
		executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}, deleteErrs: []error{deleteErr}}
		r, _ := runningEgressTestTool(t, executor, allowSubstrateEgressForTest)
		stateless(r)
		editEgressTestTool(t, r, func(tool *corev1alpha1.Tool) {
			tool.Spec.MCP.SubstrateActor.Egress = []corev1alpha1.SubstrateActorEgressRule{{Scheme: "tls", Host: "example.com"}}
		})
		if _, err := r.Reconcile(context.Background(), mcpToolRequest()); !errors.Is(err, deleteErr) {
			t.Fatalf("Reconcile() error = %v, want the failed delete so the reconcile retries", err)
		}
		if got := getEgressTestTool(t, r); !strings.Contains(got.Status.Error, "could not be stopped") {
			t.Fatalf("status = %+v, want the failed stop named", got.Status)
		}
	})
}

// A Tool moved to another template gets a new Actor. Until that one is ready
// the previous Actor is still running; when the new policy cannot be applied,
// the previous Actor must stop too.
func TestToolReconcilerStopsThePreviousActorWhenItsReplacementHasNoEgressPolicy(t *testing.T) {
	second := approvedMCPActorTemplateForTest()
	second.SetName("mcp-template-next")
	fail := false
	executor := &recordingToolWorkspaceExecutor{claimCreateds: []bool{true}}
	r, running := runningEgressTestTool(t, executor, func(context.Context, SubstrateConfig, string, []workspace.SubstrateEgressRule) error {
		if fail {
			return errors.New("control API unavailable")
		}
		return nil
	}, second)
	executor.claimCreated = true
	editEgressTestTool(t, r, func(tool *corev1alpha1.Tool) { tool.Spec.MCP.SubstrateActor.TemplateRef.Name = "mcp-template-next" })
	fail = true
	for range 3 {
		if _, err := r.Reconcile(context.Background(), mcpToolRequest()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	if len(executor.releaseReqs) == 0 {
		t.Fatal("the previous actor kept running although its replacement has no egress policy")
	}
	for _, req := range executor.releaseReqs {
		if req.Ref.ID != running.Status.Actor.ActorID {
			t.Fatalf("release request = %+v, want only the previous actor %q suspended", req, running.Status.Actor.ActorID)
		}
	}
}

func TestNativeSubstrateTemplateSuspendUnsupported(t *testing.T) {
	template := func(fidelity ateapipb.SnapshotFidelity, volume *ateapipb.Volume, mount string) *ateapipb.ActorTemplate {
		container := &ateapipb.Container{Name: "mcp"}
		if mount != "" {
			container.VolumeMounts = []*ateapipb.VolumeMount{{Name: mount, MountPath: "/data"}}
		}
		out := &ateapipb.ActorTemplate{Containers: []*ateapipb.Container{container}, SnapshotConfig: &ateapipb.SnapshotConfig{PreferredFidelity: fidelity}}
		if volume != nil {
			out.Volumes = []*ateapipb.Volume{volume}
		}
		return out
	}
	durable := func() *ateapipb.Volume {
		return &ateapipb.Volume{Name: "data", DurableDir: &ateapipb.DurableDirVolumeSource{}}
	}
	volumes, memory := ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
	for name, tc := range map[string]struct {
		template *ateapipb.ActorTemplate
		want     bool
	}{
		"volumes snapshot without volumes":       {template(volumes, nil, ""), true},
		"volumes snapshot, durable dir unused":   {template(volumes, durable(), ""), true},
		"volumes snapshot, other volume mounted": {template(volumes, &ateapipb.Volume{Name: "info"}, "info"), true},
		"volumes snapshot, durable dir mounted":  {template(volumes, durable(), "data"), false},
		"memory snapshot without volumes":        {template(memory, nil, ""), false},
	} {
		if got := nativeSubstrateTemplateSuspendUnsupported(tc.template); got != tc.want {
			t.Errorf("%s: suspend unsupported = %t, want %t", name, got, tc.want)
		}
	}
}
