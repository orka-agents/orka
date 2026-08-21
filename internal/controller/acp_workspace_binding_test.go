/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type failingAgentExecutionSnapshotPersistStore struct {
	store.AgentExecutionSnapshotStore
}

func (s failingAgentExecutionSnapshotPersistStore) PersistAgentExecutionSnapshot(
	context.Context,
	store.AgentExecutionSnapshot,
) error {
	return errors.New("snapshot persistence unavailable")
}

func workspaceBindingTestTask(mutate func(*corev1alpha1.ExecutionWorkspaceSpec)) *corev1alpha1.Task {
	task := bindingTestTask()
	workspace := &corev1alpha1.ExecutionWorkspaceSpec{
		Enabled:  true,
		Provider: corev1alpha1.WorkspaceProviderAgentSandbox,
	}
	if mutate != nil {
		mutate(workspace)
	}
	task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: workspace}
	return task
}

func TestResolveACPWorkspaceBinding(t *testing.T) {
	tests := []struct {
		name                      string
		task                      *corev1alpha1.Task
		wantErr                   string
		wantNil                   bool
		wantSession               string
		sessionUID                string
		enforceNamespaceIsolation bool
	}{
		{name: "nil task", task: nil, wantNil: true},
		{name: "no workspace", task: bindingTestTask(), wantNil: true},
		{
			name:    "disabled workspace",
			task:    workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) { ws.Enabled = false }),
			wantNil: true,
		},
		{
			name:        "defaults resolve to a per-task binding",
			task:        workspaceBindingTestTask(nil),
			wantSession: "task:11111111-1111-1111-1111-111111111111",
		},
		{
			name: "session reuse binds to the continued session",
			task: func() *corev1alpha1.Task {
				task := workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
					ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
				})
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "review-loop"}
				return task
			}(),
			sessionUID:  "session-uid-review-loop",
			wantSession: "session:session-uid-review-loop",
		},
		{
			name: "session reuse rejects non-default workspace slot",
			task: func() *corev1alpha1.Task {
				task := workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
					ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
					ws.WorkspaceSlot = "secondary"
				})
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "review-loop"}
				return task
			}(),
			sessionUID: "session-uid-review-loop",
			wantErr:    "supports only workspaceSlot",
		},
		{
			name:    "substrate without templateRef fails closed",
			task:    workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) { ws.Provider = corev1alpha1.WorkspaceProviderSubstrate }),
			wantErr: "requires templateRef.name",
		},
		{
			name: "substrate with an infrastructure template resolves",
			task: workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
				ws.Provider = corev1alpha1.WorkspaceProviderSubstrate
				ws.TemplateRef = &corev1alpha1.WorkspaceTemplateReference{Name: "orka-codex-infra", Namespace: "ate-demo"}
			}),
			wantSession: "task:11111111-1111-1111-1111-111111111111",
		},
		{
			name: "substrate cross-namespace template fails under namespace isolation",
			task: workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
				ws.Provider = corev1alpha1.WorkspaceProviderSubstrate
				ws.TemplateRef = &corev1alpha1.WorkspaceTemplateReference{Name: "orka-codex-infra", Namespace: "ate-demo"}
			}),
			enforceNamespaceIsolation: true,
			wantErr:                   "cross-namespace execution workspace templateRef is not allowed",
		},
		{
			name:    "unknown provider fails closed",
			task:    workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) { ws.Provider = corev1alpha1.WorkspaceProvider("other") }),
			wantErr: "does not support ACP RuntimeSessions",
		},
		{
			name: "templateRef fails closed",
			task: workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
				ws.TemplateRef = &corev1alpha1.WorkspaceTemplateReference{Name: "operator-template"}
			}),
			wantErr: "templateRef must be omitted",
		},
		{
			name: "retain cleanup fails closed",
			task: workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
				ws.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicyRetain
			}),
			wantErr: "always deleted after authenticated drain",
		},
		{
			name:    "onDetach fails closed",
			task:    workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) { ws.OnDetach = corev1alpha1.WorkspaceOnDetachSuspend }),
			wantErr: "onDetach is not supported",
		},
		{
			name:    "boot fails closed",
			task:    workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) { ws.Boot = true }),
			wantErr: "not supported for ACP RuntimeSessions",
		},
		{
			name: "session reuse without sessionRef fails closed",
			task: workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
				ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
			}),
			wantErr: "requires spec.sessionRef.name",
		},
		{
			name: "task-scoped workspace with sessionRef fails closed",
			task: func() *corev1alpha1.Task {
				task := workspaceBindingTestTask(nil)
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "review-loop"}
				return task
			}(),
			wantErr: "reusePolicy none cannot be used with spec.sessionRef",
		},
		{
			name: "classRef fails closed",
			task: workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
				ws.Enabled = false
				ws.ClassRef = &corev1alpha1.WorkspaceClassReference{Name: "class"}
			}),
			wantErr: "controller-first Task workspace integration",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binding, err := resolveACPWorkspaceBinding(tt.task, corev1alpha1.WorkspaceProviderAgentSandbox, tt.enforceNamespaceIsolation, tt.sessionUID)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveACPWorkspaceBinding() error = %v", err)
			}
			if tt.wantNil {
				if binding != nil {
					t.Fatalf("binding = %#v, want nil", binding)
				}
				return
			}
			if binding == nil {
				t.Fatal("binding = nil, want resolved binding")
			}
			if binding.SessionKey != tt.wantSession {
				t.Fatalf("session key = %q, want %q", binding.SessionKey, tt.wantSession)
			}
			if binding.SessionUID != tt.sessionUID {
				t.Fatalf("session UID = %q, want %q", binding.SessionUID, tt.sessionUID)
			}
			if binding.CleanupPolicy != corev1alpha1.WorkspaceCleanupPolicyDelete || binding.WorkspaceSlot != "default" {
				t.Fatalf("binding defaults = %q/%q, want delete/default", binding.CleanupPolicy, binding.WorkspaceSlot)
			}
			if binding.Provider == corev1alpha1.WorkspaceProviderSubstrate &&
				(binding.TemplateNamespace == "" || binding.TemplateName == "") {
				t.Fatalf("substrate binding template = %q/%q, want frozen infrastructure template reference", binding.TemplateNamespace, binding.TemplateName)
			}
			digest, err := acpWorkspaceBindingDigest(binding)
			if err != nil || digest != binding.BindingDigest {
				t.Fatalf("binding digest = %q (err=%v), want canonical %q", binding.BindingDigest, err, digest)
			}
			if err := validateACPWorkspaceBindingValues(binding); err != nil {
				t.Fatalf("resolved binding failed re-verification: %v", err)
			}
		})
	}
}

