package service

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

const (
	noEffectsSettlementTimeout = 5 * time.Second
	noEffectsInitialBackoff    = 25 * time.Millisecond
	noEffectsMaxBackoff        = 250 * time.Millisecond
)

var errNoEffectsObsolete = errors.New("no-effects proof is no longer applicable")

type noEffectsKey struct {
	run       string
	operation string
}

type noEffectsProof struct {
	stateUID      types.UID
	requestDigest string
	inputDigest   string
	attempt       uint64
	retry         bool
}

func (s *Coordinator) accountNoEffects(ctx context.Context, r *run, operation string, expected *executionOperation, failure error) (bool, bool, error) {
	noEffects, ok := errors.AsType[noEffectsBackendError](failure)
	if !ok || !noEffects.NoEffects() {
		return false, false, nil
	}
	retryable, hasRetry := errors.AsType[retryableBackendError](failure)
	cancelledAdmission := ctx.Err() != nil &&
		(errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded))
	proof := noEffectsProof{
		stateUID: r.object.UID, requestDigest: expected.Request.Digest, inputDigest: expected.InputDigest,
		attempt: expected.AdmissionAttempt,
		retry:   !errors.Is(failure, contract.ErrInvalid) && ((hasRetry && retryable.Retryable()) || cancelledAdmission),
	}
	key := noEffectsKey{run: r.state.ID, operation: operation}
	s.mu.Lock()
	if previous, exists := s.noEffects[key]; exists && previous != proof {
		s.mu.Unlock()
		return false, false, errNoEffectsObsolete
	}
	s.noEffects[key] = proof
	s.mu.Unlock()
	applied, err := s.persistNoEffects(ctx, key, proof)
	return applied, proof.retry, err
}

func (s *Coordinator) flushNoEffects(ctx context.Context, id string) error {
	s.mu.Lock()
	pending := make(map[noEffectsKey]noEffectsProof)
	for key, proof := range s.noEffects {
		if key.run == id {
			pending[key] = proof
		}
	}
	s.mu.Unlock()
	for key, proof := range pending {
		if _, err := s.persistNoEffects(ctx, key, proof); err != nil {
			return err
		}
	}
	return nil
}

func (s *Coordinator) persistNoEffects(ctx context.Context, key noEffectsKey, proof noEffectsProof) (bool, error) {
	// Only the captured proof authorizes this bookkeeping. Keep it in memory
	// across bounded persistence windows; process loss leaves durable uncertainty
	// fail-closed rather than inventing permission for another Start.
	accounting, cancel := context.WithTimeout(context.WithoutCancel(ctx), noEffectsSettlementTimeout)
	defer cancel()
	backoff := noEffectsInitialBackoff
	for {
		applied, err := s.persistNoEffectsOnce(accounting, key, proof)
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) ||
			apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			err = errNoEffectsObsolete
		}
		if err == nil || errors.Is(err, errNoEffectsObsolete) {
			// Obsolete or permanently rejected proof releases only its matching
			// memory entry, never durable admission state or a newer proof.
			s.mu.Lock()
			if s.noEffects[key] == proof {
				delete(s.noEffects, key)
			}
			s.mu.Unlock()
			return applied, err
		}
		if !pvkube.IsTransientError(err) {
			if errors.Is(err, errRunIdentityChanged) {
				return false, err
			}
			if errors.Is(err, contract.ErrConflict) {
				return false, contract.ErrConflict
			}
			return false, contract.ErrUnavailable
		}
		timer := time.NewTimer(backoff)
		select {
		case <-accounting.Done():
			timer.Stop()
			return false, contract.ErrUnavailable
		case <-timer.C:
		}
		backoff = min(2*backoff, noEffectsMaxBackoff)
	}
}

func (s *Coordinator) persistNoEffectsOnce(ctx context.Context, key noEffectsKey, proof noEffectsProof) (bool, error) {
	object := new(corev1.ConfigMap)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: stateName(key.run)}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return false, errNoEffectsObsolete
		}
		return false, err
	}
	if object.UID != proof.stateUID {
		return false, errNoEffectsObsolete
	}
	if err := checkRunIdentity(ctx, object.Namespace, key.run, object.UID); err != nil {
		return false, err
	}
	r, err := decodeRun(object, key.run, s.config.Namespace)
	if err != nil {
		return false, errNoEffectsObsolete
	}
	op := r.state.Executions[key.operation]
	if op == nil || op.Request.Digest != proof.requestDigest || op.InputDigest != proof.inputDigest {
		return false, errNoEffectsObsolete
	}
	if op.AdmissionAttempt != proof.attempt || op.Receipt != nil || !op.Record.empty() {
		return false, nil
	}
	if !op.Attempted || op.Rejected {
		return (proof.retry && !op.Attempted && !op.Rejected) || (!proof.retry && op.Rejected), nil
	}
	if proof.retry {
		op.Attempted = false
	} else {
		op.Rejected = true
	}
	updated, err := stateObject(r)
	if err != nil {
		return false, errNoEffectsObsolete
	}
	// Preserve API error classification here; the generic run update helper
	// deliberately exposes sanitized domain errors to API callers.
	if err := s.config.Client.Update(ctx, updated); err != nil {
		return false, err
	}
	return true, nil
}
