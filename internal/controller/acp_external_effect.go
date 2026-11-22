package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

const externalEffectLeaseSettlementMargin = time.Minute

const (
	identityKey = "identity"
	requestKey  = "request"
)

const maxACPExternalEffectCallDuration = 4 * time.Minute

const envACPPublisherEffectTimeout = "ORKA_PUBLISHER_PUBLISH_TIMEOUT"

func externalEffectCallTimeout(identity store.ExternalEffectIdentity) time.Duration {
	if !publisherBackedExternalEffectKind(identity.Kind) {
		return maxACPExternalEffectCallDuration
	}
	raw := strings.TrimSpace(os.Getenv(envACPPublisherEffectTimeout))
	if raw == "" {
		return maxACPExternalEffectCallDuration
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return maxACPExternalEffectCallDuration
	}
	return parsed
}

func publisherBackedExternalEffectKind(kind string) bool {
	return strings.HasPrefix(kind, "publisher.") || strings.HasPrefix(kind, "workspace.")
}

var externalEffectLeaseSequence atomic.Uint64

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
	return runExternalEffectWithReplayCallTimeout(
		ctx, effects, fence, identity, request, externalEffectCallTimeout(identity), call,
	)
}

var errExternalEffectAlreadyStarted = errors.New("external effect was already started")

func runExternalEffectOnceWithReplay[T any](
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	call func(context.Context) (T, error),
) (T, bool, error) {
	return runExternalEffectWithReplayMode(
		ctx, effects, fence, identity, request, externalEffectCallTimeout(identity), false, call,
	)
}

func runExternalEffectWithReplayCallTimeout[T any](
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	callTimeout time.Duration,
	call func(context.Context) (T, error),
) (T, bool, error) {
	return runExternalEffectWithReplayMode(ctx, effects, fence, identity, request, callTimeout, true, call)
}

func runExternalEffectWithReplayMode[T any](
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	callTimeout time.Duration,
	reclaim bool,
	call func(context.Context) (T, error),
) (T, bool, error) {
	var zero T
	if effects == nil {
		return zero, false, fmt.Errorf("external-effect store is required")
	}
	if callTimeout <= 0 {
		return zero, false, fmt.Errorf("external-effect call timeout must be positive")
	}
	requestDigest, err := acpDomainDigest("external-effect-request", map[string]any{
		identityKey: identity, requestKey: request,
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
	if !reclaim && effect.State == store.ExternalEffectFailed && len(effect.Response) > 0 {
		var response T
		if json.Unmarshal(effect.Response, &response) == nil {
			return response, true, nil
		}
	}
	if effect.State == store.ExternalEffectFailed || effect.State == store.ExternalEffectOutcomeUnknown {
		return zero, false, fmt.Errorf("external effect %s is terminal in state %s", effect.ID, effect.State)
	}
	if !reclaim && effect.State == store.ExternalEffectInFlight {
		return zero, false, errExternalEffectAlreadyStarted
	}
	leaseExpiry := now.Add(callTimeout + externalEffectLeaseSettlementMargin)
	leaseOwner := externalEffectLeaseOwner(fence, identity, now)
	claimed, err := effects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: effect.State,
		NewState: store.ExternalEffectInFlight, RequestDigest: requestDigest,
		ExpectedLeaseOwner: effect.LeaseOwner, LeaseOwner: leaseOwner, LeaseExpiresAt: &leaseExpiry, UpdatedAt: now,
	})
	if err != nil {
		return zero, false, err
	}
	callCtx, cancelCall := context.WithTimeout(ctx, callTimeout)
	response, callErr := call(callCtx)
	callCtxErr := callCtx.Err()
	cancelCall()
	if callErr != nil {
		return zero, false, callErr
	}
	if callCtxErr != nil {
		return zero, false, callCtxErr
	}
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return zero, false, err
	}
	responseDigest := store.CanonicalBytesDigest(encoded)
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

const defaultACPExternalEffectRetryDelay = 5 * time.Second

func runACPExternalEffectWithRetry[T any](
	ctx context.Context,
	d *ACPDispatcher,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	call func(context.Context) (T, error),
) (T, error) {
	return runACPExternalEffectWithRetryPolicy(
		ctx, d, fence, identity, request,
		defaultACPExternalEffectRetryDelay, externalEffectCallTimeout(identity), call,
	)
}

