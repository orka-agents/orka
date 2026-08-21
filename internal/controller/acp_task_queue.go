package controller

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workspace/statusrules"
)

const (
	acpRuntimePoolLabel                      = "orka.ai/acp-runtime-pool"
	acpRuntimeTrustLabel                     = "orka.ai/acp-trust-domain"
	acpRuntimeProfileLabel                   = "orka.ai/acp-profile"
	acpRuntimeWorkspaceProviderLabel         = "orka.ai/acp-execution-workspace-provider"
	acpRuntimeTaskPoolLabel                  = "orka.ai/runtime-pool"
	acpRuntimeSessionCleanupAnnotation       = "orka.ai/runtime-session-cleanup"
	acpExternalRuntimeTaskLabel              = "orka.ai/agent-runtime"
	acpRuntimeLastDemandAnnotation           = "orka.ai/acp-last-demand-at"
	acpRuntimeQueuedAtAnnotation             = "orka.ai/acp-queued-at"
	defaultACPTaskPriority             int32 = 500
	defaultACPQueueAgingStep           int32 = 25
)

const (
	DefaultACPQueueAgingInterval = 30 * time.Second
	DefaultACPQueueMaximumWait   = 5 * time.Minute
)

//nolint:gocyclo // ACP queueing keeps durable planning, recovery, and binding gates auditable together.
func (r *TaskReconciler) queueACPRuntimeTask(ctx context.Context, task *corev1alpha1.Task, _ *corev1alpha1.Agent) (ctrl.Result, error) {
	if task == nil || task.Status.AgentExecutionBinding == nil {
		return ctrl.Result{}, errors.New("immutable v2 execution binding is required before ACP queueing")
	}
	bound, err := r.loadVerifiedBoundExecution(ctx, task, task.Status.AgentExecutionBinding)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("verify immutable v2 execution before ACP queueing: %w", err)
	}
	frozenTask := bound.frozenTask
	plan := bound.plan
	if err := r.ACPAdmissionGate.Check(); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if r.DurableControlStore == nil || r.ControllerEpochManager == nil {
		return r.failTask(ctx, task, "durable ACP control store and controller epoch manager are required")
	}
	if handled, result, err := r.reconcileDurableACPPlanningFailure(ctx, task); handled || err != nil {
		return result, err
	}
	if err := validateACPWorkspacePreflight(frozenTask); err != nil {
		return r.failACPPlanningTask(ctx, task, corev1alpha1.TaskExecutionReason("InvalidWorkspace"), err.Error())
	}
	if err := validateACPRuntimeWorkspaceNamespace(plan, task.Namespace, r.ACPRuntimeNamespace); err != nil {
		return r.failACPPlanningTask(ctx, task, corev1alpha1.TaskExecutionReason("InvalidWorkspace"), err.Error())
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	pool, err := r.ensureACPRuntimePool(ctx, task.Namespace, plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if task.Status.Execution != nil && !taskExecutionStateTerminal(task.Status.Execution.State) {
		if task.Status.Execution.State == corev1alpha1.TaskExecutionStateQueued ||
			task.Status.Execution.State == corev1alpha1.TaskExecutionStateReserved {
			rebound, rebindErr := r.rebindQueuedACPRuntimeTask(ctx, task, bound, pool)
			if rebindErr != nil {
				return ctrl.Result{}, rebindErr
			}
			if rebound && r.Recorder != nil {
				r.Recorder.Eventf(task, corev1.EventTypeNormal, "ACPRuntimePoolRebound", "Rebound pre-submission attempt %d to replacement RuntimePool %s", task.Status.Execution.Attempt, pool.Name)
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if task.UID == "" {
		return r.failTask(ctx, task, "Task UID is required for ACP prompt identity")
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	attemptNumber := task.Status.Attempts + 1
	queuedAt := time.Now().UTC()
	promptID := fmt.Sprintf("prompt-%s-%d", task.UID, attemptNumber)
	requestDigest, err := acpBoundTaskRequestDigest(bound, attemptNumber, promptID)
	if err != nil {
		return ctrl.Result{}, err
	}
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: int64(attemptNumber), PromptID: promptID}
	attemptID, err := key.CanonicalID()
	if err != nil {
		return ctrl.Result{}, err
	}
	credentialBindings, credentialVersions, err := resolvePromptCredentialBindings(ctx, reader, frozenTask)
	if err != nil {
		credentialErr := fmt.Errorf("freeze ACP credential bindings: %w", err)
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, credentialErr
		}
		return r.failACPPlanningTask(ctx, task, corev1alpha1.TaskExecutionReason("InvalidWorkspace"), credentialErr.Error())
	}
	attempt, err := r.DurableControlStore.CreatePromptAttempt(ctx, &store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: requestDigest,
		BindingDigest: bound.binding.BindingDigest, SnapshotDigest: bound.snapshot.Digest,
		CredentialBindings: credentialBindings,
		ExecutionState:     store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested, CreatedAt: queuedAt,
	}, fence)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("persist ACP prompt attempt: %w", err)
	}

	metadataBase := task.DeepCopy()
	if task.Labels == nil {
		task.Labels = make(map[string]string)
	}
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Labels[acpRuntimeTaskPoolLabel] = pool.Name
	task.Annotations[acpRuntimeQueuedAtAnnotation] = queuedAt.Format(time.RFC3339Nano)
	if err := r.Patch(ctx, task, client.MergeFrom(metadataBase)); err != nil {
		return ctrl.Result{}, err
	}
	statusBase := task.DeepCopy()
	now := metav1.NewTime(queuedAt)
	task.Status.Attempts = attemptNumber
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateQueued, Attempt: attemptNumber, PromptID: promptID,
		RuntimePoolName: pool.Name, RuntimePoolUID: string(pool.UID), ControllerEpoch: fence.Epoch,
		RequestDigest:                            attempt.RequestDigest,
		ReadCredentialResourceVersion:            credentialVersions.SourceRead,
		PublicationReadCredentialResourceVersion: credentialVersions.TargetRead,
		PublicationCredentialResourceVersion:     credentialVersions.TargetWrite,
		ForgeCredentialResourceVersion:           credentialVersions.Forge,
		LastTransitionTime:                       &now,
	}
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested, LastTransitionTime: &now,
	}
	if plan.Workspace != nil {
		// Provider-neutral projection only: no claim, sandbox, or other
		// provider-native identifier ever enters public Task status.
		task.Status.ExecutionWorkspace = statusrules.Update{
			Provider:      plan.Workspace.Provider,
			Phase:         corev1alpha1.ExecutionWorkspacePhasePending,
			Reason:        corev1alpha1.ExecutionWorkspaceReasonPending,
			ReusePolicy:   plan.Workspace.ReusePolicy,
			CleanupPolicy: plan.Workspace.CleanupPolicy,
			Message:       "RuntimeSession is queued for a workspace-provider-backed RuntimePool",
			ObservedAt:    &now,
		}.Status()
	}
	if err := r.Status().Patch(ctx, task, client.MergeFrom(statusBase)); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(task, corev1.EventTypeNormal, "ACPTaskQueued", "Queued attempt %d for RuntimePool %s", attemptNumber, pool.Name)
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

