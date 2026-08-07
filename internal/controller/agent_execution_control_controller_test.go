/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

//nolint:gocyclo // The end-to-end state-machine test intentionally covers the full close/drain sequence.
func TestAgentExecutionControlReconcilerClosesRecoversAndSealsDrain(t *testing.T) {
	ctx := context.Background()
	control := agentExecutionControlTestObject()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentExecutionControl{}, &corev1alpha1.Task{}).
		WithObjects(control).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	durable := sqlite.NewStore(db, "agent-execution-control-test")
	now := time.Date(2026, 8, 6, 18, 0, 0, 0, time.UTC)
	reconciler := &AgentExecutionControlReconciler{
		Client: kubeClient, APIReader: kubeClient,
		AgentExecutionBindingReservations: durable,
		ClosureStabilityDelay:             time.Nanosecond,
		Now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(control)}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("initialize control: %v", err)
	}
	current := getAgentExecutionControlForTest(t, ctx, kubeClient)
	if current.Status.ObservedGeneration != 1 || current.Status.Backends == nil ||
		current.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeEnabled ||
		current.Status.Backends.V1.ModeRevision != 1 ||
		current.Status.Backends.V2.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeDisabled {
		t.Fatalf("initialized status = %#v", current.Status)
	}
	v1Gate, err := durable.GetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBackendV1)
	if err != nil || !v1Gate.Open || v1Gate.Revision.ModeRevision != 1 {
		t.Fatalf("initialized v1 gate = %#v, %v", v1Gate, err)
	}

	binding := agentExecutionControlTestBinding(current, "task-uid", store.AgentExecutionBackendV1)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "task-a", UID: types.UID("task-uid"), Generation: 1,
		},
		Status: corev1alpha1.TaskStatus{AgentExecutionBinding: binding.DeepCopy()},
	}
	if err := kubeClient.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	reservation, err := agentExecutionBindingReservationFor(task, binding)
	if err != nil {
		t.Fatal(err)
	}
	reservation.ReservedAt = now
	created, err := durable.CreateAgentExecutionBindingReservation(ctx, reservation)
	if err != nil {
		t.Fatalf("create pre-closing reservation: %v", err)
	}

	current.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeDrainOnly
	current.Generation = 2
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatalf("request drain-only: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("enter closing: %v", err)
	}
	closing := getAgentExecutionControlForTest(t, ctx, kubeClient)
	if closing.Status.ObservedGeneration != 1 ||
		closing.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeClosing ||
		closing.Status.Backends.V1.ModeRevision != 2 {
		t.Fatalf("closing status = %#v", closing.Status)
	}
	v1Gate, err = durable.GetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBackendV1)
	if err != nil || v1Gate.Open || v1Gate.Revision.ControlGeneration != 1 || v1Gate.Revision.ModeRevision != 1 {
		t.Fatalf("closing gate = %#v, %v", v1Gate, err)
	}

	// First closing pass recovers the Task binding's exact open reservation.
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("recover reservation: %v", err)
	}
	bound, err := durable.GetAgentExecutionBindingReservation(ctx, created.ID)
	if err != nil || bound.State != store.AgentExecutionBindingReservationBound {
		t.Fatalf("recovered reservation = %#v, %v", bound, err)
	}
	if got := getAgentExecutionControlForTest(t, ctx, kubeClient).Status.Backends.V1.EffectiveMode; got != corev1alpha1.AgentExecutionEffectiveModeClosing {
		t.Fatalf("effective mode after one inventory = %s, want closing", got)
	}

	// Two identical zero-open inventories are required before the cutoff is
	// projected. The injected clock advances beyond the stability delay.
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("record first zero-open inventory: %v", err)
	}
	if got := getAgentExecutionControlForTest(t, ctx, kubeClient).Status.Backends.V1.EffectiveMode; got != corev1alpha1.AgentExecutionEffectiveModeClosing {
		t.Fatalf("effective mode after first zero inventory = %s, want closing", got)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("seal cutoff: %v", err)
	}
	drained := getAgentExecutionControlForTest(t, ctx, kubeClient)
	if drained.Status.ObservedGeneration != drained.Generation ||
		drained.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeDrainOnly ||
		drained.Status.Backends.V1.ModeRevision != 3 ||
		drained.Status.Backends.V1.AdmissionClosedAt == nil ||
		!strings.HasPrefix(drained.Status.Backends.V1.CutoffInventoryDigest, "sha256:") {
		t.Fatalf("drained status = %#v", drained.Status)
	}
	v1Gate, err = durable.GetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBackendV1)
	if err != nil || v1Gate.Open || v1Gate.Revision.ControlGeneration != drained.Generation ||
		v1Gate.Revision.ModeRevision != drained.Status.Backends.V1.ModeRevision {
		t.Fatalf("drain-only gate = %#v, %v", v1Gate, err)
	}
}

