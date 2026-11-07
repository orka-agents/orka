package agentidentity

import (
	"context"
	"fmt"
	"sync"
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

// enforcementLeaderAnnotation marks a marker a leading controller created.
// Only such a marker starts a period: a controller creates it after it wins
// leadership, so the leader it replaced, which may not have checked Agent
// use, no longer serves requests.
const enforcementLeaderAnnotation = "orka.ai/created-by-leader"

const enforcementLeaderAnnotationValue = "true"

// EndEnforcementPeriod deletes the marker, so the next enforcing leader
// starts a new period. A controller that does not enforce Agent use calls it
// before it serves any request and again once it leads, since an enforcing
// leader may have started a period in between. Tasks and automation sources
// created before a period's start are not covered by any identity claim,
// which keeps work created while the check was off from binding an identity
// whose claim survived the gap. Errors are returned so the caller can refuse
// to run: a marker left in place would let a later enforcing leader trust
// the gap.
func EndEnforcementPeriod(ctx context.Context, c client.Client, namespace string) error {
	marker := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: EnforcementLeaseName}}
	if err := c.Delete(ctx, marker); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("end the Agent use enforcement period: %w", err)
	}
	return nil
}

// StartEnforcementPeriod starts the period of Agent use enforcement, or
// returns the one an enforcing leader already started. An enforcing
// controller calls it each time it wins leadership, never before: the leader
// it replaces may not have checked Agent use, and a non-enforcing leader
// that ran since this controller started has ended any earlier period. A
// marker no leader created is replaced.
func StartEnforcementPeriod(ctx context.Context, c client.Client, namespace string) (time.Time, error) {
	key := client.ObjectKey{Namespace: namespace, Name: EnforcementLeaseName}
	for range 3 {
		marker := &coordinationv1.Lease{}
		err := c.Get(ctx, key, marker)
		switch {
		case err == nil && marker.Annotations[enforcementLeaderAnnotation] == enforcementLeaderAnnotationValue:
			if marker.CreationTimestamp.IsZero() {
				return time.Time{}, fmt.Errorf("the Agent use enforcement marker has no creation time")
			}
			return marker.CreationTimestamp.Time, nil
		case err == nil:
			uid := marker.UID
			if err := c.Delete(ctx, marker, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return time.Time{}, fmt.Errorf("replace the Agent use enforcement marker: %w", err)
			}
			continue
		case !apierrors.IsNotFound(err):
			return time.Time{}, fmt.Errorf("read the Agent use enforcement period: %w", err)
		}
		marker = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: EnforcementLeaseName,
			Annotations: map[string]string{enforcementLeaderAnnotation: enforcementLeaderAnnotationValue},
		}}
		if err := c.Create(ctx, marker); err != nil && !apierrors.IsAlreadyExists(err) {
			return time.Time{}, fmt.Errorf("start the Agent use enforcement period: %w", err)
		}
	}
	return time.Time{}, fmt.Errorf("the Agent use enforcement marker kept changing")
}

// EnforcementPeriod holds this controller's period start once it is known.
// Config copies share it, so a period started after leadership reaches every
// reconciler.
type EnforcementPeriod struct {
	mu      sync.RWMutex
	since   time.Time
	started bool
}

// Start records the period's start.
func (p *EnforcementPeriod) Start(since time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.since, p.started = since, true
}

// Since returns the period's start, or started=false before it is known.
func (p *EnforcementPeriod) Since() (since time.Time, started bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.since, p.started
}
