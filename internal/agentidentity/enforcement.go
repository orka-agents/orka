package agentidentity

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
// Only such a marker can continue a period: a controller creates it after it
// wins leadership, so the leader it replaced, which may not have checked
// Agent use, no longer serves requests.
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

// enforcementTransitionAnnotation records on the marker the leader Lease's
// UID and transition count, as "<uid>/<count>", under the last enforcing
// leader that kept the period. The UID keeps a recreated leader Lease, whose
// count starts over, from matching an old count.
const enforcementTransitionAnnotation = "orka.ai/leader-transition"

// LeaderLease names the controller's leader election Lease and how to tell
// that this process holds it.
type LeaderLease struct {
	Key client.ObjectKey
	// HolderPrefix starts this process's holder identity. controller-runtime
	// names a holder by its hostname and a random suffix, and a process that
	// lost leadership exits, so a holder with this pod's hostname that is
	// still current is this process.
	HolderPrefix string
}

// transition returns the leader Lease's UID and transition count while this
// process holds it. Leader election advances the count whenever the holder
// changes, before the new leader runs anything.
func (l LeaderLease) transition(ctx context.Context, c client.Client) (string, int32, error) {
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, l.Key, lease); err != nil {
		return "", 0, fmt.Errorf("read the leader election Lease: %w", err)
	}
	if holder := lease.Spec.HolderIdentity; holder == nil || l.HolderPrefix == "" || !strings.HasPrefix(*holder, l.HolderPrefix) {
		return "", 0, fmt.Errorf("this controller does not hold the leader election Lease")
	}
	if lease.UID == "" {
		return "", 0, fmt.Errorf("the leader election Lease has no UID")
	}
	var count int32
	if lease.Spec.LeaseTransitions != nil {
		count = *lease.Spec.LeaseTransitions
	}
	return string(lease.UID), count, nil
}

// StartEnforcementPeriod starts the period of Agent use enforcement, or
// continues the one the previous leader kept. An enforcing controller calls
// it each time it wins leadership, never before.
//
// A period continues only when the leader just before this one enforced Agent
// use too: the marker records the leader Lease's transition count under the
// last enforcing leader that kept it, and this leader keeps it only when that
// count is its own or the one just before it, then records its own. Any other
// leader in between, such as one that did not check Agent use and had not yet
// deleted the marker, advanced the count, so a new period starts after it
// stopped serving. Without a leader Lease (leader election off), nothing shows
// who served in between, and every start begins a new period.
func StartEnforcementPeriod(ctx context.Context, c client.Client, namespace string, leader *LeaderLease) (time.Time, error) {
	recorded, leaseUID := "", ""
	var current int32
	if leader != nil {
		var err error
		if leaseUID, current, err = leader.transition(ctx, c); err != nil {
			return time.Time{}, err
		}
		recorded = leaseUID + "/" + strconv.FormatInt(int64(current), 10)
	}
	key := client.ObjectKey{Namespace: namespace, Name: EnforcementLeaseName}
	for range 3 {
		marker := &coordinationv1.Lease{}
		err := c.Get(ctx, key, marker)
		switch {
		case err == nil && leader != nil && enforcementContinues(marker, leaseUID, current):
			if marker.CreationTimestamp.IsZero() {
				return time.Time{}, fmt.Errorf("the Agent use enforcement marker has no creation time")
			}
			if marker.Annotations[enforcementTransitionAnnotation] != recorded {
				marker.Annotations[enforcementTransitionAnnotation] = recorded
				if err := c.Update(ctx, marker); apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
					continue
				} else if err != nil {
					return time.Time{}, fmt.Errorf("record this leader on the Agent use enforcement period: %w", err)
				}
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
		if leader != nil {
			marker.Annotations[enforcementTransitionAnnotation] = recorded
		}
		if err := c.Create(ctx, marker); apierrors.IsAlreadyExists(err) {
			continue
		} else if err != nil {
			return time.Time{}, fmt.Errorf("start the Agent use enforcement period: %w", err)
		}
		if marker.CreationTimestamp.IsZero() {
			return time.Time{}, fmt.Errorf("the Agent use enforcement marker has no creation time")
		}
		return marker.CreationTimestamp.Time, nil
	}
	return time.Time{}, fmt.Errorf("the Agent use enforcement marker kept changing")
}

// enforcementContinues reports whether a marker shows that the leader before
// the one at transition current of the leader Lease leaseUID enforced Agent
// use: a leader created it and recorded, for the same Lease, current or the
// transition just before it.
func enforcementContinues(marker *coordinationv1.Lease, leaseUID string, current int32) bool {
	if marker.Annotations[enforcementLeaderAnnotation] != enforcementLeaderAnnotationValue {
		return false
	}
	uid, count, found := strings.Cut(marker.Annotations[enforcementTransitionAnnotation], "/")
	recorded, err := strconv.ParseInt(count, 10, 32)
	return found && err == nil && uid == leaseUID && (int32(recorded) == current || int32(recorded) == current-1)
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