func TestAgentExecutionControlReconcilerRejectsStableOrphanBeforeDisabledCutoff(t *testing.T) {
	ctx := context.Background()
	control := agentExecutionControlTestObject()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentExecutionControl{}).
		WithObjects(control).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "orphan-control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	durable := sqlite.NewStore(db, "agent-execution-orphan-test")
	now := time.Date(2026, 8, 6, 19, 0, 0, 0, time.UTC)
	reconciler := &AgentExecutionControlReconciler{
		Client: kubeClient, APIReader: kubeClient,
		AgentExecutionBindingReservations: durable,
		ClosureStabilityDelay:             time.Nanosecond,
		Now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(control)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	current := getAgentExecutionControlForTest(t, ctx, kubeClient)
	revision := store.AgentExecutionControlRevision{
		ControlUID: string(current.UID), ControlGeneration: current.Generation,
		Backend:      store.AgentExecutionBackendV1,
		ModeRevision: current.Status.Backends.V1.ModeRevision,
	}
	orphan := store.AgentExecutionBindingReservation{
		TaskNamespace: "tenant-a", TaskName: "missing-task", TaskUID: "missing-task-uid",
		Revision:       revision,
		BindingDigest:  store.CanonicalAgentExecutionSnapshotDigest([]byte("orphan-binding")),
		SnapshotDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte("orphan-snapshot")),
		ReservedAt:     now,
	}
	created, err := durable.CreateAgentExecutionBindingReservation(ctx, orphan)
	if err != nil {
		t.Fatal(err)
	}

	current.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeDisabled
	current.Generation = 2
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("enter closing: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("record stable orphan: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reject stable orphan: %v", err)
	}
	rejected, err := durable.GetAgentExecutionBindingReservation(ctx, created.ID)
	if err != nil || rejected.State != store.AgentExecutionBindingReservationRejected ||
		rejected.TerminalReason != "closing-orphan-without-binding" {
		t.Fatalf("rejected orphan = %#v, %v", rejected, err)
	}
	if got := getAgentExecutionControlForTest(t, ctx, kubeClient).Status.Backends.V1.EffectiveMode; got != corev1alpha1.AgentExecutionEffectiveModeClosing {
		t.Fatalf("effective mode after orphan rejection = %s, want closing", got)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("record zero-open inventory: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("seal disabled cutoff: %v", err)
	}
	disabled := getAgentExecutionControlForTest(t, ctx, kubeClient)
	if disabled.Status.ObservedGeneration != disabled.Generation ||
		disabled.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeDisabled ||
		disabled.Status.Backends.V1.ModeRevision != 3 ||
		disabled.Status.Backends.V1.AdmissionClosedAt == nil ||
		disabled.Status.Backends.V1.CutoffInventoryDigest == "" {
		t.Fatalf("disabled status = %#v", disabled.Status)
	}
}

