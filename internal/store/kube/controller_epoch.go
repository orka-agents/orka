package kube

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// GetControllerEpoch reads the authoritative controller-epoch Lease.
func (s *Store) GetControllerEpoch(ctx context.Context, name string) (*store.ControllerEpoch, error) {
	if err := s.requireClient(); err != nil {
		return nil, err
	}
	normalized, err := normalizeControllerEpochName(name)
	if err != nil {
		return nil, err
	}
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochLeaseName(normalized)}
	if err := s.readClient().Get(ctx, key, lease); err != nil {
		return nil, mapKubernetesError("get controller epoch Lease", err)
	}
	result, err := controllerEpochFromLease(normalized, lease)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// CompareAndSwapControllerEpoch creates or advances the authoritative Lease.
// A same-holder, same-digest retry of an already committed target epoch is
// idempotent even when the caller's expected values are stale.
//
//nolint:gocyclo // Creation, idempotency, mutation-lock, and epoch CAS checks form one boundary.
func (s *Store) CompareAndSwapControllerEpoch(ctx context.Context, change store.ControllerEpochCAS) (*store.ControllerEpoch, error) {
	if err := s.requireClient(); err != nil {
		return nil, err
	}
	name, err := normalizeControllerEpochName(change.Name)
	if err != nil {
		return nil, err
	}
	change.Name = name
	change.HolderID = strings.TrimSpace(change.HolderID)
	if err := store.ValidateControlIdentifier("controller epoch holder ID", change.HolderID); err != nil {
		return nil, err
	}
	if err := store.ValidateCanonicalDigest("controller epoch request digest", change.RequestDigest); err != nil {
		return nil, err
	}
	if change.ExpectedVersion < 0 || change.ExpectedEpoch < 0 {
		return nil, store.ValidationErrorf("controller epoch expected version and epoch must not be negative")
	}
	if change.NewEpoch < 1 {
		return nil, store.ValidationErrorf("controller new epoch must be at least 1")
	}
	change.UpdatedAt = normalizeControlTime(change.UpdatedAt)

	if err := s.ensureControllerEpochObject(ctx, change.Name); err != nil {
		return nil, err
	}

	key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochLeaseName(change.Name)}
	lease := &coordinationv1.Lease{}
	err = s.readClient().Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
		if change.ExpectedVersion != 0 || change.ExpectedEpoch != 0 || change.NewEpoch != 1 {
			return nil, controlConflict("controller epoch %q does not exist; creation requires expected version/epoch 0 and new epoch 1", change.Name)
		}
		createdLease := newControllerEpochLease(s.controlNamespace, change)
		if createErr := s.client.Create(ctx, createdLease); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				return s.CompareAndSwapControllerEpoch(ctx, change)
			}
			return nil, mapKubernetesError("create controller epoch Lease", createErr)
		}
		result, parseErr := controllerEpochFromLease(change.Name, createdLease)
		if parseErr != nil {
			return nil, parseErr
		}
		if syncErr := s.syncControllerEpochStatus(ctx, result, createdLease.Name, createdLease.ResourceVersion); syncErr != nil {
			return nil, syncErr
		}
		return &result, nil
	}
	if err != nil {
		return nil, mapKubernetesError("get controller epoch Lease", err)
	}

	current, err := controllerEpochFromLease(change.Name, lease)
	if err != nil {
		return nil, err
	}
	if current.Epoch == change.NewEpoch {
		if current.HolderID != change.HolderID || current.RequestDigest != change.RequestDigest {
			return nil, controlConflict("controller epoch %q target %d already exists with different holder or digest", change.Name, change.NewEpoch)
		}
		if syncErr := s.syncControllerEpochStatus(ctx, current, lease.Name, lease.ResourceVersion); syncErr != nil {
			return nil, syncErr
		}
		return &current, nil
	}
	if current.Version != change.ExpectedVersion || current.Epoch != change.ExpectedEpoch {
		return nil, controlConflict("controller epoch %q is version %d epoch %d, expected version %d epoch %d", change.Name, current.Version, current.Epoch, change.ExpectedVersion, change.ExpectedEpoch)
	}
	if change.NewEpoch != change.ExpectedEpoch+1 {
		return nil, store.ValidationErrorf("controller epoch must advance exactly by one: expected new epoch %d", change.ExpectedEpoch+1)
	}
	if active, expiresAt, lockErr := controllerEpochMutationLock(lease); lockErr != nil {
		return nil, lockErr
	} else if active && expiresAt.After(time.Now().UTC()) {
		return nil, controlConflict("controller epoch %q has an active control-store mutation until %s", change.Name, expiresAt.Format(time.RFC3339Nano))
	}

	updated := lease.DeepCopy()
	setControllerEpochLease(updated, change, current.Version+1)
	if err := s.client.Update(ctx, updated); err != nil {
		return nil, mapKubernetesError("compare-and-swap controller epoch Lease", err)
	}
	result, err := controllerEpochFromLease(change.Name, updated)
	if err != nil {
		return nil, err
	}
	if err := s.syncControllerEpochStatus(ctx, result, updated.Name, updated.ResourceVersion); err != nil {
		return nil, err
	}
	return &result, nil
}

