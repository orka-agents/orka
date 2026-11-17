package controller

import (
	"context"
	"errors"
	"strconv"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A feedback repair's outcome record is the one source of truth for what
// the repair did; see ADR 0052. Its states move reserved → creating →
// started → one terminal state, and every write that decides a repair's end
// goes through it first. The repair job, the work action, and the pull
// request item are projections of the outcome: a terminal outcome that is not
// marked projected has its projections replayed until they record its end.
const (
	// repositoryMonitorRepairTaskMissing fails a started feedback repair
	// whose Task disappeared before its result was recorded.
	repositoryMonitorRepairTaskMissing = "repair_task_missing"
	// repositoryMonitorRepairTaskOutcomeUnknown fails a creating feedback
	// repair whose Task is missing: it may have been created and run, so it
	// keeps every admitted charge and is not run again.
	repositoryMonitorRepairTaskOutcomeUnknown = "repair_task_outcome_unknown"
	// repositoryMonitorFeedbackProjectionBatch bounds the terminal outcomes
	// one reconcile projects.
	repositoryMonitorFeedbackProjectionBatch = 100
)

// repositoryMonitorUncachedReader reads from the API server, not the cache.
// A cached read can lag a create, so only this reader may confirm that a
// Task is absent before an outcome is finalized or a charge released.
func (r *RepositoryMonitorReconciler) repositoryMonitorUncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// repositoryMonitorRepairTaskExists reports whether the named repair Task
// exists. The cache answers when it has the Task; an absence it reports is
// confirmed with an uncached read.
func (r *RepositoryMonitorReconciler) repositoryMonitorRepairTaskExists(ctx context.Context, namespace, name string) (bool, *corev1alpha1.Task, error) {
	if name == "" || r.Client == nil {
		return false, nil, nil
	}
	var task corev1alpha1.Task
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &task)
	if err == nil {
		return true, &task, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, nil, err
	}
	if err := r.repositoryMonitorUncachedReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &task); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, &task, nil
}

// startRepositoryMonitorFeedbackOutcome records that a feedback repair's
// Task exists, with the targets its prompt froze. Without them, every
// admitted ID is charged, so the budget fails closed.
func (r *RepositoryMonitorReconciler) startRepositoryMonitorFeedbackOutcome(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, commandID string, targets []string, taskName string) (*store.MonitorFeedbackOutcome, error) {
	outcome, err := r.repositoryMonitorFeedbackOutcome(ctx, monitor, commandID)
	if err != nil || outcome == nil {
		return outcome, err
	}
	if len(targets) == 0 {
		targets = outcome.AdmittedIDs
	}
	return r.Store.StartMonitorFeedbackRepair(ctx, store.StartMonitorFeedbackRepairRequest{Namespace: monitor.Namespace, CommandEventID: commandID, Targets: targets, TaskName: taskName})
}

// startRepositoryMonitorFeedbackOutcomeForTask starts a feedback repair's
// outcome once its Task exists, with the targets its input froze.
func (r *RepositoryMonitorReconciler) startRepositoryMonitorFeedbackOutcomeForTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, input *repositoryMonitorRepairFeedbackInput, taskName string) error {
	if !repositoryMonitorFeedbackCommand(command) {
		return nil
	}
	var targets []string
	if input != nil {
		targets = input.Targets
	}
	_, err := r.startRepositoryMonitorFeedbackOutcome(ctx, monitor, command.ID, targets, taskName)
	return err
}

// prepareRepositoryMonitorFeedbackTask records, before a feedback repair's
// Task create is requested, that the Task may exist from now on.
func (r *RepositoryMonitorReconciler) prepareRepositoryMonitorFeedbackTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, taskName string) error {
	if !repositoryMonitorFeedbackCommand(command) {
		return nil
	}
	_, err := r.Store.PrepareMonitorFeedbackRepairTask(ctx, store.PrepareMonitorFeedbackRepairTaskRequest{Namespace: monitor.Namespace, CommandEventID: command.ID, TaskName: taskName})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// releaseRepositoryMonitorFeedbackTask returns a feedback repair to
// reserved after the API server definitely refused its Task create, so the
// refused attempt charges nothing. A create failure that does not rule out a
// created Task leaves the outcome creating.
func (r *RepositoryMonitorReconciler) releaseRepositoryMonitorFeedbackTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, createErr error) error {
	if !repositoryMonitorFeedbackCommand(command) || !repositoryMonitorTaskCreateRejected(createErr) {
		return nil
	}
	_, err := r.Store.ReleaseMonitorFeedbackRepairTask(ctx, store.ReleaseMonitorFeedbackRepairTaskRequest{Namespace: monitor.Namespace, CommandEventID: command.ID})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// repositoryMonitorTaskCreateRejected reports whether the API server
// rejected a Task create before storing it: a request it refused as
// invalid, unauthorized, or throttled. Other failures, such as a timeout or
// an internal error, do not rule out that the Task was created.
func repositoryMonitorTaskCreateRejected(err error) bool {
	return apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err) || apierrors.IsRequestEntityTooLargeError(err)
}

