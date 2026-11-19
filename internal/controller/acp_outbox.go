package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskterminal"
)

const (
	stateField = "state"
)

func standaloneTaskTerminalProjectionID(task *corev1alpha1.Task, attempt int32) string {
	if task == nil {
		return ""
	}
	return standaloneTaskTerminalProjectionIDForUID(task.Namespace, task.UID, attempt)
}

func standaloneTaskTerminalProjectionIDForUID(namespace string, taskUID types.UID, attempt int32) string {
	if strings.TrimSpace(namespace) == "" || taskUID == "" {
		return ""
	}
	return store.CanonicalControlID("task-terminal-projection", namespace, string(taskUID), fmt.Sprint(attempt))
}

func (d *ACPDispatcher) enqueueStandaloneTaskProjection(ctx context.Context, task *corev1alpha1.Task, payload taskTerminalProjection) error {
	return d.enqueueTaskTerminalProjection(ctx, task, payload, false)
}

func (d *ACPDispatcher) enqueueUnboundSessionTaskProjection(ctx context.Context, task *corev1alpha1.Task, payload taskTerminalProjection) error {
	return d.enqueueTaskTerminalProjection(ctx, task, payload, true)
}

func (d *ACPDispatcher) enqueueTaskTerminalProjection(
	ctx context.Context,
	task *corev1alpha1.Task,
	payload taskTerminalProjection,
	allowSessionRef bool,
) error {
	if task == nil || (task.Spec.SessionRef != nil && !allowSessionRef) {
		return nil
	}
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	return enqueueDurableTaskTerminalProjection(ctx, d.Store, fence, task, payload)
}

func enqueueDurableTaskTerminalProjection(
	ctx context.Context,
	projectionStore store.OutboxProjectionStore,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	payload taskTerminalProjection,
) error {
	if task == nil {
		return fmt.Errorf("durable Task terminal projection identity is incomplete")
	}
	return enqueueDurableTaskTerminalProjectionForUID(ctx, projectionStore, fence, task, task.UID, payload)
}

func enqueueDurableTaskTerminalProjectionForUID(
	ctx context.Context,
	projectionStore store.OutboxProjectionStore,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	projectionTaskUID types.UID,
	payload taskTerminalProjection,
) error {
	if projectionStore == nil || task == nil || task.UID == "" || payload.Attempt < 1 {
		return fmt.Errorf("durable Task terminal projection identity is incomplete")
	}
	if projectionTaskUID == "" {
		return fmt.Errorf("durable Task terminal projection source identity is incomplete")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	projectionTime := task.CreationTimestamp.UTC()
	if payload.Execution.LastTransitionTime != nil && !payload.Execution.LastTransitionTime.IsZero() {
		projectionTime = payload.Execution.LastTransitionTime.UTC()
	} else if task.Status.CompletionTime != nil && !task.Status.CompletionTime.IsZero() {
		projectionTime = task.Status.CompletionTime.UTC()
	}
	if projectionTime.IsZero() {
		projectionTime = time.Unix(0, 0).UTC()
	}
	projection := &store.OutboxProjection{
		ID:            standaloneTaskTerminalProjectionIDForUID(task.Namespace, projectionTaskUID, payload.Attempt),
		AggregateKind: "Task", AggregateID: string(projectionTaskUID), ProjectionKind: taskTerminalProjectionKind,
		PayloadDigest: store.CanonicalBytesDigest(encoded), Payload: encoded,
		AvailableAt: projectionTime, CreatedAt: time.Now().UTC(),
	}
	if existing, getErr := projectionStore.GetOutboxProjection(ctx, projection.ID); getErr == nil {
		return validateMatchingTaskTerminalProjection(existing, projection)
	} else if !errors.Is(getErr, store.ErrNotFound) {
		return getErr
	}
	if _, err = projectionStore.EnqueueOutboxProjection(ctx, projection, fence); err == nil {
		wakeACPOutboxProjectors()
		return nil
	} else if !errors.Is(err, store.ErrConflict) {
		return err
	}
	existing, getErr := projectionStore.GetOutboxProjection(ctx, projection.ID)
	if getErr != nil {
		return errors.Join(err, getErr)
	}
	return validateMatchingTaskTerminalProjection(existing, projection)
}

func validateMatchingTaskTerminalProjection(existing *store.OutboxProjection, expected *store.OutboxProjection) error {
	if existing == nil || expected == nil || existing.ID != expected.ID ||
		existing.AggregateKind != expected.AggregateKind || existing.AggregateID != expected.AggregateID ||
		existing.ProjectionKind != expected.ProjectionKind || existing.PayloadDigest != expected.PayloadDigest ||
		!bytes.Equal(existing.Payload, expected.Payload) {
		return fmt.Errorf("%w: Task terminal projection %q was reused with different payload or metadata", store.ErrConflict, expected.ID)
	}
	return nil
}

const (
	defaultACPOutboxInterval = time.Second
	defaultACPOutboxLease    = 30 * time.Second
	defaultACPOutboxLimit    = 32
	defaultACPOutboxAttempts = 10
	// defaultACPOutboxIdleClaimInterval bounds how long an idle projector
	// goes without claiming. Each claim takes the controller-epoch mutation
	// interlock that every fenced control-store write shares, so an idle
	// projector claims only on this cadence, when a projection is enqueued,
	// or when a retry it scheduled falls due.
	defaultACPOutboxIdleClaimInterval = 10 * time.Second
)

// acpOutboxWakeups fans an enqueue out to every running projector. Every
// terminal projection is enqueued through enqueueDurableTaskTerminalProjectionForUID
// under the controller-epoch fence, so only this process's leader enqueues.
var acpOutboxWakeups = struct {
	sync.Mutex
	subscribers map[chan struct{}]struct{}
}{subscribers: map[chan struct{}]struct{}{}}

func subscribeACPOutboxWakeups() (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)
	acpOutboxWakeups.Lock()
	acpOutboxWakeups.subscribers[wake] = struct{}{}
	acpOutboxWakeups.Unlock()
	return wake, func() {
		acpOutboxWakeups.Lock()
		delete(acpOutboxWakeups.subscribers, wake)
		acpOutboxWakeups.Unlock()
	}
}

