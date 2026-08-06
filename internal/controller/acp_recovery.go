package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

const (
	taskResourceKind                     = "Task"
	acpControllerRestartRecoveredReason  = "ControllerRestartRecovered"
	acpControllerRestartRecoveredMessage = "pre-submission attempt recovered under the new controller epoch"
)

// recoverStaleAttempts classifies every old-epoch ACP attempt before the new
// leader admits work. It resumes only states that provably crossed no prompt
// request-write boundary and makes all potentially accepted prompts terminally
// OutcomeUnknown without allocating a replacement attempt.
func (d *ACPDispatcher) recoverStaleAttempts(ctx context.Context) error {
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	var tasks corev1alpha1.TaskList
	if err := d.Client.List(ctx, &tasks); err != nil {
		return err
	}
	for i := range tasks.Items {
		candidate := tasks.Items[i].DeepCopy()
		task, recoverable, readErr := d.readRecoverableTask(ctx, candidate)
		if readErr != nil {
			return fmt.Errorf("refresh stale ACP task %s/%s: %w", candidate.Namespace, candidate.Name, readErr)
		}
		if !recoverable || !taskManagedByACP(task) || task.Status.Execution == nil || task.Status.Execution.ControllerEpoch >= fence.Epoch {
			continue
		}
		if err := d.recoverStaleTask(ctx, task, fence); err != nil {
			if errors.Is(err, store.ErrNotFound) || apierrors.IsNotFound(err) {
				_, stillRecoverable, recheckErr := d.readRecoverableTask(ctx, task)
				if recheckErr != nil {
					return fmt.Errorf("recheck stale ACP task %s/%s after not found: %w", task.Namespace, task.Name, recheckErr)
				}
				if !stillRecoverable {
					continue
				}
			}
			return fmt.Errorf("recover stale ACP task %s/%s: %w", task.Namespace, task.Name, err)
		}
	}
	return nil
}

func (d *ACPDispatcher) readRecoverableTask(
	ctx context.Context,
	candidate *corev1alpha1.Task,
) (*corev1alpha1.Task, bool, error) {
	if candidate == nil {
		return nil, false, nil
	}
	reader := d.APIReader
	if reader == nil {
		reader = d.Client
	}
	if reader == nil {
		return nil, false, fmt.Errorf("ACP recovery requires a Kubernetes reader")
	}
	latest := &corev1alpha1.Task{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: candidate.Namespace, Name: candidate.Name}, latest)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if latest.UID != candidate.UID || !latest.DeletionTimestamp.IsZero() {
		return nil, false, nil
	}
	return latest, true, nil
}

