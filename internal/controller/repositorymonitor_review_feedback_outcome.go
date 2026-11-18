package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// A feedback repair's outcome record is the one source of truth for what
// the repair did; see ADR 0052. Its states move reserved → creating →
// started → one terminal state, and every write that decides a repair's end
// goes through it first. The repair job, the work action, and the pull
// request item are projections of the outcome: a terminal outcome that is not
// marked projected has its projections replayed until they record its end.
const (
	// repositoryMonitorRepairPhaseWithdrawn is the phase of a withdrawn
	// feedback repair's job, which readiness and validation ignore.
	repositoryMonitorRepairPhaseWithdrawn = "withdrawn"
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

// errRepositoryMonitorFeedbackRepairTaskGone reports that a feedback
// repair's Task create was already requested and the Task is gone, so the
// repair is settled rather than its Task created again.
var errRepositoryMonitorFeedbackRepairTaskGone = errors.New("the feedback repair's requested Task is gone")

// prepareRepositoryMonitorFeedbackTask records, before a feedback repair's
// Task create is requested, that the Task may exist from now on, and reports
// whether an earlier attempt already requested it: an outcome already
// creating or started, whose Task must only ever be adopted, never created
// again, since it may have run and been deleted.
func (r *RepositoryMonitorReconciler) prepareRepositoryMonitorFeedbackTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, taskName string) (bool, error) {
	if !repositoryMonitorFeedbackCommand(command) {
		return false, nil
	}
	outcome, err := r.repositoryMonitorFeedbackOutcome(ctx, monitor, command.ID)
	if err != nil || outcome == nil {
		return false, err
	}
	if outcome.State != store.MonitorFeedbackOutcomeReserved {
		return true, nil
	}
	_, err = r.Store.PrepareMonitorFeedbackRepairTask(ctx, store.PrepareMonitorFeedbackRepairTaskRequest{Namespace: monitor.Namespace, CommandEventID: command.ID, TaskName: taskName})
	return false, err
}

// createOrAdoptRepositoryMonitorRepairTask creates a repair's Task, or
// adopts the one an earlier attempt created, and returns it with the number
// it created. A feedback repair's outcome turns creating first, and only that
// first request creates the Task. A feedback repair whose Task an earlier
// attempt requested, creating or started, only adopts it, read uncached: if
// it is gone it may have run, so errRepositoryMonitorFeedbackRepairTaskGone
// settles the repair instead of creating its Task again.
func (r *RepositoryMonitorReconciler) createOrAdoptRepositoryMonitorRepairTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, task *corev1alpha1.Task, job *store.RepairJob) (*corev1alpha1.Task, int, error) {
	requested, err := r.prepareRepositoryMonitorFeedbackTask(ctx, monitor, command, task.Name)
	if err != nil {
		return nil, 0, err
	}
	if !requested {
		err = r.Create(ctx, task)
		if err == nil {
			return task, 1, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			if releaseErr := r.releaseRepositoryMonitorFeedbackTask(ctx, monitor, command, err); releaseErr != nil {
				return nil, 0, releaseErr
			}
			job.Phase = repositoryMonitorRepairPhaseQueued
			job.LastError = repositoryMonitorRepairTaskCreateError
			job.CompletedAt = nil
			_ = r.Store.UpdateRepairJob(ctx, job)
			return nil, 0, err
		}
	}
	// The cache may not have the Task the create just found, or may still
	// hold one that is gone.
	var existing corev1alpha1.Task
	if err := r.repositoryMonitorUncachedReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, &existing); err != nil {
		if requested && apierrors.IsNotFound(err) {
			return nil, 0, errRepositoryMonitorFeedbackRepairTaskGone
		}
		return nil, 0, err
	}
	if err := validateRepositoryMonitorRecoveredIssueActionTask(monitor, task, &existing); err != nil {
		return nil, 0, err
	}
	return &existing, 0, nil
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