func wakeACPOutboxProjectors() {
	acpOutboxWakeups.Lock()
	defer acpOutboxWakeups.Unlock()
	for wake := range acpOutboxWakeups.subscribers {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// acpOutboxPass is what one projection pass claimed and scheduled.
type acpOutboxPass struct {
	// claimedAt is the time the pass claimed at; claimed holds the claimed
	// projection IDs.
	claimedAt time.Time
	claimed   []string
	// retries maps each projection the pass returned to Pending to the time
	// it becomes claimable again.
	retries map[string]time.Time
}

// acpOutboxSchedule remembers, across passes, when each retry the projector
// scheduled falls due, so a pass that claims nothing, such as one an enqueue
// woke early, or one where the earliest retry succeeds, never forgets a later
// deadline.
type acpOutboxSchedule struct {
	retries map[string]time.Time
}

// observe folds a pass into the schedule. A claimed projection's earlier
// deadline is settled by that claim; a deadline at or before a pass that
// claimed fewer than its limit, yet did not return it, belongs to a
// projection no longer pending. A failed pass may still have committed
// retries for projections it completed before failing: those are added, and
// nothing is settled or pruned.
func (s *acpOutboxSchedule) observe(pass acpOutboxPass, err error) {
	if s.retries == nil {
		s.retries = map[string]time.Time{}
	}
	if err != nil {
		maps.Copy(s.retries, pass.retries)
		return
	}
	for _, id := range pass.claimed {
		delete(s.retries, id)
	}
	if len(pass.claimed) < defaultACPOutboxLimit {
		for id, due := range s.retries {
			if !due.After(pass.claimedAt) {
				delete(s.retries, id)
			}
		}
	}
	maps.Copy(s.retries, pass.retries)
}

// next returns when the projector should claim after a pass at now. A failed
// pass, or one that filled its claim limit, claims again on the next tick;
// otherwise the projector waits for an enqueue wakeup, the earliest
// outstanding retry, or the idle interval, whichever comes first.
func (s *acpOutboxSchedule) next(now time.Time, pass acpOutboxPass, err error, idle time.Duration) time.Time {
	if err != nil || len(pass.claimed) >= defaultACPOutboxLimit {
		return now
	}
	next := now.Add(idle)
	for _, due := range s.retries {
		if due.Before(next) {
			next = due
		}
	}
	return next
}

type ACPOutboxProjector struct {
	Client      client.Client
	Store       store.OutboxProjectionStore
	Epochs      *ControllerEpochManager
	WorkerID    string
	Interval    time.Duration
	MaxAttempts int64
	// IdleClaimInterval is the longest an idle projector goes without a
	// claim; zero selects defaultACPOutboxIdleClaimInterval.
	IdleClaimInterval time.Duration
}

func (p *ACPOutboxProjector) NeedLeaderElection() bool { return true }

func (p *ACPOutboxProjector) Start(ctx context.Context) error {
	if p.Client == nil || p.Store == nil || p.Epochs == nil {
		return fmt.Errorf("ACP outbox projector requires Kubernetes client, store, and epoch manager")
	}
	p.WorkerID = strings.TrimSpace(p.WorkerID)
	if p.WorkerID == "" {
		p.WorkerID = "acp-outbox-projector"
	}
	if p.Interval <= 0 {
		p.Interval = defaultACPOutboxInterval
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultACPOutboxAttempts
	}
	idle := p.IdleClaimInterval
	if idle <= 0 {
		idle = defaultACPOutboxIdleClaimInterval
	}
	wake, unsubscribe := subscribeACPOutboxWakeups()
	defer unsubscribe()
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	var schedule acpOutboxSchedule
	var nextClaim time.Time // claim at once: projections may predate this process
	for {
		if now := time.Now(); !now.Before(nextClaim) {
			pass, err := p.projectPass(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				logf.FromContext(ctx).Error(err, "ACP outbox projection pass failed")
			}
			schedule.observe(pass, err)
			nextClaim = schedule.next(time.Now(), pass, err, idle)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
			nextClaim = time.Time{}
		case <-ticker.C:
		}
	}
}

func (p *ACPOutboxProjector) projectOnce(ctx context.Context) error {
	_, err := p.projectPass(ctx)
	return err
}

func (p *ACPOutboxProjector) projectPass(ctx context.Context) (acpOutboxPass, error) {
	var pass acpOutboxPass
	fence, err := p.Epochs.CurrentFence(ctx)
	if err != nil {
		return pass, err
	}
	now := time.Now().UTC()
	projections, err := p.Store.ClaimOutboxProjections(ctx, store.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: p.WorkerID, Limit: defaultACPOutboxLimit, LeaseDuration: defaultACPOutboxLease, Now: now,
	})
	if err != nil {
		return pass, err
	}
	pass.claimedAt = now
	for i := range projections {
		projection := &projections[i]
		pass.claimed = append(pass.claimed, projection.ID)
		deliveryDigest, projectErr := p.deliver(ctx, *projection)
		state := store.OutboxProjectionDelivered
		lastError := ""
		availableAt := time.Time{}
		if projectErr != nil {
			lastError = sanitizeOutboxError(projectErr)
			if isPermanentOutboxDeliveryError(projectErr) && projection.Attempts >= p.MaxAttempts {
				state = store.OutboxProjectionDeadLetter
			} else {
				// Infrastructure failures (for example a Kubernetes control-plane
				// outage) must stay retryable: recovery treats any existing
				// projection as authoritative and Task deletion requires the
				// projection to reach Delivered, so dead-lettering a transient
				// failure would leave the Task nonterminal and undeletable.
				state = store.OutboxProjectionPending
				availableAt = now.Add(outboxBackoff(projection.Attempts))
			}
			deliveryDigest = ""
		}
		digest, err := acpDomainDigest("outbox-completion", map[string]any{
			"id": projection.ID, versionField: projection.Version, stateField: state,
			"deliveryDigest": deliveryDigest, "lastError": lastError, "availableAt": availableAt,
		})
		if err != nil {
			return pass, err
		}
		_, err = p.Store.CompleteOutboxProjection(ctx, store.CompleteOutboxProjectionRequest{
			ID: projection.ID, Fence: fence, ExpectedVersion: projection.Version, LeaseOwner: p.WorkerID,
			OperationID: "complete-" + fmt.Sprint(projection.Version), OperationDigest: digest,
			NewState: state, DeliveryDigest: deliveryDigest, LastError: lastError, AvailableAt: availableAt, UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			return pass, err
		}
		if state == store.OutboxProjectionPending {
			// Recorded only once the retry is committed, so a pass that fails
			// later still reports every retry it made.
			if pass.retries == nil {
				pass.retries = map[string]time.Time{}
			}
			pass.retries[projection.ID] = availableAt
		}
	}
	return pass, nil
}

type taskTerminalProjection = taskterminal.Projection

// permanentOutboxDeliveryError marks delivery failures that can never succeed
// on retry (payload corruption or identity mismatches). Only these may be
// dead-lettered; every other failure is treated as transient infrastructure
// trouble and stays pending so the terminal projection remains deliverable.
type permanentOutboxDeliveryError struct{ err error }

func (e *permanentOutboxDeliveryError) Error() string { return e.err.Error() }
func (e *permanentOutboxDeliveryError) Unwrap() error { return e.err }

func permanentOutboxDelivery(err error) error {
	if err == nil {
		return nil
	}
	return &permanentOutboxDeliveryError{err: err}
}

func isPermanentOutboxDeliveryError(err error) bool {
	var permanent *permanentOutboxDeliveryError
	return errors.As(err, &permanent)
}

func mergeTerminalExecutionStatus(existing *corev1alpha1.TaskExecutionStatus, projected corev1alpha1.TaskExecutionStatus) corev1alpha1.TaskExecutionStatus {
	if existing == nil {
		return projected
	}
	merged := *existing
	merged.State = projected.State
	merged.Outcome = projected.Outcome
	if projected.Attempt > 0 {
		merged.Attempt = projected.Attempt
	}
	if projected.PromptID != "" {
		merged.PromptID = projected.PromptID
	}
	if projected.Reason != "" {
		merged.Reason = projected.Reason
	}
	if projected.Message != "" {
		merged.Message = projected.Message
	}
	return merged
}

//nolint:gocyclo // Projection delivery handles the finite set of durable sink and stale-record outcomes.
func (p *ACPOutboxProjector) deliver(ctx context.Context, projection store.OutboxProjection) (string, error) {
	if projection.ProjectionKind != taskTerminalProjectionKind {
		return "", permanentOutboxDelivery(fmt.Errorf("unsupported projection kind %q", projection.ProjectionKind))
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		return "", permanentOutboxDelivery(fmt.Errorf("decode task terminal projection: %w", err))
	}
	if payload.Namespace == "" || payload.Task == "" || payload.TaskUID == "" || payload.Attempt < 1 {
		return "", permanentOutboxDelivery(fmt.Errorf("task terminal projection identity is incomplete"))
	}
	key := types.NamespacedName{Namespace: payload.Namespace, Name: payload.Task}
	var deliveredResourceVersion string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		task := &corev1alpha1.Task{}
		if err := p.Client.Get(ctx, key, task); err != nil {
			if apierrors.IsNotFound(err) {
				deliveredResourceVersion = "deleted"
				return nil
			}
			return err
		}
		if string(task.UID) != payload.TaskUID {
			binding := executionBinding(task)
			if binding == nil || string(binding.Task.UID) != payload.TaskUID || !acpTaskUsesRestoredSourceIdentity(task) ||
				task.Status.Execution == nil || task.Status.Execution.Attempt != payload.Attempt {
				// Permanent so MaxAttempts applies: the bounded retries cover an
				// in-progress restore settlement, while a Task name recreated
				// with a different UID can never deliver and must not be
				// claimed and retried forever.
				return permanentOutboxDelivery(fmt.Errorf("task UID does not match outbox projection"))
			}
			// Clean-cluster restore settlement already patched the new Task
			// incarnation directly. This projection remains immutable source
			// history, so delivery is an exact no-op against the restored UID.
			deliveredResourceVersion = task.ResourceVersion
			return nil
		}
		if task.Status.Execution != nil && task.Status.Execution.Attempt > payload.Attempt {
			return fmt.Errorf("task has advanced beyond projected attempt")
		}
		base := task.DeepCopy()
		now := metav1.Now()
		task.Status.Phase = payload.Phase
		task.Status.Message = payload.Message
		if task.Status.CompletionTime == nil {
			task.Status.CompletionTime = &now
		}
		execution := mergeTerminalExecutionStatus(task.Status.Execution, payload.Execution)
		execution.LastTransitionTime = &now
		task.Status.Execution = &execution
		if payload.Delivery != nil {
			delivery := taskDeliveryStatusForKubernetes(task, *payload.Delivery)
			delivery.LastTransitionTime = &now
			task.Status.Delivery = &delivery
		}
		// A terminal Task no longer waits on any approval it was parked on.
		if waiting := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeWaitingForApproval); waiting != nil &&
			waiting.Status == metav1.ConditionTrue && payload.Phase.IsTerminal() {
			meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
				Type: ConditionTypeWaitingForApproval, Status: metav1.ConditionFalse, LastTransitionTime: now,
				Reason: "Task" + string(payload.Phase), Message: taskTerminalMessage,
			})
		}
		if err := p.Client.Status().Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		deliveredResourceVersion = task.ResourceVersion
		return nil
	})
	if err != nil {
		return "", err
	}
	return acpDomainDigest("outbox-delivery", map[string]any{
		"projectionID": projection.ID, "payloadDigest": projection.PayloadDigest, "resourceVersion": deliveredResourceVersion,
	})
}

func outboxBackoff(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func sanitizeOutboxError(err error) string {
	if err == nil {
		return ""
	}
	// The outbox store rejects invalid UTF-8, so repair invalid sequences and
	// truncate on a rune boundary or the projection could never settle.
	message := strings.ToValidUTF8(strings.TrimSpace(err.Error()), "�")
	return truncateUTF8(message, 1024)
}