//nolint:gocyclo // The regression intentionally proves the full disposition, rejection, cutoff, and reopen sequence.
func TestAgentExecutionControlReconcilerTerminalizesLiveUnboundReservationBeforeRejection(t *testing.T) {
	ctx := context.Background()
	control := agentExecutionControlTestObject()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "task-a", UID: types.UID("task-uid"), Generation: 1,
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhasePending,
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentExecutionControl{}, &corev1alpha1.Task{}).
		WithObjects(control, task).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "live-unbound-control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	durable := sqlite.NewStore(db, "agent-execution-live-unbound-test")
	now := time.Date(2026, 8, 6, 19, 30, 0, 0, time.UTC)
	reconciler := &AgentExecutionControlReconciler{
		Client: kubeClient, APIReader: kubeClient,
		AgentExecutionBindingReservations: durable,
		ClosureStabilityDelay:             time.Nanosecond,
		Now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(control)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("initialize control: %v", err)
	}
	current := getAgentExecutionControlForTest(t, ctx, kubeClient)
	expectedInventoryID := agentExecutionClassificationInventoryID(current.UID, current.Generation)
	binding := agentExecutionControlTestBinding(current, string(task.UID), store.AgentExecutionBackendV1)
	reservation, err := agentExecutionBindingReservationFor(task, binding)
	if err != nil {
		t.Fatal(err)
	}
	reservation.ReservedAt = now
	created, err := durable.CreateAgentExecutionBindingReservation(ctx, reservation)
	if err != nil {
		t.Fatalf("create pre-closing reservation: %v", err)
	}

	current.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeDisabled
	current.Generation = 2
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatalf("request disabled mode: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("enter closing: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("record terminal no-execution disposition: %v", err)
	}
	open, err := durable.GetAgentExecutionBindingReservation(ctx, created.ID)
	if err != nil || open.State != store.AgentExecutionBindingReservationOpen {
		t.Fatalf("disposition-first reservation = %#v, %v; want Open", open, err)
	}
	disposed := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), disposed); err != nil {
		t.Fatal(err)
	}
	if disposed.Status.Phase != corev1alpha1.TaskPhaseCancelled ||
		disposed.Status.AgentExecutionBinding != nil ||
		disposed.Status.AgentExecutionNoExecution == nil ||
		disposed.Status.AgentExecutionNoExecution.State != corev1alpha1.AgentExecutionNoExecutionUnbound ||
		disposed.Status.AgentExecutionNoExecution.MigrationInventoryID != expectedInventoryID {
		t.Fatalf("closing Task disposition = %#v", disposed.Status)
	}
	closing := getAgentExecutionControlForTest(t, ctx, kubeClient)
	if closing.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeClosing {
		t.Fatalf("effective mode after Task disposition = %s, want closing",
			closing.Status.Backends.V1.EffectiveMode)
	}

	// The reservation remains Open until the durable Task disposition is
	// observed in a stable closing inventory, then becomes safely rejectable.
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("record disposed reservation inventory: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reject disposed reservation: %v", err)
	}
	rejected, err := durable.GetAgentExecutionBindingReservation(ctx, created.ID)
	if err != nil || rejected.State != store.AgentExecutionBindingReservationRejected ||
		rejected.TerminalReason != "closing-orphan-without-binding" {
		t.Fatalf("disposed reservation = %#v, %v; want Rejected", rejected, err)
	}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("record zero-open inventory: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("seal disabled cutoff: %v", err)
	}
	disabled := getAgentExecutionControlForTest(t, ctx, kubeClient)
	if disabled.Status.ObservedGeneration != disabled.Generation ||
		disabled.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeDisabled {
		t.Fatalf("disabled status after reservation disposition = %#v", disabled.Status)
	}

	disabled.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeEnabled
	disabled.Generation = 3
	if err := kubeClient.Update(ctx, disabled); err != nil {
		t.Fatalf("request explicit reopen: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reopen recovered backend: %v", err)
	}
	reopened := getAgentExecutionControlForTest(t, ctx, kubeClient)
	gate, err := durable.GetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBackendV1)
	if err != nil || !gate.Open || gate.Revision.ControlGeneration != reopened.Generation ||
		reopened.Status.ObservedGeneration != reopened.Generation ||
		reopened.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		t.Fatalf("reopened status/gate = %#v / %#v, %v", reopened.Status, gate, err)
	}
}