func controllerEpochLeaseName(name string) string {
	return objectName(controllerEpochLeasePrefix, name)
}

func controllerEpochObjectName(name string) string {
	return objectName(controllerEpochNamePrefix, name)
}

func newControllerEpochLease(namespace string, change store.ControllerEpochCAS) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      controllerEpochLeaseName(change.Name),
			Labels:    controlLabels(change.Name),
		},
	}
	setControllerEpochLease(lease, change, 1)
	return lease
}

func setControllerEpochLease(lease *coordinationv1.Lease, change store.ControllerEpochCAS, version int64) {
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[annotationLogicalName] = change.Name
	lease.Annotations[annotationControllerEpoch] = strconv.FormatInt(change.NewEpoch, 10)
	lease.Annotations[annotationDomainVersion] = strconv.FormatInt(version, 10)
	lease.Annotations[annotationRequestDigest] = change.RequestDigest
	lease.Annotations[annotationAcquiredAt] = formatTime(change.UpdatedAt)
	delete(lease.Annotations, annotationMutationToken)
	delete(lease.Annotations, annotationMutationExpiresAt)
	holder := change.HolderID
	acquired := metav1.NewMicroTime(change.UpdatedAt)
	lease.Spec.HolderIdentity = &holder
	lease.Spec.AcquireTime = &acquired
	lease.Spec.RenewTime = &acquired
}

func controllerEpochFromLease(name string, lease *coordinationv1.Lease) (store.ControllerEpoch, error) {
	if lease == nil || lease.Spec.HolderIdentity == nil || strings.TrimSpace(*lease.Spec.HolderIdentity) == "" {
		return store.ControllerEpoch{}, fmt.Errorf("controller epoch Lease %q has no holder", controllerEpochLeaseName(name))
	}
	annotations := lease.Annotations
	if annotations[annotationLogicalName] != name {
		return store.ControllerEpoch{}, fmt.Errorf("controller epoch Lease %q logical name mismatch", lease.Name)
	}
	epoch, err := parsePositiveInt64("controller epoch", annotations[annotationControllerEpoch])
	if err != nil {
		return store.ControllerEpoch{}, err
	}
	version, err := parsePositiveInt64("controller epoch version", annotations[annotationDomainVersion])
	if err != nil {
		return store.ControllerEpoch{}, err
	}
	requestDigest := annotations[annotationRequestDigest]
	if err := store.ValidateCanonicalDigest("controller epoch request digest", requestDigest); err != nil {
		return store.ControllerEpoch{}, fmt.Errorf("invalid controller epoch Lease: %w", err)
	}
	acquiredAt, err := parseTime("controller epoch acquisition time", annotations[annotationAcquiredAt])
	if err != nil {
		return store.ControllerEpoch{}, err
	}
	return store.ControllerEpoch{
		Name:          name,
		Epoch:         epoch,
		HolderID:      *lease.Spec.HolderIdentity,
		RequestDigest: requestDigest,
		Version:       version,
		AcquiredAt:    acquiredAt,
		UpdatedAt:     acquiredAt,
	}, nil
}

func (s *Store) ensureControllerEpochObject(ctx context.Context, name string) error {
	key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochObjectName(name)}
	object := &corev1alpha1.ControllerEpoch{}
	if err := s.readClient().Get(ctx, key, object); err == nil {
		if object.Spec.Name != name {
			return controlConflict("controller epoch object %s/%s has a different logical name", key.Namespace, key.Name)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return mapKubernetesError("get controller epoch object", err)
	}
	object = &corev1alpha1.ControllerEpoch{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.controlNamespace,
			Name:      key.Name,
			Labels:    controlLabels(name),
		},
		Spec: corev1alpha1.ControllerEpochSpec{Name: name},
	}
	if err := s.client.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
		return mapKubernetesError("create controller epoch object", err)
	}
	return nil
}

func (s *Store) syncControllerEpochStatus(ctx context.Context, epoch store.ControllerEpoch, leaseName, leaseResourceVersion string) error {
	key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochObjectName(epoch.Name)}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		object := &corev1alpha1.ControllerEpoch{}
		if err := s.readClient().Get(ctx, key, object); err != nil {
			return err
		}
		if object.Spec.Name != epoch.Name {
			return controlConflict("controller epoch object %s/%s has a different logical name", key.Namespace, key.Name)
		}
		object.Status = corev1alpha1.ControllerEpochStatus{
			Epoch:                epoch.Epoch,
			HolderID:             epoch.HolderID,
			RequestDigest:        epoch.RequestDigest,
			Version:              epoch.Version,
			AcquiredAt:           metaTime(epoch.AcquiredAt),
			UpdatedAt:            metaTime(epoch.UpdatedAt),
			LeaseName:            leaseName,
			LeaseResourceVersion: leaseResourceVersion,
		}
		return s.client.Status().Update(ctx, object)
	})
}

