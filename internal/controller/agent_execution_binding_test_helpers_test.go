package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
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
			sqliteStore.SetAgentExecutionSnapshotCipher(cipher)
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
			snapshotStore.SetAgentExecutionSnapshotCipher(cipher)
			reconciler.AgentExecutionSnapshots = snapshotStore
		}
	}
	if reconciler.AgentExecutionBindingReservations == nil {
		if reservationStore, ok := reconciler.AgentExecutionSnapshots.(*sqlite.Store); ok {
			reconciler.AgentExecutionBindingReservations = reservationStore
		} else if reservationStore, ok := reconciler.DurableControlStore.(*sqlite.Store); ok {
			reconciler.AgentExecutionBindingReservations = reservationStore
		} else {
			t.Fatal("binding test fixture requires a SQLite binding reservation store")
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
	control := &corev1alpha1.AgentExecutionControl{}
	err = reconciler.Get(ctx, client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}, control)
	if apierrors.IsNotFound(err) {
		control = bindingTestControl()
		if err := reconciler.Create(ctx, control); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	configureAgentExecutionBindingTestGate(
		t, ctx, reconciler.AgentExecutionBindingReservations, control, store.AgentExecutionBackendV2,
	)

	current := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
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

func configureAgentExecutionBindingTestGate(
	t *testing.T,
	ctx context.Context,
	reservationStore store.AgentExecutionBindingReservationStore,
	control *corev1alpha1.AgentExecutionControl,
	backend store.AgentExecutionBackendKey,
) {
	t.Helper()
	if reservationStore == nil || control == nil || control.Status.Backends == nil {
		t.Fatal("binding test gate requires a reservation store and observed control")
	}
	status := control.Status.Backends.V2
	if backend == store.AgentExecutionBackendV1 {
		status = control.Status.Backends.V1
	}
	revision := store.AgentExecutionControlRevision{
		ControlUID: string(control.UID), ControlGeneration: control.Generation,
		Backend: backend, ModeRevision: status.ModeRevision,
	}
	version := int64(0)
	if current, err := reservationStore.GetAgentExecutionBindingReservationGate(ctx, backend); err == nil {
		version = current.Version
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read binding test reservation gate: %v", err)
	}
	if _, err := reservationStore.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: revision,
		Open:     status.EffectiveMode == corev1alpha1.AgentExecutionEffectiveModeEnabled,
		Version:  version, UpdatedAt: time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("configure binding test reservation gate: %v", err)
	}
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