// finishRepositoryMonitorFeedbackOutcome records how a feedback repair
// ended, unless it already ended: the first terminal state is final. A
// creating outcome whose repair Task exists starts first, so the Task's
// targets are charged; one whose Task is gone keeps every admitted charge.
// A repair whose Task may have existed was not withdrawn, and one that never
// started neither succeeded nor made no change, so those record a failure.
func (r *RepositoryMonitorReconciler) finishRepositoryMonitorFeedbackOutcome(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, commandID, state, reason, resultSHA string) error {
	outcome, err := r.repositoryMonitorFeedbackOutcome(ctx, monitor, commandID)
	if err != nil || outcome == nil || store.IsMonitorFeedbackOutcomeTerminal(outcome.State) {
		return err
	}
	if outcome.State == store.MonitorFeedbackOutcomeCreating {
		if outcome, err = r.recoverRepositoryMonitorFeedbackStart(ctx, monitor, outcome); err != nil {
			return err
		}
	}
	switch {
	case state == store.MonitorFeedbackOutcomeWithdrawn && outcome.State != store.MonitorFeedbackOutcomeReserved,
		(state == store.MonitorFeedbackOutcomeSucceeded || state == store.MonitorFeedbackOutcomeNoChange) && outcome.State != store.MonitorFeedbackOutcomeStarted:
		state, resultSHA = store.MonitorFeedbackOutcomeFailed, ""
	}
	_, err = r.Store.FinishMonitorFeedbackRepair(ctx, store.FinishMonitorFeedbackRepairRequest{Namespace: monitor.Namespace, CommandEventID: commandID, State: state, Reason: reason, ResultSHA: resultSHA})
	return err
}

// recoverRepositoryMonitorFeedbackStart starts a creating outcome whose
// repair Task exists, with the targets the Task's prompt froze, and returns
// the outcome as it now is.
func (r *RepositoryMonitorReconciler) recoverRepositoryMonitorFeedbackStart(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, outcome *store.MonitorFeedbackOutcome) (*store.MonitorFeedbackOutcome, error) {
	exists, task, err := r.repositoryMonitorRepairTaskExists(ctx, monitor.Namespace, outcome.TaskName)
	if err != nil || !exists {
		return outcome, err
	}
	var targets []string
	if input, ok := repositoryMonitorRepairFeedbackInputFromPrompt(task.Spec.Prompt, outcome.HeadSHA); ok {
		targets = input.Targets
	}
	started, err := r.startRepositoryMonitorFeedbackOutcome(ctx, monitor, outcome.CommandEventID, targets, task.Name)
	if err != nil || started == nil {
		return outcome, err
	}
	return started, nil
}

// endRepositoryMonitorFeedbackOutcomeFromTask records how a feedback
// repair's Task ended, with the head it pushed. An outcome that is still
// reserved or creating, because the controller stopped between creating the
// Task and starting the outcome, starts first with the targets the Task's
// prompt froze.
func (r *RepositoryMonitorReconciler) endRepositoryMonitorFeedbackOutcomeFromTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, outcome *store.MonitorFeedbackOutcome, task *corev1alpha1.Task, headSHA, state, reason, resultSHA string) error {
	if outcome == nil {
		return nil
	}
	if outcome.State == store.MonitorFeedbackOutcomeReserved || outcome.State == store.MonitorFeedbackOutcomeCreating {
		var targets []string
		if input, ok := repositoryMonitorRepairFeedbackInputFromPrompt(task.Spec.Prompt, headSHA); ok {
			targets = input.Targets
		}
		if _, err := r.startRepositoryMonitorFeedbackOutcome(ctx, monitor, outcome.CommandEventID, targets, task.Name); err != nil {
			return err
		}
	}
	return r.finishRepositoryMonitorFeedbackOutcome(ctx, monitor, outcome.CommandEventID, state, reason, resultSHA)
}

