package agentidentity

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnforcementLeaseName names the Lease that marks an unbroken period of
// Agent use enforcement in the controller's namespace. It carries no holder;
// only its creation time, which the API server sets, is read.
const EnforcementLeaseName = "orka-agent-use-enforcement"

// EstablishEnforcementPeriod records whether this controller enforces Agent
// use, before it serves any request, and returns when the current period of
// enforcement began. A controller that enforces keeps the marker an earlier
// enforcing run created, or creates one; a controller that does not deletes
// it, so the next enforcing run starts a new period. Tasks and automation
// sources created before that start are not covered by any identity claim,
// which keeps work created while the check was off from binding an identity
// whose claim survived the gap. Errors are returned so the caller can refuse
// to start: without the marker a later enforcing run would trust a gap.
func EstablishEnforcementPeriod(ctx context.Context, c client.Client, namespace string, enforced bool) (time.Time, error) {
	key := client.ObjectKey{Namespace: namespace, Name: EnforcementLeaseName}
	if !enforced {
		marker := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: EnforcementLeaseName}}
		if err := c.Delete(ctx, marker); err != nil && !apierrors.IsNotFound(err) {
			return time.Time{}, fmt.Errorf("end the Agent use enforcement period: %w", err)
		}
		return time.Time{}, nil
	}
	marker := &coordinationv1.Lease{}
	err := c.Get(ctx, key, marker)
	if apierrors.IsNotFound(err) {
		marker = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: EnforcementLeaseName}}
		err = c.Create(ctx, marker)
		if apierrors.IsAlreadyExists(err) {
			err = c.Get(ctx, key, marker)
		}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("start the Agent use enforcement period: %w", err)
	}
	if marker.CreationTimestamp.IsZero() {
		return time.Time{}, fmt.Errorf("the Agent use enforcement marker has no creation time")
	}
	return marker.CreationTimestamp.Time, nil
}