func TestApplyACPWorkspaceBindingToPlanChangesPoolIdentity(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	configuration, err := resolveACPAgentSessionConfiguration(ctx, reconciler.Client, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	plainPlan, err := PlanACPRuntimeWithConfiguration(task, agent, reconciler.ACPRuntimeImages, configuration)
	if err != nil {
		t.Fatal(err)
	}

	binding, err := resolveACPWorkspaceBinding(workspaceBindingTestTask(nil), corev1alpha1.WorkspaceProviderAgentSandbox, false, "")
	if err != nil || binding == nil {
		t.Fatalf("resolveACPWorkspaceBinding() = %#v, %v", binding, err)
	}
	workspacePlan, err := applyACPWorkspaceBindingToPlan(plainPlan, binding)
	if err != nil {
		t.Fatal(err)
	}
	if workspacePlan.PoolName == plainPlan.PoolName {
		t.Fatal("workspace-backed plan reused the plain RuntimePool identity")
	}
	if !strings.HasPrefix(workspacePlan.PoolName, "acp-ws-codex-") {
		t.Fatalf("workspace pool name = %q, want acp-ws-codex- prefix", workspacePlan.PoolName)
	}
	if workspacePlan.Workspace == nil || workspacePlan.Workspace.BindingDigest != binding.BindingDigest {
		t.Fatalf("workspace plan binding = %#v, want %q", workspacePlan.Workspace, binding.BindingDigest)
	}

	again, err := applyACPWorkspaceBindingToPlan(plainPlan, binding)
	if err != nil || again.PoolName != workspacePlan.PoolName {
		t.Fatalf("workspace pool identity is not deterministic: %q vs %q (err=%v)", again.PoolName, workspacePlan.PoolName, err)
	}

	unchanged, err := applyACPWorkspaceBindingToPlan(plainPlan, nil)
	if err != nil || unchanged.PoolName != plainPlan.PoolName || unchanged.Workspace != nil {
		t.Fatalf("nil binding changed the plan: %#v (err=%v)", unchanged, err)
	}
}

func TestAgentExecutionBindingFreezesWorkspaceBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := workspaceBindingTestTask(nil)
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	reconciler.ExecutionWorkspaceDefaultProvider = corev1alpha1.WorkspaceProviderAgentSandbox

	live := task.DeepCopy()
	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, live, agent); err != nil || handled {
		t.Fatalf("ensure binding = handled=%v err=%v", handled, err)
	}
	bound := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, bound); err != nil {
		t.Fatal(err)
	}
	verified, err := reconciler.loadVerifiedBoundExecution(ctx, bound, bound.Status.AgentExecutionBinding)
	if err != nil {
		t.Fatalf("loadVerifiedBoundExecution() error = %v", err)
	}
	if verified.plan.Workspace == nil {
		t.Fatal("verified plan is missing the frozen workspace binding")
	}
	if !strings.HasPrefix(verified.plan.PoolName, "acp-ws-codex-") {
		t.Fatalf("verified pool name = %q, want workspace-backed pool identity", verified.plan.PoolName)
	}
	if verified.body.ExecutionWorkspace == nil ||
		verified.body.ExecutionWorkspace.BindingDigest != verified.plan.Workspace.BindingDigest {
		t.Fatalf("snapshot body workspace = %#v, want frozen binding digest", verified.body.ExecutionWorkspace)
	}
	if verified.plan.Workspace.SessionKey != "task:"+string(task.UID) {
		t.Fatalf("frozen session key = %q, want task-scoped key", verified.plan.Workspace.SessionKey)
	}
}