// settleRepositoryMonitorFeedbackRepair settles a feedback repair's handoff
// from its outcome, and reports whether it did. A reserved outcome, or one
// whose Task exists, leaves the handoff to continue. An outcome that ended
// has its end projected again. A creating outcome whose Task is gone may
// have run, so it fails and keeps every admitted charge; a started one whose
// Task is gone ran, so it fails and keeps its targets' charges. Neither runs
// again.
func (r *RepositoryMonitorReconciler) settleRepositoryMonitorFeedbackRepair(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, taskExists bool) (bool, error) {
	if !repositoryMonitorFeedbackCommand(command) {
		return false, nil
	}
	outcome, err := r.repositoryMonitorFeedbackOutcome(ctx, monitor, command.ID)
	if err != nil || outcome == nil || outcome.State == store.MonitorFeedbackOutcomeReserved {
		return false, err
	}
	if !store.IsMonitorFeedbackOutcomeTerminal(outcome.State) {
		if taskExists {
			return false, nil
		}
		reason := repositoryMonitorRepairTaskMissing
		if outcome.State == store.MonitorFeedbackOutcomeCreating {
			reason = repositoryMonitorRepairTaskOutcomeUnknown
		}
		if err := r.finishRepositoryMonitorFeedbackOutcome(ctx, monitor, command.ID, store.MonitorFeedbackOutcomeFailed, reason, ""); err != nil {
			return true, err
		}
		if outcome, err = r.repositoryMonitorFeedbackOutcome(ctx, monitor, command.ID); err != nil || outcome == nil {
			return true, err
		}
	}
	return true, r.projectRepositoryMonitorFeedbackOutcome(ctx, monitor, *outcome)
}

// failRepositoryMonitorFeedbackRepairWithMissingTask fails the creating or
// started feedback repair of a queued repair job whose Task an uncached read
// confirms is gone before its result was recorded. The projection pass then
// records the failure on the job, the action, and the item.
func (r *RepositoryMonitorReconciler) failRepositoryMonitorFeedbackRepairWithMissingTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, job *store.RepairJob) (bool, error) {
	exists, _, err := r.repositoryMonitorRepairTaskExists(ctx, monitor.Namespace, job.TaskName)
	if err != nil || exists {
		return false, err
	}
	outcomes, err := r.repositoryMonitorFeedbackOutcomes(ctx, monitor, "", job.PRNumber)
	if err != nil {
		return false, err
	}
	for commandID, outcome := range outcomes {
		if outcome.TaskName != job.TaskName {
			continue
		}
		reason := ""
		switch outcome.State {
		case store.MonitorFeedbackOutcomeStarted:
			reason = repositoryMonitorRepairTaskMissing
		case store.MonitorFeedbackOutcomeCreating:
			reason = repositoryMonitorRepairTaskOutcomeUnknown
		default:
			continue
		}
		return true, r.finishRepositoryMonitorFeedbackOutcome(ctx, monitor, commandID, store.MonitorFeedbackOutcomeFailed, reason, "")
	}
	return false, nil
}

// settleRepositoryMonitorFeedbackOutcomes projects the monitor's terminal
// feedback outcomes that are not marked projected, and reports whether it
// projected any. It runs on every repair ingestion, so a controller that
// stopped between recording an outcome and its projections, even one whose
// Task is gone, records them on a later reconcile.
func (r *RepositoryMonitorReconciler) settleRepositoryMonitorFeedbackOutcomes(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (bool, error) {
	outcomes, _, err := r.Store.ListMonitorFeedbackOutcomes(ctx, store.MonitorFeedbackOutcomeFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Unprojected: true, Limit: repositoryMonitorFeedbackProjectionBatch})
	if err != nil {
		return false, err
	}
	for _, outcome := range outcomes {
		if err := r.projectRepositoryMonitorFeedbackOutcome(ctx, monitor, outcome); err != nil {
			return false, err
		}
	}
	return len(outcomes) > 0, nil
}

