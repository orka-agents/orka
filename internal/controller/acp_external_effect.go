package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

const acpExternalEffectLease = 5 * time.Minute

var externalEffectLeaseSequence atomic.Uint64

// runACPExternalEffect persists a canonical pre-execution identity before one
// idempotent publisher/broker operation. A committed response is replayed from
// the ledger; an in-flight identity is reclaimed only under the current epoch
// and exact request digest.
func runACPExternalEffect[T any](
	ctx context.Context,
	d *ACPDispatcher,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	call func(context.Context) (T, error),
) (T, error) {
	return runExternalEffect(ctx, d.Store, fence, identity, request, call)
}

func runExternalEffect[T any](
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	call func(context.Context) (T, error),
) (T, error) {
	response, _, err := runExternalEffectWithReplay(ctx, effects, fence, identity, request, call)
	return response, err
}

func runExternalEffectWithReplay[T any](
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	call func(context.Context) (T, error),
) (T, bool, error) {
	var zero T
	if effects == nil {
		return zero, false, fmt.Errorf("external-effect store is required")
	}
	requestDigest, err := acpDomainDigest("external-effect-request", map[string]any{
		"identity": identity, "request": request,
	})
	if err != nil {
		return zero, false, err
	}
	now := time.Now().UTC()
	effect, err := effects.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
		Identity: identity, RequestDigest: requestDigest, Fence: fence, CreatedAt: now,
	})
	if err != nil {
		return zero, false, err
	}
	if effect.State == store.ExternalEffectSucceeded {
		var response T
		if len(effect.Response) == 0 || json.Unmarshal(effect.Response, &response) != nil {
			return zero, false, fmt.Errorf("external effect %s has an invalid committed response", effect.ID)
		}
		return response, true, nil
	}
	if effect.State == store.ExternalEffectFailed || effect.State == store.ExternalEffectOutcomeUnknown {
		return zero, false, fmt.Errorf("external effect %s is terminal in state %s", effect.ID, effect.State)
	}
	leaseExpiry := now.Add(acpExternalEffectLease)
	leaseOwner := externalEffectLeaseOwner(fence, identity, now)
	claimed, err := effects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: effect.State,
		NewState: store.ExternalEffectInFlight, RequestDigest: requestDigest,
		ExpectedLeaseOwner: effect.LeaseOwner, LeaseOwner: leaseOwner, LeaseExpiresAt: &leaseExpiry, UpdatedAt: now,
	})
	if err != nil {
		return zero, false, err
	}
	response, callErr := call(ctx)
	if callErr != nil {
		// Leave the exact effect in-flight. The same identity/digest may be
		// reclaimed and classified by a later reconciliation; a different request
		// cannot reuse it.
		return zero, false, callErr
	}
	if err := ctx.Err(); err != nil {
		// The side effect may have crossed its external boundary, but prompt
		// authority was revoked before a response could be committed. Leave the
		// ledger in-flight for explicit reconciliation rather than claiming success.
		return zero, false, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return zero, false, err
	}
	sum := sha256.Sum256(encoded)
	responseDigest := "sha256:" + hex.EncodeToString(sum[:])
	completed, err := effects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: claimed.ID, Fence: fence, ExpectedVersion: claimed.Version, ExpectedState: store.ExternalEffectInFlight,
		NewState: store.ExternalEffectSucceeded, RequestDigest: requestDigest,
		ResponseDigest: responseDigest, Response: encoded, ExpectedLeaseOwner: leaseOwner,
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return zero, false, err
	}
	if completed.State != store.ExternalEffectSucceeded {
		return zero, false, fmt.Errorf("external effect %s did not commit success", completed.ID)
	}
	return response, false, nil
}

func externalEffectLeaseOwner(fence store.ControllerEpochFence, identity store.ExternalEffectIdentity, now time.Time) string {
	sequence := externalEffectLeaseSequence.Add(1)
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s\x00%d", fence.HolderID, fence.Epoch, identity.OperationID, now.UTC().Format(time.RFC3339Nano), sequence))
	return "effect-" + hex.EncodeToString(sum[:16])
}