func TestAgentExecutionBindingFreezesImmutableWorkspaceSessionUID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
		ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
	})
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "review-loop", Create: true, Append: true}
	agent := bindingTestAgent()
	reconciler, durableStore := newBindingTestReconciler(t, task, bindingTestNamespace())
	reconciler.ExecutionWorkspaceDefaultProvider = corev1alpha1.WorkspaceProviderAgentSandbox
	reconciler.DurableControlStore = durableStore
	reconciler.SessionManager = NewSessionManager(durableStore)
	epochs := NewControllerEpochManager(durableStore, "workspace-session-binding-test")
	reconciler.ControllerEpochManager = epochs
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	defer func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("stop epoch manager: %v", err)
		}
	}()
	if _, err := epochs.CurrentFence(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, task.DeepCopy(), agent); err != nil || handled {
		t.Fatalf("ensure session-reused binding = handled=%v err=%v", handled, err)
	}
	control, err := durableStore.GetSessionControl(ctx, task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		t.Fatalf("load established SessionControl: %v", err)
	}
	bound := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), bound); err != nil {
		t.Fatal(err)
	}
	verified, err := reconciler.loadVerifiedBoundExecution(ctx, bound, bound.Status.AgentExecutionBinding)
	if err != nil {
		t.Fatal(err)
	}
	if verified.plan.Workspace == nil || verified.body.ExecutionWorkspace == nil {
		t.Fatal("session-reused execution snapshot is missing its workspace binding")
	}
	if verified.plan.Workspace.SessionUID != control.SessionUID ||
		verified.body.ExecutionWorkspace.SessionUID != control.SessionUID ||
		verified.plan.Workspace.SessionKey != "session:"+control.SessionUID {
		t.Fatalf("frozen workspace identity = plan %#v snapshot %#v, want Session UID %q", verified.plan.Workspace, verified.body.ExecutionWorkspace, control.SessionUID)
	}
}