// projectRepositoryMonitorFeedbackOutcome makes a terminal outcome's
// projections record its end, then marks it projected. Each write is
// idempotent and only advances a projection that has not ended: a queued
// repair job takes the outcome's end and result head, its push mutation and
// work action end, and the pull request item's repair state is derived for
// its head again.
func (r *RepositoryMonitorReconciler) projectRepositoryMonitorFeedbackOutcome(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, outcome store.MonitorFeedbackOutcome) error {
	if !store.IsMonitorFeedbackOutcomeTerminal(outcome.State) {
		return nil
	}
	ran := outcome.StartedAt != nil || outcome.TaskCreateAt != nil
	succeeded := outcome.State == store.MonitorFeedbackOutcomeSucceeded || outcome.State == store.MonitorFeedbackOutcomeNoChange
	jobPhase := repositoryMonitorRepairPhaseFailed
	if succeeded {
		jobPhase = repositoryMonitorRepairPhaseSucceeded
	}
	if ran {
		if err := r.projectRepositoryMonitorFeedbackJob(ctx, monitor, outcome, jobPhase); err != nil {
			return err
		}
	}
	command := &store.CommandEvent{ID: outcome.CommandEventID, Intent: repositoryMonitorCommandIntentFix, Source: repositoryMonitorControllerPolicySource, Permission: repositoryMonitorFeedbackCommandPermission}
	actionKind := repositoryMonitorRepairWorkflowActionKind(command.Intent)
	action, err := r.Store.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForIntent(command.Intent)))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err != nil || store.IsRepositoryMonitorWorkActionActive(action.Status) {
		status, phase := repositoryMonitorWorkActionStatusFailed, jobPhase
		switch {
		case outcome.State == store.MonitorFeedbackOutcomeWithdrawn:
			status, phase = repositoryMonitorWorkActionStatusBlocked, "repair_withdrawn"
		case !ran:
			status, phase = repositoryMonitorWorkActionStatusBlocked, "repair_blocked"
		case succeeded:
			status = repositoryMonitorWorkActionStatusSucceeded
		}
		if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, nil, command, repositoryMonitorPullRequestKind, outcome.PRNumber, outcome.HeadSHA, "", actionKind, status, phase, outcome.TaskName, outcome.Reason); err != nil {
			return err
		}
	}
	item, err := r.Store.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, strconv.FormatInt(outcome.PRNumber, 10))
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	case item.HeadSHA == outcome.HeadSHA || outcome.ResultSHA != "" && item.HeadSHA == outcome.ResultSHA:
		state, err := r.repositoryMonitorRepairStateForHead(ctx, monitor, outcome.PRNumber, item.HeadSHA)
		if err != nil {
			return err
		}
		// A pushed repair resets the review of the head it replaced, once:
		// after the reset the item no longer records that head as reviewed.
		resetReview := outcome.ResultSHA != "" && outcome.ResultSHA != outcome.HeadSHA && item.LastReviewedHeadSHA == outcome.HeadSHA
		if state != item.RepairState || resetReview {
			item.RepairState = state
			if resetReview {
				repositoryMonitorResetItemAfterRepairPush(item)
			}
			if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
				return err
			}
		}
	}
	_, err = r.Store.MarkMonitorFeedbackOutcomeProjected(ctx, store.MarkMonitorFeedbackOutcomeProjectedRequest{Namespace: monitor.Namespace, CommandEventID: outcome.CommandEventID})
	return err
}

// projectRepositoryMonitorFeedbackJob ends the repair job and its push
// mutation for a terminal outcome whose Task may have run. Each is advanced
// on its own only while it has not ended, so a replay after an interrupted
// projection finishes whichever is still open.
func (r *RepositoryMonitorReconciler) projectRepositoryMonitorFeedbackJob(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, outcome store.MonitorFeedbackOutcome, phase string) error {
	jobID := "repair-" + repositoryMonitorShortHash(outcome.CommandEventID)
	lastError := outcome.Reason
	if phase == repositoryMonitorRepairPhaseSucceeded {
		lastError = ""
	}
	job, err := r.Store.GetRepairJob(ctx, monitor.Namespace, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	case job.Phase == repositoryMonitorRepairPhaseQueued:
		job.Phase, job.LastError, job.PushedSHA, job.CompletedAt = phase, lastError, outcome.ResultSHA, outcome.CompletedAt
		if err := r.Store.UpdateRepairJob(ctx, job); err != nil {
			return err
		}
	}
	mutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, "ghmut-"+repositoryMonitorShortHash(jobID+"-push"))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || mutation.Status == repositoryMonitorRunPhaseSucceeded || mutation.Status == repositoryMonitorRunPhaseFailed {
		return err
	}
	mutation.CommandEventID, mutation.ExternalID, mutation.Error = outcome.CommandEventID, outcome.ResultSHA, lastError
	mutation.Status = repositoryMonitorRunPhaseFailed
	if phase == repositoryMonitorRepairPhaseSucceeded {
		mutation.Status = repositoryMonitorRunPhaseSucceeded
	}
	return r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation)
}
