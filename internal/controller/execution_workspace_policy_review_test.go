package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

const workspacePolicyReviewProfileHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestExecutionWorkspaceClassReadinessPolicyMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mutate     func(*workspacev1alpha1.ExecutionWorkspaceClass, *workspacev1alpha1.ExecutionWorkspace)
		wantReason string
	}{
		{
			name: "new workspace requires pinned profile hash",
			mutate: func(class *workspacev1alpha1.ExecutionWorkspaceClass, _ *workspacev1alpha1.ExecutionWorkspace) {
				class.Status.ProfileHash = ""
			},
			wantReason: "ClassNotReady",
		},
		{
			name: "new workspace requires current class status",
			mutate: func(class *workspacev1alpha1.ExecutionWorkspaceClass, _ *workspacev1alpha1.ExecutionWorkspace) {
				class.Status.ObservedGeneration = class.Generation - 1
			},
			wantReason: "ClassNotReady",
		},
		{
			name: "new workspace requires ready condition",
			mutate: func(class *workspacev1alpha1.ExecutionWorkspaceClass, _ *workspacev1alpha1.ExecutionWorkspace) {
				class.Status.Conditions = nil
			},
			wantReason: "ClassNotReady",
		},
		{
			name: "new workspace rejects stale ready condition",
			mutate: func(class *workspacev1alpha1.ExecutionWorkspaceClass, _ *workspacev1alpha1.ExecutionWorkspace) {
				class.Status.Conditions[0].ObservedGeneration = class.Generation - 1
			},
			wantReason: "ClassNotReady",
		},
		{
			name: "new workspace rejects false ready condition",
			mutate: func(class *workspacev1alpha1.ExecutionWorkspaceClass, _ *workspacev1alpha1.ExecutionWorkspace) {
				class.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			wantReason: "ClassNotReady",
		},
		{
			name:       "new workspace accepts current ready pinned class",
			mutate:     func(*workspacev1alpha1.ExecutionWorkspaceClass, *workspacev1alpha1.ExecutionWorkspace) {},
			wantReason: string(workspacev1alpha1.ReasonReady),
		},
		{
			name: "provisioned workspace preserves pinned class while class is not ready",
			mutate: func(class *workspacev1alpha1.ExecutionWorkspaceClass, workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
				class.Status.ObservedGeneration = class.Generation - 1
				class.Status.Conditions[0].ObservedGeneration = class.Generation - 1
				class.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			wantReason: string(workspacev1alpha1.ReasonReady),
		},
		{
			name: "provisioned workspace still requires pinned profile hash",
			mutate: func(class *workspacev1alpha1.ExecutionWorkspaceClass, workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
				class.Status.ProfileHash = ""
			},
			wantReason: "ClassNotReady",
		},
		{
			name: "provisioned workspace rejects profile mismatch",
			mutate: func(class *workspacev1alpha1.ExecutionWorkspaceClass, workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
				class.Status.ProfileHash = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			},
			wantReason: "ClassProfileMismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			class, provider, workspace := workspacePolicyReviewFixture(t)
			tt.mutate(class, workspace)
			reason := validateWorkspacePolicyReviewBindings(t, class, provider, workspace)
			if reason != tt.wantReason {
				t.Fatalf("validateWorkspaceBindings reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestExecutionWorkspaceReuseScopePolicyMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		allowed    []workspacev1alpha1.WorkspaceReuseScope
		session    bool
		wantReason string
	}{
		{
			name:       "none allowed",
			allowed:    []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeNone},
			wantReason: string(workspacev1alpha1.ReasonReady),
		},
		{
			name:       "none rejected when only session allowed",
			allowed:    []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeSession},
			wantReason: "ReuseScopeNotAllowed",
		},
		{
			name:       "session allowed",
			allowed:    []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeSession},
			session:    true,
			wantReason: string(workspacev1alpha1.ReasonReady),
		},
		{
			name:       "session rejected when only none allowed",
			allowed:    []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeNone},
			session:    true,
			wantReason: "ReuseScopeNotAllowed",
		},
		{
			name: "both allow none",
			allowed: []workspacev1alpha1.WorkspaceReuseScope{
				workspacev1alpha1.WorkspaceReuseScopeNone,
				workspacev1alpha1.WorkspaceReuseScopeSession,
			},
			wantReason: string(workspacev1alpha1.ReasonReady),
		},
		{
			name: "both allow session",
			allowed: []workspacev1alpha1.WorkspaceReuseScope{
				workspacev1alpha1.WorkspaceReuseScopeNone,
				workspacev1alpha1.WorkspaceReuseScopeSession,
			},
			session:    true,
			wantReason: string(workspacev1alpha1.ReasonReady),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			class, provider, workspace := workspacePolicyReviewFixture(t)
			class.Spec.AllowedReuseScopes = tt.allowed
			if tt.session {
				workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
					Name: "session",
					UID:  types.UID("session-uid"),
				}
			}
			reason := validateWorkspacePolicyReviewBindings(t, class, provider, workspace)
			if reason != tt.wantReason {
				t.Fatalf("validateWorkspaceBindings reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestExecutionWorkspaceProviderLifecyclePolicyMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		lifecycle      workspacev1alpha1.ExecutionWorkspaceProviderLifecycleState
		workspaceState workspacev1alpha1.ExecutionWorkspaceState
		classReady     bool
		wantReason     string
	}{
		{
			name:       "active accepts new workspace",
			lifecycle:  workspacev1alpha1.ExecutionWorkspaceProviderActive,
			classReady: true,
			wantReason: string(workspacev1alpha1.ReasonReady),
		},
		{
			name:           "active preserves provisioned workspace",
			lifecycle:      workspacev1alpha1.ExecutionWorkspaceProviderActive,
			workspaceState: workspacev1alpha1.ExecutionWorkspaceStateReady,
			classReady:     true,
			wantReason:     string(workspacev1alpha1.ReasonReady),
		},
		{
			name:       "draining rejects new workspace",
			lifecycle:  workspacev1alpha1.ExecutionWorkspaceProviderDraining,
			classReady: true,
			wantReason: string(workspacev1alpha1.ReasonProviderDraining),
		},
		{
			name:           "draining preserves provisioned workspace after class becomes not ready",
			lifecycle:      workspacev1alpha1.ExecutionWorkspaceProviderDraining,
			workspaceState: workspacev1alpha1.ExecutionWorkspaceStateSuspended,
			wantReason:     string(workspacev1alpha1.ReasonReady),
		},
		{
			name:       "disabled rejects new workspace",
			lifecycle:  workspacev1alpha1.ExecutionWorkspaceProviderDisabled,
			classReady: true,
			wantReason: string(workspacev1alpha1.ReasonProviderDisabled),
		},
		{
			name:           "disabled rejects provisioned workspace even after class becomes not ready",
			lifecycle:      workspacev1alpha1.ExecutionWorkspaceProviderDisabled,
			workspaceState: workspacev1alpha1.ExecutionWorkspaceStateReady,
			wantReason:     string(workspacev1alpha1.ReasonProviderDisabled),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			class, provider, workspace := workspacePolicyReviewFixture(t)
			provider.Spec.LifecycleState = tt.lifecycle
			workspace.Status.State = tt.workspaceState
			if !tt.classReady {
				class.Status.Conditions[0].Status = metav1.ConditionFalse
			}
			reason := validateWorkspacePolicyReviewBindings(t, class, provider, workspace)
			if reason != tt.wantReason {
				t.Fatalf("validateWorkspaceBindings reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestExecutionWorkspaceDisabledProviderMaintenanceIntentMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		desired     workspacev1alpha1.ExecutionWorkspaceDesiredState
		wantDesired workspacev1alpha1.ExecutionWorkspaceDesiredState
	}{
		{
			name:        "ready intent is quarantined",
			desired:     workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			wantDesired: workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined,
		},
		{
			name:        "delete intent is preserved",
			desired:     workspacev1alpha1.ExecutionWorkspaceDesiredDeleted,
			wantDesired: workspacev1alpha1.ExecutionWorkspaceDesiredDeleted,
		},
		{
			name:        "quarantine intent is preserved",
			desired:     workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined,
			wantDesired: workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			class, provider, workspace := workspacePolicyReviewFixture(t)
			provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDisabled
			class.Status.Conditions[0].Status = metav1.ConditionFalse
			workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
			workspace.Spec.DesiredState = tt.desired
			workspace.Finalizers = []string{executionWorkspaceFinalizer}

			c := fake.NewClientBuilder().
				WithScheme(testWorkspaceScheme(t)).
				WithStatusSubresource(workspace).
				WithObjects(class, provider, workspace).
				Build()
			reconciler := &ExecutionWorkspaceReconciler{Client: c}
			request := ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: workspace.Namespace,
				Name:      workspace.Name,
			}}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			got := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(context.Background(), request.NamespacedName, got); err != nil {
				t.Fatalf("get workspace: %v", err)
			}
			if got.Spec.DesiredState != tt.wantDesired {
				t.Fatalf("desired state = %q, want %q", got.Spec.DesiredState, tt.wantDesired)
			}
			admittedConditionFound := false
			admitted := false
			for i := range got.Status.Conditions {
				condition := got.Status.Conditions[i]
				if condition.Type != string(workspacev1alpha1.ConditionWorkspaceAdmitted) {
					continue
				}
				admittedConditionFound = true
				admitted = condition.Status == metav1.ConditionTrue
				if condition.Reason != string(workspacev1alpha1.ReasonProviderDisabled) {
					t.Fatalf("admission reason = %q, want %q", condition.Reason, workspacev1alpha1.ReasonProviderDisabled)
				}
			}
			if !admittedConditionFound {
				t.Fatal("workspace admission condition was not written")
			}
			if admitted {
				t.Fatal("disabled provider left workspace admitted")
			}
		})
	}
}

func TestExecutionWorkspaceDeletionFinalizerDispositionMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		mode          workspacev1alpha1.ExecutionWorkspaceMode
		state         workspacev1alpha1.ExecutionWorkspaceState
		disposition   *workspacev1alpha1.ExecutionWorkspaceDisposition
		staleStatus   bool
		wantFinalizer bool
	}{
		{
			name:          "waits for deleted state",
			mode:          workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state:         workspacev1alpha1.ExecutionWorkspaceStateDeleting,
			disposition:   workspacePolicyReviewValidDisposition(),
			wantFinalizer: true,
		},
		{
			name:          "stale deleted observation",
			mode:          workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state:         workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition:   workspacePolicyReviewValidDisposition(),
			staleStatus:   true,
			wantFinalizer: true,
		},
		{
			name:          "missing disposition",
			mode:          workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state:         workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			wantFinalizer: true,
		},
		{
			name:  "pending disposition",
			mode:  workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state: workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition: workspacePolicyReviewDispositionWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.Compute = workspacev1alpha1.DispositionPending
			}),
			wantFinalizer: true,
		},
		{
			name:  "failed disposition",
			mode:  workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state: workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition: workspacePolicyReviewDispositionWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.EphemeralSecrets = workspacev1alpha1.DispositionFailed
			}),
			wantFinalizer: true,
		},
		{
			name:  "persistent volume policy mismatch",
			mode:  workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state: workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition: workspacePolicyReviewDispositionWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.PersistentVolumes = workspacev1alpha1.DispositionDeleted
			}),
			wantFinalizer: true,
		},
		{
			name:  "checkpoint policy mismatch",
			mode:  workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state: workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition: workspacePolicyReviewDispositionWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.Checkpoints = workspacev1alpha1.DispositionRetained
			}),
			wantFinalizer: true,
		},
		{
			name:  "provider resource policy mismatch",
			mode:  workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state: workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition: workspacePolicyReviewDispositionWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.ProviderResources = workspacev1alpha1.DispositionRetained
			}),
			wantFinalizer: true,
		},
		{
			name:  "interactive credentials cannot be not applicable",
			mode:  workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state: workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition: workspacePolicyReviewDispositionWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.AccessCredentials = workspacev1alpha1.DispositionNotApplicable
			}),
			wantFinalizer: true,
		},
		{
			name:  "service credentials may be not applicable",
			mode:  workspacev1alpha1.ExecutionWorkspaceModeService,
			state: workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition: workspacePolicyReviewDispositionWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.AccessCredentials = workspacev1alpha1.DispositionNotApplicable
			}),
		},
		{
			name:        "valid interactive terminal disposition",
			mode:        workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			state:       workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			disposition: workspacePolicyReviewValidDisposition(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workspace := testBoundWorkspace(
				t,
				"policy-review",
				"deletion-workspace",
				"policy-class",
				"policy-provider",
			)
			workspace.Finalizers = []string{executionWorkspaceFinalizer}
			deletionTimestamp := metav1.Now()
			workspace.DeletionTimestamp = &deletionTimestamp
			workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
			workspace.Spec.Mode = tt.mode
			workspace.Status.State = tt.state
			workspace.Status.ObservedGeneration = workspace.Generation
			if tt.staleStatus {
				workspace.Status.ObservedGeneration = workspace.Generation - 1
			}
			workspace.Status.Disposition = tt.disposition

			c := fake.NewClientBuilder().
				WithScheme(testWorkspaceScheme(t)).
				WithStatusSubresource(workspace).
				WithObjects(workspace).
				Build()
			reconciler := &ExecutionWorkspaceReconciler{Client: c}
			result, err := reconciler.reconcileWorkspaceDeletion(context.Background(), workspace)
			if err != nil {
				t.Fatalf("reconcileWorkspaceDeletion: %v", err)
			}

			got := &workspacev1alpha1.ExecutionWorkspace{}
			err = c.Get(context.Background(), types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, got)
			if tt.wantFinalizer {
				if err != nil {
					t.Fatalf("get workspace retaining finalizer: %v", err)
				}
				if !controllerutil.ContainsFinalizer(got, executionWorkspaceFinalizer) {
					t.Fatal("workspace finalizer was removed before cleanup was validated")
				}
				if result.RequeueAfter != workspaceRequeueInterval {
					t.Fatalf("requeue after = %v, want %v", result.RequeueAfter, workspaceRequeueInterval)
				}
				return
			}
			if err != nil && !apierrors.IsNotFound(err) {
				t.Fatalf("get finalized workspace: %v", err)
			}
			if err == nil && controllerutil.ContainsFinalizer(got, executionWorkspaceFinalizer) {
				t.Fatal("workspace finalizer remains after valid terminal cleanup disposition")
			}
			if result.RequeueAfter != 0 {
				t.Fatalf("result = %#v, want no requeue after finalization", result)
			}
		})
	}
}