func (d *ACPDispatcher) recoverStaleTask(ctx context.Context, task *corev1alpha1.Task, fence store.ControllerEpochFence) error {
	attemptID, err := promptAttemptIDFromTask(task)
	if err != nil {
		return err
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	sessionBound, err := promptAttemptSessionBound(attempt)
	if err != nil {
		return err
	}
	continuitySession := task.Spec.SessionRef != nil && sessionBound
	if store.IsTerminalPromptExecutionState(attempt.ExecutionState) && store.IsTerminalPromptDeliveryState(attempt.DeliveryState) {
		if continuitySession {
			if err := d.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
				return err
			}
		}
		cleanupComplete, cleanupErr := d.cleanupRecoveredTaskScopedRuntimeSession(ctx, task)
		if cleanupErr != nil {
			return cleanupErr
		}
		if !cleanupComplete {
			return nil
		}
		exists, projectionErr := d.validateExistingStandaloneTaskProjection(ctx, task, attempt)
		if projectionErr != nil {
			return projectionErr
		}
		if exists {
			return d.patchRecoveredTerminalEpoch(ctx, task, fence.Epoch)
		}
	}
	switch attempt.ExecutionState {
	case store.PromptExecutionQueued, store.PromptExecutionReserved:
		return d.patchRecoveredTaskReserved(ctx, task, fence.Epoch, attempt.ExecutionState == store.PromptExecutionQueued)
	case store.PromptExecutionSessionStarting, store.PromptExecutionPlanned:
		digest, err := acpDomainDigest("pre-submission-recovery", map[string]any{
			"attemptID": attempt.ID, "state": attempt.ExecutionState, "version": attempt.Version, "epoch": fence.Epoch,
		})
		if err != nil {
			return err
		}
		if _, err := d.Store.RecoverPromptAttemptPreSubmission(ctx, store.PromptAttemptPreSubmissionRecovery{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			OperationID: "recover-pre-submit-" + strconv.FormatInt(fence.Epoch, 10), OperationDigest: digest, RecoveredAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		return d.patchRecoveredTaskReserved(ctx, task, fence.Epoch, false)
	case store.PromptExecutionSubmitting, store.PromptExecutionSubmittedUnknown,
		store.PromptExecutionAccepted, store.PromptExecutionRunning, store.PromptExecutionSettling:
		const reason = "RuntimeLost"
		const message = "controller leadership changed after the prompt request-write boundary; outcome is unknown and was not replayed"
		if err := d.persistOutcomeUnknown(ctx, attempt.ID, fence, reason, message); err != nil {
			return err
		}
		if err := d.finalizeRecoveredSessionUnknown(ctx, task, fence, attempt.ID, reason); err != nil {
			return err
		}
		return d.failTask(ctx, task, corev1alpha1.TaskExecutionStateOutcomeUnknown, corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, reason, message)
	case store.PromptExecutionSucceeded:
		if err := d.recoverSucceededTaskProjection(ctx, task, attempt, fence); err != nil {
			return err
		}
		return d.patchRecoveredTerminalEpoch(ctx, task, fence.Epoch)
	case store.PromptExecutionFailed, store.PromptExecutionCancelled, store.PromptExecutionOutcomeUnknown:
		if !continuitySession {
			return d.recoverMissingStandaloneTerminalProjection(ctx, task, attempt)
		}
		if err := d.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
			return err
		}
		return d.patchRecoveredTerminalExecution(ctx, task, attempt, fence.Epoch)
	default:
		return fmt.Errorf("unsupported stale prompt attempt state %s", attempt.ExecutionState)
	}
}

func taskScopedRuntimeSessionCleanupDigest(
	taskUID types.UID,
	attempt int32,
	runtimeInstanceID string,
	runtimeSessionUID string,
	runtimeSessionGeneration int64,
) (string, error) {
	if taskUID == "" || attempt < 1 || strings.TrimSpace(runtimeInstanceID) == "" ||
		strings.TrimSpace(runtimeSessionUID) == "" || runtimeSessionGeneration < 1 {
		return "", fmt.Errorf("%w: task-scoped RuntimeSession cleanup identity is incomplete", store.ErrConflict)
	}
	return acpDomainDigest("task-runtime-session-cleanup", map[string]any{
		"taskUID": string(taskUID), "attempt": attempt, "runtimeInstanceID": runtimeInstanceID,
		"runtimeSessionUID": runtimeSessionUID, "runtimeSessionGeneration": runtimeSessionGeneration,
	})
}

func taskScopedRuntimeSessionCleanupComplete(task *corev1alpha1.Task) bool {
	if task == nil || task.Status.Execution == nil || strings.TrimSpace(task.Status.Execution.RuntimeSessionUID) == "" {
		return true
	}
	if task.Spec.SessionRef != nil && (task.Spec.Workspace == nil || task.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentWrite) {
		return true
	}
	digest, err := taskScopedRuntimeSessionCleanupDigest(
		task.UID, task.Status.Execution.Attempt, task.Status.Execution.RuntimeInstanceID,
		task.Status.Execution.RuntimeSessionUID, task.Status.Execution.RuntimeSessionGeneration,
	)
	return err == nil && task.Status.Execution.RuntimeSessionCleanupDigest == digest
}

func (d *ACPDispatcher) markTaskScopedRuntimeSessionCleanupComplete(
	ctx context.Context,
	task *corev1alpha1.Task,
	runtimeInstanceID string,
	runtimeSessionUID string,
	runtimeSessionGeneration int64,
) error {
	if task == nil || task.Status.Execution == nil {
		return nil
	}
	if task.Spec.SessionRef != nil && (task.Spec.Workspace == nil || task.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentWrite) {
		return nil
	}
	digest, err := taskScopedRuntimeSessionCleanupDigest(
		task.UID, task.Status.Execution.Attempt, runtimeInstanceID, runtimeSessionUID, runtimeSessionGeneration,
	)
	if err != nil {
		return err
	}
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, key, latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.Status.Execution == nil {
			return fmt.Errorf("%w: Task execution status is missing during RuntimeSession cleanup receipt", store.ErrConflict)
		}
		if latest.Status.Execution.RuntimeSessionCleanupDigest == digest {
			return nil
		}
		latest.Status.Execution.RuntimeSessionCleanupDigest = digest
		return d.Client.Status().Update(ctx, latest)
	})
}

func (d *ACPDispatcher) prepareRecoveredTaskScopedRuntimeSessionForSettlement(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	return d.reconcileRecoveredTaskScopedRuntimeSession(ctx, task, false)
}

func (d *ACPDispatcher) cleanupRecoveredTaskScopedRuntimeSession(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	return d.reconcileRecoveredTaskScopedRuntimeSession(ctx, task, true)
}

