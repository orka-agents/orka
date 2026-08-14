package controller

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

func TestRunACPExternalEffectWithRetryRetainsLeaseAcrossTransientFailure(t *testing.T) {
	controlStore, fence, closeStore := newACPSessionTestStore(
		t, filepath.Join(t.TempDir(), "external-effect-retry.db"),
	)
	defer closeStore()
	dispatcher := &ACPDispatcher{Store: controlStore}
	identity := store.ExternalEffectIdentity{
		Kind: "workspace.prepare", Namespace: "default", AggregateID: "task-1", OperationID: "workspace-prepare-prompt-1",
	}
	request := map[string]string{"source": "immutable"}
	attempts := 0

	result, err := runACPExternalEffectWithRetryDelay(
		context.Background(), dispatcher, fence, identity, request, time.Millisecond,
		func(context.Context) (string, error) {
			attempts++
			if attempts == 1 {
				return "", &publisherservice.ClientError{
					StatusCode: http.StatusBadGateway,
					Response: publisherservice.ErrorResponse{
						Code: "scm_failure", Message: "Git operation failed", Retryable: true,
					},
				}
			}
			return "prepared", nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result != "prepared" || attempts != 2 {
		t.Fatalf("retry result = %q after %d attempts, want prepared after 2", result, attempts)
	}
	effectID, err := identity.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	effect, err := controlStore.GetExternalEffect(context.Background(), effectID)
	if err != nil {
		t.Fatal(err)
	}
	if effect.State != store.ExternalEffectSucceeded || effect.Attempts != 1 {
		t.Fatalf("external effect = state %s attempts %d, want Succeeded with one lease claim", effect.State, effect.Attempts)
	}
}

func TestRunACPExternalEffectWithRetryStopsWithinLeaseBudget(t *testing.T) {
	controlStore, fence, closeStore := newACPSessionTestStore(
		t, filepath.Join(t.TempDir(), "external-effect-budget.db"),
	)
	defer closeStore()
	dispatcher := &ACPDispatcher{Store: controlStore}
	identity := store.ExternalEffectIdentity{
		Kind: "workspace.resolve", Namespace: "default", AggregateID: "task-budget", OperationID: "workspace-resolve-budget",
	}
	attempts := 0
	started := time.Now()

	_, err := runACPExternalEffectWithRetryPolicy(
		context.Background(), dispatcher, fence, identity, map[string]string{"source": "immutable"},
		time.Millisecond, 25*time.Millisecond,
		func(context.Context) (string, error) {
			attempts++
			return "", errors.New("publisher response could not be classified")
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry budget error = %v, want context deadline exceeded", err)
	}
	if attempts < 2 {
		t.Fatalf("retry attempts = %d, want multiple attempts before the budget expired", attempts)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("retry budget expired after %s, want a prompt bounded return", elapsed)
	}
	assertInFlightExternalEffectWithOneLease(t, controlStore, identity)
}

func TestRunACPExternalEffectWithRetryRejectsSuccessAfterBudgetExpiry(t *testing.T) {
	controlStore, fence, closeStore := newACPSessionTestStore(
		t, filepath.Join(t.TempDir(), "external-effect-late-success.db"),
	)
	defer closeStore()
	dispatcher := &ACPDispatcher{Store: controlStore}
	identity := store.ExternalEffectIdentity{
		Kind: "workspace.prepare", Namespace: "default", AggregateID: "task-late", OperationID: "workspace-prepare-late",
	}

	result, err := runACPExternalEffectWithRetryPolicy(
		context.Background(), dispatcher, fence, identity, map[string]string{"source": "immutable"},
		time.Millisecond, 5*time.Millisecond,
		func(context.Context) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return "late-success", nil
		},
	)
	if result != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late result = %q, error = %v; want no result and context deadline exceeded", result, err)
	}
	assertInFlightExternalEffectWithOneLease(t, controlStore, identity)
}

func TestRunACPExternalEffectWithRetryStopsOnCallerCancellation(t *testing.T) {
	controlStore, fence, closeStore := newACPSessionTestStore(
		t, filepath.Join(t.TempDir(), "external-effect-cancel.db"),
	)
	defer closeStore()
	dispatcher := &ACPDispatcher{Store: controlStore}
	identity := store.ExternalEffectIdentity{
		Kind: "workspace.resolve", Namespace: "default", AggregateID: "task-cancel", OperationID: "workspace-resolve-cancel",
	}
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	_, err := runACPExternalEffectWithRetryPolicy(
		ctx, dispatcher, fence, identity, map[string]string{"source": "immutable"},
		time.Second, time.Second,
		func(context.Context) (string, error) {
			attempts++
			cancel()
			return "", errors.New("transient publisher failure")
		},
	)
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("cancelled retry = attempts %d error %v, want one attempt and context cancellation", attempts, err)
	}
	assertInFlightExternalEffectWithOneLease(t, controlStore, identity)
}

