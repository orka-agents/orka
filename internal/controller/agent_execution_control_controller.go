/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const defaultAgentExecutionClosureStabilityDelay = 5 * time.Second

type agentExecutionClosureSample struct {
	digest     string
	watermark  int64
	openCount  int
	observedAt time.Time
}

// AgentExecutionControlReconciler is the sole state owner for effective
// backend modes and the SQLite reservation gates that linearize admission.
// The store gate always moves before the corresponding status projection, so
// an interrupted transition remains fail-closed and is safe to resume.
type AgentExecutionControlReconciler struct {
	client.Client
	APIReader client.Reader

	AgentExecutionBindingReservations store.AgentExecutionBindingReservationStore
	ClosureStabilityDelay             time.Duration
	Now                               func() time.Time

	mu      sync.Mutex
	samples map[string]agentExecutionClosureSample
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=agentexecutioncontrols,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentexecutioncontrols/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentexecutionpolicies,verbs=get;list;watch

// Reconcile initializes and advances the singleton durable backend control.
func (r *AgentExecutionControlReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	if req.Namespace != corev1alpha1.AgentExecutionControlNamespace ||
		req.Name != corev1alpha1.AgentExecutionControlName {
		return ctrl.Result{}, nil
	}
	if r == nil || r.Client == nil || r.AgentExecutionBindingReservations == nil {
		return ctrl.Result{}, errors.New("AgentExecutionControl client and binding reservation store are required")
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	control := &corev1alpha1.AgentExecutionControl{}
	if err := reader.Get(ctx, req.NamespacedName, control); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("read AgentExecutionControl: %w", err)
	}
	if control.UID == "" || control.Generation < 1 {
		return ctrl.Result{}, errors.New("AgentExecutionControl has incomplete Kubernetes identity")
	}
	if control.Status.Backends == nil {
		return r.initialize(ctx, control)
	}
	if control.Status.ObservedGeneration < 0 || control.Status.ObservedGeneration > control.Generation {
		return ctrl.Result{}, fmt.Errorf(
			"AgentExecutionControl observed generation %d is invalid for generation %d",
			control.Status.ObservedGeneration, control.Generation,
		)
	}
	if control.Status.ObservedGeneration == 0 && control.Generation == 1 {
		// A newly created object whose UID replaced persisted gate authority is
		// deliberately cleanup-only. Re-reading the same desiredMode must never
		// reopen admission; an operator must make an explicit spec update, which
		// advances generation, before reconciliation can continue.
		return ctrl.Result{RequeueAfter: r.stabilityDelay()}, nil
	}

	updated := control.DeepCopy()
	wait := false
	for _, backend := range []store.AgentExecutionBackendKey{
		store.AgentExecutionBackendV1,
		store.AgentExecutionBackendV2,
	} {
		current := agentExecutionBackendStatus(updated.Status.Backends, backend)
		desired := agentExecutionDesiredMode(control, backend)
		next, backendWait, err := r.reconcileBackend(
			ctx, reader, control, backend, desired, current,
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		*current = next
		if backendWait {
			wait = true
			break
		}
	}

	settled, err := r.backendsSettledAtCurrentGeneration(ctx, control, updated.Status.Backends)
	if err != nil {
		return ctrl.Result{}, err
	}
	if settled {
		updated.Status.ObservedGeneration = control.Generation
	}
	changed := !reflect.DeepEqual(control.Status, updated.Status)
	if changed {
		base := control.DeepCopy()
		control.Status = updated.Status
		if err := r.Status().Patch(
			ctx, control,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}),
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("project AgentExecutionControl status: %w", err)
		}
		return ctrl.Result{RequeueAfter: r.stabilityDelay()}, nil
	}
	if wait || !settled {
		return ctrl.Result{RequeueAfter: r.stabilityDelay()}, nil
	}
	return ctrl.Result{}, nil
}

