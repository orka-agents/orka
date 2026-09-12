package controller

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func configureAgentExecutionBindingTest(
	t *testing.T,
	ctx context.Context,
	reconciler *TaskReconciler,
	task *corev1alpha1.Task,
) *corev1alpha1.Task {
	t.Helper()
	if reconciler == nil || reconciler.Client == nil || reconciler.Scheme == nil || task == nil {
		t.Fatal("binding test fixture requires reconciler client/scheme and Task")
	}
	if err := corev1.AddToScheme(reconciler.Scheme); err != nil {
		t.Fatal(err)
	}
	if reconciler.AgentExecutionSnapshots == nil {
		if sqliteStore, ok := reconciler.DurableControlStore.(*sqlite.Store); ok {
			cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x35}, sqlite.AgentExecutionSnapshotKeyBytes))
			if err != nil {
				t.Fatal(err)
			}
			if err := sqliteStore.SetAgentExecutionSnapshotCipher(cipher); err != nil {
				t.Fatal(err)
			}
			reconciler.AgentExecutionSnapshots = sqliteStore
		} else {
			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "binding-snapshots.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			snapshotStore := sqlite.NewStore(db, "binding-test")
			cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x35}, sqlite.AgentExecutionSnapshotKeyBytes))
			if err != nil {
				t.Fatal(err)
			}
			if err := snapshotStore.SetAgentExecutionSnapshotCipher(cipher); err != nil {
				t.Fatal(err)
			}
			reconciler.AgentExecutionSnapshots = snapshotStore
		}
	}
	namespace := &corev1.Namespace{}
	err := reconciler.Get(ctx, client.ObjectKey{Name: task.Namespace}, namespace)
	if apierrors.IsNotFound(err) {
		namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: task.Namespace, UID: types.UID(fmt.Sprintf("%s-namespace-uid", task.Namespace)),
		}}
		if err := reconciler.Create(ctx, namespace); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, labels.TaskFinalizer) {
		controllerutil.AddFinalizer(current, labels.TaskFinalizer)
		if err := reconciler.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
		if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
	}
	if current.Generation < 1 {
		current.Generation = 1
		if err := reconciler.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
		if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
	}
	return current
}

func bindACPQueueTaskForTest(
	t *testing.T,
	ctx context.Context,
	reconciler *TaskReconciler,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) *corev1alpha1.Task {
	t.Helper()
	current := configureAgentExecutionBindingTest(t, ctx, reconciler, task)
	boundAgent := agent.DeepCopy()
	if boundAgent.Generation < 1 {
		boundAgent.Generation = 1
	}
	if result, err, handled := reconciler.ensureAgentExecutionBinding(ctx, current, boundAgent); err != nil || handled {
		t.Fatalf("establish test execution binding: result=%#v handled=%v err=%v", result, handled, err)
	}
	bound := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(current), bound); err != nil {
		t.Fatal(err)
	}
	return bound
}