//nolint:gocyclo // Rebinding keeps every queued-attempt fence and optimistic-lock check auditable together.
func (r *TaskReconciler) rebindQueuedACPRuntimeTask(
	ctx context.Context,
	task *corev1alpha1.Task,
	bound *verifiedAgentExecution,
	pool *corev1alpha1.RuntimePool,
) (bool, error) {
	if task == nil || task.Status.Execution == nil ||
		(task.Status.Execution.State != corev1alpha1.TaskExecutionStateQueued &&
			task.Status.Execution.State != corev1alpha1.TaskExecutionStateReserved) ||
		pool == nil || pool.UID == "" {
		return false, nil
	}
	if acpRuntimePoolBindingMatches(task.Status.Execution, pool) {
		return r.patchACPRuntimePoolTaskLabel(ctx, task, pool)
	}
	requestMatches, err := acpQueuedTaskRequestMatchesBinding(bound, task.Status.Execution)
	if err != nil {
		return false, err
	}
	if !requestMatches {
		return false, nil
	}
	attemptID, err := promptAttemptIDFromTask(task)
	if err != nil {
		return false, err
	}
	attempt, err := r.DurableControlStore.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		return false, err
	}
	expectedAttemptState := store.PromptExecutionQueued
	if task.Status.Execution.State == corev1alpha1.TaskExecutionStateReserved {
		expectedAttemptState = store.PromptExecutionReserved
	}
	if !queuedPromptAttemptMatchesTask(attempt, task) || attempt.ExecutionState != expectedAttemptState ||
		attempt.DeliveryState != store.PromptDeliveryNotRequested || promptAttemptHasRuntimeOrSessionBinding(attempt) ||
		taskExecutionHasRuntimeOrSessionBinding(task.Status.Execution) {
		return false, nil
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return false, err
	}
	if expectedAttemptState == store.PromptExecutionReserved {
		safe, safeErr := r.reservedACPRuntimeTaskRebindSafe(ctx, task, attempt, fence)
		if safeErr != nil {
			return false, safeErr
		}
		if !safe {
			return false, nil
		}
		attempt, err = r.refreshReservedACPRuntimeTaskRebind(ctx, task, attempt, pool, fence)
		if err != nil {
			return false, err
		}
	}

	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	expectedTaskUID := task.UID
	expectedAttempt := task.Status.Execution.Attempt
	expectedPromptID := task.Status.Execution.PromptID
	expectedRequestDigest := task.Status.Execution.RequestDigest
	expectedAttemptVersion := attempt.Version
	rebound := false
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := r.taskMetadataReader().Get(ctx, key, current); err != nil {
			return err
		}
		status := current.Status.Execution
		if current.UID != expectedTaskUID || status == nil || status.State != task.Status.Execution.State ||
			status.Attempt != expectedAttempt || status.PromptID != expectedPromptID || status.RequestDigest != expectedRequestDigest ||
			taskExecutionHasRuntimeOrSessionBinding(status) {
			return nil
		}
		if acpRuntimePoolBindingMatches(status, pool) {
			rebound = true
			return nil
		}
		currentAttempt, err := r.DurableControlStore.GetPromptAttempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if currentAttempt.Version != expectedAttemptVersion || currentAttempt.ExecutionState != expectedAttemptState ||
			!queuedPromptAttemptMatchesTask(currentAttempt, current) ||
			currentAttempt.DeliveryState != store.PromptDeliveryNotRequested || promptAttemptHasRuntimeOrSessionBinding(currentAttempt) {
			return nil
		}
		if expectedAttemptState == store.PromptExecutionReserved {
			safe, safeErr := r.reservedACPRuntimeTaskRebindSafe(ctx, current, currentAttempt, fence)
			if safeErr != nil {
				return safeErr
			}
			if !safe {
				return nil
			}
		}
		base := current.DeepCopy()
		status.RuntimePoolName = pool.Name
		status.RuntimePoolUID = string(pool.UID)
		status.ControllerEpoch = fence.Epoch
		status.Reason = ""
		status.Message = ""
		status.LastTransitionTime = nowMeta()
		if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		rebound = true
		return nil
	}); err != nil {
		return false, err
	}
	if !rebound {
		return false, nil
	}
	labelPatched, err := r.patchACPRuntimePoolTaskLabel(ctx, task, pool)
	if err != nil {
		return false, err
	}
	return rebound || labelPatched, nil
}

func (r *TaskReconciler) patchACPRuntimePoolTaskLabel(
	ctx context.Context,
	task *corev1alpha1.Task,
	pool *corev1alpha1.RuntimePool,
) (bool, error) {
	if task == nil || pool == nil {
		return false, nil
	}
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	expectedTaskUID := task.UID
	patched := false
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := r.taskMetadataReader().Get(ctx, key, current); err != nil {
			return err
		}
		if current.UID != expectedTaskUID || !acpRuntimePoolBindingMatches(current.Status.Execution, pool) {
			return nil
		}
		if current.Labels != nil && current.Labels[acpRuntimeTaskPoolLabel] == pool.Name {
			return nil
		}
		base := current.DeepCopy()
		if current.Labels == nil {
			current.Labels = make(map[string]string)
		}
		current.Labels[acpRuntimeTaskPoolLabel] = pool.Name
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		patched = true
		return nil
	}); err != nil {
		return false, err
	}
	return patched, nil
}