func (r *AgentExecutionControlReconciler) initialize(
	ctx context.Context,
	control *corev1alpha1.AgentExecutionControl,
) (ctrl.Result, error) {
	recreated, err := r.controlUIDChanged(ctx, control)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := &corev1alpha1.AgentExecutionBackendsStatus{}
	for _, backend := range []store.AgentExecutionBackendKey{
		store.AgentExecutionBackendV1,
		store.AgentExecutionBackendV2,
	} {
		desired := agentExecutionDesiredMode(control, backend)
		effective := agentExecutionDesiredEffectiveMode(desired)
		if recreated {
			effective = corev1alpha1.AgentExecutionEffectiveModeDisabled
		}
		revision, err := r.bootstrapRevision(ctx, control, backend, effective)
		if err != nil {
			return ctrl.Result{}, err
		}
		gate, err := r.moveGate(ctx, revision, effective == corev1alpha1.AgentExecutionEffectiveModeEnabled)
		if err != nil {
			return ctrl.Result{}, err
		}
		backendStatus := agentExecutionBackendStatus(status, backend)
		backendStatus.EffectiveMode = effective
		backendStatus.ModeRevision = gate.Revision.ModeRevision
		if effective != corev1alpha1.AgentExecutionEffectiveModeEnabled {
			now := metav1.NewTime(r.now())
			backendStatus.AdmissionClosedAt = &now
			backendStatus.CutoffInventoryDigest = agentExecutionEmptyCutoffDigest(gate.Revision)
		}
	}
	base := control.DeepCopy()
	control.Status.Backends = status
	if recreated {
		control.Status.ObservedGeneration = 0
	} else {
		control.Status.ObservedGeneration = control.Generation
	}
	if err := r.Status().Patch(
		ctx, control,
		client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}),
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("initialize AgentExecutionControl status: %w", err)
	}
	return ctrl.Result{RequeueAfter: r.stabilityDelay()}, nil
}

// controlUIDChanged detects a newly created singleton attempting to replace
// persisted admission authority. The store gate is the durable witness because
// Kubernetes status is deleted with the old object. Any backend mismatch blocks
// both planes so a partial recreation cannot reopen the other protocol.
func (r *AgentExecutionControlReconciler) controlUIDChanged(
	ctx context.Context,
	control *corev1alpha1.AgentExecutionControl,
) (bool, error) {
	for _, backend := range []store.AgentExecutionBackendKey{
		store.AgentExecutionBackendV1,
		store.AgentExecutionBackendV2,
	} {
		gate, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservationGate(ctx, backend)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read %s reservation gate for control UID preflight: %w", backend, err)
		}
		if gate.Revision.ControlUID != string(control.UID) {
			return true, nil
		}
	}
	return false, nil
}

func (r *AgentExecutionControlReconciler) bootstrapRevision(
	ctx context.Context,
	control *corev1alpha1.AgentExecutionControl,
	backend store.AgentExecutionBackendKey,
	effective corev1alpha1.AgentExecutionEffectiveMode,
) (store.AgentExecutionControlRevision, error) {
	revision := store.AgentExecutionControlRevision{
		ControlUID: string(control.UID), ControlGeneration: control.Generation,
		Backend: backend, ModeRevision: 1,
	}
	gate, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservationGate(ctx, backend)
	if errors.Is(err, store.ErrNotFound) {
		return revision, nil
	}
	if err != nil {
		return store.AgentExecutionControlRevision{}, fmt.Errorf("read %s reservation gate: %w", backend, err)
	}
	if gate.Revision.ControlUID == revision.ControlUID &&
		gate.Revision.ControlGeneration == revision.ControlGeneration {
		revision.ModeRevision = gate.Revision.ModeRevision
		if effective == corev1alpha1.AgentExecutionEffectiveModeEnabled && !gate.Open {
			revision.ModeRevision++
		}
	} else if gate.Revision.ModeRevision >= revision.ModeRevision {
		revision.ModeRevision = gate.Revision.ModeRevision + 1
	}
	return revision, nil
}

