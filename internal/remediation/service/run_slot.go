package service

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

const runSlotName = "remediation-active-run"

func slotMatches(slot *corev1.ConfigMap, r *run) bool {
	return slot.DeletionTimestamp == nil && slot.UID != "" &&
		slot.Data["id"] == r.state.ID && slot.Data["uid"] == string(r.object.UID) &&
		slot.Data["digest"] == r.state.InputDigest
}

func (s *Coordinator) acquireRunSlot(ctx context.Context, r *run) error {
	if !r.state.Authority.BoundedExecution {
		return nil
	}
	if err := s.requireCurrentRun(ctx, r); err != nil {
		return err
	}
	slot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: runSlotName, Namespace: r.state.Namespace},
		Data: map[string]string{
			"id": r.state.ID, "uid": string(r.object.UID), "digest": r.state.InputDigest,
		},
	}
	if err := s.config.Client.Create(ctx, slot); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return contract.ErrUnavailable
	}
	if err := s.config.Reader.Get(ctx, client.ObjectKeyFromObject(slot), slot); err != nil {
		return contract.ErrUnavailable
	}
	if slotMatches(slot, r) {
		return nil
	}
	// A deadline is not proof of cleanup. Only the exact retained, settled run
	// can release its slot; missing state is an operator-visible conflict.
	previous, err := s.load(contextWithoutRunIdentity(ctx), slot.Data["id"])
	if err != nil || !slotMatches(slot, previous) || !previous.state.Settled ||
		!contract.TerminalPhase(previous.state.Status.Phase) {
		return contract.ErrConflict
	}
	if err := s.deleteRunSlot(ctx, slot); err != nil {
		return err
	}
	return contract.ErrConflict
}

func (s *Coordinator) requireRunSlot(ctx context.Context, r *run) error {
	if !r.state.Authority.BoundedExecution {
		return nil
	}
	slot := new(corev1.ConfigMap)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: r.state.Namespace, Name: runSlotName}, slot); err != nil {
		if apierrors.IsNotFound(err) {
			return contract.ErrConflict
		}
		return contract.ErrUnavailable
	}
	if !slotMatches(slot, r) {
		return contract.ErrConflict
	}
	return nil
}

func (s *Coordinator) releaseRunSlot(ctx context.Context, r *run) error {
	if !r.state.Authority.BoundedExecution || !r.state.Settled || !contract.TerminalPhase(r.state.Status.Phase) {
		return nil
	}
	slot := new(corev1.ConfigMap)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: r.state.Namespace, Name: runSlotName}, slot); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return contract.ErrUnavailable
	}
	if !slotMatches(slot, r) {
		return contract.ErrConflict
	}
	return s.deleteRunSlot(ctx, slot)
}

func (s *Coordinator) deleteRunSlot(ctx context.Context, slot *corev1.ConfigMap) error {
	uid, version := slot.UID, slot.ResourceVersion
	err := s.config.Client.Delete(ctx, slot, &client.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return contract.ErrUnavailable
	}
	return nil
}

// Inspecting the slot's previous owner must not inherit the new driver's UID.
// Cancellation and deadlines are preserved.
func contextWithoutRunIdentity(ctx context.Context) context.Context {
	return context.WithValue(ctx, runIdentityContextKey{}, nil)
}