func taskExecutionHasRuntimeOrSessionBinding(status *corev1alpha1.TaskExecutionStatus) bool {
	return status != nil && (strings.TrimSpace(status.RuntimeInstanceID) != "" ||
		strings.TrimSpace(status.RuntimeSessionUID) != "" || status.RuntimeSessionGeneration != 0 ||
		strings.TrimSpace(status.RuntimeSessionSupervisorBootID) != "" ||
		strings.TrimSpace(status.RuntimeSessionProfileDigest) != "" || strings.TrimSpace(status.RuntimeSessionMCPDigest) != "" ||
		strings.TrimSpace(status.RuntimeSessionWorkspaceDigest) != "" || status.RuntimeSessionRecreationPending)
}

func promptAttemptHasRuntimeOrSessionBinding(attempt *store.PromptAttempt) bool {
	return attempt != nil && (strings.TrimSpace(attempt.RuntimeInstanceID) != "" ||
		strings.TrimSpace(attempt.SessionUID) != "" || attempt.SessionLeaseGeneration != 0)
}

func (r *TaskReconciler) refreshReservedACPRuntimeTaskRebind(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
	pool *corev1alpha1.RuntimePool,
	fence store.ControllerEpochFence,
) (*store.PromptAttempt, error) {
	if task == nil || task.Status.Execution == nil || attempt == nil || pool == nil {
		return nil, fmt.Errorf("reserved RuntimePool rebind identity is incomplete")
	}
	operationID := store.CanonicalControlID(
		"rebind-reserved-runtime-pool", attempt.ID, task.Status.Execution.RuntimePoolUID, string(pool.UID),
	)
	operationDigest, err := acpDomainDigest("reserved-runtime-pool-rebind", map[string]any{
		"attemptID": attempt.ID, "requestDigest": attempt.RequestDigest,
		"fromPoolUID": task.Status.Execution.RuntimePoolUID, "toPoolUID": string(pool.UID),
		"controllerEpoch": fence.Epoch,
	})
	if err != nil {
		return nil, err
	}
	return r.DurableControlStore.RecoverPromptAttemptPreSubmission(ctx, store.PromptAttemptPreSubmissionRecovery{
		ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: store.PromptExecutionReserved,
		OperationID: operationID, OperationDigest: operationDigest, RecoveredAt: time.Now().UTC(),
	})
}

func (r *TaskReconciler) reservedACPRuntimeTaskRebindSafe(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
	fence store.ControllerEpochFence,
) (bool, error) {
	if task == nil || task.Status.Execution == nil || attempt == nil ||
		task.Status.Execution.State != corev1alpha1.TaskExecutionStateReserved ||
		attempt.ExecutionState != store.PromptExecutionReserved || taskExecutionHasRuntimeOrSessionBinding(task.Status.Execution) ||
		promptAttemptHasRuntimeOrSessionBinding(attempt) || task.Status.Execution.ControllerEpoch != fence.Epoch ||
		attempt.ControllerEpoch != fence.Epoch || attempt.ControllerEpochName != fence.Name {
		return false, nil
	}
	oldPool := &corev1alpha1.RuntimePool{}
	key := types.NamespacedName{Namespace: task.Namespace, Name: strings.TrimSpace(task.Status.Execution.RuntimePoolName)}
	if err := r.taskMetadataReader().Get(ctx, key, oldPool); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if string(oldPool.UID) != strings.TrimSpace(task.Status.Execution.RuntimePoolUID) {
		return true, nil
	}
	if oldPool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing &&
		oldPool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
		return false, nil
	}
	now := time.Now().UTC()
	for i := range oldPool.Status.Capacity.Reservations {
		reservation := oldPool.Status.Capacity.Reservations[i]
		if reservation.PoolUID == string(oldPool.UID) && reservation.TaskUID == string(task.UID) &&
			reservation.Attempt == task.Status.Execution.Attempt && reservation.ControllerEpoch == fence.Epoch &&
			reservation.ExpiresAt.After(now) {
			return false, nil
		}
	}
	return true, nil
}

func queuedPromptAttemptMatchesTask(attempt *store.PromptAttempt, task *corev1alpha1.Task) bool {
	return attempt != nil && task != nil && task.Status.Execution != nil &&
		attempt.Key.Namespace == task.Namespace && attempt.Key.TaskUID == string(task.UID) &&
		attempt.Key.Attempt == int64(task.Status.Execution.Attempt) && attempt.Key.PromptID == task.Status.Execution.PromptID &&
		attempt.RequestDigest == task.Status.Execution.RequestDigest
}

type acpTaskQueueRank struct {
	promoted          bool
	effectivePriority int32
	queuedAt          time.Time
	createdAt         time.Time
	namespace         string
	name              string
	uid               string
}

func sortACPTasksByQueuePriority(tasks []*corev1alpha1.Task, now time.Time) {
	sortTasksByQueuePriority(tasks, now, acpTaskQueuedAt)
}

func sortTasksByQueuePriority(
	tasks []*corev1alpha1.Task,
	now time.Time,
	queuedAt func(*corev1alpha1.Task) time.Time,
) {
	now = now.UTC()
	ranks := make(map[*corev1alpha1.Task]acpTaskQueueRank, len(tasks))
	for _, task := range tasks {
		ranks[task] = rankTaskForQueue(task, now, queuedAt(task))
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		return taskQueueRankLess(ranks[tasks[i]], ranks[tasks[j]])
	})
}

func rankTaskForQueue(task *corev1alpha1.Task, now, queuedAt time.Time) acpTaskQueueRank {
	age := max(now.Sub(queuedAt), 0)
	priority := defaultACPTaskPriority
	if task != nil && task.Spec.Priority != nil {
		priority = *task.Spec.Priority
	}
	agingSteps := int64(age / DefaultACPQueueAgingInterval)
	agedPriority := min(int64(priority)+agingSteps*int64(defaultACPQueueAgingStep), 1000)
	createdAt := queuedAt
	var namespace, name, uid string
	if task != nil {
		if !task.CreationTimestamp.IsZero() {
			createdAt = task.CreationTimestamp.UTC()
		}
		namespace, name, uid = task.Namespace, task.Name, string(task.UID)
	}
	return acpTaskQueueRank{
		promoted: age >= DefaultACPQueueMaximumWait, effectivePriority: int32(agedPriority),
		queuedAt: queuedAt, createdAt: createdAt, namespace: namespace, name: name, uid: uid,
	}
}