// settleRepositoryMonitorFeedbackJobWithoutTask settles the feedback repair
// of a queued repair job whose Task an uncached read confirms is gone, and
// reports whether the job belongs to a feedback repair. A creating repair
// may have run, so it fails keeping every admitted charge; a started one ran
// and fails with repair_task_missing; and every feedback repair that has
// ended, including one that never had a Task, is projected, which ends its
// job so it is no longer queued.
func (r *RepositoryMonitorReconciler) settleRepositoryMonitorFeedbackJobWithoutTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, job *store.RepairJob) (bool, error) {
	exists, _, err := r.repositoryMonitorRepairTaskExists(ctx, monitor.Namespace, job.TaskName)
	if err != nil || exists {
		return false, err
	}
	outcomes, err := r.repositoryMonitorFeedbackOutcomes(ctx, monitor, "", job.PRNumber)
	if err != nil {
		return false, err
	}
	for commandID := range outcomes {
		if "repair-"+repositoryMonitorShortHash(commandID) != job.ID {
			continue
		}
		outcome := outcomes[commandID]
		switch outcome.State {
		case store.MonitorFeedbackOutcomeReserved:
			// Its handoff has not requested the Task yet.
			return true, nil
		case store.MonitorFeedbackOutcomeStarted, store.MonitorFeedbackOutcomeCreating:
			reason := repositoryMonitorRepairTaskMissing
			if outcome.State == store.MonitorFeedbackOutcomeCreating {
				reason = repositoryMonitorRepairTaskOutcomeUnknown
			}
			if err := r.finishRepositoryMonitorFeedbackOutcome(ctx, monitor, commandID, store.MonitorFeedbackOutcomeFailed, reason, ""); err != nil {
				return true, err
			}
		}
		stored, err := r.repositoryMonitorFeedbackOutcome(ctx, monitor, commandID)
		if err != nil || stored == nil {
			return true, err
		}
		return true, r.projectRepositoryMonitorFeedbackOutcome(ctx, monitor, *stored)
	}
	return false, nil
}

// recordRepositoryMonitorFeedbackTaskEnd records how a feedback repair's
// Task ended, unless its outcome already ended, and then projects the stored
// outcome: the Task's own result never overrides an outcome that ended
// first, such as a refusal after the Task started.
func (r *RepositoryMonitorReconciler) recordRepositoryMonitorFeedbackTaskEnd(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, outcome *store.MonitorFeedbackOutcome, task *corev1alpha1.Task, headSHA, state, reason, resultSHA string) error {
	if err := r.endRepositoryMonitorFeedbackOutcomeFromTask(ctx, monitor, outcome, task, headSHA, state, reason, resultSHA); err != nil {
		return err
	}
	stored, err := r.repositoryMonitorFeedbackOutcome(ctx, monitor, outcome.CommandEventID)
	if err != nil || stored == nil {
		return err
	}
	return r.projectRepositoryMonitorFeedbackOutcome(ctx, monitor, *stored)
}

// settleRepositoryMonitorFeedbackOutcomes projects the monitor's terminal
// feedback outcomes that are not marked projected, and reports whether it
// projected any. It runs on every repair ingestion, so a controller that
// stopped between recording an outcome and its projections, even one whose
// Task is gone, records them on a later reconcile. Each pass resumes where
// the last stopped and wraps around, so outcomes whose projection keeps
// failing cannot keep the others from being projected.
func (r *RepositoryMonitorReconciler) settleRepositoryMonitorFeedbackOutcomes(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (bool, error) {
	cursor := ""
	if value, ok := r.feedbackProjectCursors.Load(monitor.UID); ok {
		cursor = value.(string)
	}
	outcomes, next, err := r.Store.ListMonitorFeedbackOutcomes(ctx, store.MonitorFeedbackOutcomeFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Unprojected: true, Limit: repositoryMonitorFeedbackProjectionBatch, Cursor: cursor})
	if err != nil {
		// A cursor the store no longer accepts starts over.
		r.feedbackProjectCursors.Delete(monitor.UID)
		return false, err
	}
	r.feedbackProjectCursors.Store(monitor.UID, next)
	// One outcome's failure does not hold up the others.
	var errs []error
	for _, outcome := range outcomes {
		if err := r.projectRepositoryMonitorFeedbackOutcome(ctx, monitor, outcome); err != nil {
			log.FromContext(ctx).Error(err, "failed to project feedback repair outcome", "commandEvent", outcome.CommandEventID)
			errs = append(errs, err)
		}
	}
	return len(outcomes) > 0, errors.Join(errs...)
}

// repositoryMonitorFeedbackProjection is what a terminal outcome's
// projections must record.
type repositoryMonitorFeedbackProjection struct {
	// jobPhase is the repair job's phase. A withdrawn repair's job is
	// withdrawn, which readiness ignores, so a withdrawal reads as if no
	// repair had been requested.
	jobPhase string
	// mutationStatus is the push mutation's status.
	mutationStatus string
	// actionStatus and actionPhase are the work action's.
	actionStatus, actionPhase string
}