func TestResolveAgentExecutionCandidateDoesNotCreateWorkspaceSessionBeforeValidation(t *testing.T) {
	ctx := context.Background()
	task := workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
		ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
	})
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "invalid-candidate", Create: true, Append: true}
	reconciler, durableStore := newBindingTestReconciler(t, task)
	reconciler.ExecutionWorkspaceDefaultProvider = corev1alpha1.WorkspaceProviderAgentSandbox
	reconciler.DurableControlStore = durableStore
	reconciler.SessionManager = NewSessionManager(durableStore)
	reconciler.ControllerEpochManager = NewControllerEpochManager(durableStore, "workspace-session-pure-candidate-test")

	if _, err := reconciler.resolveAgentExecutionCandidate(ctx, task, bindingTestAgent()); err == nil ||
		!strings.Contains(err.Error(), "resolve task namespace identity") {
		t.Fatalf("candidate error = %v, want namespace validation failure", err)
	}
	if _, err := durableStore.GetSessionControl(ctx, task.Namespace, task.Spec.SessionRef.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed candidate created SessionControl: %v", err)
	}
	if _, err := durableStore.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed candidate created transcript Session: %v", err)
	}
}

func TestAgentExecutionBindingDoesNotCreateWorkspaceSessionBeforeSnapshotPersistence(t *testing.T) {
	ctx := context.Background()
	task := workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
		ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
	})
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "snapshot-failure", Create: true, Append: true}
	reconciler, durableStore := newBindingTestReconciler(t, task, bindingTestNamespace())
	reconciler.ExecutionWorkspaceDefaultProvider = corev1alpha1.WorkspaceProviderAgentSandbox
	reconciler.DurableControlStore = durableStore
	reconciler.SessionManager = NewSessionManager(durableStore)
	reconciler.ControllerEpochManager = NewControllerEpochManager(durableStore, "workspace-session-snapshot-test")
	reconciler.AgentExecutionSnapshots = failingAgentExecutionSnapshotPersistStore{
		AgentExecutionSnapshotStore: reconciler.AgentExecutionSnapshots,
	}

	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, task.DeepCopy(), bindingTestAgent()); err != nil || !handled {
		t.Fatalf("snapshot failure binding result = handled=%v err=%v, want handled retry", handled, err)
	}
	if _, err := durableStore.GetSessionControl(ctx, task.Namespace, task.Spec.SessionRef.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("snapshot persistence failure created SessionControl: %v", err)
	}
	if _, err := durableStore.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("snapshot persistence failure created transcript Session: %v", err)
	}
}