func taskQueueRankLess(left, right acpTaskQueueRank) bool {
	if left.promoted != right.promoted {
		return left.promoted
	}
	if !left.promoted && left.effectivePriority != right.effectivePriority {
		return left.effectivePriority > right.effectivePriority
	}
	if !left.queuedAt.Equal(right.queuedAt) {
		return left.queuedAt.Before(right.queuedAt)
	}
	if !left.createdAt.Equal(right.createdAt) {
		return left.createdAt.Before(right.createdAt)
	}
	if left.namespace != right.namespace {
		return left.namespace < right.namespace
	}
	if left.name != right.name {
		return left.name < right.name
	}
	return left.uid < right.uid
}

func acpTaskQueuedAt(task *corev1alpha1.Task) time.Time {
	if task == nil {
		return time.Unix(0, 0).UTC()
	}
	if value := strings.TrimSpace(task.Annotations[acpRuntimeQueuedAtAnnotation]); value != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC()
		}
	}
	if task.Status.Execution != nil && task.Status.Execution.LastTransitionTime != nil && !task.Status.Execution.LastTransitionTime.IsZero() {
		return task.Status.Execution.LastTransitionTime.UTC()
	}
	if !task.CreationTimestamp.IsZero() {
		return task.CreationTimestamp.UTC()
	}
	return time.Unix(0, 0).UTC()
}