func (r *AgentExecutionControlReconciler) reconcileBackend(
	ctx context.Context,
	reader client.Reader,
	control *corev1alpha1.AgentExecutionControl,
	backend store.AgentExecutionBackendKey,
	desired corev1alpha1.AgentExecutionDesiredMode,
	current *corev1alpha1.AgentExecutionBackendStatus,
) (corev1alpha1.AgentExecutionBackendStatus, bool, error) {
	if current == nil || current.ModeRevision < 1 {
		return corev1alpha1.AgentExecutionBackendStatus{}, false,
			fmt.Errorf("harness %s backend has invalid mode revision", backend)
	}
	if current.EffectiveMode == corev1alpha1.AgentExecutionEffectiveModeClosing {
		return r.reconcileClosing(ctx, reader, control, backend, desired, *current)
	}

	desiredEffective := agentExecutionDesiredEffectiveMode(desired)
	if current.EffectiveMode == corev1alpha1.AgentExecutionEffectiveModeEnabled &&
		desiredEffective == corev1alpha1.AgentExecutionEffectiveModeEnabled {
		gate, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservationGate(ctx, backend)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !gate.Open) {
			// Missing or unexpectedly closed admission authority is never
			// recreated open in place. Enter the same closure proof used by an
			// operator drain, then reopen only on a new revision.
			source := store.AgentExecutionControlRevision{
				ControlUID: string(control.UID), ControlGeneration: control.Status.ObservedGeneration,
				Backend: backend, ModeRevision: current.ModeRevision,
			}
			if _, moveErr := r.moveGate(ctx, source, false); moveErr != nil {
				return *current, false, moveErr
			}
			r.clearClosureSample(backend)
			return corev1alpha1.AgentExecutionBackendStatus{
				EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeClosing,
				ModeRevision:  current.ModeRevision + 1,
			}, true, nil
		}
		if err != nil {
			return *current, false, fmt.Errorf("read %s enabled reservation gate: %w", backend, err)
		}
	}
	if current.EffectiveMode == corev1alpha1.AgentExecutionEffectiveModeEnabled &&
		desiredEffective != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		// Close the current store gate before publishing closing. If a prior
		// generation move was interrupted, changing its current gate to the
		// old admitted revision while closing it is still fail-closed.
		source := store.AgentExecutionControlRevision{
			ControlUID: string(control.UID), ControlGeneration: control.Status.ObservedGeneration,
			Backend: backend, ModeRevision: current.ModeRevision,
		}
		if _, err := r.moveGate(ctx, source, false); err != nil {
			return *current, false, err
		}
		r.clearClosureSample(backend)
		return corev1alpha1.AgentExecutionBackendStatus{
			EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeClosing,
			ModeRevision:  current.ModeRevision + 1,
		}, true, nil
	}

	next := *current
	transition := current.EffectiveMode != desiredEffective
	if transition {
		next.ModeRevision++
	}
	revision := store.AgentExecutionControlRevision{
		ControlUID: string(control.UID), ControlGeneration: control.Generation,
		Backend: backend, ModeRevision: next.ModeRevision,
	}
	gate, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservationGate(ctx, backend)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return *current, false, fmt.Errorf("read %s reservation gate: %w", backend, err)
	}
	if desiredEffective == corev1alpha1.AgentExecutionEffectiveModeEnabled && err == nil &&
		!gate.Open && gate.Revision == revision {
		// A revision that was ever closed is never reopened. Allocate a new
		// mode revision even when status projection was interrupted.
		revision.ModeRevision++
		next.ModeRevision = revision.ModeRevision
	}
	if _, err := r.moveGate(
		ctx, revision, desiredEffective == corev1alpha1.AgentExecutionEffectiveModeEnabled,
	); err != nil {
		return *current, false, err
	}
	next.EffectiveMode = desiredEffective
	if desiredEffective == corev1alpha1.AgentExecutionEffectiveModeEnabled {
		next.AdmissionClosedAt = nil
		next.CutoffInventoryDigest = ""
	} else if next.AdmissionClosedAt == nil || next.CutoffInventoryDigest == "" {
		now := metav1.NewTime(r.now())
		next.AdmissionClosedAt = &now
		next.CutoffInventoryDigest = agentExecutionEmptyCutoffDigest(revision)
	}
	r.clearClosureSample(backend)
	return next, false, nil
}

