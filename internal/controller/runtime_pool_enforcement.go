package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errRuntimePoolEnforcementRetirementPending = errors.New("retained physical enforcement binding must retire before starting a replacement runtime")

// A live binding remains owned by the pool through ordinary drain. Only
// requireRetired makes it block finalization; inactive bindings always block
// replacement until every provider confirms retirement.
func (r *RuntimePoolReconciler) reconcileRuntimePoolEnforcementRetirement(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	requireRetired bool,
) (pending bool, err error) {
	if pool.Status.EnforcementBinding == nil {
		return false, nil
	}
	if r.APIReader == nil || r.Enforcement == nil {
		return false, errors.New("physical enforcement cleanup requires a fresh API reader and its operator-trusted providers")
	}
	binding := *pool.Status.EnforcementBinding
	pod := &corev1.Pod{}
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: binding.PodNamespace, Name: binding.PodName}, pod)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("observe retained enforcement container: %w", err)
	}
	if err == nil && string(pod.UID) == binding.PodUID {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == binding.ContainerName && status.ContainerID == binding.ContainerID &&
				status.State.Running != nil {
				return requireRetired, nil
			}
		}
	}
	// Kubernetes absence is not physical-stop proof. Providers retain controls
	// until their own runtime check succeeds and seal delayed Ensure requests.
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	retired, err := r.Enforcement.Retire(callCtx, &binding)
	cancel()
	if err != nil || !retired {
		return !retired, err
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1alpha1.RuntimePool{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(pool), current); err != nil {
			return err
		}
		if current.UID != pool.UID {
			return fmt.Errorf("%w: enforcement pool incarnation changed during retirement", store.ErrConflict)
		}
		if current.Status.EnforcementBinding != nil {
			if !reflect.DeepEqual(current.Status.EnforcementBinding, &binding) {
				return fmt.Errorf("%w: physical enforcement association changed during retirement", store.ErrConflict)
			}
			base := current.DeepCopy()
			current.Status.EnforcementBinding = nil
			if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
		*pool = *current
		return nil
	})
	return false, err
}