// repositoryMonitorFeedbackProjectionFor derives every projection of a
// terminal outcome from the outcome alone.
func repositoryMonitorFeedbackProjectionFor(outcome store.MonitorFeedbackOutcome) repositoryMonitorFeedbackProjection {
	ran := outcome.StartedAt != nil || outcome.TaskCreateAt != nil
	switch {
	case outcome.State == store.MonitorFeedbackOutcomeSucceeded || outcome.State == store.MonitorFeedbackOutcomeNoChange:
		return repositoryMonitorFeedbackProjection{jobPhase: repositoryMonitorRepairPhaseSucceeded, mutationStatus: repositoryMonitorRunPhaseSucceeded,
			actionStatus: repositoryMonitorWorkActionStatusSucceeded, actionPhase: repositoryMonitorRepairPhaseSucceeded}
	case outcome.State == store.MonitorFeedbackOutcomeWithdrawn:
		return repositoryMonitorFeedbackProjection{jobPhase: repositoryMonitorRepairPhaseWithdrawn, mutationStatus: repositoryMonitorRunPhaseFailed,
			actionStatus: repositoryMonitorWorkActionStatusBlocked, actionPhase: "repair_withdrawn"}
	case !ran:
		return repositoryMonitorFeedbackProjection{jobPhase: repositoryMonitorRepairPhaseFailed, mutationStatus: repositoryMonitorRunPhaseFailed,
			actionStatus: repositoryMonitorWorkActionStatusBlocked, actionPhase: "repair_blocked"}
	}
	return repositoryMonitorFeedbackProjection{jobPhase: repositoryMonitorRepairPhaseFailed, mutationStatus: repositoryMonitorRunPhaseFailed,
		actionStatus: repositoryMonitorWorkActionStatusFailed, actionPhase: repositoryMonitorRepairPhaseFailed}
}

// projectRepositoryMonitorFeedbackOutcome makes every projection of a
// terminal outcome record what the stored outcome says, then marks it
// projected. Projections always follow the stored outcome: a repair job,
// push mutation, or work action that records anything else, whether it has
// not ended or ended differently, is corrected, and the pull request item's
// repair state is derived for its head again. Each write is idempotent, so
// replaying a projection is safe, and the outcome is only marked projected
// once none contradicts it.
func (r *RepositoryMonitorReconciler) projectRepositoryMonitorFeedbackOutcome(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, outcome store.MonitorFeedbackOutcome) error {
	if !store.IsMonitorFeedbackOutcomeTerminal(outcome.State) {
		return nil
	}
	ctx = context.WithValue(ctx, repositoryMonitorFeedbackProjectingKey{}, true)
	want := repositoryMonitorFeedbackProjectionFor(outcome)
	if err := r.projectRepositoryMonitorFeedbackJob(ctx, monitor, outcome, want); err != nil {
		return err
	}
	command := &store.CommandEvent{ID: outcome.CommandEventID, Intent: repositoryMonitorCommandIntentFix, Source: repositoryMonitorControllerPolicySource, Permission: repositoryMonitorFeedbackCommandPermission}
	action, err := r.Store.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForIntent(command.Intent)))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err != nil || !repositoryMonitorFeedbackActionAgrees(action, want) {
		if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, nil, command, repositoryMonitorPullRequestKind, outcome.PRNumber, outcome.HeadSHA, "", repositoryMonitorRepairWorkflowActionKind(command.Intent), want.actionStatus, want.actionPhase, outcome.TaskName, outcome.Reason); err != nil {
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
	// A write the workflow refused, or one a concurrent writer undid, must
	// never count as projected: the records are read back first, and the
	// work action this projection wrote must exist.
	contradicted, err := r.repositoryMonitorFeedbackProjectionContradicted(ctx, monitor, outcome, true)
	if err != nil {
		return err
	}
	if contradicted {
		return fmt.Errorf("%w: feedback repair %s", errRepositoryMonitorFeedbackProjectionContradicted, outcome.CommandEventID)
	}
	_, err = r.Store.MarkMonitorFeedbackOutcomeProjected(ctx, store.MarkMonitorFeedbackOutcomeProjectedRequest{Namespace: monitor.Namespace, CommandEventID: outcome.CommandEventID})
	return err
}