func runACPExternalEffectWithRetryPolicy[T any](
	ctx context.Context,
	d *ACPDispatcher,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	retryDelay time.Duration,
	retryBudget time.Duration,
	call func(context.Context) (T, error),
) (T, error) {
	var zero T
	if retryDelay <= 0 {
		return zero, fmt.Errorf("external-effect retry delay must be positive")
	}
	if retryBudget <= 0 || retryBudget > externalEffectCallTimeout(identity) {
		return zero, fmt.Errorf("external-effect retry budget must be positive and leave the required lease settlement margin")
	}
	return runACPExternalEffect(ctx, d, fence, identity, request, func(callCtx context.Context) (T, error) {
		retryCtx, cancel := context.WithTimeout(callCtx, retryBudget)
		defer cancel()
		for {
			value, err := call(retryCtx)
			if retryErr := retryCtx.Err(); retryErr != nil {
				return zero, fmt.Errorf("bounded external-effect reconciliation expired: %w", retryErr)
			}
			if err == nil {
				return value, nil
			}
			if !retryableACPExternalEffectError(err) {
				return zero, err
			}
			timer := time.NewTimer(retryDelay)
			select {
			case <-retryCtx.Done():
				timer.Stop()
				return zero, fmt.Errorf("bounded external-effect reconciliation expired: %w", retryCtx.Err())
			case <-timer.C:
			}
		}
	})
}

func retryableACPExternalEffectError(err error) bool {
	if err == nil {
		return false
	}
	if clientErr, ok := errors.AsType[*publisherservice.ClientError](err); ok {
		return clientErr.Response.Retryable || clientErr.StatusCode == http.StatusTooManyRequests || clientErr.StatusCode >= 500
	}
	if errors.Is(err, store.ErrConflict) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
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
		responseDigest = store.CanonicalBytesDigest(value)
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

func (d *ACPDispatcher) reconcileExpiredExternalEffects(ctx context.Context, tasks []corev1alpha1.Task) error {
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
	approvalEffects := make(map[string]acpMCPApprovalEffect)
	for i := range effects.Items {
		effect := &effects.Items[i]
		if effect.Spec.Kind == acpMCPToolEffectKind && mcpApprovalEffectTaskUID(effect) != "" {
			candidate := mcpApprovalEffectSnapshot(effect)
			if mcpApprovalEffectNeedsRecovery(&candidate.ExternalEffect, fence, now) {
				approvalEffects[effect.Spec.ID] = candidate
			}
		}
		if store.ExternalEffectState(effect.Status.State) != store.ExternalEffectInFlight || effect.Status.LeaseExpiresAt == nil ||
			now.Before(effect.Status.LeaseExpiresAt.Add(acpExternalEffectReconcileGrace)) {
			continue
		}
		settled, err := d.Store.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
			ID: effect.Spec.ID, Fence: fence, ExpectedVersion: effect.Status.Version,
			ExpectedState: store.ExternalEffectInFlight, NewState: store.ExternalEffectOutcomeUnknown,
			RequestDigest: effect.Spec.RequestDigest, ExpectedLeaseOwner: effect.Status.LeaseOwner, UpdatedAt: now,
		})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("reconcile expired external effect %s/%s: %w", effect.Namespace, effect.Name, err)
		}
		if err == nil && effect.Spec.Kind == acpMCPToolEffectKind && mcpApprovalEffectTaskUID(effect) != "" {
			candidate := mcpApprovalEffectSnapshot(effect)
			candidate.ExternalEffect = *settled
			approvalEffects[effect.Spec.ID] = candidate
		}
	}
	return d.reconcileMCPApprovalExecutions(ctx, fence, tasks, approvalEffects)
}

func RunExternalEffectWithReplayCallTimeout[T any](
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	request any,
	callTimeout time.Duration,
	call func(context.Context) (T, error),
) (T, bool, error) {
	return runExternalEffectWithReplayCallTimeout(ctx, effects, fence, identity, request, callTimeout, call)
}

func SettleExternalEffect(
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	state store.ExternalEffectState,
) error {
	return settleExternalEffectStore(ctx, effects, fence, identity, state, nil)
}

var ErrExternalEffectNotPending = errors.New("external effect is no longer pending")

func SettlePendingExternalEffect(
	ctx context.Context,
	effects store.ExternalEffectStore,
	fence store.ControllerEpochFence,
	identity store.ExternalEffectIdentity,
	state store.ExternalEffectState,
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
	if effect.State != store.ExternalEffectPending {
		return fmt.Errorf("%w: external effect %s is %s", ErrExternalEffectNotPending, effect.ID, effect.State)
	}
	if _, err := effects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: store.ExternalEffectPending,
		NewState: state, RequestDigest: effect.RequestDigest, ExpectedLeaseOwner: effect.LeaseOwner, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("%w: %w", ErrExternalEffectNotPending, err)
		}
		return err
	}
	return nil
}

func ExternalEffectRequestDigest(identity store.ExternalEffectIdentity, request any) (string, error) {
	return acpDomainDigest("external-effect-request", map[string]any{
		identityKey: identity, requestKey: request,
	})
}