func (r *TaskReconciler) cancelACPTaskBeforeDurableAttempt(ctx context.Context, task *corev1alpha1.Task, message string) (ctrl.Result, error) {
	reader := r.taskMetadataReader()
	latest := &corev1alpha1.Task{}
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	if err := reader.Get(ctx, key, latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	nowUTC := time.Now().UTC()
	deadline, expired := r.pendingAgentTaskDeadline(ctx, latest, nowUTC)
	if latest.UID != task.UID || latest.Spec.Type != corev1alpha1.TaskTypeAgent || !expired || nowUTC.Before(deadline) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if latest.Status.Execution != nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	var attempt *store.PromptAttempt
	if r.DurableControlStore != nil && latest.UID != "" {
		attemptNumber := latest.Status.Attempts + 1
		promptID := fmt.Sprintf("prompt-%s-%d", latest.UID, attemptNumber)
		attemptKey := store.PromptAttemptKey{
			Namespace: latest.Namespace, TaskUID: string(latest.UID), Attempt: int64(attemptNumber), PromptID: promptID,
		}
		attemptID, err := attemptKey.CanonicalID()
		if err != nil {
			return ctrl.Result{}, err
		}
		attempt, err = r.DurableControlStore.GetPromptAttempt(ctx, attemptID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return ctrl.Result{}, err
		}
		if attempt != nil {
			if attempt.ExecutionState == store.PromptExecutionQueued {
				if r.ControllerEpochManager == nil {
					return ctrl.Result{}, fmt.Errorf("controller epoch manager is required to cancel queued ACP attempt")
				}
				fence, fenceErr := r.ControllerEpochManager.CurrentFence(ctx)
				if fenceErr != nil {
					return ctrl.Result{}, fenceErr
				}
				operationID := "timeout-before-status-" + fmt.Sprint(attempt.Version)
				digest, digestErr := acpDomainDigest("attempt-transition", map[string]any{
					"id": attempt.ID, "from": attempt.ExecutionState, "to": store.PromptExecutionCancelled,
					"operation": operationID, "version": attempt.Version,
				})
				if digestErr != nil {
					return ctrl.Result{}, digestErr
				}
				attempt, err = r.DurableControlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
					ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: store.PromptExecutionQueued,
					NewState: store.PromptExecutionCancelled, OperationID: operationID, OperationDigest: digest,
					TerminalReason: acpTaskTimeoutReason, OutcomeMarker: message, UpdatedAt: time.Now().UTC(),
				})
				if err != nil {
					return ctrl.Result{}, err
				}
			} else if attempt.ExecutionState != store.PromptExecutionCancelled {
				return ctrl.Result{}, fmt.Errorf("prompt attempt %q is %s while Task execution status is empty", attempt.ID, attempt.ExecutionState)
			}
		}
	}

	now := metav1.Now()
	statusBound := false
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := reader.Get(ctx, key, current); err != nil {
			return err
		}
		currentNow := time.Now().UTC()
		currentDeadline, currentHasDeadline := r.pendingAgentTaskDeadline(ctx, current, currentNow)
		if current.UID != task.UID || current.Spec.Type != corev1alpha1.TaskTypeAgent ||
			!currentHasDeadline || currentNow.Before(currentDeadline) {
			statusBound = true
			return nil
		}
		if current.Status.Execution != nil {
			statusBound = true
			return nil
		}
		base := current.DeepCopy()
		current.Status.Phase = corev1alpha1.TaskPhaseCancelled
		current.Status.CompletionTime = &now
		current.Status.Message = message
		current.Status.Execution = &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateCancelled, Outcome: corev1alpha1.TaskExecutionOutcomeCancelled,
			Reason: acpTaskTimeoutReason, Message: message, LastTransitionTime: &now,
		}
		if attempt != nil {
			if current.Status.Attempts < int32(attempt.Key.Attempt) {
				current.Status.Attempts = int32(attempt.Key.Attempt)
			}
			current.Status.Execution.Attempt = int32(attempt.Key.Attempt)
			current.Status.Execution.PromptID = attempt.Key.PromptID
			current.Status.Execution.RequestDigest = attempt.RequestDigest
			current.Status.Execution.ControllerEpoch = attempt.ControllerEpoch
		}
		current.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
			State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
			LastTransitionTime: &now,
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: ConditionTypeWaitingForApproval, Status: metav1.ConditionFalse, LastTransitionTime: now,
			Reason: "TaskCancelled", Message: "task is terminal",
		})
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: ConditionTypeComplete, Status: metav1.ConditionTrue, LastTransitionTime: now,
			Reason: "TaskCancelled", Message: message,
		})
		return r.Status().Patch(ctx, current, client.MergeFrom(base))
	}); err != nil {
		return ctrl.Result{}, err
	}
	if statusBound {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *TaskReconciler) failACPPlanningTask(
	ctx context.Context,
	task *corev1alpha1.Task,
	reason corev1alpha1.TaskExecutionReason,
	message string,
) (ctrl.Result, error) {
	attempt, err := r.settleACPPlanningFailureAttempt(ctx, task, reason, message)
	if errors.Is(err, store.ErrNotReady) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if attempt != nil {
		canonicalReason := corev1alpha1.TaskExecutionReason(attempt.TerminalReason)
		canonicalMessage := strings.TrimSpace(attempt.OutcomeMarker)
		if canonicalReason != reason || canonicalMessage == "" {
			return ctrl.Result{}, fmt.Errorf("%w: settled ACP planning failure marker is inconsistent", store.ErrConflict)
		}
		reason = canonicalReason
		message = canonicalMessage
		if err := r.enqueueACPPlanningFailureProjection(ctx, task, attempt, reason, message); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.projectACPPlanningFailureTask(ctx, task, attempt, reason, message)
}

func (r *TaskReconciler) reconcileDurableACPPlanningFailure(
	ctx context.Context,
	task *corev1alpha1.Task,
) (bool, ctrl.Result, error) {
	if task == nil || task.Status.Execution == nil || task.Status.Execution.Attempt < 1 ||
		(task.Status.Execution.State != corev1alpha1.TaskExecutionStateQueued &&
			task.Status.Execution.State != corev1alpha1.TaskExecutionStateReserved) {
		return false, ctrl.Result{}, nil
	}
	attemptID, err := promptAttemptIDFromTask(task)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	attempt, err := r.DurableControlStore.GetPromptAttempt(ctx, attemptID)
	if errors.Is(err, store.ErrNotFound) {
		return false, ctrl.Result{}, nil
	}
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if attempt.ExecutionState != store.PromptExecutionFailed {
		return false, ctrl.Result{}, nil
	}
	reason := corev1alpha1.TaskExecutionReason(attempt.TerminalReason)
	if reason != corev1alpha1.TaskExecutionReason("InvalidRuntimeProfile") &&
		reason != corev1alpha1.TaskExecutionReason("InvalidWorkspace") {
		return false, ctrl.Result{}, nil
	}
	message := strings.TrimSpace(attempt.OutcomeMarker)
	if message == "" || !queuedPromptAttemptMatchesTask(attempt, task) {
		return true, ctrl.Result{}, fmt.Errorf("%w: durable ACP planning failure is missing its canonical Task projection", store.ErrConflict)
	}
	if err := r.enqueueACPPlanningFailureProjection(ctx, task, attempt, reason, message); err != nil {
		return true, ctrl.Result{}, err
	}
	result, err := r.projectACPPlanningFailureTask(ctx, task, attempt, reason, message)
	return true, result, err
}

func (r *TaskReconciler) projectACPPlanningFailureTask(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
	reason corev1alpha1.TaskExecutionReason,
	message string,
) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1alpha1.Task{}
		if err := r.taskMetadataReader().Get(ctx, key, latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != task.UID {
			return fmt.Errorf("%w: Task identity changed while projecting ACP planning failure", store.ErrConflict)
		}
		base := latest.DeepCopy()
		terminalTime := metav1.Now()
		if attempt != nil {
			terminalTime = metav1.NewTime(attempt.UpdatedAt.UTC())
		} else if latest.Status.Execution != nil && latest.Status.Execution.State == corev1alpha1.TaskExecutionStateFailed &&
			latest.Status.Execution.Reason == reason && latest.Status.Execution.LastTransitionTime != nil {
			terminalTime = *latest.Status.Execution.LastTransitionTime
		}
		if attempt == nil {
			execution := latest.Status.Execution
			switch {
			case execution == nil:
				latest.Status.Execution = &corev1alpha1.TaskExecutionStatus{
					State: corev1alpha1.TaskExecutionStateFailed, Outcome: corev1alpha1.TaskExecutionOutcomeFailed,
					Reason: reason, Message: message, LastTransitionTime: &terminalTime,
				}
			case execution.State == corev1alpha1.TaskExecutionStateFailed && execution.Reason == reason &&
				execution.Attempt == 0 && execution.PromptID == "" && execution.RequestDigest == "":
				execution.Outcome = corev1alpha1.TaskExecutionOutcomeFailed
				execution.Message = message
				execution.LastTransitionTime = &terminalTime
			default:
				return fmt.Errorf("%w: Task acquired durable execution identity while projecting ACP planning failure", store.ErrConflict)
			}
		} else {
			execution := latest.Status.Execution
			if execution == nil || !queuedPromptAttemptMatchesTask(attempt, latest) {
				return fmt.Errorf("%w: Task execution identity changed after ACP planning failure settlement", store.ErrConflict)
			}
			if execution.State != corev1alpha1.TaskExecutionStateQueued &&
				execution.State != corev1alpha1.TaskExecutionStateReserved &&
				(execution.State != corev1alpha1.TaskExecutionStateFailed || execution.Reason != reason) {
				return fmt.Errorf("%w: Task execution advanced to %s after ACP planning failure settlement", store.ErrConflict, execution.State)
			}
			execution.State = corev1alpha1.TaskExecutionStateFailed
			execution.Outcome = corev1alpha1.TaskExecutionOutcomeFailed
			execution.Reason = reason
			execution.Message = message
			execution.ControllerEpoch = attempt.ControllerEpoch
			execution.LastTransitionTime = &terminalTime
		}
		if latest.Status.Delivery == nil {
			latest.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
				LastTransitionTime: &terminalTime,
			}
		} else if latest.Status.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested ||
			latest.Status.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeNotRequested {
			return fmt.Errorf("%w: ACP planning failure cannot replace delivery state %s", store.ErrConflict, latest.Status.Delivery.State)
		} else {
			latest.Status.Delivery.LastTransitionTime = &terminalTime
		}
		latest.Status.Phase = corev1alpha1.TaskPhaseFailed
		if attempt != nil || latest.Status.CompletionTime == nil {
			latest.Status.CompletionTime = &terminalTime
		}
		latest.Status.Message = message
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type: ConditionTypeWaitingForApproval, Status: metav1.ConditionFalse, LastTransitionTime: terminalTime,
			Reason: "TaskFailed", Message: "task is terminal",
		})
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type: ConditionTypeComplete, Status: metav1.ConditionFalse, LastTransitionTime: terminalTime,
			Reason: "TaskFailed", Message: message,
		})
		return r.Status().Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *TaskReconciler) enqueueACPPlanningFailureProjection(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
	reason corev1alpha1.TaskExecutionReason,
	message string,
) error {
	if task == nil || attempt == nil || r.DurableControlStore == nil || r.ControllerEpochManager == nil {
		return fmt.Errorf("ACP planning failure projection dependencies are incomplete")
	}
	latest := &corev1alpha1.Task{}
	if err := r.taskMetadataReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, latest); err != nil {
		return client.IgnoreNotFound(err)
	}
	if latest.UID != task.UID || latest.Status.Execution == nil || !queuedPromptAttemptMatchesTask(attempt, latest) ||
		attempt.ExecutionState != store.PromptExecutionFailed || attempt.TerminalReason != string(reason) {
		return fmt.Errorf("%w: ACP planning failure projection does not match its Task and PromptAttempt", store.ErrConflict)
	}
	state := latest.Status.Execution.State
	if state != corev1alpha1.TaskExecutionStateQueued && state != corev1alpha1.TaskExecutionStateReserved &&
		(state != corev1alpha1.TaskExecutionStateFailed || latest.Status.Execution.Reason != reason) {
		return fmt.Errorf("%w: Task execution advanced to %s before ACP planning failure projection", store.ErrConflict, state)
	}
	execution := *latest.Status.Execution
	execution.State = corev1alpha1.TaskExecutionStateFailed
	execution.Outcome = corev1alpha1.TaskExecutionOutcomeFailed
	execution.Reason = reason
	execution.Message = message
	execution.ControllerEpoch = attempt.ControllerEpoch
	transitionTime := metav1.NewTime(attempt.UpdatedAt.UTC())
	execution.LastTransitionTime = &transitionTime
	delivery := latest.Status.Delivery
	if delivery == nil {
		delivery = &corev1alpha1.TaskDeliveryStatus{
			State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
			LastTransitionTime: &transitionTime,
		}
	} else if delivery.State != corev1alpha1.TaskDeliveryStateNotRequested ||
		delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeNotRequested {
		return fmt.Errorf("%w: ACP planning failure projection cannot replace delivery state %s", store.ErrConflict, delivery.State)
	} else {
		copyDelivery := *delivery
		copyDelivery.LastTransitionTime = &transitionTime
		delivery = &copyDelivery
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return err
	}
	return enqueueDurableTaskTerminalProjection(ctx, r.DurableControlStore, fence, latest, taskTerminalProjection{
		Namespace: latest.Namespace, Task: latest.Name, TaskUID: string(latest.UID), Attempt: int32(attempt.Key.Attempt),
		Phase: corev1alpha1.TaskPhaseFailed, Message: message, Execution: execution, Delivery: delivery,
	})
}