func (s *Store) requireControllerEpoch(ctx context.Context, fence store.ControllerEpochFence) (store.ControllerEpochFence, epochSnapshot, error) {
	normalized, err := normalizeEpochFence(fence)
	if err != nil {
		return store.ControllerEpochFence{}, epochSnapshot{}, err
	}
	const (
		acquireTimeout = 2 * time.Second
		maxRetryDelay  = 200 * time.Millisecond
	)
	deadline := time.Now().Add(acquireTimeout)
	retryDelay := 10 * time.Millisecond
	var lastConflict error
	for {
		lease := &coordinationv1.Lease{}
		key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochLeaseName(normalized.Name)}
		if err := s.readClient().Get(ctx, key, lease); err != nil {
			if apierrors.IsNotFound(err) {
				return store.ControllerEpochFence{}, epochSnapshot{}, controlConflict("controller epoch %q does not exist", normalized.Name)
			}
			return store.ControllerEpochFence{}, epochSnapshot{}, mapKubernetesError("get controller epoch fence Lease", err)
		}
		current, err := controllerEpochFromLease(normalized.Name, lease)
		if err != nil {
			return store.ControllerEpochFence{}, epochSnapshot{}, err
		}
		if current.Epoch != normalized.Epoch || current.HolderID != normalized.HolderID {
			return store.ControllerEpochFence{}, epochSnapshot{}, controlConflict("controller epoch fence %s/%d/%s does not match current %d/%s", normalized.Name, normalized.Epoch, normalized.HolderID, current.Epoch, current.HolderID)
		}
		active, expiresAt, err := controllerEpochMutationLock(lease)
		if err != nil {
			return store.ControllerEpochFence{}, epochSnapshot{}, err
		}
		now := time.Now().UTC()
		if active && expiresAt.After(now) {
			lastConflict = controlConflict("controller epoch %q is serializing another control-store mutation until %s", normalized.Name, expiresAt.Format(time.RFC3339Nano))
		} else {
			updated := lease.DeepCopy()
			if updated.Annotations == nil {
				updated.Annotations = map[string]string{}
			}
			token := uuid.NewString()
			updated.Annotations[annotationMutationToken] = token
			updated.Annotations[annotationMutationExpiresAt] = formatTime(now.Add(2 * time.Minute))
			if err := s.client.Update(ctx, updated); err == nil {
				return normalized, epochSnapshot{
					Name: current.Name, Epoch: current.Epoch, HolderID: current.HolderID,
					LeaseResourceVersion: updated.ResourceVersion, MutationToken: token, MutationLease: updated.DeepCopy(),
				}, nil
			} else if apierrors.IsConflict(err) {
				lastConflict = mapKubernetesError("acquire controller epoch mutation fence", err)
			} else {
				return store.ControllerEpochFence{}, epochSnapshot{}, mapKubernetesError("acquire controller epoch mutation fence", err)
			}
		}

		if !time.Now().Before(deadline) {
			return store.ControllerEpochFence{}, epochSnapshot{}, lastConflict
		}
		wait := retryDelay
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return store.ControllerEpochFence{}, epochSnapshot{}, ctx.Err()
		case <-timer.C:
		}
		retryDelay = min(retryDelay*2, maxRetryDelay)
	}
}

func controllerEpochMutationLock(lease *coordinationv1.Lease) (bool, time.Time, error) {
	if lease == nil || lease.Annotations == nil || lease.Annotations[annotationMutationToken] == "" {
		return false, time.Time{}, nil
	}
	expiresAt, err := parseTime("controller epoch mutation expiry", lease.Annotations[annotationMutationExpiresAt])
	if err != nil {
		return false, time.Time{}, err
	}
	return true, expiresAt, nil
}

func (s *Store) releaseControllerEpochMutation(snapshot epochSnapshot) {
	if snapshot.MutationToken == "" || snapshot.MutationLease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease := snapshot.MutationLease.DeepCopy()
	_ = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if lease.Annotations[annotationMutationToken] != snapshot.MutationToken || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != snapshot.HolderID {
			return nil
		}
		epoch, err := parsePositiveInt64("controller epoch", lease.Annotations[annotationControllerEpoch])
		if err != nil || epoch != snapshot.Epoch {
			return nil
		}
		updated := lease.DeepCopy()
		delete(updated.Annotations, annotationMutationToken)
		delete(updated.Annotations, annotationMutationExpiresAt)
		if err := s.client.Update(ctx, updated); err != nil {
			if !apierrors.IsConflict(err) {
				return err
			}
			refreshed := &coordinationv1.Lease{}
			key := types.NamespacedName{Namespace: lease.Namespace, Name: lease.Name}
			if getErr := s.readClient().Get(ctx, key, refreshed); getErr != nil {
				return getErr
			}
			lease = refreshed
			return err
		}
		return nil
	})
}