//nolint:gocyclo // Recovery keeps exact runtime-state and fence cleanup decisions in one fail-closed boundary.
func (d *ACPDispatcher) reconcileRecoveredTaskScopedRuntimeSession(
	ctx context.Context,
	task *corev1alpha1.Task,
	deleteAfterSettlement bool,
) (bool, error) {
	if task == nil || task.Status.Execution == nil || strings.TrimSpace(task.Status.Execution.RuntimeSessionUID) == "" ||
		task.Status.Execution.RuntimeSessionGeneration < 1 {
		return true, nil
	}
	if task.Spec.SessionRef != nil && (task.Spec.Workspace == nil || task.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentWrite) {
		return true, nil
	}
	if taskScopedRuntimeSessionCleanupComplete(task) {
		return true, nil
	}
	execution := task.Status.Execution
	var target acpDispatchTarget
	if poolName := strings.TrimSpace(execution.RuntimePoolName); poolName != "" {
		pool := &corev1alpha1.RuntimePool{}
		if err := d.APIReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: poolName}, pool); err != nil {
			if apierrors.IsNotFound(err) {
				if !deleteAfterSettlement {
					return true, nil
				}
				if markErr := d.markTaskScopedRuntimeSessionCleanupComplete(ctx, task, execution.RuntimeInstanceID, execution.RuntimeSessionUID, execution.RuntimeSessionGeneration); markErr != nil {
					return false, markErr
				}
				return true, nil
			}
			return false, err
		}
		active := pool.Status.ActiveInstance
		currentFence, err := d.Epochs.CurrentFence(ctx)
		if err != nil {
			return false, err
		}
		if active == nil || active.RuntimeInstanceID != execution.RuntimeInstanceID {
			if !deleteAfterSettlement {
				return true, nil
			}
			if markErr := d.markTaskScopedRuntimeSessionCleanupComplete(ctx, task, execution.RuntimeInstanceID, execution.RuntimeSessionUID, execution.RuntimeSessionGeneration); markErr != nil {
				return false, markErr
			}
			return true, nil
		}
		if active.ControllerEpoch != currentFence.Epoch {
			return false, nil
		}
		target.pool = pool
	} else if runtimeName := strings.TrimSpace(execution.AgentRuntimeName); runtimeName != "" {
		runtime := &corev1alpha1.AgentRuntime{}
		if err := d.APIReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: runtimeName}, runtime); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if string(runtime.UID) != execution.AgentRuntimeUID || runtime.Status.ObservedCapabilities.RuntimeInstanceID != execution.RuntimeInstanceID {
			if !deleteAfterSettlement {
				return true, nil
			}
			if markErr := d.markTaskScopedRuntimeSessionCleanupComplete(ctx, task, execution.RuntimeInstanceID, execution.RuntimeSessionUID, execution.RuntimeSessionGeneration); markErr != nil {
				return false, markErr
			}
			return true, nil
		}
		currentFence, err := d.Epochs.CurrentFence(ctx)
		if err != nil {
			return false, err
		}
		if runtime.Status.ObservedCapabilities.ControllerEpoch != currentFence.Epoch {
			return false, nil
		}
		target.external = runtime
	} else {
		return true, nil
	}
	runtimeClient, runtimeFence, _, _, err := d.runtimeClient(ctx, target)
	if err != nil {
		return false, err
	}
	runtimeFence.RuntimeSessionUID = harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID)
	runtimeFence.RuntimeSessionGeneration = uint64(execution.RuntimeSessionGeneration)
	status, statusErr := runtimeClient.Status(ctx)
	if statusErr != nil {
		return false, statusErr
	}
	observed, present := runtimeSessionStatusForFence(status.Sessions, runtimeFence)
	if !present {
		if !deleteAfterSettlement {
			return true, nil
		}
		if markErr := d.markTaskScopedRuntimeSessionCleanupComplete(
			ctx, task, execution.RuntimeInstanceID, execution.RuntimeSessionUID, execution.RuntimeSessionGeneration,
		); markErr != nil {
			return false, markErr
		}
		return true, nil
	}
	switch observed.State {
	case harnessv2.RuntimeSessionStatePublicationPrepared:
		if task.Spec.Workspace == nil || task.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentWrite {
			return false, fmt.Errorf("recover RuntimeSession publication finalization: prepared session is not bound to a write workspace")
		}
		deltaID := harnessv2.WorkspaceDeltaID("delta-" + execution.PromptID)
		finalization, finalizationErr := d.runtimeSessionPublicationFinalization(ctx, publicationIDForTask(task), deltaID)
		if errors.Is(finalizationErr, store.ErrNotFound) && task.Status.Delivery != nil &&
			task.Status.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeNoChange {
			finalization, finalizationErr = runtimeSessionDeltaAbandonmentFinalization(task, deltaID, *task.Status.Delivery)
		}
		if finalizationErr != nil {
			return false, fmt.Errorf("recover task-scoped RuntimeSession publication finalization: %w", finalizationErr)
		}
		if err := d.finalizeRuntimeSessionPublication(
			context.WithoutCancel(ctx), runtimeClient, harnessv2.RuntimeSessionID(runtimeSessionID(runtimeFence)), task, runtimeFence, finalization,
		); err != nil {
			return false, fmt.Errorf("recover task-scoped RuntimeSession publication finalization: %w", err)
		}
	case harnessv2.RuntimeSessionStateFinalizing, harnessv2.RuntimeSessionStateIdle, harnessv2.RuntimeSessionStatePoisoned:
		// Ready for exact deletion.
	case harnessv2.RuntimeSessionStateDeleting:
		return false, nil
	default:
		return false, nil
	}
	if !deleteAfterSettlement {
		return true, nil
	}
	if err := d.deleteRuntimeSession(
		context.WithoutCancel(ctx), runtimeClient, harnessv2.RuntimeSessionID(runtimeSessionID(runtimeFence)), task, runtimeFence, "terminal_recovery",
	); err != nil {
		return false, fmt.Errorf("recover task-scoped RuntimeSession cleanup: %w", err)
	}
	if err := d.markTaskScopedRuntimeSessionCleanupComplete(
		ctx, task, execution.RuntimeInstanceID, execution.RuntimeSessionUID, execution.RuntimeSessionGeneration,
	); err != nil {
		return false, err
	}
	return true, nil
}