func (r *TaskReconciler) reservedACPPlanningFailureSafe(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
) (bool, error) {
	if task == nil || task.Status.Execution == nil || attempt == nil ||
		taskExecutionHasRuntimeOrSessionBinding(task.Status.Execution) || promptAttemptHasRuntimeOrSessionBinding(attempt) {
		return false, nil
	}
	poolName := strings.TrimSpace(task.Status.Execution.RuntimePoolName)
	poolUID := strings.TrimSpace(task.Status.Execution.RuntimePoolUID)
	if poolName == "" || poolUID == "" {
		return false, nil
	}
	pool := &corev1alpha1.RuntimePool{}
	if err := r.taskMetadataReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: poolName}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if string(pool.UID) != poolUID {
		return true, nil
	}
	now := time.Now().UTC()
	for i := range pool.Status.Capacity.Reservations {
		reservation := pool.Status.Capacity.Reservations[i]
		if reservation.PoolUID == poolUID && reservation.TaskUID == string(task.UID) &&
			reservation.Attempt == task.Status.Execution.Attempt && reservation.ExpiresAt.After(now) {
			return false, nil
		}
	}
	return true, nil
}

func (r *TaskReconciler) settleACPPlanningFailureAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	reason corev1alpha1.TaskExecutionReason,
	message string,
) (*store.PromptAttempt, error) {
	if task == nil {
		return nil, nil
	}
	latest := &corev1alpha1.Task{}
	if err := r.taskMetadataReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, latest); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if latest.UID != task.UID {
		return nil, fmt.Errorf("%w: Task identity changed while settling ACP planning failure", store.ErrConflict)
	}
	execution := latest.Status.Execution
	if execution == nil {
		return nil, nil
	}
	projectedFailure := execution.State == corev1alpha1.TaskExecutionStateFailed && execution.Reason == reason
	if execution.State != corev1alpha1.TaskExecutionStateQueued &&
		execution.State != corev1alpha1.TaskExecutionStateReserved && !projectedFailure {
		return nil, fmt.Errorf("%w: cannot settle ACP planning failure from Task execution state %s", store.ErrConflict, execution.State)
	}
	if projectedFailure && execution.Attempt == 0 {
		return nil, nil
	}
	attemptID, err := promptAttemptIDFromTask(latest)
	if err != nil {
		return nil, err
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return nil, err
	}
	for range 3 {
		attempt, getErr := r.DurableControlStore.GetPromptAttempt(ctx, attemptID)
		if getErr != nil {
			return nil, getErr
		}
		if !queuedPromptAttemptMatchesTask(attempt, latest) || attempt.DeliveryState != store.PromptDeliveryNotRequested {
			return nil, fmt.Errorf("%w: durable PromptAttempt does not match the Task planning-failure projection", store.ErrConflict)
		}
		if attempt.ExecutionState == store.PromptExecutionFailed && attempt.TerminalReason == string(reason) {
			return attempt, nil
		}
		if attempt.ExecutionState != store.PromptExecutionQueued && attempt.ExecutionState != store.PromptExecutionReserved {
			return nil, fmt.Errorf("%w: durable PromptAttempt advanced to %s before planning failure settlement", store.ErrConflict, attempt.ExecutionState)
		}
		if attempt.ExecutionState == store.PromptExecutionReserved {
			safe, safeErr := r.reservedACPPlanningFailureSafe(ctx, latest, attempt)
			if safeErr != nil {
				return nil, safeErr
			}
			if !safe {
				return nil, fmt.Errorf("%w: reserved ACP attempt still owns live RuntimePool capacity", store.ErrNotReady)
			}
		}
		operationID := store.CanonicalControlID("fail-acp-planning", attempt.ID, fmt.Sprint(attempt.Version), string(reason))
		operationDigest, digestErr := acpDomainDigest("planning-failure-attempt-transition", map[string]any{
			"attemptID": attempt.ID, "requestDigest": attempt.RequestDigest, "from": attempt.ExecutionState,
			"to": store.PromptExecutionFailed, "version": attempt.Version, "reason": reason, "message": message,
		})
		if digestErr != nil {
			return nil, digestErr
		}
		settled, transitionErr := r.DurableControlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: store.PromptExecutionFailed, OperationID: operationID, OperationDigest: operationDigest,
			TerminalReason: string(reason), OutcomeMarker: message, UpdatedAt: time.Now().UTC(),
		})
		if errors.Is(transitionErr, store.ErrConflict) {
			continue
		}
		return settled, transitionErr
	}
	return nil, fmt.Errorf("%w: durable PromptAttempt changed repeatedly during planning failure settlement", store.ErrConflict)
}