func TestAgentExecutionControlRecreationStaysClosedUntilExplicitSpecGeneration(t *testing.T) {
	ctx := context.Background()
	control := agentExecutionControlTestObject()
	control.UID = types.UID("replacement-control-uid")
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentExecutionControl{}).
		WithObjects(control).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "recreated-control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	durable := sqlite.NewStore(db, "agent-execution-recreated-control-test")
	now := time.Date(2026, 8, 6, 21, 0, 0, 0, time.UTC)
	for _, backend := range []store.AgentExecutionBackendKey{
		store.AgentExecutionBackendV1,
		store.AgentExecutionBackendV2,
	} {
		if _, err := durable.SetAgentExecutionBindingReservationGate(
			ctx,
			store.AgentExecutionBindingReservationGate{
				Revision: store.AgentExecutionControlRevision{
					ControlUID:        "deleted-control-uid",
					ControlGeneration: 9,
					Backend:           backend,
					ModeRevision:      12,
				},
				Open:      true,
				UpdatedAt: now,
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	reconciler := &AgentExecutionControlReconciler{
		Client:                            kubeClient,
		APIReader:                         kubeClient,
		AgentExecutionBindingReservations: durable,
		ClosureStabilityDelay:             time.Nanosecond,
		Now:                               func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(control)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("initialize recreated control: %v", err)
	}
	blocked := getAgentExecutionControlForTest(t, ctx, kubeClient)
	if blocked.Status.ObservedGeneration != 0 || blocked.Status.Backends == nil ||
		blocked.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeDisabled ||
		blocked.Status.Backends.V2.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeDisabled {
		t.Fatalf("recreated control status = %#v, want unreconciled disabled backends", blocked.Status)
	}
	for _, backend := range []store.AgentExecutionBackendKey{
		store.AgentExecutionBackendV1,
		store.AgentExecutionBackendV2,
	} {
		gate, err := durable.GetAgentExecutionBindingReservationGate(ctx, backend)
		if err != nil || gate.Open || gate.Revision.ControlUID != string(blocked.UID) {
			t.Fatalf("recreated %s gate = %#v, %v; want replacement UID closed", backend, gate, err)
		}
	}

	// Merely reconciling desiredMode: enabled again must not reopen either gate.
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile blocked replacement: %v", err)
	}
	for _, backend := range []store.AgentExecutionBackendKey{
		store.AgentExecutionBackendV1,
		store.AgentExecutionBackendV2,
	} {
		gate, err := durable.GetAgentExecutionBindingReservationGate(ctx, backend)
		if err != nil || gate.Open {
			t.Fatalf("blocked replacement reopened %s gate: %#v, %v", backend, gate, err)
		}
	}

	// A deliberate spec mutation advances generation and acknowledges the new
	// singleton identity. Both desired enabled modes may then receive fresh open
	// revisions under that exact UID/generation.
	blocked.Spec.Backends.V2.DesiredMode = corev1alpha1.AgentExecutionModeEnabled
	blocked.Generation = 2
	if err := kubeClient.Update(ctx, blocked); err != nil {
		t.Fatalf("acknowledge replacement control: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile acknowledged replacement: %v", err)
	}
	acknowledged := getAgentExecutionControlForTest(t, ctx, kubeClient)
	if acknowledged.Status.ObservedGeneration != acknowledged.Generation ||
		acknowledged.Status.Backends.V1.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeEnabled ||
		acknowledged.Status.Backends.V2.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		t.Fatalf("acknowledged control status = %#v", acknowledged.Status)
	}
}