func runtimeSessionStatusForFence(sessions []harnessv2.RuntimeSessionStatus, fence harnessv2.Fence) (harnessv2.RuntimeSessionStatus, bool) {
	for i := range sessions {
		if sessions[i].RuntimeSessionUID == fence.RuntimeSessionUID && sessions[i].Generation == fence.RuntimeSessionGeneration {
			return sessions[i], true
		}
	}
	return harnessv2.RuntimeSessionStatus{}, false
}

func (d *ACPDispatcher) recoverMissingStandaloneTerminalProjection(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
) error {
	if task == nil || attempt == nil || task.Status.Execution == nil {
		return fmt.Errorf("%w: standalone terminal recovery identity is incomplete", store.ErrConflict)
	}
	state := corev1alpha1.TaskExecutionState(attempt.ExecutionState)
	outcome := corev1alpha1.TaskExecutionOutcome(attempt.ExecutionState)
	reason := task.Status.Execution.Reason
	message := task.Status.Execution.Message
	switch attempt.ExecutionState {
	case store.PromptExecutionFailed:
		if reason == "" {
			reason = "PromptFailed"
		}
		if message == "" {
			message = "prompt failed"
		}
	case store.PromptExecutionCancelled:
		if reason == "" {
			reason = "Cancelled"
		}
		if message == "" {
			message = "prompt cancelled"
		}
	case store.PromptExecutionOutcomeUnknown:
		if reason == "" {
			reason = "RuntimeLost"
		}
		if message == "" {
			message = "prompt outcome is unknown"
		}
	default:
		return fmt.Errorf("unsupported standalone terminal recovery state %s", attempt.ExecutionState)
	}
	recoveryTask := task.DeepCopy()
	if recoveryTask.Status.Delivery == nil || store.PromptDeliveryState(recoveryTask.Status.Delivery.State) != attempt.DeliveryState {
		recoveryTask.Status.Delivery = deliveryStatusFromPromptState(attempt.DeliveryState)
	}
	bound, err := promptAttemptSessionBound(attempt)
	if err != nil {
		return err
	}
	if task.Spec.SessionRef != nil && !bound {
		return d.failTaskBeforeSessionBinding(ctx, recoveryTask, state, outcome, reason, message)
	}
	return d.failTask(ctx, recoveryTask, state, outcome, reason, message)
}