//nolint:gocyclo // Workspace preflight intentionally keeps all fail-closed publication gates together.
func validateACPWorkspacePreflight(task *corev1alpha1.Task) error {
	if task == nil {
		return nil
	}
	intent := effectiveACPWorkspaceIntent(task)
	workspace := task.Spec.Workspace
	if workspace == nil {
		if intent == corev1alpha1.WorkspaceIntentWrite {
			return fmt.Errorf("write workspace intent requires Task.spec.workspace")
		}
		return nil
	}
	if strings.TrimSpace(workspace.GitRepo) == "" {
		switch {
		case strings.TrimSpace(workspace.Branch) != "":
			return fmt.Errorf("branch requires gitRepo")
		case strings.TrimSpace(workspace.Ref) != "":
			return fmt.Errorf("ref requires gitRepo")
		case strings.TrimSpace(workspace.SubPath) != "":
			return fmt.Errorf("subPath requires gitRepo")
		case workspace.SourceRepository != nil:
			return fmt.Errorf("sourceRepository requires gitRepo")
		case workspace.ReadCredentialRef != nil:
			return fmt.Errorf("readCredentialRef requires gitRepo")
		}
	} else {
		if _, err := workspaceRepository(workspace); err != nil {
			return err
		}
	}
	// Apply the RuntimeSession relative-root rule here so an unsafe subPath
	// fails preflight before repository resolution and archive preparation
	// spend SCM and artifact work on a Task session creation must reject.
	if err := harnessv2.ValidateWorkspaceRelativeRoot(workspace.SubPath); err != nil {
		return fmt.Errorf("subPath is invalid: %w", err)
	}
	if workspace.MaxChangedFiles != nil && *workspace.MaxChangedFiles <= 0 {
		return fmt.Errorf("maxChangedFiles must be positive when configured")
	}
	for _, pattern := range workspace.AllowedPaths {
		cleaned := strings.TrimPrefix(strings.TrimSpace(pattern), "./")
		if cleaned == "" || len(cleaned) > 1024 || strings.ContainsAny(cleaned, "\\\x00") || strings.HasPrefix(cleaned, "/") || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
			return fmt.Errorf("allowedPaths contains an invalid pattern")
		}
		if _, err := path.Match(cleaned, ""); err != nil {
			return fmt.Errorf("allowedPaths contains an invalid pattern: %w", err)
		}
	}
	if intent != corev1alpha1.WorkspaceIntentWrite {
		switch {
		case strings.TrimSpace(workspace.PublicationGitRepo) != "":
			return fmt.Errorf("publicationGitRepo requires write workspace intent")
		case workspace.PublicationRepository != nil:
			return fmt.Errorf("publicationRepository requires write workspace intent")
		case workspace.PublicationReadCredentialRef != nil:
			return fmt.Errorf("publicationReadCredentialRef requires write workspace intent")
		case workspace.PublicationCredentialRef != nil:
			return fmt.Errorf("publicationCredentialRef requires write workspace intent")
		case workspace.ForgeCredentialRef != nil:
			return fmt.Errorf("forgeCredentialRef requires write workspace intent")
		case strings.TrimSpace(workspace.PushBranch) != "":
			return fmt.Errorf("pushBranch requires write workspace intent")
		case strings.TrimSpace(workspace.PRBaseBranch) != "":
			return fmt.Errorf("prBaseBranch requires write workspace intent")
		case workspace.CreatePR:
			return fmt.Errorf("createPR requires write workspace intent")
		}
		if strings.TrimSpace(workspace.ExpectedRemoteSHA) != "" || workspace.MaxChangedFiles != nil || len(workspace.AllowedPaths) > 0 || workspace.DenyRepositoryControlPaths || workspace.RejectBinaryFiles || workspace.RejectSecretLikeContent {
			return fmt.Errorf("publication expectations and change policies require write workspace intent")
		}
		return nil
	}
	if workspace.PublicationCredentialRef == nil || strings.TrimSpace(workspace.PublicationCredentialRef.Name) == "" {
		return fmt.Errorf("write workspace intent requires publicationCredentialRef before prompt execution")
	}
	if strings.TrimSpace(workspace.GitRepo) == "" {
		return fmt.Errorf("write workspace intent requires a source gitRepo")
	}
	if _, err := workspacePublicationRepository(workspace); err != nil {
		return err
	}
	if pushBranch := strings.TrimSpace(workspace.PushBranch); pushBranch != "" {
		if _, err := canonicalWorkspaceBranchRef(pushBranch); err != nil {
			return fmt.Errorf("pushBranch is invalid: %w", err)
		}
	}
	if workspace.CreatePR && strings.TrimSpace(workspace.PRBaseBranch) == "" {
		return fmt.Errorf("createPR requires prBaseBranch")
	}
	if workspace.CreatePR {
		if _, err := canonicalWorkspaceBranchRef(workspace.PRBaseBranch); err != nil {
			return fmt.Errorf("prBaseBranch is invalid: %w", err)
		}
	}
	if workspace.CreatePR && (workspace.ForgeCredentialRef == nil || strings.TrimSpace(workspace.ForgeCredentialRef.Name) == "") {
		return fmt.Errorf("createPR requires forgeCredentialRef before prompt execution")
	}
	return nil
}

func validateACPRuntimeWorkspaceNamespace(
	plan ACPRuntimePlan,
	taskNamespace, configuredRuntimeNamespace string,
) error {
	if plan.Workspace == nil || plan.Workspace.Provider != corev1alpha1.WorkspaceProviderSubstrate {
		return nil
	}
	runtimeNamespace := strings.TrimSpace(configuredRuntimeNamespace)
	if runtimeNamespace == "" {
		runtimeNamespace = strings.TrimSpace(taskNamespace)
	}
	return validateSubstrateTemplateRuntimeNamespace(plan.Workspace.TemplateNamespace, runtimeNamespace)
}

