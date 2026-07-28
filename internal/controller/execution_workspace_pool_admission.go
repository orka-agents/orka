package controller

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

const (
	workspacePoolAdmissionLeaseDuration     = 30 * time.Second
	workspacePoolAdmissionCriticalTimeout   = 10 * time.Second
	workspacePoolAdmissionLeaseAttempts     = 8
	workspacePoolAdmissionContentionRequeue = time.Second
)

var (
	errWorkspacePoolAdmissionContended  = errors.New("workspace pool admission is contended")
	errWorkspacePoolCapacityUnavailable = errors.New("workspace pool capacity is unavailable")
)

func (r *ExecutionWorkspaceReconciler) acquireWorkspacePoolAdmission(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (func() error, *workspacev1alpha1.ImmutableObjectBinding, error) {
	if workspace == nil || workspace.Spec.CoreAdmission != nil {
		return func() error { return nil }, nil, nil
	}
	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	classKey := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Spec.ClassBinding.Name}
	if err := r.workspacePolicyReader().Get(ctx, classKey, class); err != nil {
		return nil, nil, fmt.Errorf("get workspace class for pool admission: %w", err)
	}
	if class.Spec.PoolRef == nil {
		return func() error { return nil }, nil, nil
	}
	pool := &workspacev1alpha1.ExecutionWorkspacePool{}
	poolKey := types.NamespacedName{Namespace: class.Namespace, Name: class.Spec.PoolRef.Name}
	if err := r.workspacePolicyReader().Get(ctx, poolKey, pool); err != nil {
		return nil, nil, fmt.Errorf("get workspace pool for admission lock: %w", err)
	}
	holder, err := newWorkspacePoolAdmissionHolder(workspace.UID)
	if err != nil {
		return nil, nil, err
	}
	acquired, err := r.acquireWorkspacePoolAdmissionLease(ctx, pool, holder)
	if err != nil {
		return nil, nil, err
	}
	if !acquired {
		return nil, nil, errWorkspacePoolAdmissionContended
	}
	release := func() error {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return r.releaseWorkspacePoolAdmissionLease(releaseCtx, pool, holder)
	}
	lockedPool := &workspacev1alpha1.ExecutionWorkspacePool{}
	if err := r.workspacePolicyReader().Get(ctx, poolKey, lockedPool); err != nil {
		releaseErr := release()
		return nil, nil, errors.Join(fmt.Errorf("re-read locked workspace pool: %w", err), releaseErr)
	}
	if lockedPool.UID != pool.UID {
		releaseErr := release()
		return nil, nil, errors.Join(fmt.Errorf("workspace pool was replaced during admission"), releaseErr)
	}
	reason, _, err := validateWorkspacePoolObject(lockedPool, workspace.Spec.ProviderBinding.Name)
	if err != nil {
		releaseErr := release()
		return nil, nil, errors.Join(err, releaseErr)
	}
	if reason != string(workspacev1alpha1.ReasonReady) {
		releaseErr := release()
		return nil, nil, errors.Join(errWorkspacePoolCapacityUnavailable, releaseErr)
	}
	classValidator := &ExecutionWorkspaceClassReconciler{
		Client: r.Client, APIReader: r.workspacePolicyReader(), RESTMapper: r.RESTMapper,
	}
	resolvedHash, err := classValidator.resolvedClassProfileHash(ctx, class)
	if err != nil {
		releaseErr := release()
		return nil, nil, errors.Join(fmt.Errorf("resolve locked workspace class profile: %w", err), releaseErr)
	}
	if resolvedHash != workspace.Spec.ClassBinding.ProfileHash {
		releaseErr := release()
		return nil, nil, errors.Join(fmt.Errorf("workspace class profile changed during pool admission"), releaseErr)
	}
	available, err := r.workspacePoolHasAdmissionCapacity(ctx, lockedPool, workspace)
	if err != nil {
		releaseErr := release()
		return nil, nil, errors.Join(err, releaseErr)
	}
	if !available {
		releaseErr := release()
		return nil, nil, errors.Join(errWorkspacePoolCapacityUnavailable, releaseErr)
	}
	return release, &workspacev1alpha1.ImmutableObjectBinding{
		Name: lockedPool.Name, UID: lockedPool.UID, Generation: lockedPool.Generation,
	}, nil
}

func (r *ExecutionWorkspaceReconciler) workspacePoolHasAdmissionCapacity(
	ctx context.Context,
	pool *workspacev1alpha1.ExecutionWorkspacePool,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	workspaces := &workspacev1alpha1.ExecutionWorkspaceList{}
	if err := r.workspacePolicyReader().List(ctx, workspaces, client.InNamespace(pool.Namespace)); err != nil {
		return false, fmt.Errorf("list workspace pool reservations: %w", err)
	}
	reserved := int32(0)
	for i := range workspaces.Items {
		candidate := &workspaces.Items[i]
		if candidate.UID == workspace.UID || !workspaceHasCoreAdmission(candidate) ||
			workspaceComputeCapacityReleased(candidate) || candidate.Spec.CoreAdmission.PoolBinding == nil ||
			candidate.Spec.CoreAdmission.PoolBinding.UID != pool.UID {
			continue
		}
		reserved++
	}
	return reserved < pool.Spec.Capacity.MaxSize, nil
}