func (d *ACPDispatcher) validateExistingStandaloneTaskProjection(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
) (bool, error) {
	if task == nil || attempt == nil {
		return false, nil
	}
	bound, err := promptAttemptSessionBound(attempt)
	if err != nil {
		return false, err
	}
	if task.Spec.SessionRef != nil && bound {
		return false, nil
	}
	projectionID := standaloneTaskTerminalProjectionID(task, int32(attempt.Key.Attempt))
	projection, err := d.Store.GetOutboxProjection(ctx, projectionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if projection.AggregateKind != taskResourceKind || projection.AggregateID != string(task.UID) || projection.ProjectionKind != "TaskTerminalStatus" {
		return false, fmt.Errorf("%w: standalone terminal projection %q has mismatched identity", store.ErrConflict, projectionID)
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		return false, fmt.Errorf("decode standalone terminal projection %q: %w", projectionID, err)
	}
	if payload.Namespace != task.Namespace || payload.Task != task.Name || payload.TaskUID != string(task.UID) || int64(payload.Attempt) != attempt.Key.Attempt {
		return false, fmt.Errorf("%w: standalone terminal projection %q payload identity does not match its Task", store.ErrConflict, projectionID)
	}
	if string(payload.Execution.State) != string(attempt.ExecutionState) {
		return false, fmt.Errorf("%w: standalone terminal projection %q execution state does not match its PromptAttempt", store.ErrConflict, projectionID)
	}
	projectedDelivery := store.PromptDeliveryNotRequested
	if payload.Delivery != nil {
		projectedDelivery = store.PromptDeliveryState(payload.Delivery.State)
	}
	if projectedDelivery != attempt.DeliveryState {
		return false, fmt.Errorf("%w: standalone terminal projection %q delivery state does not match its PromptAttempt", store.ErrConflict, projectionID)
	}
	return true, nil
}

func (d *ACPDispatcher) patchRecoveredTerminalEpoch(ctx context.Context, task *corev1alpha1.Task, epoch int64) error {
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, key, latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.Status.Execution == nil || latest.Status.Execution.ControllerEpoch >= epoch {
			return nil
		}
		latest.Status.Execution.ControllerEpoch = epoch
		return d.Client.Status().Update(ctx, latest)
	})
}

func (d *ACPDispatcher) patchRecoveredTaskReserved(ctx context.Context, task *corev1alpha1.Task, epoch int64, keepQueued bool) error {
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, key, latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		base := latest.DeepCopy()
		if latest.Status.Execution == nil {
			return nil
		}
		if keepQueued {
			latest.Status.Execution.State = corev1alpha1.TaskExecutionStateQueued
		} else {
			latest.Status.Execution.State = corev1alpha1.TaskExecutionStateReserved
		}
		latest.Status.Execution.ControllerEpoch = epoch
		latest.Status.Execution.RuntimeInstanceID = ""
		latest.Status.Execution.RuntimeSessionUID = ""
		latest.Status.Execution.RuntimeSessionGeneration = 0
		latest.Status.Execution.Reason = acpControllerRestartRecoveredReason
		latest.Status.Execution.Message = acpControllerRestartRecoveredMessage
		latest.Status.Execution.LastTransitionTime = nowMeta()
		return d.Client.Status().Patch(ctx, latest, client.MergeFrom(base))
	})
}

func (d *ACPDispatcher) finalizeRecoveredSessionUnknown(ctx context.Context, task *corev1alpha1.Task, fence store.ControllerEpochFence, attemptID, reason string) error {
	attempt, err := d.Store.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	session, err := d.recoveredTaskSession(ctx, task, attempt)
	if err != nil || session == nil {
		return err
	}
	return d.finalizeTaskSessionUnknown(ctx, task, fence, session, reason+": controller takeover classified "+attemptID+" as OutcomeUnknown")
}