func (r *AgentExecutionControlReconciler) reconcileClosing(
	ctx context.Context,
	reader client.Reader,
	control *corev1alpha1.AgentExecutionControl,
	backend store.AgentExecutionBackendKey,
	desired corev1alpha1.AgentExecutionDesiredMode,
	current corev1alpha1.AgentExecutionBackendStatus,
) (corev1alpha1.AgentExecutionBackendStatus, bool, error) {
	if current.ModeRevision < 2 {
		return current, false, fmt.Errorf("harness %s closing revision has no prior enabled revision", backend)
	}
	source := store.AgentExecutionControlRevision{
		ControlUID: string(control.UID), ControlGeneration: control.Status.ObservedGeneration,
		Backend: backend, ModeRevision: current.ModeRevision - 1,
	}
	gate, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservationGate(ctx, backend)
	if errors.Is(err, store.ErrNotFound) {
		if _, err := r.moveGate(ctx, source, false); err != nil {
			return current, false, err
		}
	} else if err != nil {
		return current, false, fmt.Errorf("read %s closing reservation gate: %w", backend, err)
	} else if gate.Open {
		pendingTarget := gate.Revision.ControlUID == string(control.UID) &&
			gate.Revision.ControlGeneration == control.Generation &&
			gate.Revision.ModeRevision > current.ModeRevision &&
			agentExecutionDesiredEffectiveMode(desired) == corev1alpha1.AgentExecutionEffectiveModeEnabled
		if !pendingTarget {
			if _, err := r.moveGate(ctx, gate.Revision, false); err != nil {
				return current, false, err
			}
			return current, true, nil
		}
	}

	inventory, changed, orphanIDs, err := r.closingInventory(ctx, reader, control, backend, source, current.ModeRevision)
	if err != nil {
		return current, false, err
	}
	if changed {
		r.clearClosureSample(backend)
		return current, true, nil
	}
	now := r.now()
	if len(orphanIDs) > 0 {
		if !r.closureSampleStable(backend, inventory, now) {
			return current, true, nil
		}
		for _, id := range orphanIDs {
			reservation, getErr := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservation(ctx, id)
			if getErr != nil {
				return current, false, fmt.Errorf("reload orphan binding reservation %q: %w", id, getErr)
			}
			if reservation.State != store.AgentExecutionBindingReservationOpen {
				continue
			}
			if _, settleErr := r.AgentExecutionBindingReservations.SettleAgentExecutionBindingReservation(
				ctx, store.SettleAgentExecutionBindingReservationRequest{
					ID: reservation.ID, ExpectedVersion: reservation.Version,
					TargetState:    store.AgentExecutionBindingReservationRejected,
					BindingDigest:  reservation.BindingDigest,
					TerminalReason: "closing-orphan-without-binding", SettledAt: now,
				},
			); settleErr != nil {
				return current, false, fmt.Errorf("reject orphan binding reservation %q: %w", id, settleErr)
			}
		}
		r.clearClosureSample(backend)
		return current, true, nil
	}
	if inventory.openCount != 0 {
		return current, false, fmt.Errorf("harness %s closing inventory retained %d unclassified open reservations", backend, inventory.openCount)
	}
	if !r.closureSampleStable(backend, inventory, now) {
		return current, true, nil
	}

	effective := agentExecutionDesiredEffectiveMode(desired)
	nextRevision := store.AgentExecutionControlRevision{
		ControlUID: string(control.UID), ControlGeneration: control.Generation,
		Backend: backend, ModeRevision: current.ModeRevision + 1,
	}
	gate, err = r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservationGate(ctx, backend)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return current, false, fmt.Errorf("read %s terminal reservation gate: %w", backend, err)
	}
	if effective == corev1alpha1.AgentExecutionEffectiveModeEnabled && err == nil &&
		!gate.Open && gate.Revision == nextRevision {
		nextRevision.ModeRevision++
	}
	if _, err := r.moveGate(
		ctx, nextRevision, effective == corev1alpha1.AgentExecutionEffectiveModeEnabled,
	); err != nil {
		return current, false, err
	}
	next := corev1alpha1.AgentExecutionBackendStatus{
		EffectiveMode: effective, ModeRevision: nextRevision.ModeRevision,
	}
	if effective != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		closedAt := metav1.NewTime(now)
		next.AdmissionClosedAt = &closedAt
		next.CutoffInventoryDigest = inventory.digest
	}
	r.clearClosureSample(backend)
	return next, false, nil
}

