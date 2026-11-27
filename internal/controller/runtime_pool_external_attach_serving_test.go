package controller

import (
	"context"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/operatoraccess"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func servedExternalPool(t *testing.T) *externalRuntimePoolFixture {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	f.serve(t)
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Annotations[externalRuntimeServedAnnotation] == "" || !externalRuntimePoolAdmitting(&pool) {
		t.Fatalf("the serving pool recorded no admitted state: %s/%s", pool.Status.Lifecycle, pool.Status.AdmissionState)
	}
	return f
}

func (f *externalRuntimePoolFixture) attachWithGenerationLag(t *testing.T, mutate func(*workspacev1alpha1.ExecutionWorkspace)) {
	t.Helper()
	workspace := f.currentWorkspace(t)
	status := workspace.Status.DeepCopy()
	workspace.Spec.AttachmentEpoch++
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{Epoch: workspace.Spec.AttachmentEpoch,
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: "attaching", UID: "attaching-uid"}}
	workspace.Generation++
	if mutate != nil {
		mutate(workspace)
	}
	if err := f.r.Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	workspace.Status = *status
	if mutate != nil {
		mutate(workspace)
	}
	if err := f.r.Status().Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if got := f.currentWorkspace(t); got.Status.ObservedGeneration == got.Generation {
		t.Fatal("fixture did not open a generation lag")
	}
}

func TestAttachmentOnlyGenerationChangeKeepsTheRuntimeServing(t *testing.T) {
	f := servedExternalPool(t)
	before := runtimePoolTestGetPool(t, f.r, f.pool).Status.ActiveInstance.DeepCopy()
	probes, seeds := f.supervisor.probeCalls, f.seeds
	f.attachWithGenerationLag(t, nil)
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if !externalRuntimePoolAdmitting(&pool) || pool.Status.ActiveInstance.PodUID != before.PodUID || pool.Status.ActiveInstance.BootID != before.BootID {
		t.Fatalf("an attachment closed the serving runtime: %s/%s: %s", pool.Status.Lifecycle, pool.Status.AdmissionState, pool.Status.Message)
	}
	if f.supervisor.probeCalls != probes || f.seeds != seeds {
		t.Fatalf("the runtime was re-probed or re-bootstrapped: probes %d->%d seeds %d->%d", probes, f.supervisor.probeCalls, seeds, f.seeds)
	}

	workspace := f.currentWorkspace(t)
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	if err := f.r.Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	status := f.currentWorkspace(t)
	markWorkspaceAdmittedForPolicyReview(status, status.Generation)
	status.Status.Allocation, status.Status.State = workspace.Status.Allocation, workspace.Status.State
	if err := f.r.Status().Update(context.Background(), status); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if pool := runtimePoolTestGetPool(t, f.r, f.pool); !externalRuntimePoolAdmitting(&pool) || pool.Status.ActiveInstance.BootID != before.BootID {
		t.Fatalf("the caught-up runtime was not serving: %s/%s: %s", pool.Status.Lifecycle, pool.Status.AdmissionState, pool.Status.Message)
	}
}

func TestNonAttachmentGenerationChangesStillCloseTheRuntime(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
	}{
		{name: "runtime spec changed", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: 1234}
		}},
		{name: "provider reported another instance", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Allocation.Identity.InstanceID = "replacement"
		}},
		{name: "core admission withdrawn", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Conditions = []metav1.Condition{{Type: string(workspacev1alpha1.ConditionWorkspaceAdmitted), Status: metav1.ConditionFalse,
				Reason: "ClassDeleting", ObservedGeneration: w.Generation}}
		}},
		{name: "core admission condition missing", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Conditions = nil
		}},
		{name: "core admission condition unknown", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Conditions = []metav1.Condition{{Type: string(workspacev1alpha1.ConditionWorkspaceAdmitted), Status: metav1.ConditionUnknown,
				Reason: "Pending", ObservedGeneration: w.Spec.CoreAdmission.AdmittedGeneration}}
		}},
		{name: "core admission condition stale generation", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Conditions = []metav1.Condition{{Type: string(workspacev1alpha1.ConditionWorkspaceAdmitted), Status: metav1.ConditionTrue,
				Reason: string(workspacev1alpha1.ReasonReady), ObservedGeneration: w.Spec.CoreAdmission.AdmittedGeneration - 1}}
		}},
		{name: "operator tainted", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[operatoraccess.TaintAnnotation] = "x"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := servedExternalPool(t)
			f.attachWithGenerationLag(t, test.mutate)
			runtimePoolReconcile(t, f.r, f.pool)
			if pool := runtimePoolTestGetPool(t, f.r, f.pool); externalRuntimePoolAdmitting(&pool) {
				t.Fatal("a change beyond the attachment kept the runtime admitting")
			}
		})
	}

	t.Run("no admitted record yet", func(t *testing.T) {
		f := newExternalRuntimePoolFixture(t)
		f.serve(t)
		pool := runtimePoolTestGetPool(t, f.r, f.pool)
		delete(pool.Annotations, externalRuntimeServedAnnotation)
		if err := f.r.Update(context.Background(), &pool); err != nil {
			t.Fatal(err)
		}
		f.attachWithGenerationLag(t, nil)
		runtimePoolReconcile(t, f.r, f.pool)
		if pool := runtimePoolTestGetPool(t, f.r, f.pool); externalRuntimePoolAdmitting(&pool) {
			t.Fatal("a pool with no admitted record kept admitting across the lag")
		}
	})
}