func TestVerifyBoundAgentExecutionBackendModeRequiresSettledPreCutoffReservation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	closedAt := metav1.NewTime(time.Date(2026, 8, 6, 20, 0, 0, 0, time.UTC))
	control := agentExecutionControlTestObject()
	control.Generation = 2
	control.Status = corev1alpha1.AgentExecutionControlStatus{
		ObservedGeneration: 2,
		Backends: &corev1alpha1.AgentExecutionBackendsStatus{
			V1: corev1alpha1.AgentExecutionBackendStatus{
				EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeDrainOnly,
				ModeRevision:  3, AdmissionClosedAt: &closedAt,
				CutoffInventoryDigest: "sha256:" + strings.Repeat("a", 64),
			},
			V2: corev1alpha1.AgentExecutionBackendStatus{
				EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeDisabled, ModeRevision: 1,
			},
		},
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: "tenant-a", Name: "task-a", UID: types.UID("task-uid"), Generation: 1,
	}}
	binding := agentExecutionControlTestBinding(control, string(task.UID), store.AgentExecutionBackendV1)
	binding.BackendControl.Generation = 1
	binding.BackendControl.ModeRevision = 1
	task.Status.AgentExecutionBinding = binding.DeepCopy()
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentExecutionControl{}, &corev1alpha1.Task{}).
		WithObjects(control, task).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "authorization.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	durable := sqlite.NewStore(db, "agent-execution-authorization-test")
	revision := store.AgentExecutionControlRevision{
		ControlUID: string(control.UID), ControlGeneration: 1,
		Backend: store.AgentExecutionBackendV1, ModeRevision: 1,
	}
	if _, err := durable.SetAgentExecutionBindingReservationGate(ctx, store.AgentExecutionBindingReservationGate{
		Revision: revision, Open: true, UpdatedAt: closedAt.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	reservation, err := agentExecutionBindingReservationFor(task, binding)
	if err != nil {
		t.Fatal(err)
	}
	reservation.ReservedAt = closedAt.Add(-time.Minute)
	created, err := durable.CreateAgentExecutionBindingReservation(ctx, reservation)
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &TaskReconciler{
		Client: kubeClient, APIReader: kubeClient,
		AgentExecutionBindingReservations: durable,
	}
	if err := reconciler.verifyBoundAgentExecutionBackendMode(
		ctx, kubeClient, task, binding, store.AgentExecutionBackendV1,
	); err == nil {
		t.Fatal("open reservation unexpectedly authorized drain-only execution")
	}
	if _, err := durable.SettleAgentExecutionBindingReservation(ctx, store.SettleAgentExecutionBindingReservationRequest{
		ID: created.ID, ExpectedVersion: created.Version,
		TargetState:   store.AgentExecutionBindingReservationBound,
		BindingDigest: created.BindingDigest, SettledAt: closedAt.Add(-30 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.verifyBoundAgentExecutionBackendMode(
		ctx, kubeClient, task, binding, store.AgentExecutionBackendV1,
	); err != nil {
		t.Fatalf("settled pre-cutoff reservation was rejected: %v", err)
	}

	current := getAgentExecutionControlForTest(t, ctx, kubeClient)
	base := current.DeepCopy()
	current.Status.Backends.V1.EffectiveMode = corev1alpha1.AgentExecutionEffectiveModeDisabled
	current.Status.Backends.V1.ModeRevision++
	if err := kubeClient.Status().Patch(ctx, current, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.verifyBoundAgentExecutionBackendMode(
		ctx, kubeClient, task, binding, store.AgentExecutionBackendV1,
	); err == nil {
		t.Fatal("disabled backend unexpectedly authorized executor work")
	}
}

func agentExecutionControlTestObject() *corev1alpha1.AgentExecutionControl {
	return &corev1alpha1.AgentExecutionControl{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: corev1alpha1.AgentExecutionControlNamespace,
			Name:      corev1alpha1.AgentExecutionControlName,
			UID:       types.UID("control-uid"), Generation: 1,
		},
		Spec: corev1alpha1.AgentExecutionControlSpec{Backends: corev1alpha1.AgentExecutionBackendsSpec{
			V1: corev1alpha1.AgentExecutionBackendSpec{DesiredMode: corev1alpha1.AgentExecutionModeEnabled},
			V2: corev1alpha1.AgentExecutionBackendSpec{DesiredMode: corev1alpha1.AgentExecutionModeDisabled},
		}},
	}
}

func agentExecutionControlTestBinding(
	control *corev1alpha1.AgentExecutionControl,
	taskUID string,
	backend store.AgentExecutionBackendKey,
) *corev1alpha1.AgentExecutionBinding {
	contract := corev1alpha1.AgentRuntimeContractHarnessV1
	executionBackend := corev1alpha1.AgentExecutionBackendHarnessWrapper
	if backend == store.AgentExecutionBackendV2 {
		contract = corev1alpha1.AgentRuntimeContractHarnessV2
		executionBackend = corev1alpha1.AgentExecutionBackendRuntimePool
	}
	bindingDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("binding-" + taskUID))
	snapshotDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("snapshot-" + taskUID))
	return &corev1alpha1.AgentExecutionBinding{
		SchemaVersion: 1, Mode: corev1alpha1.AgentExecutionBindingModeExecute,
		ContractVersion: contract, Backend: executionBackend,
		Provenance:    corev1alpha1.AgentExecutionProvenanceNewlyBound,
		BindingDigest: bindingDigest,
		Task: corev1alpha1.AgentExecutionBindingTaskRef{
			NamespaceUID: types.UID("tenant-a-uid"), UID: types.UID(taskUID), BoundSpecGeneration: 1,
		},
		BackendControl: &corev1alpha1.AgentExecutionBackendControlRef{
			Name: control.Name, UID: control.UID, Generation: 1, ModeRevision: 1,
			AdmittedMode: corev1alpha1.AgentExecutionEffectiveModeEnabled,
		},
		Snapshot: corev1alpha1.AgentExecutionSnapshotRef{
			ID: taskUID + "/" + snapshotDigest, Digest: snapshotDigest, SchemaVersion: 1,
		},
		BoundAt: metav1.NewTime(time.Date(2026, 8, 6, 17, 0, 0, 0, time.UTC)),
	}
}

func getAgentExecutionControlForTest(
	t *testing.T,
	ctx context.Context,
	kubeClient client.Client,
) *corev1alpha1.AgentExecutionControl {
	t.Helper()
	control := &corev1alpha1.AgentExecutionControl{}
	if err := kubeClient.Get(ctx, client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}, control); err != nil {
		t.Fatal(err)
	}
	return control
}
