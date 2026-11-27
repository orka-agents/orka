package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func warmExpiryFixture(t *testing.T) (*warmSettlementFixture, time.Time) {
	t.Helper()
	f := newWarmSettlementFixture(t, warmTestWindow)
	f.settle(t)
	f.task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := f.r.Status().Update(context.Background(), f.task); err != nil {
		t.Fatal(err)
	}
	deadline, open := acpWorkspaceWarmWindowOpen(f.workspace(t))
	if !open {
		t.Fatal("settlement opened no warm window")
	}
	return f, deadline
}

func (f *warmSettlementFixture) continuationTask(t *testing.T, name string) *corev1alpha1.Task {
	t.Helper()
	continuation := f.task.DeepCopy()
	continuation.ObjectMeta = metav1.ObjectMeta{Namespace: f.task.Namespace, Name: name, UID: types.UID(name + "-uid")}
	continuation.Status = corev1alpha1.TaskStatus{}
	if err := f.r.Create(context.Background(), continuation); err != nil {
		t.Fatal(err)
	}
	return continuation
}

func (f *warmSettlementFixture) expireAt(t *testing.T, at time.Time, c client.Client) ctrl.Result {
	t.Helper()
	retention := &ACPWorkspaceRetentionReconciler{Client: c, APIReader: c, Now: func() time.Time { return at }}
	result, err := retention.Reconcile(context.Background(), ctrl.Request{NamespacedName: f.key})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWarmWindowExpiryDefersForLiveContinuationDemand(t *testing.T) {
	t.Parallel()
	t.Run("a bound continuation defers expiry and then attaches warm", func(t *testing.T) {
		t.Parallel()
		f, deadline := warmExpiryFixture(t)
		continuation := f.continuationTask(t, "bound-continuation")
		result := f.expireAt(t, deadline.Add(time.Second), f.r.APIReader.(client.Client))
		if got := f.workspace(t); got.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
			t.Fatal("the window suspended a workspace a bound continuation is about to attach")
		}
		if result.RequeueAfter <= 0 || result.RequeueAfter > 5*time.Second {
			t.Fatalf("deferred expiry requeue = %s, want a short recheck", result.RequeueAfter)
		}
		if _, ready := attachTestACPWorkspace(t, f.r, continuation, f.plan, f.key.Name); !ready {
			t.Fatal("the continuation could not attach the warm workspace")
		}
		if _, open := acpWorkspaceWarmWindowOpen(f.workspace(t)); open {
			t.Fatal("the attachment did not end the window")
		}
	})

	t.Run("the deferral is bounded", func(t *testing.T) {
		t.Parallel()
		f, deadline := warmExpiryFixture(t)
		f.continuationTask(t, "stuck-continuation")
		f.expireAt(t, deadline.Add(acpWorkspaceWarmExpiryGrace+time.Second), f.r.APIReader.(client.Client))
		if got := f.workspace(t); got.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			t.Fatalf("desired state = %s, want Suspended once the grace ran out", got.Spec.DesiredState)
		}
	})

	t.Run("without demand the window suspends at its deadline", func(t *testing.T) {
		t.Parallel()
		f, deadline := warmExpiryFixture(t)
		f.expireAt(t, deadline.Add(time.Second), f.r.APIReader.(client.Client))
		if got := f.workspace(t); got.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			t.Fatalf("desired state = %s, want Suspended", got.Spec.DesiredState)
		}
	})

	t.Run("an open speculative start defers expiry", func(t *testing.T) {
		t.Parallel()
		f, deadline := warmExpiryFixture(t)
		f.patchWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(deadline.Add(time.Minute), "requester-uid")
		})
		f.expireAt(t, deadline.Add(time.Second), f.r.APIReader.(client.Client))
		if got := f.workspace(t); got.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
			t.Fatal("the window suspended a workspace with an open speculative start")
		}
	})

	t.Run("the demand stamp fences a suspension already in flight", func(t *testing.T) {
		t.Parallel()
		f, deadline := warmExpiryFixture(t)
		raced := false
		racing := interceptor.NewClient(f.r.APIReader.(client.WithWatch), interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*workspacev1alpha1.ExecutionWorkspace); ok && !raced {
					raced = true
					late := f.continuationTask(t, "late-continuation")
					current := f.workspace(t)
					if err := f.r.stampACPWorkspaceWarmDemand(ctx, current, late, deadline.Add(-time.Second)); err != nil {
						return err
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		f.expireAt(t, deadline.Add(time.Second), racing)
		if !raced {
			t.Fatal("retention never attempted the delayed suspension")
		}
		f.expireAt(t, deadline.Add(2*time.Second), f.r.APIReader.(client.Client))
		got := f.workspace(t)
		if got.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
			!acpWorkspaceResumeDemandBelongsToTask(got.Annotations[acpWorkspaceResumeRequestedAnnotation], "late-continuation-uid") {
			t.Fatalf("desired state = %s, want the late continuation's demand to keep the window", got.Spec.DesiredState)
		}
	})

	t.Run("a continuation near the deadline stamps demand before it attaches", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		f, _ := warmExpiryFixture(t)
		f.patchWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceSuspendAtAnnotation] = time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339Nano)
		})
		continuation := f.continuationTask(t, "near-deadline-continuation")
		stamped := false
		recording := interceptor.NewClient(f.r.APIReader.(client.WithWatch), interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if w, ok := obj.(*workspacev1alpha1.ExecutionWorkspace); ok && w.Spec.Attachment == nil &&
					acpWorkspaceResumeDemandBelongsToTask(w.Annotations[acpWorkspaceResumeRequestedAnnotation], string(continuation.UID)) {
					stamped = true
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		r := &TaskReconciler{Client: authorizedACPFixtureClient{recording}, APIReader: f.r.APIReader, Scheme: f.r.Scheme,
			WorkspaceProviderAPIEnabled: true, WorkspaceSettlementProtected: true}
		if _, _, err := r.ensureACPClassWorkspace(ctx, continuation, f.plan); err != nil {
			t.Fatal(err)
		}
		if !stamped {
			t.Fatal("the continuation attached without first recording its demand")
		}
	})
}

func TestWarmWindowExpiryDeferralIsReported(t *testing.T) {
	t.Parallel()
	f, deadline := warmExpiryFixture(t)
	f.continuationTask(t, "reported-continuation")
	recorder := k8sevents.NewFakeRecorder(4)
	c := f.r.APIReader.(client.Client)
	retention := &ACPWorkspaceRetentionReconciler{Client: c, APIReader: c, Recorder: recorder,
		Now: func() time.Time { return deadline.Add(time.Second) }}
	if _, err := retention.Reconcile(context.Background(), ctrl.Request{NamespacedName: f.key}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "WarmWindowExpiryDeferred") {
			t.Fatalf("event = %q, want the deferral", event)
		}
	default:
		t.Fatal("the deferred expiry reported nothing")
	}
}
