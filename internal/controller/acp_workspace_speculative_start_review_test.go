package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	"github.com/orka-agents/orka/internal/operatoraccess"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var speculativeTestRevision = "sha256:" + strings.Repeat("e", 64)

func speculativeTestIdentity() workspacev1alpha1.InstanceIdentity {
	return workspacev1alpha1.InstanceIdentity{AllocationID: "allocation", InstanceID: "speculative-instance", RequestRevision: speculativeTestRevision}
}

// retireSpeculativeWorkload stands in for the RuntimePool controller and the
// provider: the booted speculative request is retired with action and the
// provider reports it stopped, with retained data only for a Suspend.
func (f *speculativeTaskFixture) retireSpeculativeWorkload(t *testing.T, action workspacev1alpha1.WorkloadRetirementAction) {
	t.Helper()
	identity := speculativeTestIdentity()
	f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Retirement = &workspacev1alpha1.WorkloadRetirement{Sequence: 1, Identity: identity, Action: action}
	})
	workspace := f.workspace(t)
	admitTestACPWorkspace(t, f.r, workspace)
	f.patchStatus(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStatePending
		w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 1, Key: w.Spec.Workload.Key, Identity: identity, State: workspacev1alpha1.AllocationStopped}
		if action == workspacev1alpha1.WorkloadRetirementSuspend {
			w.Status.Allocation.RetainedData = &workspacev1alpha1.RetainedDataReference{ID: "tag", SourceInstance: identity, ProofSHA256: "sha256:" + strings.Repeat("d", 64)}
		}
	})
}

// A class that disallows Suspend would retire an unclaimed first turn with
// Stop and no retained data, so it keeps the attach-first path.
func TestNonSuspendClassFirstTurnNeverSpeculates(t *testing.T) {
	t.Parallel()
	f := newSpeculativeTaskFixtureFor(t, newACPClassFixture(t, RuntimeProviderBackendSubstrate), nil)
	if acpWorkspaceSuspendCapable(f.workspace(t)) {
		t.Fatal("fixture class allows Suspend")
	}
	f.start(t, f.task, true, "")
	if _, present := f.workspace(t).Annotations[acpWorkspaceSpeculativeStartAnnotation]; present {
		t.Fatal("a non-Suspend class recorded a speculative start")
	}
	if _, exists := f.pool(t); exists {
		t.Fatal("a non-Suspend class created RuntimePool demand before its attachment")
	}
	if _, ready := attachTestACPWorkspace(t, f.r, f.task, f.plan, f.key.Name); !ready {
		t.Fatal("the attach-first path did not attach the first turn")
	}
}

