package applicationexec

import (
	"context"
	"errors"
	"time"

	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrActive = errors.New("application execution has unsettled retained effects")

// EnsureNoActive guards disabling the backend or changing to source-harness.
// Readiness Tasks exist before coordinator runs, so looking only at coordinator
// state is insufficient to establish that removing target credentials is safe.
func EnsureNoActive(ctx context.Context, reader client.Reader, namespace string) error {
	if reader == nil {
		return contract.ErrInvalid
	}
	runs, err := activeRuns(ctx, reader, namespace)
	if err != nil {
		return err
	}
	if len(runs) != 0 {
		return ErrActive
	}
	return nil
}

func activeRuns(ctx context.Context, reader client.Reader, namespace string) ([]*run, error) {
	var active []*run
	token := ""
	for {
		objects := &corev1.ConfigMapList{}
		if err := reader.List(ctx, objects, client.InNamespace(namespace), client.MatchingLabels{stateLabel: "v1"},
			client.Limit(32), client.Continue(token)); err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
		for i := range objects.Items {
			expectedNamespace := namespace
			if expectedNamespace == "" {
				expectedNamespace = objects.Items[i].Namespace
			}
			r, err := decodeRun(&objects.Items[i], expectedNamespace)
			if err != nil {
				return nil, err
			}
			if !r.state.Settled {
				active = append(active, r)
				if len(active) > 32 {
					return nil, ErrActive
				}
			}
		}
		if objects.Continue == "" {
			return active, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
		token = objects.Continue
	}
}

func (b *Backend) recoverRetained(ctx context.Context) error {
	runs, err := activeRuns(ctx, b.controlReader, b.config.Namespace)
	if err != nil {
		return err
	}
	// Refuse configuration/target changes before doing anything. Cleanup must
	// be drained with the prior configuration rather than redirected to a new
	// API by a reused namespace or workload name.
	for _, r := range runs {
		if r.state.Configuration != b.ConfigurationDigest() || r.state.ControlUID != b.config.ControlClusterUID ||
			r.state.TargetUID != b.config.TargetClusterUID {
			return contract.ErrConflict
		}
		if r.state.Task.UID == "" && r.state.Task.Attempted && !r.state.Task.Rejected {
			return ErrUncertain
		}
	}
	for _, retained := range runs {
		if retained.state.Task.UID == "" {
			if err := b.recoverRootReservation(ctx, retained); err != nil {
				return err
			}
			continue
		}
		receipt := retained.receipt()
		for {
			current, err := b.boundRun(ctx, receipt)
			if err != nil {
				return err
			}
			if current.state.Settled {
				break
			}
			parentErr := b.parent(ctx, current)
			if current.state.Cancelled || current.state.Failed != "" || anyTaskRejected(current) || !time.Now().Before(current.state.Deadline) ||
				!current.object.DeletionTimestamp.IsZero() || errors.Is(parentErr, contract.ErrConflict) || apierrors.IsNotFound(parentErr) {
				if err := b.Cancel(ctx, receipt); err != nil {
					return err
				}
				break
			}
			if parentErr != nil {
				return parentErr
			}
			if err := b.lockEffects(ctx); err != nil {
				return err
			}
			if current.state.Qualification {
				_, err = b.advanceQualification(ctx, current)
			} else {
				_, _, err = b.advanceRequest(ctx, current.state.Request, receipt)
			}
			<-b.effectGate
			if err != nil {
				return err
			}
			if err := b.pause(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Backend) recoverRootReservation(ctx context.Context, r *run) error {
	if err := b.lockEffects(ctx); err != nil {
		return err
	}
	defer func() { <-b.effectGate }()
	return b.settleRootWithoutExecution(ctx, r, r.state.Task.Rejected)
}

func (b *Backend) sweepRootReservation(ctx context.Context, r *run) error {
	if r.state.Task.Rejected {
		return b.recoverRootReservation(ctx, r)
	}
	if r.state.Task.Attempted {
		return ErrUncertain
	}
	parentErr := b.parent(ctx, r)
	if r.state.Cancelled || r.state.Failed != "" || !r.object.DeletionTimestamp.IsZero() ||
		!time.Now().Before(r.state.Deadline) || errors.Is(parentErr, contract.ErrConflict) || apierrors.IsNotFound(parentErr) {
		return b.recoverRootReservation(ctx, r)
	}
	return parentErr
}
