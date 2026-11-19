package controller

import (
	"context"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// An eviction or upgrade drain of a detached warm runtime ends its window at
// once, so the frozen Suspend applies inside the drain's deadline rather
// than when a window longer than that deadline would have ended.
func TestRuntimeDrainEndsAWarmWindowAndSuspendsAtOnce(t *testing.T) {
	for _, test := range []struct {
		name  string
		drain func(*testing.T, *externalRuntimePoolFixture, *workspacev1alpha1.ExecutionWorkspace)
	}{
		{name: "worker eviction", drain: func(t *testing.T, f *externalRuntimePoolFixture, w *workspacev1alpha1.ExecutionWorkspace) {
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			deadline := runtimePoolTestNow.Add(30 * time.Minute)
			if _, err := f.r.beginExternalRuntimeEviction(context.Background(), &pool, w, externalEvictionSourceWorker, runtimePoolTestNow, deadline); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "upgrade retirement", drain: func(t *testing.T, f *externalRuntimePoolFixture, _ *workspacev1alpha1.ExecutionWorkspace) {
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Annotations[externalRuntimeRetirementRequestedAnnotation] = booleanTrueValue
			if err := f.r.Update(context.Background(), &pool); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			// A one-hour window: longer than the 30-minute eviction deadline.
			window := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			f.suspendCapableWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
				w.Annotations[acpWorkspaceSuspendAtAnnotation] = window
				w.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
				w.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{Name: acpTestSessionName, UID: "session-uid"}
			})
			served, _ := f.serve(t)
			// A live continuation's demand would otherwise defer expiry.
			continuation := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: served.Namespace, Name: "waiting", UID: "waiting-uid"}}
			if err := f.r.Create(ctx, continuation); err != nil {
				t.Fatal(err)
			}
			f.patchWorkspaceAnnotations(t, func(annotations map[string]string) {
				annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano) + " waiting waiting-uid"
			})
			test.drain(t, f, f.currentWorkspace(t))
			runtimePoolReconcile(t, f.r, f.pool)
			ended := f.currentWorkspace(t)
			deadline, open := acpWorkspaceWarmWindowOpen(ended)
			if !open || deadline.After(time.Now()) || ended.Annotations[acpWorkspaceWarmWindowDrainedAnnotation] == "" {
				t.Fatalf("drain left the window open until %s", ended.Annotations[acpWorkspaceSuspendAtAnnotation])
			}
			// A second pass never moves the deadline again.
			runtimePoolReconcile(t, f.r, f.pool)
			if again := f.currentWorkspace(t); again.Annotations[acpWorkspaceSuspendAtAnnotation] != ended.Annotations[acpWorkspaceSuspendAtAnnotation] {
				t.Fatal("a later pass rewrote the drained deadline")
			}

			retention := &ACPWorkspaceRetentionReconciler{Client: f.r.Client, APIReader: f.r.Client, Now: time.Now}
			if _, handled, err := retention.reconcileWarmDetachWindow(ctx, f.currentWorkspace(t), time.Now(), acpWorkspaceRetentionRequeue); err != nil || !handled {
				t.Fatalf("retention = (%v, %v)", handled, err)
			}
			suspended := f.currentWorkspace(t)
			if suspended.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
				t.Fatalf("desired state = %s, want Suspended inside the drain deadline", suspended.Spec.DesiredState)
			}
			for _, key := range []string{acpWorkspaceSuspendAtAnnotation, acpWorkspaceWarmWindowDrainedAnnotation} {
				if _, present := suspended.Annotations[key]; present {
					t.Fatalf("suspension kept %s", key)
				}
			}
		})
	}
}

// A drain of an attached runtime, or of a workspace with no open window, is
// left to the ordinary retirement path.
func TestRuntimeDrainLeavesAttachedAndColdWorkspacesAlone(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
	}{
		{name: "attached", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{Epoch: 1, TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: "task", UID: "task-uid"}}
		}},
		{name: "no window", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			delete(w.Annotations, acpWorkspaceSuspendAtAnnotation)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			f.suspendCapableWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
				w.Annotations[acpWorkspaceSuspendAtAnnotation] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				test.mutate(w)
			})
			before := f.currentWorkspace(t).Annotations[acpWorkspaceSuspendAtAnnotation]
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Annotations[externalRuntimeRetirementRequestedAnnotation] = booleanTrueValue
			if err := f.r.Update(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			ended, err := endACPWorkspaceWarmWindowForDrain(ctx, f.r.Client, &pool, f.currentWorkspace(t), runtimePoolTestNow)
			if err != nil || ended {
				t.Fatalf("drain ended a window it does not own: (%v, %v)", ended, err)
			}
			if got := f.currentWorkspace(t); got.Annotations[acpWorkspaceSuspendAtAnnotation] != before {
				t.Fatal("the drain moved a deadline it does not own")
			}
		})
	}
}
