package controller

import (
	"context"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestFirstTurnWaitsForTheStagedInstall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSpeculativeTaskFixture(t)
	f.start(t, f.task, true, "")
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan); err != nil || ready {
		t.Fatalf("ensure = (%v, %v), want waiting", ready, err)
	}
	if got := f.workspace(t); got.Spec.Attachment != nil {
		t.Fatal("the first turn attached separately instead of waiting for the staged install")
	}
	installed := f.installStagedAttachment(t, false)
	if installed == nil || installed.TaskRef.UID != f.task.UID {
		t.Fatalf("installed attachment = %#v", installed)
	}
	acknowledgeTestACPWorkspaceAttachment(t, f.r, f.workspace(t))
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan); err != nil || !ready {
		t.Fatalf("ensure after install = (%v, %v), want ready", ready, err)
	}
}

func TestFirstTurnStopsWaitingForTheStagedInstall(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		end  func(*testing.T, *speculativeTaskFixture)
	}{
		{name: "publication landed without the install", end: func(t *testing.T, f *speculativeTaskFixture) {
			f.bootSpeculativeWorkload(t)
		}},
		{name: "speculative start expired", end: func(t *testing.T, f *speculativeTaskFixture) {
			f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(time.Now().Add(-time.Second), f.task.UID)
			})
		}},
		{name: "pool scaled down", end: func(t *testing.T, f *speculativeTaskFixture) {
			pool, _ := f.pool(t)
			pool.Spec.DesiredReplicas = 0
			if err := f.r.Update(context.Background(), pool); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newSpeculativeTaskFixture(t)
			f.start(t, f.task, true, "")
			test.end(t, f)
			if _, ready := attachTestACPWorkspace(t, f.r, f.task, f.plan, f.key.Name); !ready {
				t.Fatal("the first turn kept waiting for an install that cannot come")
			}
			if attached := f.workspace(t); attached.Spec.Attachment == nil || attached.Spec.Attachment.TaskRef.UID != f.task.UID {
				t.Fatal("the first turn did not attach itself")
			}
		})
	}
}

func TestSpeculativeStartSeesAWorkspaceTheCacheHasNotSeen(t *testing.T) {
	t.Parallel()
	f := newSpeculativeTaskFixture(t)
	gr := schema.GroupResource{Group: workspacev1alpha1.GroupVersion.Group, Resource: "executionworkspaces"}
	cold := interceptor.NewClient(f.r.APIReader.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*workspacev1alpha1.ExecutionWorkspace); ok {
				return apierrors.NewNotFound(gr, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &TaskReconciler{Client: authorizedACPFixtureClient{cold}, APIReader: f.r.APIReader, Scheme: f.r.Scheme,
		WorkspaceProviderAPIEnabled: true, WorkspaceSettlementProtected: true}
	r.startACPWorkspaceSpeculativeRuntime(context.Background(), f.task, f.plan, true, "")
	if pool, exists := f.pool(t); !exists || pool.Spec.DesiredReplicas != 1 {
		t.Fatal("a stale cache kept the creation pass from activating the pool")
	}
}