// errRepositoryMonitorFeedbackProjectionContradicted reports a projection
// whose records, read back, still contradict the stored outcome.
var errRepositoryMonitorFeedbackProjectionContradicted = errors.New("the projection does not record the stored outcome")

// cancelRepositoryMonitorPullRequestWorkActions cancels a pull request's
// active work actions, as CancelWorkActions does, and then projects again
// every feedback repair whose outcome has ended: the bulk cancel cannot tell
// such a repair's action from an active one, so it may have cancelled an
// action its stored outcome records otherwise.
func (r *RepositoryMonitorReconciler) cancelRepositoryMonitorPullRequestWorkActions(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, number int64, reason, exceptID string) error {
	_, err := r.Store.CancelWorkActions(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, number, reason, exceptID)
	if err != nil {
		return err
	}
	outcomes, err := r.repositoryMonitorFeedbackOutcomes(ctx, monitor, "", number)
	if err != nil {
		return err
	}
	var errs []error
	for _, outcome := range outcomes {
		if !store.IsMonitorFeedbackOutcomeTerminal(outcome.State) {
			continue
		}
		contradicted, err := r.repositoryMonitorFeedbackProjectionContradicted(ctx, monitor, outcome, false)
		if err == nil && contradicted {
			err = r.projectRepositoryMonitorFeedbackOutcome(ctx, monitor, outcome)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

type repositoryMonitorFeedbackProjectingKey struct{}

// repositoryMonitorFeedbackActionAgrees reports whether a work action
// records what want says. A cancelled action is final and records a stop,
// which every outcome that is not a success agrees with; a coalesced one
// belongs to another action.
func repositoryMonitorFeedbackActionAgrees(action *store.WorkAction, want repositoryMonitorFeedbackProjection) bool {
	return action.Status == want.actionStatus ||
		action.Status == repositoryMonitorWorkActionStatusCompleted ||
		action.Status == repositoryMonitorWorkActionStatusCancelled && want.actionStatus != repositoryMonitorWorkActionStatusSucceeded
}

// repositoryMonitorFeedbackJobAgrees reports whether a repair job records
// the outcome's end. Its error is not compared: validation records its own on
// a succeeded repair's job.
func repositoryMonitorFeedbackJobAgrees(job *store.RepairJob, outcome store.MonitorFeedbackOutcome, want repositoryMonitorFeedbackProjection) bool {
	return job.Phase == want.jobPhase && job.PushedSHA == outcome.ResultSHA && job.CompletedAt != nil
}

// repositoryMonitorFeedbackMutationAgrees reports whether a push mutation
// records the outcome's end.
func repositoryMonitorFeedbackMutationAgrees(mutation *store.GitHubMutationRecord, outcome store.MonitorFeedbackOutcome, want repositoryMonitorFeedbackProjection) bool {
	return mutation.Status == want.mutationStatus && mutation.ExternalID == outcome.ResultSHA
}

// projectTerminalRepositoryMonitorFeedbackAction reports whether a work
// action write for a repair command belongs to a feedback repair whose
// outcome has ended, and if so projects the stored outcome instead of the
// write: no path but projection writes such a repair's action.
func (r *RepositoryMonitorReconciler) projectTerminalRepositoryMonitorFeedbackAction(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, actionKind string) (bool, error) {
	if ctx.Value(repositoryMonitorFeedbackProjectingKey{}) != nil || actionKind != repositoryMonitorRepairWorkflowActionKind(repositoryMonitorCommandIntentFix) {
		return false, nil
	}
	outcome, err := r.repositoryMonitorFeedbackOutcome(ctx, monitor, command.ID)
	if err != nil || outcome == nil || !store.IsMonitorFeedbackOutcomeTerminal(outcome.State) {
		return false, err
	}
	return true, r.projectRepositoryMonitorFeedbackOutcome(ctx, monitor, *outcome)
}

// repositoryMonitorFeedbackVerifyBatch bounds the projected outcomes one
// reconcile checks again.
const repositoryMonitorFeedbackVerifyBatch = 20

// verifyProjectedRepositoryMonitorFeedbackOutcomes checks a rotating window
// of the monitor's projected terminal outcomes against their repair job,
// push mutation, and work action, and projects again any that one of them
// contradicts, as a writer outside projection could. Each reconcile checks
// at most repositoryMonitorFeedbackVerifyBatch outcomes and resumes where the
// last stopped, so every outcome is checked again within a bounded number of
// reconciles.
func (r *RepositoryMonitorReconciler) verifyProjectedRepositoryMonitorFeedbackOutcomes(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (bool, error) {
	cursor := ""
	if value, ok := r.feedbackVerifyCursors.Load(monitor.UID); ok {
		cursor = value.(string)
	}
	outcomes, next, err := r.Store.ListMonitorFeedbackOutcomes(ctx, store.MonitorFeedbackOutcomeFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: repositoryMonitorFeedbackVerifyBatch, Cursor: cursor})
	if err != nil {
		// A cursor the store no longer accepts starts over.
		r.feedbackVerifyCursors.Delete(monitor.UID)
		return false, err
	}
	r.feedbackVerifyCursors.Store(monitor.UID, next)
	corrected := false
	var errs []error
	for _, outcome := range outcomes {
		if outcome.ProjectedAt == nil || !store.IsMonitorFeedbackOutcomeTerminal(outcome.State) {
			continue
		}
		contradicted, err := r.repositoryMonitorFeedbackProjectionContradicted(ctx, monitor, outcome, false)
		if err == nil && contradicted {
			if err = r.projectRepositoryMonitorFeedbackOutcome(ctx, monitor, outcome); err == nil {
				corrected = true
			}
		}
		errs = append(errs, err)
	}
	return corrected, errors.Join(errs...)
}

// repositoryMonitorFeedbackProjectionContradicted reports whether the repair
// job, push mutation, or work action of a terminal outcome records anything
// other than the outcome's end. A missing work action contradicts it only
// when requireAction is set, as it is right after projection wrote one; a
// record that was never made, or that retention removed later, otherwise
// contradicts nothing.
func (r *RepositoryMonitorReconciler) repositoryMonitorFeedbackProjectionContradicted(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, outcome store.MonitorFeedbackOutcome, requireAction bool) (bool, error) {
	want := repositoryMonitorFeedbackProjectionFor(outcome)
	jobID := "repair-" + repositoryMonitorShortHash(outcome.CommandEventID)
	job, err := r.Store.GetRepairJob(ctx, monitor.Namespace, jobID)
	switch {
	case err == nil:
		if !repositoryMonitorFeedbackJobAgrees(job, outcome, want) {
			return true, nil
		}
	case !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	mutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, "ghmut-"+repositoryMonitorShortHash(jobID+"-push"))
	switch {
	case err == nil:
		if !repositoryMonitorFeedbackMutationAgrees(mutation, outcome, want) {
			return true, nil
		}
	case !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	action, err := r.Store.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(outcome.CommandEventID, store.RepositoryMonitorDesiredActionForIntent(repositoryMonitorCommandIntentFix)))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return requireAction, nil
	case err != nil:
		return false, err
	}
	return !repositoryMonitorFeedbackActionAgrees(action, want), nil
}

// projectRepositoryMonitorFeedbackJob makes the repair job and its push
// mutation, when they exist, record want, whether or not a Task ran. Each is
// corrected on its own whenever it records anything else, so a replay after
// an interrupted projection, or a writer that recorded a contradicting end,
// is set right.
func (r *RepositoryMonitorReconciler) projectRepositoryMonitorFeedbackJob(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, outcome store.MonitorFeedbackOutcome, want repositoryMonitorFeedbackProjection) error {
	jobID := "repair-" + repositoryMonitorShortHash(outcome.CommandEventID)
	lastError := outcome.Reason
	if want.jobPhase == repositoryMonitorRepairPhaseSucceeded {
		lastError = ""
	}
	job, err := r.Store.GetRepairJob(ctx, monitor.Namespace, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	case !repositoryMonitorFeedbackJobAgrees(job, outcome, want):
		job.Phase, job.LastError, job.PushedSHA = want.jobPhase, lastError, outcome.ResultSHA
		if job.CompletedAt == nil {
			job.CompletedAt = outcome.CompletedAt
		}
		if err := r.Store.UpdateRepairJob(ctx, job); err != nil {
			return err
		}
	}
	mutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, "ghmut-"+repositoryMonitorShortHash(jobID+"-push"))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || repositoryMonitorFeedbackMutationAgrees(mutation, outcome, want) {
		return err
	}
	mutation.CommandEventID, mutation.ExternalID, mutation.Error, mutation.Status = outcome.CommandEventID, outcome.ResultSHA, lastError, want.mutationStatus
	return r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation)
}