type agentExecutionClosingInventory struct {
	digest    string
	watermark int64
	openCount int
}

type agentExecutionClosingReservationDigest struct {
	ID             string `json:"id"`
	TaskUID        string `json:"taskUid"`
	BindingDigest  string `json:"bindingDigest"`
	SnapshotDigest string `json:"snapshotDigest"`
	State          string `json:"state"`
	Version        int64  `json:"version"`
}

type agentExecutionClosingTaskDigest struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	UID           string `json:"uid"`
	BindingDigest string `json:"bindingDigest"`
	ControlGen    int64  `json:"controlGeneration"`
	ModeRevision  int64  `json:"modeRevision"`
	ReservationID string `json:"reservationId"`
}

//nolint:gocyclo // Closure inventory keeps all fail-closed evidence classifications in one audit path.
func (r *AgentExecutionControlReconciler) closingInventory(
	ctx context.Context,
	reader client.Reader,
	control *corev1alpha1.AgentExecutionControl,
	backend store.AgentExecutionBackendKey,
	source store.AgentExecutionControlRevision,
	closingModeRevision int64,
) (agentExecutionClosingInventory, bool, []string, error) {
	stored, err := r.AgentExecutionBindingReservations.ListAgentExecutionBindingReservations(ctx, source)
	if err != nil {
		return agentExecutionClosingInventory{}, false, nil,
			fmt.Errorf("list harness %s closing reservations: %w", backend, err)
	}
	orphans := make([]string, 0)
	changed := false
	reservationDigests := make([]agentExecutionClosingReservationDigest, 0, len(stored.Reservations))
	for i := range stored.Reservations {
		reservation := &stored.Reservations[i]
		task, binding, taskErr := readAgentExecutionReservationTask(ctx, reader, reservation)
		switch reservation.State {
		case store.AgentExecutionBindingReservationOpen:
			if taskErr == nil && binding != nil && agentExecutionReservationMatchesBinding(reservation, task, binding) {
				if _, err := r.AgentExecutionBindingReservations.SettleAgentExecutionBindingReservation(
					ctx, store.SettleAgentExecutionBindingReservationRequest{
						ID: reservation.ID, ExpectedVersion: reservation.Version,
						TargetState:   store.AgentExecutionBindingReservationBound,
						BindingDigest: reservation.BindingDigest, SettledAt: r.now(),
					},
				); err != nil {
					return agentExecutionClosingInventory{}, false, nil,
						fmt.Errorf("recover bound reservation %q: %w", reservation.ID, err)
				}
				changed = true
			} else if taskErr == nil && binding != nil {
				return agentExecutionClosingInventory{}, false, nil,
					fmt.Errorf("open reservation %q conflicts with the Task's immutable binding", reservation.ID)
			} else if taskErr == nil && task != nil && string(task.UID) == reservation.TaskUID {
				// The Task may have persisted its reservation immediately before
				// admission closed but not yet projected the write-once binding.
				// Keep that exact reservation Open so the idempotent binding path
				// can finish after gate closure. Only absent or replacement Tasks
				// are safe to classify as abandoned reservations below.
			} else if taskErr != nil && !apierrors.IsNotFound(taskErr) {
				return agentExecutionClosingInventory{}, false, nil, taskErr
			} else {
				orphans = append(orphans, reservation.ID)
			}
		case store.AgentExecutionBindingReservationBound:
			if taskErr != nil || binding == nil || !agentExecutionReservationMatchesBinding(reservation, task, binding) {
				return agentExecutionClosingInventory{}, false, nil,
					fmt.Errorf("bound reservation %q has no exact immutable Task binding", reservation.ID)
			}
		case store.AgentExecutionBindingReservationRejected:
			if taskErr == nil && binding != nil && agentExecutionReservationMatchesBinding(reservation, task, binding) {
				return agentExecutionClosingInventory{}, false, nil,
					fmt.Errorf("rejected reservation %q has a late immutable Task binding", reservation.ID)
			}
		default:
			return agentExecutionClosingInventory{}, false, nil,
				fmt.Errorf("reservation %q has unsupported state %q", reservation.ID, reservation.State)
		}
		reservationDigests = append(reservationDigests, agentExecutionClosingReservationDigest{
			ID: reservation.ID, TaskUID: reservation.TaskUID,
			BindingDigest: reservation.BindingDigest, SnapshotDigest: reservation.SnapshotDigest,
			State: string(reservation.State), Version: reservation.Version,
		})
	}
	if changed {
		return agentExecutionClosingInventory{}, true, nil, nil
	}

	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks); err != nil {
		return agentExecutionClosingInventory{}, false, nil,
			fmt.Errorf("list Tasks for harness %s closing inventory: %w", backend, err)
	}
	taskDigests := make([]agentExecutionClosingTaskDigest, 0)
	for i := range tasks.Items {
		task := &tasks.Items[i]
		binding := task.Status.AgentExecutionBinding
		if !agentExecutionBindingUsesBackend(binding, backend) || binding.BackendControl == nil ||
			binding.BackendControl.UID != control.UID {
			continue
		}
		if binding.BackendControl.ModeRevision >= closingModeRevision {
			return agentExecutionClosingInventory{}, false, nil,
				fmt.Errorf("task %s/%s carries a post-closing harness %s binding revision %d",
					task.Namespace, task.Name, backend, binding.BackendControl.ModeRevision)
		}
		want, err := agentExecutionBindingReservationFor(task, binding)
		if err != nil {
			return agentExecutionClosingInventory{}, false, nil, err
		}
		reservation, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservation(ctx, want.ID)
		if err != nil {
			return agentExecutionClosingInventory{}, false, nil,
				fmt.Errorf("task %s/%s binding lacks its exact reservation: %w", task.Namespace, task.Name, err)
		}
		if reservation.State != store.AgentExecutionBindingReservationBound ||
			!agentExecutionReservationMatchesBinding(reservation, task, binding) {
			return agentExecutionClosingInventory{}, false, nil,
				fmt.Errorf("task %s/%s binding reservation is not exact and Bound", task.Namespace, task.Name)
		}
		taskDigests = append(taskDigests, agentExecutionClosingTaskDigest{
			Namespace: task.Namespace, Name: task.Name, UID: string(task.UID),
			BindingDigest: binding.BindingDigest,
			ControlGen:    binding.BackendControl.Generation, ModeRevision: binding.BackendControl.ModeRevision,
			ReservationID: reservation.ID,
		})
	}
	sort.Slice(taskDigests, func(i, j int) bool {
		if taskDigests[i].Namespace != taskDigests[j].Namespace {
			return taskDigests[i].Namespace < taskDigests[j].Namespace
		}
		if taskDigests[i].Name != taskDigests[j].Name {
			return taskDigests[i].Name < taskDigests[j].Name
		}
		return taskDigests[i].UID < taskDigests[j].UID
	})
	digest, err := agentExecutionClosingInventoryDigest(source, stored.Watermark, reservationDigests, taskDigests)
	if err != nil {
		return agentExecutionClosingInventory{}, false, nil, err
	}
	return agentExecutionClosingInventory{
		digest: digest, watermark: stored.Watermark, openCount: stored.OpenCount,
	}, false, orphans, nil
}