func TestRunACPExternalEffectWithRetryDoesNotRetryNonRetryableResponse(t *testing.T) {
	controlStore, fence, closeStore := newACPSessionTestStore(
		t, filepath.Join(t.TempDir(), "external-effect-nonretryable.db"),
	)
	defer closeStore()
	dispatcher := &ACPDispatcher{Store: controlStore}
	identity := store.ExternalEffectIdentity{
		Kind: "workspace.resolve", Namespace: "default", AggregateID: "task-rejected", OperationID: "workspace-resolve-rejected",
	}
	attempts := 0

	_, err := runACPExternalEffectWithRetryPolicy(
		context.Background(), dispatcher, fence, identity, map[string]string{"source": "invalid"},
		time.Millisecond, 50*time.Millisecond,
		func(context.Context) (string, error) {
			attempts++
			return "", &publisherservice.ClientError{
				StatusCode: http.StatusBadRequest,
				Response: publisherservice.ErrorResponse{
					Code: "invalid_request", Message: "request is invalid", Retryable: false,
				},
			}
		},
	)
	if err == nil || attempts != 1 {
		t.Fatalf("non-retryable result = attempts %d error %v, want one failed attempt", attempts, err)
	}
	assertInFlightExternalEffectWithOneLease(t, controlStore, identity)
}

func TestRunExternalEffectBoundsCallToLeaseAccountableDuration(t *testing.T) {
	controlStore, fence, closeStore := newACPSessionTestStore(
		t, filepath.Join(t.TempDir(), "external-effect-call-bound.db"),
	)
	defer closeStore()
	identity := store.ExternalEffectIdentity{
		Kind: "acp-mcp-tool", Namespace: "default", AggregateID: "session-bound", OperationID: "mcp-call-bound",
	}
	start := time.Now()
	var deadline time.Time
	var hasDeadline bool

	result, err := runExternalEffect(
		context.Background(), controlStore, fence, identity, map[string]string{"call": "custom"},
		func(ctx context.Context) (string, error) {
			deadline, hasDeadline = ctx.Deadline()
			return "committed", nil
		},
	)
	if err != nil || result != "committed" {
		t.Fatalf("bounded call = %q error %v, want committed success", result, err)
	}
	if !hasDeadline {
		t.Fatal("external-effect call context has no deadline; a call must not outlive its ledger lease")
	}
	if deadline.After(start.Add(maxACPExternalEffectCallDuration + time.Second)) {
		t.Fatalf(
			"external-effect call deadline = %s after start, want at most the lease-accountable %s",
			deadline.Sub(start), maxACPExternalEffectCallDuration,
		)
	}
	if maxACPExternalEffectCallDuration >= acpExternalEffectLease {
		t.Fatalf(
			"call bound %s does not leave settlement margin inside the %s lease",
			maxACPExternalEffectCallDuration, acpExternalEffectLease,
		)
	}
}

func assertInFlightExternalEffectWithOneLease(
	t *testing.T,
	controlStore store.ExternalEffectStore,
	identity store.ExternalEffectIdentity,
) {
	t.Helper()
	effectID, err := identity.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	effect, err := controlStore.GetExternalEffect(context.Background(), effectID)
	if err != nil {
		t.Fatal(err)
	}
	if effect.State != store.ExternalEffectInFlight || effect.Attempts != 1 {
		t.Fatalf("external effect = state %s attempts %d, want InFlight with one lease claim", effect.State, effect.Attempts)
	}
}