func TestSessionWorkspacePoolIdentityRotatesWithSessionUID(t *testing.T) {
	ctx := context.Background()
	task := workspaceBindingTestTask(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
		ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
	})
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "review-loop"}
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	configuration, err := resolveACPAgentSessionConfiguration(ctx, reconciler.Client, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	plainPlan, err := PlanACPRuntimeWithConfiguration(task, agent, reconciler.ACPRuntimeImages, configuration)
	if err != nil {
		t.Fatal(err)
	}
	first, err := resolveACPWorkspaceBinding(task, corev1alpha1.WorkspaceProviderAgentSandbox, false, "session-incarnation-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolveACPWorkspaceBinding(task, corev1alpha1.WorkspaceProviderAgentSandbox, false, "session-incarnation-two")
	if err != nil {
		t.Fatal(err)
	}
	firstPlan, err := applyACPWorkspaceBindingToPlan(plainPlan, first)
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := applyACPWorkspaceBindingToPlan(plainPlan, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstPlan.PoolName == secondPlan.PoolName || first.BindingDigest == second.BindingDigest {
		t.Fatalf("recreated Session reused workspace identity: first=%s/%s second=%s/%s", firstPlan.PoolName, first.BindingDigest, secondPlan.PoolName, second.BindingDigest)
	}
}

func TestVerifiedSnapshotWorkspaceBindingRejectsTamperedIdentity(t *testing.T) {
	binding := &corev1alpha1.AgentExecutionBinding{
		Task: corev1alpha1.AgentExecutionBindingTaskRef{UID: types.UID("11111111-1111-1111-1111-111111111111")},
	}
	frozen, err := resolveACPWorkspaceBinding(workspaceBindingTestTask(nil), corev1alpha1.WorkspaceProviderAgentSandbox, false, "")
	if err != nil || frozen == nil {
		t.Fatalf("resolveACPWorkspaceBinding() = %#v, %v", frozen, err)
	}
	valid := agentExecutionSnapshotWorkspaceBinding{
		Provider:      string(frozen.Provider),
		ReusePolicy:   string(frozen.ReusePolicy),
		CleanupPolicy: string(frozen.CleanupPolicy),
		WorkspaceSlot: frozen.WorkspaceSlot,
		SessionUID:    frozen.SessionUID,
		SessionKey:    frozen.SessionKey,
		BindingDigest: frozen.BindingDigest,
	}

	if _, err := verifiedSnapshotWorkspaceBinding(binding, agentExecutionSnapshotBody{ExecutionWorkspace: &valid}); err != nil {
		t.Fatalf("valid frozen binding rejected: %v", err)
	}

	// A stale digest fails first; recompute a valid digest over the tampered
	// session key so the immutable Task-identity check is what rejects it.
	tamperedSession := valid
	tamperedSession.SessionKey = "task:another-task-uid"
	recomputed, err := acpWorkspaceBindingDigest(&ACPRuntimeWorkspaceBinding{
		Provider:      corev1alpha1.WorkspaceProvider(tamperedSession.Provider),
		ReusePolicy:   corev1alpha1.WorkspaceReusePolicy(tamperedSession.ReusePolicy),
		CleanupPolicy: corev1alpha1.WorkspaceCleanupPolicy(tamperedSession.CleanupPolicy),
		WorkspaceSlot: tamperedSession.WorkspaceSlot,
		SessionUID:    tamperedSession.SessionUID,
		SessionKey:    tamperedSession.SessionKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	tamperedSession.BindingDigest = recomputed
	if _, err := verifiedSnapshotWorkspaceBinding(binding, agentExecutionSnapshotBody{ExecutionWorkspace: &tamperedSession}); err == nil ||
		!strings.Contains(err.Error(), "session key") {
		t.Fatalf("tampered session key error = %v, want session key mismatch", err)
	}

	tamperedDigest := valid
	tamperedDigest.CleanupPolicy = string(corev1alpha1.WorkspaceCleanupPolicyRetain)
	if _, err := verifiedSnapshotWorkspaceBinding(binding, agentExecutionSnapshotBody{ExecutionWorkspace: &tamperedDigest}); err == nil {
		t.Fatal("tampered cleanup policy passed verification")
	}
}

func TestEnsureACPRuntimePoolCreatesWorkspaceBackedPool(t *testing.T) {
	ctx := context.Background()
	task := workspaceBindingTestTask(nil)
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	configuration, err := resolveACPAgentSessionConfiguration(ctx, reconciler.Client, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanACPRuntimeWithConfiguration(task, agent, reconciler.ACPRuntimeImages, configuration)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBinding(task, corev1alpha1.WorkspaceProviderAgentSandbox, false, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err = applyACPWorkspaceBindingToPlan(plan, binding)
	if err != nil {
		t.Fatal(err)
	}

	pool, preexisting, err := reconciler.ensureACPRuntimePool(ctx, task.Namespace, plan)
	if err != nil {
		t.Fatalf("ensureACPRuntimePool() error = %v", err)
	}
	if preexisting {
		t.Fatal("new workspace RuntimePool reported as preexisting")
	}
	if pool.Spec.ExecutionWorkspace == nil ||
		pool.Spec.ExecutionWorkspace.Provider != corev1alpha1.WorkspaceProviderAgentSandbox ||
		pool.Spec.ExecutionWorkspace.BindingDigest != binding.BindingDigest {
		t.Fatalf("pool executionWorkspace = %#v, want frozen binding", pool.Spec.ExecutionWorkspace)
	}
	if pool.Spec.Capacity == nil || pool.Spec.Capacity.MaxResidentSessions != 1 || pool.Spec.Capacity.MaxRunningPrompts != 1 {
		t.Fatalf("pool capacity = %#v, want single-session 1/1", pool.Spec.Capacity)
	}
	if pool.Labels[acpRuntimeWorkspaceProviderLabel] != string(corev1alpha1.WorkspaceProviderAgentSandbox) {
		t.Fatalf("pool labels = %#v, want workspace provider label", pool.Labels)
	}
	reattached, preexisting, err := reconciler.ensureACPRuntimePool(ctx, task.Namespace, plan)
	if err != nil {
		t.Fatalf("reattach workspace RuntimePool: %v", err)
	}
	if !preexisting || reattached.UID != pool.UID {
		t.Fatalf("reattached pool = %s/%t, want existing UID %s", reattached.UID, preexisting, pool.UID)
	}

	// A frozen plain plan must never bind to a workspace-backed pool.
	plainPlan := plan
	plainPlan.Workspace = nil
	if _, _, err := reconciler.ensureACPRuntimePool(ctx, task.Namespace, plainPlan); err == nil ||
		!strings.Contains(err.Error(), "execution workspace binding does not match") {
		t.Fatalf("mismatched pool binding error = %v, want exact-binding rejection", err)
	}
}

func TestACPRuntimePoolWorkspaceMatchesPlanRequiresExactProviderFields(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	plan := ACPRuntimePlan{Workspace: &ACPRuntimeWorkspaceBinding{
		Provider: corev1alpha1.WorkspaceProviderSubstrate, BindingDigest: digest,
		TemplateNamespace: "ate-demo", TemplateName: "orka-codex-infra",
	}}
	pool := &corev1alpha1.RuntimePool{Spec: corev1alpha1.RuntimePoolSpec{
		ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
			Provider: corev1alpha1.WorkspaceProviderSubstrate, BindingDigest: digest,
			Substrate: &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
				BaseTemplateNamespace: "ate-demo", BaseTemplateName: "orka-codex-infra",
			},
		},
	}}
	if !acpRuntimePoolWorkspaceMatchesPlan(pool, plan) {
		t.Fatal("exact Substrate workspace binding did not match")
	}

	pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateName = "other-infrastructure"
	if acpRuntimePoolWorkspaceMatchesPlan(pool, plan) {
		t.Fatal("Substrate workspace binding ignored the infrastructure template name")
	}
	pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateName = plan.Workspace.TemplateName
	pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateNamespace = "other-namespace"
	if acpRuntimePoolWorkspaceMatchesPlan(pool, plan) {
		t.Fatal("Substrate workspace binding ignored the infrastructure template namespace")
	}
}

func TestReapIdlePoolsDeletesStoppedWorkspacePools(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stopped := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "acp-ws-codex-0123456789abcdef", UID: types.UID("ws-pool-uid"), Generation: 1,
			Annotations: map[string]string{acpRuntimeLastDemandAnnotation: now.Add(-3 * time.Hour).Format(time.RFC3339Nano)},
		},
		Spec: corev1alpha1.RuntimePoolSpec{
			DesiredReplicas: 0,
			ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
				Provider:      corev1alpha1.WorkspaceProviderAgentSandbox,
				BindingDigest: "sha256:" + strings.Repeat("9", 64),
			},
		},
		Status: corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleStopped, ObservedGeneration: 1},
	}
	plain := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "acp-codex-0123456789abcdef", UID: types.UID("plain-pool-uid"),
			Annotations: map[string]string{acpRuntimeLastDemandAnnotation: now.Add(-3 * time.Hour).Format(time.RFC3339Nano)},
		},
		Spec:   corev1alpha1.RuntimePoolSpec{DesiredReplicas: 0},
		Status: corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleStopped},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RuntimePool{}).
		WithObjects(stopped, plain).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "ws-pool-gc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	epochs := NewControllerEpochManager(controlStore, "ws-pool-gc-controller")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := epochs.CurrentFence(ctx); err != nil {
		t.Fatal(err)
	}

	dispatcher := &ACPDispatcher{Client: kubeClient, Epochs: epochs, IdlePoolTTL: time.Minute}
	if err := dispatcher.reapIdlePools(ctx, nil); err != nil {
		t.Fatal(err)
	}

	got := &corev1alpha1.RuntimePool{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: stopped.Name}, got); err == nil {
		t.Fatal("stopped idle workspace pool was not garbage collected")
	}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: plain.Name}, got); err != nil {
		t.Fatalf("plain stopped pool must be retained for reuse: %v", err)
	}

	cancelEpoch()
	if err := <-epochDone; err != nil {
		t.Fatal(err)
	}
}