func TestAttachmentOnlyLagRequiresTheAdmittedBoot(t *testing.T) {
	f := servedExternalPool(t)
	f.attachWithGenerationLag(t, nil)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	w := f.currentWorkspace(t)
	if !externalAttachmentOnlyLag(&pool, w) {
		t.Fatal("fixture lag is not attachment-only")
	}
	pool.Status.ActiveInstance.BootID = "another-boot"
	if externalAttachmentOnlyLag(&pool, w) {
		t.Fatal("a different active boot passed as the admitted runtime")
	}
	pool.Status.ActiveInstance = nil
	if externalAttachmentOnlyLag(&pool, w) {
		t.Fatal("a pool without an active instance passed as serving")
	}
}

func TestAttachmentOnlyLagDispatchBoundary(t *testing.T) {
	now := metav1.Now().Time
	makeWorkspace := func() (*workspacev1alpha1.ExecutionWorkspace, *corev1alpha1.RuntimePool) {
		f := servedExternalPool(t)
		pool := runtimePoolTestGetPool(t, f.r, f.pool)
		w := f.currentWorkspace(t)
		if w.Annotations == nil {
			w.Annotations = map[string]string{}
		}
		w.Annotations[acpExecutionWorkspacePoolAnnotation] = pool.Name
		w.Spec.AttachmentEpoch = 2
		w.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
			Epoch:     2,
			TaskRef:   workspacev1alpha1.ObjectIdentityReference{Name: "holder-a", UID: "task-a-uid"},
			ExpiresAt: metav1.NewTime(now.Add(10 * 60e9)),
		}
		w.Status.AttachedEpoch = 2
		w.Status.Conditions = append(w.Status.Conditions, metav1.Condition{
			Type:               string(workspacev1alpha1.ConditionWorkspaceAttached),
			Status:             metav1.ConditionTrue,
			Reason:             string(workspacev1alpha1.ReasonReady),
			ObservedGeneration: w.Generation,
		})
		return w, &pool
	}

	t.Run("same holder bearer rotation blocks re-queueing until observed and re-admitted", func(t *testing.T) {
		w, pool := makeWorkspace()
		w.Spec.AttachmentEpoch = 3
		w.Spec.Attachment.Epoch = 3
		w.Generation++
		if !externalAttachmentOnlyLag(pool, w) {
			t.Fatal("expected pool to remain Serving during same-holder bearer rotation lag")
		}
		err := acpWorkspacePoolReadinessFailure(w, pool, string(w.UID), "task-a-uid", now)
		if err == nil {
			t.Fatal("expected readiness gate to hold new queueing while rotation generation lags")
		}
		errOther := acpWorkspacePoolReadinessFailure(w, pool, string(w.UID), "task-b-uid", now)
		if errOther == nil || errOther.Error() != "the linked workspace is attached to a different Task" {
			t.Fatalf("different holder error = %v, want different-Task refusal", errOther)
		}
	})

	t.Run("revocation blocks dispatch even while pool stays serving during lag", func(t *testing.T) {
		w, pool := makeWorkspace()
		w.Spec.Attachment = nil
		w.Generation++
		err := acpWorkspacePoolReadinessFailure(w, pool, string(w.UID), "task-a-uid", now)
		if err == nil || err.Error() != "the linked workspace attachment is being revoked" {
			t.Fatalf("revoked attachment error = %v, want revocation refusal", err)
		}
	})

	t.Run("stale queued work with mismatched workspace UID is refused", func(t *testing.T) {
		w, pool := makeWorkspace()
		err := acpWorkspacePoolReadinessFailure(w, pool, "stale-workspace-uid", "task-a-uid", now)
		if err == nil || err.Error() != "the linked workspace was replaced" {
			t.Fatalf("stale workspace UID error = %v, want replacement refusal", err)
		}
	})
}