func (d *ACPDispatcher) recoveredTaskSession(ctx context.Context, task *corev1alpha1.Task, attempt *store.PromptAttempt) (*acpTaskSession, error) {
	if task.Spec.SessionRef == nil || d.Sessions == nil {
		return nil, nil
	}
	if attempt == nil {
		return nil, fmt.Errorf("%w: session-backed ACP recovery requires a PromptAttempt", store.ErrConflict)
	}
	if attempt.Key.TaskUID != string(task.UID) || attempt.Key.Attempt != int64(task.Status.Execution.Attempt) || attempt.Key.PromptID != task.Status.Execution.PromptID {
		return nil, fmt.Errorf("%w: recovered PromptAttempt does not match Task execution identity", store.ErrConflict)
	}
	if strings.TrimSpace(attempt.SessionUID) == "" || attempt.SessionLeaseGeneration < 1 {
		return nil, fmt.Errorf("%w: session-backed terminal PromptAttempt lacks its durable SessionTurn identity", store.ErrConflict)
	}
	control, err := d.Store.GetSessionControl(ctx, task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		return nil, err
	}
	if control.SessionUID != attempt.SessionUID {
		return nil, fmt.Errorf("%w: recovered SessionControl UID does not match PromptAttempt", store.ErrConflict)
	}
	key := store.SessionTurnKey{
		SessionUID: attempt.SessionUID, LeaseGeneration: attempt.SessionLeaseGeneration,
		TaskUID: attempt.Key.TaskUID, Attempt: attempt.Key.Attempt, PromptID: attempt.Key.PromptID,
	}
	turnID, err := key.CanonicalID()
	if err != nil {
		return nil, err
	}
	turn, err := d.Store.GetSessionTurn(ctx, turnID)
	if err != nil {
		return nil, fmt.Errorf("load recovered ACP SessionTurn: %w", err)
	}
	if turn.Key != key || turn.PromptAttemptID != attempt.ID {
		return nil, fmt.Errorf("%w: recovered SessionTurn does not match PromptAttempt identity", store.ErrConflict)
	}
	appendPolicy := acpSessionTranscriptAppendPolicyForTask(task)
	expectedTurnDigest, err := acpSessionTurnDigest(
		turn.ID, attempt.ID, attempt.RequestDigest, turn.UserPrompt,
		appendPolicy.skipTranscriptAppend, appendPolicy.skipUserPromptAppend,
	)
	if err != nil {
		return nil, err
	}
	if turn.RequestDigest != expectedTurnDigest && (appendPolicy.skipTranscriptAppend || appendPolicy.skipUserPromptAppend) {
		legacyDigest, legacyErr := acpSessionTurnDigest(
			turn.ID, attempt.ID, attempt.RequestDigest, turn.UserPrompt, false, false,
		)
		if legacyErr == nil && turn.RequestDigest == legacyDigest {
			appendPolicy = acpSessionTranscriptAppendPolicy{}
			expectedTurnDigest = legacyDigest
		}
	}
	if turn.RequestDigest != expectedTurnDigest {
		return nil, fmt.Errorf("%w: recovered SessionTurn transcript append policy does not match its durable digest", store.ErrConflict)
	}
	if turn.State == store.SessionTurnOpen {
		lease := control.Lease
		if lease == nil || lease.Generation != key.LeaseGeneration || lease.TaskUID != key.TaskUID || lease.Attempt != key.Attempt || lease.PromptID != key.PromptID {
			return nil, fmt.Errorf("%w: open recovered SessionTurn lacks its matching mutation lease", store.ErrConflict)
		}
	}
	bootstrap, userPrompt, err := d.resolveTaskSessionBootstrap(ctx, task, control)
	if err != nil {
		return nil, err
	}
	if turn.UserPrompt != userPrompt {
		return nil, fmt.Errorf("%w: recovered SessionTurn prompt does not match bounded Task input", store.ErrConflict)
	}
	return &acpTaskSession{
		Turn: &ACPSessionTurn{
			Lease: ACPSessionLease{Session: *control, Key: key}, Turn: *turn,
			SkipTranscriptAppend: appendPolicy.skipTranscriptAppend,
			SkipUserPromptAppend: appendPolicy.skipUserPromptAppend,
		},
		Binding:    ACPRuntimeSessionBinding{SessionUID: control.SessionUID},
		Bootstrap:  bootstrap,
		UserPrompt: userPrompt,
	}, nil
}