func TestProjectACPExecutionWorkspaceStatusTransitions(t *testing.T) {
	scheme := bindingTestScheme(t)
	task := workspaceBindingTestTask(nil)
	task.Labels = map[string]string{acpRuntimeTaskPoolLabel: "acp-ws-codex-0123456789abcdef"}
	task.Status = corev1alpha1.TaskStatus{
		Phase: corev1alpha1.TaskPhaseRunning,
		Execution: &corev1alpha1.TaskExecutionStatus{
			State:           corev1alpha1.TaskExecutionStateRunning,
			RuntimePoolName: "acp-ws-codex-0123456789abcdef",
		},
		ExecutionWorkspace: &corev1alpha1.ExecutionWorkspaceStatus{
			Provider: corev1alpha1.WorkspaceProviderAgentSandbox,
			Phase:    corev1alpha1.ExecutionWorkspacePhasePending,
			Reason:   corev1alpha1.ExecutionWorkspaceReasonPending,
			Reused:   true,
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	reconciler := &TaskReconciler{Client: kubeClient, Scheme: scheme}
	ctx := context.Background()

	if err := reconciler.projectACPExecutionWorkspaceStatus(ctx, task); err != nil {
		t.Fatalf("project running: %v", err)
	}
	if task.Status.ExecutionWorkspace.Phase != corev1alpha1.ExecutionWorkspacePhaseReady ||
		task.Status.ExecutionWorkspace.Reason != corev1alpha1.ExecutionWorkspaceReasonReady ||
		!task.Status.ExecutionWorkspace.Reused {
		t.Fatalf("running projection = %q/%q, want Ready/WorkspaceReady", task.Status.ExecutionWorkspace.Phase, task.Status.ExecutionWorkspace.Reason)
	}

	task.Status.Execution.State = corev1alpha1.TaskExecutionStateSucceeded
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateValidating}
	if err := reconciler.projectACPExecutionWorkspaceStatus(ctx, task); err != nil {
		t.Fatalf("project successful execution with nonterminal delivery: %v", err)
	}
	if task.Status.ExecutionWorkspace.Phase != corev1alpha1.ExecutionWorkspacePhaseReady {
		t.Fatalf("nonterminal delivery projection = %q, want Ready", task.Status.ExecutionWorkspace.Phase)
	}

	task.Status.Delivery.State = corev1alpha1.TaskDeliveryStateVerifiedExact
	if err := reconciler.projectACPExecutionWorkspaceStatus(ctx, task); err != nil {
		t.Fatalf("project terminal delivery: %v", err)
	}
	if task.Status.ExecutionWorkspace.Phase != corev1alpha1.ExecutionWorkspacePhaseReleased ||
		task.Status.ExecutionWorkspace.Reason != corev1alpha1.ExecutionWorkspaceReasonReleased {
		t.Fatalf("terminal projection = %q/%q, want Released/WorkspaceReleased", task.Status.ExecutionWorkspace.Phase, task.Status.ExecutionWorkspace.Reason)
	}

	// A Failed projection is never overridden.
	task.Status.ExecutionWorkspace.Phase = corev1alpha1.ExecutionWorkspacePhaseFailed
	task.Status.ExecutionWorkspace.Reason = corev1alpha1.ExecutionWorkspaceReasonValidationFailed
	if err := reconciler.projectACPExecutionWorkspaceStatus(ctx, task); err != nil {
		t.Fatalf("project failed: %v", err)
	}
	if task.Status.ExecutionWorkspace.Phase != corev1alpha1.ExecutionWorkspacePhaseFailed {
		t.Fatal("Failed workspace projection was overridden")
	}
}