func readAgentExecutionReservationTask(
	ctx context.Context,
	reader client.Reader,
	reservation *store.AgentExecutionBindingReservation,
) (*corev1alpha1.Task, *corev1alpha1.AgentExecutionBinding, error) {
	task := &corev1alpha1.Task{}
	err := reader.Get(ctx, types.NamespacedName{
		Namespace: reservation.TaskNamespace, Name: reservation.TaskName,
	}, task)
	if err != nil {
		return nil, nil, err
	}
	if string(task.UID) != reservation.TaskUID {
		return task, nil, nil
	}
	return task, task.Status.AgentExecutionBinding, nil
}

func agentExecutionReservationMatchesBinding(
	reservation *store.AgentExecutionBindingReservation,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) bool {
	if reservation == nil || task == nil || binding == nil {
		return false
	}
	want, err := agentExecutionBindingReservationFor(task, binding)
	return err == nil && reservation.ID == want.ID && reservation.TaskNamespace == want.TaskNamespace &&
		reservation.TaskName == want.TaskName && reservation.TaskUID == want.TaskUID &&
		reservation.Revision == want.Revision && reservation.BindingDigest == want.BindingDigest &&
		reservation.SnapshotDigest == want.SnapshotDigest
}

func agentExecutionBindingUsesBackend(
	binding *corev1alpha1.AgentExecutionBinding,
	backend store.AgentExecutionBackendKey,
) bool {
	if binding == nil {
		return false
	}
	if backend == store.AgentExecutionBackendV1 {
		return binding.ContractVersion == corev1alpha1.AgentRuntimeContractHarnessV1
	}
	return backend == store.AgentExecutionBackendV2 &&
		binding.ContractVersion == corev1alpha1.AgentRuntimeContractHarnessV2
}

