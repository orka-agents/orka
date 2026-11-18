package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	v2eventjournal "github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
)

// errInjectedDeliveryTransition fails one delivery transition, as a lost
// response or a store that is briefly unavailable would.
var errInjectedDeliveryTransition = errors.New("injected delivery transition failure")

// failingDeliveryStore fails the next failures delivery transitions.
type failingDeliveryStore struct {
	store.DurableControlStore
	failures int
}

func (s *failingDeliveryStore) TransitionPromptAttemptDelivery(
	ctx context.Context, transition store.PromptAttemptDeliveryTransition,
) (*store.PromptAttempt, error) {
	if s.failures > 0 {
		s.failures--
		return nil, errInjectedDeliveryTransition
	}
	return s.DurableControlStore.TransitionPromptAttemptDelivery(ctx, transition)
}

// acknowledgedValidationHold holds the fixture's Task for a receipt lost after
// workspace validation started, with its prompt accepted in the journal and
// no terminal event, and acknowledges the hold. It returns the Task.
func acknowledgedValidationHold(t *testing.T, fixture *recoveryFixture) *corev1alpha1.Task {
	t.Helper()
	task, _, _ := settlingTaskWithReceipt(t, fixture)
	if err := fixture.dispatcher.transitionDelivery(
		fixture.ctx, fixture.attemptID, fixture.fence, store.PromptDeliveryNotRequested, store.PromptDeliveryValidating, "validate-workspace", "",
	); err != nil {
		t.Fatal(err)
	}
	loseResultReceipt(t, fixture, task)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("recovery error = %v, want a storage recovery hold", err)
	}
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord, key: fixture.attemptID,
	}
	acknowledgeTaskStorageHold(t, fixture, task, hold.id())
	return task
}

// requireValidationSettledAsConflict checks that the attempt's outcome is
// unknown, its delivery a conflict, and that the Task records both.
func requireValidationSettledAsConflict(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task) {
	t.Helper()
	attempt := requireAttemptState(t, fixture, store.PromptExecutionOutcomeUnknown)
	if attempt.DeliveryState != store.PromptDeliveryConflict {
		t.Fatalf("attempt delivery = %s, want %s", attempt.DeliveryState, store.PromptDeliveryConflict)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution == nil || updated.Status.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown ||
		updated.Status.Delivery == nil || updated.Status.Delivery.State != corev1alpha1.TaskDeliveryStateDeliveryConflict {
		t.Fatalf("Task status = execution %#v delivery %#v", updated.Status.Execution, updated.Status.Delivery)
	}
}

// journaledPromptTerminal returns the terminal event the Task's prompt journal
// records, or nil while the journal is open.
func journaledPromptTerminal(
	t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task,
) *v2eventjournal.PromptTerminalEvidence {
	t.Helper()
	latest := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), latest); err != nil {
		t.Fatal(err)
	}
	identity, ok := mappedPromptRecoveryIdentity(latest)
	if !ok {
		t.Fatal("the Task has no prompt recovery identity")
	}
	evidence, err := (v2eventjournal.Journal{
		EventStore: fixture.controlStore, MapContext: mappedPromptRecoveryContext(latest), RecoveryIdentity: identity,
	}).FindPromptTerminal(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

// A pass that fails before the validation is settled leaves the journal open,
// so the next pass settles the validation before recording the outcome.
func TestACPDispatcherSettlesWorkspaceValidationBeforeClosingThePromptJournal(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := acknowledgedValidationHold(t, fixture)
	controls := fixture.dispatcher.Store
	fixture.dispatcher.Store = &failingDeliveryStore{DurableControlStore: controls, failures: 1}

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); !errors.Is(err, errInjectedDeliveryTransition) {
		t.Fatalf("recovery error = %v, want the injected failure", err)
	}
	if evidence := journaledPromptTerminal(t, fixture, task); evidence != nil {
		t.Fatalf("the failed pass journaled a %s terminal before settling the validation", evidence.TerminalEvent)
	}

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after the failed pass: %v", err)
	}
	requireValidationSettledAsConflict(t, fixture, task)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after settlement: %v", err)
	}
}

// A terminal event an earlier recovery pass journaled before it settled the
// validation does not leave the delivery Validating once the outcome is
// recorded.
func TestACPDispatcherSettlesWorkspaceValidationAfterAJournaledRecoveryTerminal(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := acknowledgedValidationHold(t, fixture)
	latest := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), latest); err != nil {
		t.Fatal(err)
	}
	if err := fixture.dispatcher.closeRecoveredPromptJournal(
		fixture.ctx, latest, time.Now().UTC(), "an earlier recovery pass closed the journal",
	); err != nil {
		t.Fatal(err)
	}

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after the journaled terminal: %v", err)
	}
	requireValidationSettledAsConflict(t, fixture, task)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after settlement: %v", err)
	}
}
