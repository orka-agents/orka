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

const EnforcementLeaseName = "orka-agent-use-enforcement"

const enforcementLeaderAnnotation = "orka.ai/created-by-leader"

const enforcementLeaderAnnotationValue = "true"

func EndEnforcementPeriod(ctx context.Context, c client.Client, namespace string) error {
	marker := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: EnforcementLeaseName}}
	if err := c.Delete(ctx, marker); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("end the Agent use enforcement period: %w", err)
	}
	return nil
}

const enforcementTransitionAnnotation = "orka.ai/leader-transition"

type LeaderLease struct {
	Key          client.ObjectKey
	HolderPrefix string
}

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

func enforcementContinues(marker *coordinationv1.Lease, leaseUID string, current int32) bool {
	if marker.Annotations[enforcementLeaderAnnotation] != enforcementLeaderAnnotationValue {
		return false
	}
	uid, count, found := strings.Cut(marker.Annotations[enforcementTransitionAnnotation], "/")
	recorded, err := strconv.ParseInt(count, 10, 32)
	return found && err == nil && uid == leaseUID && (int32(recorded) == current || int32(recorded) == current-1)
}

type EnforcementPeriod struct {
	mu      sync.RWMutex
	since   time.Time
	started bool
}

func (p *EnforcementPeriod) Start(since time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.since, p.started = since, true
}

func (p *EnforcementPeriod) Since() (since time.Time, started bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.since, p.started
}