func (r *ExecutionWorkspaceReconciler) acquireWorkspacePoolAdmissionLease(
	ctx context.Context,
	pool *workspacev1alpha1.ExecutionWorkspacePool,
	holder string,
) (bool, error) {
	key := r.workspacePoolAdmissionLeaseKey(pool)
	now := time.Now().UTC()
	durationSeconds := int32(workspacePoolAdmissionLeaseDuration / time.Second)
	for range workspacePoolAdmissionLeaseAttempts {
		lease := &coordinationv1.Lease{}
		err := r.workspacePolicyReader().Get(ctx, key, lease)
		if apierrors.IsNotFound(err) {
			lease = &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &holder,
					LeaseDurationSeconds: &durationSeconds,
					AcquireTime:          &metav1.MicroTime{Time: now},
					RenewTime:            &metav1.MicroTime{Time: now},
				},
			}
			if createErr := r.Create(ctx, lease); createErr == nil {
				return true, nil
			} else if apierrors.IsAlreadyExists(createErr) || apierrors.IsConflict(createErr) {
				continue
			} else {
				return false, fmt.Errorf("create workspace pool admission Lease: %w", createErr)
			}
		}
		if err != nil {
			return false, fmt.Errorf("get workspace pool admission Lease: %w", err)
		}
		if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != holder && !workspaceAdmissionLeaseExpired(lease, now) {
			return false, nil
		}
		before := lease.DeepCopy()
		lease.Spec.HolderIdentity = &holder
		lease.Spec.LeaseDurationSeconds = &durationSeconds
		lease.Spec.RenewTime = &metav1.MicroTime{Time: now}
		if lease.Spec.AcquireTime == nil || workspaceAdmissionLeaseExpired(before, now) {
			lease.Spec.AcquireTime = &metav1.MicroTime{Time: now}
		}
		if err := r.Patch(ctx, lease, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err == nil {
			return true, nil
		} else if apierrors.IsConflict(err) {
			continue
		} else {
			return false, fmt.Errorf("claim workspace pool admission Lease: %w", err)
		}
	}
	return false, fmt.Errorf("claim workspace pool admission Lease after %d attempts", workspacePoolAdmissionLeaseAttempts)
}

func (r *ExecutionWorkspaceReconciler) releaseWorkspacePoolAdmissionLease(
	ctx context.Context,
	pool *workspacev1alpha1.ExecutionWorkspacePool,
	holder string,
) error {
	key := r.workspacePoolAdmissionLeaseKey(pool)
	for range workspacePoolAdmissionLeaseAttempts {
		lease := &coordinationv1.Lease{}
		if err := r.workspacePolicyReader().Get(ctx, key, lease); err != nil {
			return client.IgnoreNotFound(err)
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
			return nil
		}
		before := lease.DeepCopy()
		lease.Spec.HolderIdentity = nil
		lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now().UTC()}
		if err := r.Patch(ctx, lease, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err == nil {
			return nil
		} else if apierrors.IsConflict(err) {
			continue
		} else {
			return fmt.Errorf("release workspace pool admission Lease: %w", err)
		}
	}
	return fmt.Errorf("release workspace pool admission Lease after %d attempts", workspacePoolAdmissionLeaseAttempts)
}

func (r *ExecutionWorkspaceReconciler) workspacePoolAdmissionLeaseKey(
	pool *workspacev1alpha1.ExecutionWorkspacePool,
) types.NamespacedName {
	namespace := strings.TrimSpace(r.AdmissionLeaseNamespace)
	if namespace == "" {
		namespace = pool.Namespace
	}
	return types.NamespacedName{Namespace: namespace, Name: workspacePoolAdmissionLeaseName(pool)}
}

func workspaceComputeCapacityReleased(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil ||
		(workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted && workspace.DeletionTimestamp.IsZero()) ||
		workspace.Status.ObservedGeneration != workspace.Generation ||
		workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateDeleted ||
		workspace.Status.Disposition == nil {
		return false
	}
	return workspace.Status.Disposition.Compute == workspacev1alpha1.DispositionDeleted ||
		workspace.Status.Disposition.Compute == workspacev1alpha1.DispositionNotApplicable
}

func newWorkspacePoolAdmissionHolder(workspaceUID types.UID) (string, error) {
	var nonce [16]byte
	if _, err := cryptorand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate workspace pool admission holder: %w", err)
	}
	return string(workspaceUID) + "/" + hex.EncodeToString(nonce[:]), nil
}

func workspaceAdmissionLeaseExpired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease == nil || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return !lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second).After(now)
}

func workspacePoolAdmissionLeaseName(pool *workspacev1alpha1.ExecutionWorkspacePool) string {
	digest := sha256.Sum256([]byte(string(pool.UID) + "/" + pool.Namespace + "/" + pool.Name))
	return "workspace-admission-" + hex.EncodeToString(digest[:])[:16]
}