func (r *TaskReconciler) ensureACPRuntimePool(ctx context.Context, namespace string, plan ACPRuntimePlan) (*corev1alpha1.RuntimePool, error) {
	if err := validateACPRuntimeWorkspaceNamespace(plan, namespace, r.ACPRuntimeNamespace); err != nil {
		return nil, err
	}
	pool := &corev1alpha1.RuntimePool{}
	key := types.NamespacedName{Namespace: namespace, Name: plan.PoolName}
	err := r.Get(ctx, key, pool)
	if apierrors.IsNotFound(err) {
		capacity := &corev1alpha1.RuntimePoolCapacitySpec{
			MaxResidentSessions: corev1alpha1.DefaultRuntimePoolMaxResidentSessions,
			MaxRunningPrompts:   corev1alpha1.DefaultRuntimePoolMaxRunningPrompts,
		}
		labels := map[string]string{
			acpRuntimePoolLabel: "true", acpRuntimeTrustLabel: namespace,
			acpRuntimeProfileLabel: strings.TrimPrefix(string(plan.Digest), "sha256:")[:16],
		}
		var executionWorkspace *corev1alpha1.RuntimePoolExecutionWorkspaceSpec
		if plan.Workspace != nil {
			// A workspace-backed pool hosts exactly one logical RuntimeSession
			// inside one provider-owned physical workspace.
			capacity = &corev1alpha1.RuntimePoolCapacitySpec{MaxResidentSessions: 1, MaxRunningPrompts: 1}
			labels[acpRuntimeWorkspaceProviderLabel] = string(plan.Workspace.Provider)
			executionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
				Provider:      plan.Workspace.Provider,
				BindingDigest: plan.Workspace.BindingDigest,
			}
			if plan.Workspace.Provider == corev1alpha1.WorkspaceProviderSubstrate {
				executionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
					BaseTemplateNamespace: plan.Workspace.TemplateNamespace,
					BaseTemplateName:      plan.Workspace.TemplateName,
				}
			}
		}
		pool = &corev1alpha1.RuntimePool{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace, Name: plan.PoolName,
				Annotations: map[string]string{acpRuntimeLastDemandAnnotation: time.Now().UTC().Format(time.RFC3339Nano)},
				Labels:      labels,
			},
			Spec: corev1alpha1.RuntimePoolSpec{
				TrustDomain:             corev1alpha1.RuntimePoolTrustDomain{Namespace: namespace, Identity: "namespace:" + namespace},
				RuntimeNamespace:        strings.TrimSpace(r.ACPRuntimeNamespace),
				Runtime:                 corev1alpha1.RuntimePoolRuntimeSpec{Image: plan.Image, Profile: RuntimePoolProfileFromPlan(plan)},
				ExecutionWorkspace:      executionWorkspace,
				DesiredReplicas:         1,
				Capacity:                capacity,
				ColdStartTimeoutSeconds: corev1alpha1.DefaultRuntimePoolColdStartTimeoutSeconds,
			},
		}
		if err := r.Create(ctx, pool); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("create RuntimePool: %w", err)
			}
			if err := r.Get(ctx, key, pool); err != nil {
				return nil, err
			}
		}
		return pool, nil
	}
	if err != nil {
		return nil, err
	}
	if pool.Spec.Runtime.Image != plan.Image || pool.Spec.Runtime.Profile.Digest != string(plan.Digest) {
		return nil, fmt.Errorf("RuntimePool %s profile does not match queued Task", pool.Name)
	}
	if !acpRuntimePoolWorkspaceMatchesPlan(pool, plan) {
		return nil, fmt.Errorf("RuntimePool %s execution workspace binding does not match queued Task", pool.Name)
	}
	base := pool.DeepCopy()
	changed := false
	if pool.Spec.DesiredReplicas == 0 {
		pool.Spec.DesiredReplicas = 1
		changed = true
	}
	if pool.Annotations == nil {
		pool.Annotations = make(map[string]string)
	}
	lastDemand, _ := time.Parse(time.RFC3339Nano, pool.Annotations[acpRuntimeLastDemandAnnotation])
	if time.Since(lastDemand) >= time.Minute {
		pool.Annotations[acpRuntimeLastDemandAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		changed = true
	}
	if changed {
		if err := r.Patch(ctx, pool, client.MergeFrom(base)); err != nil {
			return nil, err
		}
	}
	return pool, nil
}

func acpBoundTaskRequestDigest(bound *verifiedAgentExecution, attempt int32, promptID string) (string, error) {
	if bound == nil || bound.binding == nil || bound.snapshot == nil {
		return "", errors.New("verified binding and execution snapshot are required for ACP request identity")
	}
	return acpDomainDigest("task-request", map[string]any{
		"taskUID": bound.binding.Task.UID, "taskGeneration": bound.binding.Task.BoundSpecGeneration,
		"attempt": attempt, "promptID": promptID, "prompt": bound.body.Prompt,
		"agentUID": bound.body.Agent.UID, "agentGeneration": bound.body.Agent.Generation,
		"agentConfiguration":   bound.configuration,
		"runtimeProfileDigest": bound.body.ProfileDigest, "workspace": bound.body.Workspace,
		"bindingDigest": bound.binding.BindingDigest, "snapshotDigest": bound.snapshot.Digest,
	})
}

func acpQueuedTaskRequestMatchesBinding(bound *verifiedAgentExecution, status *corev1alpha1.TaskExecutionStatus) (bool, error) {
	if bound == nil || status == nil || status.Attempt < 1 || strings.TrimSpace(status.PromptID) == "" ||
		strings.TrimSpace(status.RequestDigest) == "" {
		return false, nil
	}
	digest, err := acpBoundTaskRequestDigest(bound, status.Attempt, status.PromptID)
	if err != nil {
		return false, err
	}
	return digest == status.RequestDigest, nil
}

func taskExecutionStateTerminal(state corev1alpha1.TaskExecutionState) bool {
	switch state {
	case corev1alpha1.TaskExecutionStateSucceeded, corev1alpha1.TaskExecutionStateFailed,
		corev1alpha1.TaskExecutionStateCancelled, corev1alpha1.TaskExecutionStateOutcomeUnknown:
		return true
	default:
		return false
	}
}

func taskManagedByACP(task *corev1alpha1.Task) bool {
	return task != nil && task.Spec.Type == corev1alpha1.TaskTypeAgent && task.Status.Execution != nil &&
		(strings.TrimSpace(task.Status.Execution.RuntimePoolName) != "" || strings.TrimSpace(task.Status.Execution.AgentRuntimeName) != "")
}