func agentExecutionClosingInventoryDigest(
	revision store.AgentExecutionControlRevision,
	watermark int64,
	reservations []agentExecutionClosingReservationDigest,
	tasks []agentExecutionClosingTaskDigest,
) (string, error) {
	payload := struct {
		Revision     store.AgentExecutionControlRevision      `json:"revision"`
		Watermark    int64                                    `json:"watermark"`
		Reservations []agentExecutionClosingReservationDigest `json:"reservations"`
		Tasks        []agentExecutionClosingTaskDigest        `json:"tasks"`
	}{revision, watermark, reservations, tasks}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode AgentExecutionControl cutoff inventory: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func agentExecutionEmptyCutoffDigest(revision store.AgentExecutionControlRevision) string {
	digest, err := agentExecutionClosingInventoryDigest(revision, 0, nil, nil)
	if err != nil {
		panic(err)
	}
	return digest
}

func (r *AgentExecutionControlReconciler) closureSampleStable(
	backend store.AgentExecutionBackendKey,
	inventory agentExecutionClosingInventory,
	now time.Time,
) bool {
	key := string(backend)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.samples == nil {
		r.samples = make(map[string]agentExecutionClosureSample)
	}
	previous, ok := r.samples[key]
	if !ok || previous.digest != inventory.digest || previous.watermark != inventory.watermark ||
		previous.openCount != inventory.openCount {
		r.samples[key] = agentExecutionClosureSample{
			digest: inventory.digest, watermark: inventory.watermark,
			openCount: inventory.openCount, observedAt: now,
		}
		return false
	}
	return !now.Before(previous.observedAt.Add(r.stabilityDelay()))
}

func (r *AgentExecutionControlReconciler) clearClosureSample(backend store.AgentExecutionBackendKey) {
	r.mu.Lock()
	delete(r.samples, string(backend))
	r.mu.Unlock()
}

func (r *AgentExecutionControlReconciler) moveGate(
	ctx context.Context,
	revision store.AgentExecutionControlRevision,
	open bool,
) (*store.AgentExecutionBindingReservationGate, error) {
	if err := revision.Validate(); err != nil {
		return nil, err
	}
	version := int64(0)
	current, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservationGate(ctx, revision.Backend)
	if err == nil {
		version = current.Version
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("read %s reservation gate: %w", revision.Backend, err)
	}
	gate, err := r.AgentExecutionBindingReservations.SetAgentExecutionBindingReservationGate(
		ctx, store.AgentExecutionBindingReservationGate{
			Revision: revision, Open: open, Version: version, UpdatedAt: r.now(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("move %s reservation gate: %w", revision.Backend, err)
	}
	return gate, nil
}

func (r *AgentExecutionControlReconciler) backendsSettledAtCurrentGeneration(
	ctx context.Context,
	control *corev1alpha1.AgentExecutionControl,
	statuses *corev1alpha1.AgentExecutionBackendsStatus,
) (bool, error) {
	for _, backend := range []store.AgentExecutionBackendKey{
		store.AgentExecutionBackendV1,
		store.AgentExecutionBackendV2,
	} {
		status := agentExecutionBackendStatus(statuses, backend)
		if status.EffectiveMode == corev1alpha1.AgentExecutionEffectiveModeClosing ||
			status.EffectiveMode != agentExecutionDesiredEffectiveMode(agentExecutionDesiredMode(control, backend)) {
			return false, nil
		}
		gate, err := r.AgentExecutionBindingReservations.GetAgentExecutionBindingReservationGate(ctx, backend)
		if err != nil {
			return false, fmt.Errorf("verify %s reservation gate: %w", backend, err)
		}
		if gate.Revision.ControlUID != string(control.UID) ||
			gate.Revision.ControlGeneration != control.Generation ||
			gate.Revision.ModeRevision != status.ModeRevision ||
			gate.Open != (status.EffectiveMode == corev1alpha1.AgentExecutionEffectiveModeEnabled) {
			return false, nil
		}
	}
	return true, nil
}

func agentExecutionBackendStatus(
	statuses *corev1alpha1.AgentExecutionBackendsStatus,
	backend store.AgentExecutionBackendKey,
) *corev1alpha1.AgentExecutionBackendStatus {
	if backend == store.AgentExecutionBackendV1 {
		return &statuses.V1
	}
	return &statuses.V2
}

func agentExecutionDesiredMode(
	control *corev1alpha1.AgentExecutionControl,
	backend store.AgentExecutionBackendKey,
) corev1alpha1.AgentExecutionDesiredMode {
	if backend == store.AgentExecutionBackendV1 {
		return control.Spec.Backends.V1.DesiredMode
	}
	return control.Spec.Backends.V2.DesiredMode
}

func agentExecutionDesiredEffectiveMode(
	desired corev1alpha1.AgentExecutionDesiredMode,
) corev1alpha1.AgentExecutionEffectiveMode {
	switch desired {
	case corev1alpha1.AgentExecutionModeEnabled:
		return corev1alpha1.AgentExecutionEffectiveModeEnabled
	case corev1alpha1.AgentExecutionModeDrainOnly:
		return corev1alpha1.AgentExecutionEffectiveModeDrainOnly
	case corev1alpha1.AgentExecutionModeDisabled:
		return corev1alpha1.AgentExecutionEffectiveModeDisabled
	default:
		return corev1alpha1.AgentExecutionEffectiveModeDisabled
	}
}

func (r *AgentExecutionControlReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *AgentExecutionControlReconciler) stabilityDelay() time.Duration {
	if r.ClosureStabilityDelay > 0 {
		return r.ClosureStabilityDelay
	}
	return defaultAgentExecutionClosureStabilityDelay
}

// SetupWithManager registers the singleton controller as an explicitly
// leader-elected, single-concurrency reconciler.
func (r *AgentExecutionControlReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	needLeaderElection := true
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentExecutionControl{}).
		WithOptions(controlleroptions.Options{
			MaxConcurrentReconciles: 1,
			NeedLeaderElection:      &needLeaderElection,
		}).
		Named("agentexecutioncontrol").
		Complete(r)
}