func (d *ACPDispatcher) finalizeRecoveredTerminalSession(ctx context.Context, task *corev1alpha1.Task, attempt *store.PromptAttempt, fence store.ControllerEpochFence) error {
	prepared, err := d.prepareRecoveredTaskScopedRuntimeSessionForSettlement(ctx, task)
	if err != nil {
		return err
	}
	if !prepared {
		return fmt.Errorf("%w: RuntimeSession publication finalization is not ready for Session settlement", store.ErrNotReady)
	}
	session, err := d.recoveredTaskSession(ctx, task, attempt)
	if err != nil || session == nil {
		return err
	}
	if session.Turn.Turn.State == store.SessionTurnFinalized {
		_, err := d.Sessions.ResumeSessionTurnFinalization(ctx, ACPResumeSessionTurnFinalizationRequest{SessionTurn: *session.Turn, Fence: fence})
		if err == nil {
			session.finalized = true
			d.removeRuntimeSessionBinding(session.Binding.SessionUID)
		}
		return err
	}
	var finalizeErr error
	switch attempt.ExecutionState {
	case store.PromptExecutionOutcomeUnknown:
		finalizeErr = d.finalizeTaskSessionUnknown(ctx, task, fence, session, "controller restart recovered terminal OutcomeUnknown")
	case store.PromptExecutionCancelled:
		execution := corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateCancelled, Outcome: corev1alpha1.TaskExecutionOutcomeCancelled, Attempt: task.Status.Execution.Attempt, PromptID: task.Status.Execution.PromptID}
		finalizeErr = d.finalizeTaskSessionMarker(ctx, task, fence, session, "Cancelled", "controller restart recovered terminal cancellation", corev1alpha1.TaskPhaseCancelled, execution)
	case store.PromptExecutionFailed:
		execution := corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateFailed, Outcome: corev1alpha1.TaskExecutionOutcomeFailed, Attempt: task.Status.Execution.Attempt, PromptID: task.Status.Execution.PromptID}
		finalizeErr = d.finalizeTaskSessionMarker(ctx, task, fence, session, "Failed", "controller restart recovered terminal failure", corev1alpha1.TaskPhaseFailed, execution)
	case store.PromptExecutionSucceeded:
		delivery := task.Status.Delivery
		if delivery == nil {
			delivery = deliveryStatusFromPromptState(attempt.DeliveryState)
		}
		phase := corev1alpha1.TaskPhaseFailed
		switch attempt.DeliveryState {
		case store.PromptDeliveryNotRequested, store.PromptDeliveryReadValidated, store.PromptDeliveryNoChange,
			store.PromptDeliveryVerifiedExact, store.PromptDeliveryDeliveredSuperseded:
			phase = corev1alpha1.TaskPhaseSucceeded
		case store.PromptDeliveryCancelledBeforePublish:
			phase = corev1alpha1.TaskPhaseCancelled
		}
		result, resultErr := d.ResultStore.GetResult(ctx, task.Namespace, task.Name)
		if resultErr != nil {
			return resultErr
		}
		publicationID := ""
		if attempt.DeliveryState == store.PromptDeliveryVerifiedExact || attempt.DeliveryState == store.PromptDeliveryDeliveredSuperseded ||
			attempt.DeliveryState == store.PromptDeliveryConflict || attempt.DeliveryState == store.PromptDeliveryPublicationOutcomeUnknown ||
			attempt.DeliveryState == store.PromptDeliveryCancelledBeforePublish {
			candidate := publicationIDForTask(task)
			if _, publicationErr := d.Store.GetPublication(ctx, candidate); publicationErr == nil {
				publicationID = candidate
			} else if !errors.Is(publicationErr, store.ErrNotFound) {
				return publicationErr
			}
		}
		finalizeErr = d.finalizeTaskSessionResult(ctx, task, fence, session, string(result), publicationID, phase, *delivery)
	}
	if finalizeErr == nil {
		session.finalized = true
		d.removeRuntimeSessionBinding(session.Binding.SessionUID)
	}
	return finalizeErr
}

func (d *ACPDispatcher) recoverSucceededTaskProjection(ctx context.Context, task *corev1alpha1.Task, attempt *store.PromptAttempt, fence store.ControllerEpochFence) error {
	if !store.IsTerminalPromptDeliveryState(attempt.DeliveryState) {
		switch attempt.DeliveryState {
		case store.PromptDeliveryValidating:
			if err := d.transitionDelivery(ctx, attempt.ID, fence, store.PromptDeliveryValidating, store.PromptDeliveryConflict,
				"controller-restart-workspace-lost", "runtime workspace was lost before durable validation completed"); err != nil {
				return err
			}
			updatedAttempt, err := d.Store.GetPromptAttempt(ctx, attempt.ID)
			if err != nil {
				return err
			}
			attempt = updatedAttempt
			if err := d.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
				return err
			}
			status := corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
				Reason: "RuntimeWorkspaceLost", Message: "runtime workspace was lost before durable validation completed", LastTransitionTime: nowMeta(),
			}
			return d.failTaskForDelivery(ctx, task, status, status.Message)
		case store.PromptDeliveryPreparing, store.PromptDeliveryPrepared, store.PromptDeliveryPublishing, store.PromptDeliveryVerifying:
			result, err := d.reconcilePersistedPublication(ctx, task, attempt.ID, fence)
			if err != nil {
				return err
			}
			if err := d.patchDeliveryStatus(ctx, task, result.Status); err != nil {
				return err
			}
			attempt, err = d.Store.GetPromptAttempt(ctx, attempt.ID)
			if err != nil {
				return err
			}
			if err := d.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
				return err
			}
			switch result.Status.Outcome {
			case corev1alpha1.TaskDeliveryOutcomeVerifiedExact, corev1alpha1.TaskDeliveryOutcomeDeliveredSuperseded:
				return d.completeSuccessWithDelivery(ctx, task, result.Status, "ACP publication recovered after controller restart")
			case corev1alpha1.TaskDeliveryOutcomeCancelledBeforePublish:
				return d.cancelTaskAfterExecution(ctx, task, result.Status, "publication cancelled before push during recovery")
			default:
				return d.failTaskForDelivery(ctx, task, result.Status, "ACP publication recovery reached a terminal delivery failure")
			}
		default:
			return fmt.Errorf("unsupported nonterminal delivery state %s during recovery", attempt.DeliveryState)
		}
	}
	if err := d.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
		return err
	}
	return d.patchRecoveredTerminalExecution(ctx, task, attempt, fence.Epoch)
}