func TestExpiredSpeculativeStartNeverStallsItsTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	expire := func(t *testing.T, f *speculativeTaskFixture) {
		t.Helper()
		f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(time.Now().Add(-time.Second), f.task.UID)
		})
	}

	t.Run("expiry retires with Suspend and the requester attaches", func(t *testing.T) {
		t.Parallel()
		f := newSpeculativeTaskFixture(t)
		f.start(t, f.task, true, "")
		f.bootSpeculativeWorkload(t)
		expire(t, f)
		demand := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.APIReader}
		if _, err := demand.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
			t.Fatal(err)
		}
		if pool, _ := f.pool(t); pool.Spec.DesiredReplicas != 0 {
			t.Fatal("an expired speculative start kept its pool")
		}
		// The retirement is authorized but still running: the requester waits.
		f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Retirement = &workspacev1alpha1.WorkloadRetirement{Sequence: 1, Identity: speculativeTestIdentity(), Action: workspacev1alpha1.WorkloadRetirementSuspend}
		})
		if err := validateACPClassWorkspaceAttachmentAttempt(f.workspace(t), f.task, f.plan.Workspace, f.plan.PoolName, time.Now()); err == nil {
			t.Fatal("attached a request whose retirement is still running")
		}
		f.retireSpeculativeWorkload(t, workspacev1alpha1.WorkloadRetirementSuspend)
		if _, ready := attachTestACPWorkspace(t, f.r, f.task, f.plan, f.key.Name); !ready {
			t.Fatal("the requester could not attach the retired speculative request")
		}
		if attached := f.workspace(t); attached.Spec.Attachment == nil || attached.Spec.Attachment.TaskRef.UID != f.task.UID {
			t.Fatalf("attachment = %#v, want the requester", attached.Spec.Attachment)
		}
	})

	t.Run("an attach that beats expiry keeps the pool", func(t *testing.T) {
		t.Parallel()
		f := newSpeculativeTaskFixture(t)
		f.start(t, f.task, true, "")
		f.bootSpeculativeWorkload(t)
		expire(t, f)
		// An expired start whose request still boots stays attachable.
		if _, ready := attachTestACPWorkspace(t, f.r, f.task, f.plan, f.key.Name); !ready {
			t.Fatal("the requester could not attach its booting request after the deadline")
		}
		stale := interceptor.NewClient(f.r.APIReader.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if workspace, ok := obj.(*workspacev1alpha1.ExecutionWorkspace); ok {
					workspace.Spec.Attachment = nil
				}
				return nil
			},
		})
		later := time.Now().Add(time.Hour)
		demand := &WorkspaceRuntimePoolReconciler{Client: stale, APIReader: f.r.APIReader, Now: func() time.Time { return later }}
		if _, err := demand.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
			t.Fatal(err)
		}
		if pool, _ := f.pool(t); pool.Spec.DesiredReplicas != 1 {
			t.Fatal("a stale view of an expired start scaled down the attached runtime")
		}
	})

	t.Run("a stop without retained data fails the Task clearly", func(t *testing.T) {
		t.Parallel()
		f := newSpeculativeTaskFixture(t)
		f.start(t, f.task, true, "")
		f.bootSpeculativeWorkload(t)
		expire(t, f)
		f.retireSpeculativeWorkload(t, workspacev1alpha1.WorkloadRetirementStop)
		_, ready, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan)
		if ready || !errors.Is(err, errACPWorkspaceTerminalFailure) || !strings.Contains(err.Error(), "retry the Task") {
			t.Fatalf("ensure = (%v, %v), want a terminal failure that asks for a retry", ready, err)
		}
		// The failed first turn's settlement deletes the empty incarnation,
		// so the retry gets a fresh workspace.
		if done, err := f.r.settleACPClassWorkspace(ctx, f.reloadTask(t, f.task)); err != nil || !done {
			t.Fatalf("settle = (%v, %v)", done, err)
		}
		remaining := &workspacev1alpha1.ExecutionWorkspace{}
		if err := f.r.Get(ctx, f.key, remaining); err == nil {
			if remaining.DeletionTimestamp.IsZero() {
				t.Fatal("settlement kept the workspace that cannot be continued")
			}
		} else if !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	})

	t.Run("a committed lineage that lost its retained data asks for a new Session", func(t *testing.T) {
		t.Parallel()
		f := newSpeculativeTaskFixture(t)
		f.start(t, f.task, true, "")
		f.bootSpeculativeWorkload(t)
		f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
		})
		f.retireSpeculativeWorkload(t, workspacev1alpha1.WorkloadRetirementStop)
		if _, _, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan); !errors.Is(err, errACPWorkspaceTerminalFailure) ||
			!strings.Contains(err.Error(), "start a new Session") {
			t.Fatalf("ensure error = %v, want a terminal failure that asks for a new Session", err)
		}
	})

	t.Run("a taint during speculation fails the Task clearly", func(t *testing.T) {
		t.Parallel()
		f := newSpeculativeTaskFixture(t)
		f.start(t, f.task, true, "")
		f.bootSpeculativeWorkload(t)
		f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) { w.Annotations[operatoraccess.TaintAnnotation] = "x" })
		if _, _, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan); !errors.Is(err, errACPWorkspaceTerminalFailure) ||
			!errors.Is(err, operatoraccess.ErrTainted) {
			t.Fatalf("ensure error = %v, want a terminal taint failure", err)
		}
	})

	t.Run("a non-Suspend start left by an earlier build and stopped still attaches", func(t *testing.T) {
		t.Parallel()
		f := newSpeculativeTaskFixtureFor(t, newACPClassFixture(t, RuntimeProviderBackendSubstrate), nil)
		f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(time.Now().Add(-time.Second), f.task.UID)
		})
		f.bootSpeculativeWorkload(t)
		f.retireSpeculativeWorkload(t, workspacev1alpha1.WorkloadRetirementStop)
		if _, ready := attachTestACPWorkspace(t, f.r, f.task, f.plan, f.key.Name); !ready {
			t.Fatal("a stopped legacy speculative request stranded its requester")
		}
	})
}