func workspacePolicyReviewFixture(
	t *testing.T,
) (*workspacev1alpha1.ExecutionWorkspaceClass, *workspacev1alpha1.ExecutionWorkspaceProvider, *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	class := testGenericClass("default", "class", "provider")
	provider := testGenericProvider("provider")
	workspace := testBoundWorkspace(t, "default", "workspace", class.Name, provider.Name)
	class.Status.ObservedGeneration = class.Generation
	class.Status.ProfileHash = workspacePolicyReviewProfileHash
	class.Status.Conditions = []metav1.Condition{{
		Type:               string(workspacev1alpha1.ConditionClassReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: class.Generation,
	}}
	workspace.Spec.ClassBinding.ProfileHash = class.Status.ProfileHash
	return class, provider, workspace
}

func validateWorkspacePolicyReviewBindings(
	t *testing.T,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) string {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithObjects(class, provider).
		Build()
	reconciler := &ExecutionWorkspaceReconciler{Client: c}
	reason, _, err := reconciler.validateWorkspaceBindings(context.Background(), workspace)
	if err != nil {
		t.Fatalf("validateWorkspaceBindings: %v", err)
	}
	return reason
}

func workspacePolicyReviewValidDisposition() *workspacev1alpha1.ExecutionWorkspaceDisposition {
	return &workspacev1alpha1.ExecutionWorkspaceDisposition{
		Compute:           workspacev1alpha1.DispositionDeleted,
		AccessCredentials: workspacev1alpha1.DispositionRevoked,
		EphemeralSecrets:  workspacev1alpha1.DispositionDeleted,
		WorkspaceData:     workspacev1alpha1.DispositionDeleted,
		PersistentVolumes: workspacev1alpha1.DispositionRetained,
		Checkpoints:       workspacev1alpha1.DispositionDeleted,
		ProviderResources: workspacev1alpha1.DispositionDeleted,
	}
}

func workspacePolicyReviewDispositionWith(
	mutate func(*workspacev1alpha1.ExecutionWorkspaceDisposition),
) *workspacev1alpha1.ExecutionWorkspaceDisposition {
	disposition := workspacePolicyReviewValidDisposition()
	mutate(disposition)
	return disposition
}