func (d *ACPDispatcher) recoveredTerminalDeliveryStatus(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
) (*corev1alpha1.TaskDeliveryStatus, error) {
	switch attempt.DeliveryState {
	case store.PromptDeliveryVerifiedExact, store.PromptDeliveryDeliveredSuperseded,
		store.PromptDeliveryConflict, store.PromptDeliveryCredentialBlocked,
		store.PromptDeliveryPublicationOutcomeUnknown, store.PromptDeliveryCancelledBeforePublish:
		publication, err := d.Store.GetPublication(ctx, publicationIDForTask(task))
		if errors.Is(err, store.ErrNotFound) && task.Status.Delivery != nil && task.Status.Delivery.Outcome != "" {
			status := *task.Status.Delivery
			return &status, nil
		}
		if err != nil {
			return nil, err
		}
		artifact := harnessv2.ArtifactReference{
			ArtifactID: harnessv2.ArtifactID(publication.ArtifactID), Digest: publication.ArtifactDigest,
			SizeBytes: publication.ArtifactSizeBytes, MediaType: publication.ArtifactMediaType,
		}
		delta := harnessv2.WorkspaceDeltaDescriptor{
			State: harnessv2.WorkspaceDeltaPrepared, Intent: harnessv2.WorkspaceIntentWrite,
			VerifiedBaseline: harnessv2.WorkspaceBaseline{RepositoryIdentity: publication.SourceRepositoryID, Revision: publication.SourceBaselineSHA},
			RelativeRoot:     strings.TrimSpace(task.Spec.Workspace.SubPath), Artifact: &artifact,
			PublicationSafe: true, NoFollowVerified: true,
		}
		status := publicationTaskDeliveryStatus(task.Spec.Workspace, delta.VerifiedBaseline, delta, publication, strings.TrimPrefix(publication.TargetRef, "refs/heads/"))
		if task.Status.Delivery != nil && task.Status.Delivery.Reason == corev1alpha1.TaskDeliveryReasonCancellationRequestedAfterPublish {
			status.Reason = task.Status.Delivery.Reason
			status.Message = task.Status.Delivery.Message
		}
		return &status, nil
	default:
		if task.Status.Delivery != nil && store.PromptDeliveryState(task.Status.Delivery.State) == attempt.DeliveryState {
			return task.Status.Delivery.DeepCopy(), nil
		}
		return deliveryStatusFromPromptState(attempt.DeliveryState), nil
	}
}

func (d *ACPDispatcher) patchRecoveredTerminalExecution(ctx context.Context, task *corev1alpha1.Task, attempt *store.PromptAttempt, epoch int64) error {
	if attempt.ExecutionState == store.PromptExecutionSucceeded {
		status, err := d.recoveredTerminalDeliveryStatus(ctx, task, attempt)
		if err != nil {
			return err
		}
		switch attempt.DeliveryState {
		case store.PromptDeliveryNotRequested, store.PromptDeliveryReadValidated, store.PromptDeliveryNoChange,
			store.PromptDeliveryVerifiedExact, store.PromptDeliveryDeliveredSuperseded:
			return d.completeSuccessWithDelivery(ctx, task, *status, "ACP task recovered after controller restart")
		default:
			return d.failTaskForDelivery(ctx, task, *status, "ACP delivery recovered as terminal failure after controller restart")
		}
	}
	return d.patchRecoveredExecutionMessage(ctx, task, epoch, acpControllerRestartRecoveredReason, "terminal ACP attempt recovered under the new controller epoch")
}

func (d *ACPDispatcher) patchRecoveredExecutionMessage(ctx context.Context, task *corev1alpha1.Task, epoch int64, reason corev1alpha1.TaskExecutionReason, message string) error {
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, key, latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		base := latest.DeepCopy()
		if latest.Status.Execution == nil {
			return nil
		}
		latest.Status.Execution.ControllerEpoch = epoch
		latest.Status.Execution.Reason = reason
		latest.Status.Execution.Message = message
		latest.Status.Execution.LastTransitionTime = nowMeta()
		return d.Client.Status().Patch(ctx, latest, client.MergeFrom(base))
	})
}

func deliveryStatusFromPromptState(state store.PromptDeliveryState) *corev1alpha1.TaskDeliveryStatus {
	now := metav1.Now()
	status := &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryState(state), Outcome: corev1alpha1.TaskDeliveryOutcome(state), LastTransitionTime: &now}
	return status
}