// runACPExternalEffectWithRetry retries the same immutable external-effect
// identity until it commits, a non-retryable response is proven, or the caller's
// bounded reconciliation context expires. Prompt input is never replayed.
func runACPExternalEffectWithRetry[T any](
	ctx context.Context,
	d *ACPDispatcher,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	call func(context.Context) (T, error),
) (T, error) {
	var zero T
	for {
		value, err := runACPExternalEffect(ctx, d, fence, identity, request, call)
		if err == nil {
			return value, nil
		}
		if !retryableACPExternalEffectError(err) {
			return zero, err
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, fmt.Errorf("bounded external-effect reconciliation expired: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func retryableACPExternalEffectError(err error) bool {
	if err == nil {
		return false
	}
	var clientErr *publisherservice.ClientError
	if errors.As(err, &clientErr) {
		return clientErr.Response.Retryable || clientErr.StatusCode == http.StatusTooManyRequests || clientErr.StatusCode >= 500
	}
	if errors.Is(err, store.ErrConflict) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	// Transport failures intentionally do not expose a typed upstream response.
	// Their side-effect outcome is ambiguous and must be reconciled with the same
	// durable identity rather than classified as a deterministic request error.
	return true
}

func settleACPExternalEffect(
	ctx context.Context,
	d *ACPDispatcher,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	state store.ExternalEffectState,
	response any,
) error {
	return settleExternalEffectStore(ctx, d.Store, fence, identity, state, response)
}

func settleExternalEffectStore(
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	state store.ExternalEffectState,
	response any,
) error {
	id, err := identity.CanonicalID()
	if err != nil {
		return err
	}
	effect, err := effects.GetExternalEffect(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if effect.State == state {
		return nil
	}
	if effect.State == store.ExternalEffectSucceeded || effect.State == store.ExternalEffectFailed || effect.State == store.ExternalEffectOutcomeUnknown {
		return fmt.Errorf("external effect %s is already terminal in state %s", effect.ID, effect.State)
	}
	var encoded json.RawMessage
	responseDigest := ""
	if response != nil {
		value, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			return marshalErr
		}
		encoded = value
		sum := sha256.Sum256(value)
		responseDigest = "sha256:" + hex.EncodeToString(sum[:])
	}
	_, err = effects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: effect.State,
		NewState: state, RequestDigest: effect.RequestDigest, ResponseDigest: responseDigest, Response: encoded,
		ExpectedLeaseOwner: effect.LeaseOwner, UpdatedAt: time.Now().UTC(),
	})
	return err
}

func settleACPExternalEffectError(
	ctx context.Context,
	d *ACPDispatcher,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	callErr error,
) error {
	state := store.ExternalEffectOutcomeUnknown
	var clientErr *publisherservice.ClientError
	if errors.As(callErr, &clientErr) && !clientErr.Response.Retryable && clientErr.StatusCode != http.StatusTooManyRequests && clientErr.StatusCode < 500 {
		state = store.ExternalEffectFailed
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return settleACPExternalEffect(settleCtx, d, fence, identity, state, nil)
}

const acpExternalEffectReconcileGrace = time.Minute

func (d *ACPDispatcher) reconcileExpiredExternalEffects(ctx context.Context) error {
	if d.Client == nil || d.Store == nil || d.Epochs == nil {
		return nil
	}
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	var effects corev1alpha1.ExternalEffectList
	if err := d.Client.List(ctx, &effects); err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range effects.Items {
		effect := &effects.Items[i]
		if store.ExternalEffectState(effect.Status.State) != store.ExternalEffectInFlight || effect.Status.LeaseExpiresAt == nil ||
			now.Before(effect.Status.LeaseExpiresAt.Add(acpExternalEffectReconcileGrace)) {
			continue
		}
		_, err := d.Store.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
			ID: effect.Spec.ID, Fence: fence, ExpectedVersion: effect.Status.Version,
			ExpectedState: store.ExternalEffectInFlight, NewState: store.ExternalEffectOutcomeUnknown,
			RequestDigest: effect.Spec.RequestDigest, ExpectedLeaseOwner: effect.Status.LeaseOwner, UpdatedAt: now,
		})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("reconcile expired external effect %s/%s: %w", effect.Namespace, effect.Name, err)
		}
	}
	return nil
}
