/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskwait"
	"github.com/orka-agents/orka/internal/tracing"
)

const (
	// taskWaitPollInterval is the fallback re-check while an event is
	// awaited; Task watches and approval nudges normally wake the wait first.
	taskWaitPollInterval = 30 * time.Second
	// taskWaitPendingTurnInterval re-checks a requested wait whose turn has
	// not settled yet.
	taskWaitPendingTurnInterval = 5 * time.Second
	maxTaskWaitMessageLength    = 1024
	// taskWaitStepInterval runs the next durable step, or retries a lost
	// optimistic-concurrency race, promptly.
	taskWaitStepInterval = 100 * time.Millisecond
	// taskWaitLinkAttempts bounds how often one reconcile retries linking a
	// continuation it created when the status write loses a race.
	taskWaitLinkAttempts = 4

	taskWaitReasonPrefix = "Awaiting"
)

// TaskWaitReconciler drives data-only parked turns. It arms a wait only after
// the exact turn that requested it settled Succeeded, so no prompt, tool
// call, or publication is in flight. It never preserves or restores process
// memory: when the awaited event arrives it creates one continuation Task on
// the same Session, which re-freezes and revalidates policy like any other
// Session continuation.
type TaskWaitReconciler struct {
	client.Client
	// APIReader reads authoritative state for exactly-once transitions.
	APIReader client.Reader
	// ExecutionEvents holds parked approval requests and decisions.
	ExecutionEvents store.DeduplicatingExecutionEventStore
	// TaskProvenanceProtected reports that Task provenance admission reserves
	// the continuation lineage annotations to the controller. Only then can an
	// existing Task with the reserved name be proven to be Orka's continuation.
	TaskProvenanceProtected bool
	// Now is overridable for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks/status,verbs=get;update;patch

func (r *TaskWaitReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// reader serves the frequent, watch-driven reads from the cache. Every
// transition is fenced on the Task resourceVersion, so a stale read can only
// lose a race and requeue.
func (r *TaskWaitReconciler) reader() client.Reader {
	return r.Client
}

// authoritativeReader bypasses the cache where a stale miss would cause a
// redundant create attempt.
func (r *TaskWaitReconciler) authoritativeReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Reconcile advances one parked wait by at most one durable transition per step.
func (r *TaskWaitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	task := &corev1alpha1.Task{}
	if err := r.reader().Get(ctx, req.NamespacedName, task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if controllerutil.ContainsFinalizer(task, labels.ContinuationLinkFinalizer) {
		if err := r.releaseContinuationLink(ctx, task.Namespace, task.Name); err != nil {
			return ctrl.Result{}, err
		}
	}
	wait := task.Status.Wait
	if wait == nil {
		return ctrl.Result{}, nil
	}
	if !wait.State.IsTerminal() {
		// Act on the authoritative object: a cached Task may predate the
		// turn's settlement or a cancellation request.
		if err := r.authoritativeReader().Get(ctx, req.NamespacedName, task); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if wait = task.Status.Wait; wait == nil {
			return ctrl.Result{}, nil
		}
	}
	if wait.State.IsTerminal() {
		return ctrl.Result{}, r.syncWaitingCondition(ctx, task)
	}
	if reason, cancelled := taskWaitCancellation(task); cancelled {
		return r.cancel(ctx, task, reason)
	}
	switch wait.State {
	case corev1alpha1.TaskWaitStateRequested:
		return r.reconcileRequested(ctx, task)
	case corev1alpha1.TaskWaitStateWaiting:
		return r.reconcileWaiting(ctx, task)
	case corev1alpha1.TaskWaitStateResuming:
		return r.reconcileResuming(ctx, task)
	default:
		return ctrl.Result{}, nil
	}
}

// taskWaitRetry requeues promptly when a fenced transition lost a race.
func taskWaitRetry(done bool) ctrl.Result {
	if done {
		return ctrl.Result{}
	}
	return ctrl.Result{RequeueAfter: taskWaitStepInterval}
}

func taskWaitCancellation(task *corev1alpha1.Task) (string, bool) {
	if !task.DeletionTimestamp.IsZero() {
		return "the Task is being deleted", true
	}
	if reason := strings.TrimSpace(task.Annotations[labels.AnnotationCancelWait]); reason != "" {
		return "cancelled: " + boundTaskWaitText(redact.SensitiveText(reason), 256), true
	}
	return "", false
}

// reconcileRequested arms a wait only on proven success of its exact turn.
func (r *TaskWaitReconciler) reconcileRequested(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	wait := task.Status.Wait
	execution := task.Status.Execution
	if execution == nil || execution.Attempt != wait.Attempt || execution.PromptID != wait.PromptID {
		return r.finishWithoutResume(ctx, task, corev1alpha1.TaskWaitStateAbandoned, "the turn that requested the wait was superseded")
	}
	if !taskwait.TerminalPhase(task.Status.Phase) {
		return ctrl.Result{RequeueAfter: taskWaitPendingTurnInterval}, nil
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded ||
		execution.State != corev1alpha1.TaskExecutionStateSucceeded ||
		execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded {
		return r.finishWithoutResume(ctx, task, corev1alpha1.TaskWaitStateAbandoned,
			"the turn that requested the wait did not succeed, so nothing resumes")
	}
	now := r.now()
	deadline := wait.Deadline.Time
	if chain := taskWaitChainDeadline(task); chain.Before(deadline) {
		deadline = chain
	}
	if wait.Kind == corev1alpha1.TaskWaitKindApproval {
		if err := taskwait.EnsureParkedApprovalRequested(ctx, r.ExecutionEvents, task, wait); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure parked approval request: %w", err)
		}
	}
	armed, err := r.transition(ctx, task, func(next *corev1alpha1.TaskWaitStatus) {
		next.State = corev1alpha1.TaskWaitStateWaiting
		since := metav1.NewTime(now)
		next.WaitingSince = &since
		next.Deadline = metav1.NewTime(deadline.Truncate(time.Second))
		next.Message = taskWaitArmedMessage(next)
	})
	if err != nil || !armed {
		return taskWaitRetry(armed), err
	}
	metrics.RecordTaskWaitTransition(string(wait.Kind), string(corev1alpha1.TaskWaitStateWaiting), 0)
	return ctrl.Result{RequeueAfter: taskWaitStepInterval}, nil
}

func taskWaitArmedMessage(wait *corev1alpha1.TaskWaitStatus) string {
	switch wait.Kind {
	case corev1alpha1.TaskWaitKindApproval:
		return "turn parked; runtime released; waiting for an approval decision"
	case corev1alpha1.TaskWaitKindTasks:
		return fmt.Sprintf("turn parked; runtime released; waiting for %d child task(s)", len(wait.References))
	case corev1alpha1.TaskWaitKindTimer:
		return "turn parked; runtime released; waiting for the backoff timer"
	default:
		return "turn parked; runtime released"
	}
}

// reconcileWaiting checks the awaited event and the deadlines.
func (r *TaskWaitReconciler) reconcileWaiting(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	wait := task.Status.Wait
	now := r.now()
	outcome, ready, retryAfter, err := r.evaluate(ctx, task, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		if now.Before(wait.Deadline.Time) {
			return ctrl.Result{RequeueAfter: min(retryAfter, wait.Deadline.Sub(now)+time.Second)}, nil
		}
		// The wait deadline passed first. An undecided parked approval is
		// closed before anything resumes; a racing decision wins instead.
		if wait.Kind == corev1alpha1.TaskWaitKindApproval {
			closed, closeErr := r.closeParkedApproval(ctx, task, true, "the wait deadline passed")
			if closeErr != nil || !closed {
				return taskWaitRetry(closed), closeErr
			}
		}
		outcome = "The wait reached its deadline before the awaited event arrived."
	}
	if taskWaitChainDeadline(task).Sub(now) < taskwait.MinContinuationBudget {
		return r.finishWithoutResume(ctx, task, corev1alpha1.TaskWaitStateExpired,
			"the Task deadline leaves no time for a continuation turn")
	}
	name := taskwait.ContinuationTaskName(task)
	resuming, err := r.transition(ctx, task, func(next *corev1alpha1.TaskWaitStatus) {
		next.State = corev1alpha1.TaskWaitStateResuming
		next.ContinuationTaskName = name
		next.Message = boundTaskWaitText(outcome, maxTaskWaitMessageLength)
	})
	if err != nil || !resuming {
		return taskWaitRetry(resuming), err
	}
	return ctrl.Result{RequeueAfter: taskWaitStepInterval}, nil
}

// evaluate reports whether the awaited event arrived and, if so, the
// controller-authored description handed to the continuation turn.
func (r *TaskWaitReconciler) evaluate(ctx context.Context, task *corev1alpha1.Task, now time.Time) (string, bool, time.Duration, error) {
	wait := task.Status.Wait
	switch wait.Kind {
	case corev1alpha1.TaskWaitKindTimer:
		if wait.ResumeAt == nil || !now.Before(wait.ResumeAt.Time) {
			return "The requested backoff period elapsed.", true, 0, nil
		}
		return "", false, wait.ResumeAt.Sub(now), nil
	case corev1alpha1.TaskWaitKindApproval:
		return r.evaluateApproval(ctx, task)
	case corev1alpha1.TaskWaitKindTasks:
		return r.evaluateTasks(ctx, task)
	default:
		return "", false, taskWaitPollInterval, fmt.Errorf("unsupported wait kind %q", wait.Kind)
	}
}

func (r *TaskWaitReconciler) evaluateApproval(ctx context.Context, task *corev1alpha1.Task) (string, bool, time.Duration, error) {
	wait := task.Status.Wait
	if r.ExecutionEvents == nil || len(wait.References) != 1 {
		return "", false, taskWaitPollInterval, errors.New("parked approvals require execution event storage")
	}
	listed, err := approvals.ListEvents(ctx, r.ExecutionEvents, task.Namespace, task.Name)
	if err != nil {
		return "", false, taskWaitPollInterval, err
	}
	approvalID := wait.References[0]
	for _, approval := range approvals.Derive(approvals.FilterEventsForTaskUID(listed, string(task.UID)), time.Time{}) {
		if approval.ID != approvalID || approval.Binding != nil {
			continue
		}
		switch approval.Status {
		case approvals.StatusPending:
			return "", false, taskWaitPollInterval, nil
		case approvals.StatusApproved, approvals.StatusDeclined:
			text := fmt.Sprintf("Your sign-off request was %s", approval.Status)
			if actor := strings.TrimSpace(approval.DecisionActor); actor != "" {
				text += " by " + boundTaskWaitText(redact.SensitiveText(actor), 128)
			}
			text += "."
			if note := strings.TrimSpace(approval.DecisionReason); note != "" && !strings.HasPrefix(note, "approval ") {
				text += " Reviewer note: " + boundTaskWaitText(redact.SensitiveText(note), 512)
			}
			return text, true, 0, nil
		default:
			return fmt.Sprintf("Your sign-off request was %s without a decision.", approval.Status), true, 0, nil
		}
	}
	// The request event is appended when the wait arms; until it is visible
	// the approval is simply pending.
	return "", false, taskWaitPollInterval, nil
}

func (r *TaskWaitReconciler) evaluateTasks(ctx context.Context, task *corev1alpha1.Task) (string, bool, time.Duration, error) {
	wait := task.Status.Wait
	owners, err := r.taskWaitChildOwners(ctx, task)
	if err != nil {
		return "", false, taskWaitPollInterval, err
	}
	lines := make([]string, 0, len(wait.References))
	for i, name := range wait.References {
		if len(validation.IsDNS1123Subdomain(name)) > 0 {
			lines = append(lines, "an invalid task reference")
			continue
		}
		child := &corev1alpha1.Task{}
		if err := r.reader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, child); err != nil {
			if apierrors.IsNotFound(err) {
				lines = append(lines, name+": no longer exists")
				continue
			}
			return "", false, taskWaitPollInterval, err
		}
		// Only the exact child authorized when the wait was recorded counts;
		// a same-named replacement never controls when this Task resumes.
		if len(wait.ReferenceUIDs) != len(wait.References) || string(child.UID) != wait.ReferenceUIDs[i] {
			lines = append(lines, name+": is no longer the task this wait was authorized for")
			continue
		}
		if !taskWaitChildBelongsToAny(owners, child) {
			lines = append(lines, name+": is no longer this task's child")
			continue
		}
		tail, err := taskwait.ResolveTail(ctx, r.reader(), child)
		if err != nil {
			if errors.Is(err, taskwait.ErrChainBroken) || apierrors.IsNotFound(err) {
				lines = append(lines, name+": its continuation chain is broken")
				continue
			}
			return "", false, taskWaitPollInterval, err
		}
		if !taskwait.Settled(tail) {
			return "", false, taskWaitPollInterval, nil
		}
		line := fmt.Sprintf("%s: %s", name, tail.Status.Phase)
		if tail.Name != child.Name {
			line += fmt.Sprintf(" (continued as %s)", tail.Name)
		}
		if tail.Status.Wait != nil && tail.Status.Wait.State != corev1alpha1.TaskWaitStateResumed {
			line += fmt.Sprintf(", its own wait ended %s", tail.Status.Wait.State)
		}
		lines = append(lines, line)
	}
	return "The child tasks you waited for have finished: " + strings.Join(lines, "; ") +
		". Call wait_for_tasks with the same names to read their results.", true, 0, nil
}

// taskWaitChildOwners returns the parked Task and its verified predecessors.
// A continuation keeps authority over the children its predecessors delegated.
func (r *TaskWaitReconciler) taskWaitChildOwners(ctx context.Context, task *corev1alpha1.Task) ([]*corev1alpha1.Task, error) {
	owners := []*corev1alpha1.Task{task}
	current := task
	for range taskwait.MaxChainLength {
		previous, ok, err := taskwait.Predecessor(ctx, r.reader(), current)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		owners = append(owners, previous)
		current = previous
	}
	return owners, nil
}

func taskWaitChildBelongsToAny(owners []*corev1alpha1.Task, child *corev1alpha1.Task) bool {
	for _, owner := range owners {
		if taskWaitChildBelongs(owner, child) {
			return true
		}
	}
	return false
}

// taskWaitChildBelongs re-checks the parent link verified when the wait was
// requested, so a deleted-and-recreated name is not mistaken for the child.
func taskWaitChildBelongs(parent, child *corev1alpha1.Task) bool {
	if labels.ParentTaskName(child.Labels, child.Annotations) != parent.Name {
		return false
	}
	if uid := strings.TrimSpace(child.Annotations[labels.AnnotationParentTaskUID]); uid != "" && uid != string(parent.UID) {
		return false
	}
	if owner := metav1.GetControllerOf(child); owner != nil && owner.Kind == taskResourceKind && owner.UID != parent.UID {
		return false
	}
	return true
}

// reconcileResuming creates the continuation exactly once. The name was
// reserved durably before creation, so a restart adopts the existing Task.
func (r *TaskWaitReconciler) reconcileResuming(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	wait := task.Status.Wait
	name := strings.TrimSpace(wait.ContinuationTaskName)
	if name == "" {
		return r.finishWithoutResume(ctx, task, corev1alpha1.TaskWaitStateAbandoned, "no continuation name was reserved")
	}
	existing := &corev1alpha1.Task{}
	err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, existing)
	switch {
	case err == nil:
		if existing.Annotations[labels.AnnotationContinuationOfUID] != string(task.UID) {
			return r.finishWithoutResume(ctx, task, corev1alpha1.TaskWaitStateAbandoned,
				"the continuation name is held by an unrelated Task")
		}
		if !r.TaskProvenanceProtected {
			// Without provenance admission anyone who can create Tasks could
			// forge the lineage annotation; linking it would hand that Task
			// this one's authority. Fail closed.
			return r.finishWithoutResume(ctx, task, corev1alpha1.TaskWaitStateAbandoned,
				"an existing continuation cannot be proven to be Orka's because Task provenance admission is disabled")
		}
		// A restart may have landed between creation and sealing. Seal before
		// linking so the retry happens while this Task is still Resuming.
		if err := r.ensureContinuationRequesterSealed(ctx, task, existing); err != nil && !errors.Is(err, ErrChildSealRefused) {
			return ctrl.Result{}, fmt.Errorf("seal adopted continuation requester: %w", err)
		}
		return r.markResumed(ctx, task, existing)
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	now := r.now()
	chainDeadline := taskWaitChainDeadline(task)
	remaining := chainDeadline.Sub(now).Truncate(time.Second)
	if remaining < taskwait.MinContinuationBudget {
		return r.finishWithoutResume(ctx, task, corev1alpha1.TaskWaitStateExpired,
			"the Task deadline leaves no time for a continuation turn")
	}
	continuation := buildTaskContinuation(task, name, remaining, chainDeadline)
	if continuationRequesterSealable(task) {
		// Mark the requester as API-sourced from creation so admission waits
		// for the seal instead of freezing a snapshot without the requester's
		// connectors.
		continuation.Annotations[labels.AnnotationRequestedBySource] = labels.RequestedBySourceAPI
	}
	tracing.StampTaskTraceContext(ctx, continuation)
	if err := r.Create(ctx, continuation); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: taskWaitStepInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("create continuation task: %w", err)
	}
	logf.FromContext(ctx).Info("Created continuation for parked task", "task", task.Name, "continuation", name, "kind", wait.Kind)
	if err := r.ensureContinuationRequesterSealed(ctx, task, continuation); err != nil {
		// Linking the continuation that now exists matters more than its
		// connectors. Admission waits out the stamp grace period and then
		// fails its connector tools closed, like an unsealed scheduled run.
		logf.FromContext(ctx).Info("Continuation requester was not sealed; its connector tools fail closed",
			"task", task.Name, "continuation", name, "reason", err.Error())
	}
	return r.markCreatedResumed(ctx, task, continuation)
}

// markCreatedResumed links the continuation this reconcile created. Creating
// it is the proof that it is Orka's, and a later reconcile can re-prove that
// only with provenance admission, so a status write that loses a race is
// retried here against the authoritative Task while its wait still reserves
// this continuation and has not been cancelled.
func (r *TaskWaitReconciler) markCreatedResumed(ctx context.Context, task, continuation *corev1alpha1.Task) (ctrl.Result, error) {
	for attempt := range taskWaitLinkAttempts {
		if attempt > 0 {
			latest := &corev1alpha1.Task{}
			if err := r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			wait := latest.Status.Wait
			if latest.UID != task.UID || wait == nil || wait.State != corev1alpha1.TaskWaitStateResuming ||
				wait.ContinuationTaskName != continuation.Name {
				return ctrl.Result{RequeueAfter: taskWaitStepInterval}, nil
			}
			if reason, cancelled := taskWaitCancellation(latest); cancelled {
				return r.cancel(ctx, latest, reason)
			}
			task = latest
		}
		linked, result, err := r.linkContinuation(ctx, task, continuation)
		if err != nil || linked {
			return result, err
		}
	}
	return ctrl.Result{RequeueAfter: taskWaitStepInterval}, nil
}

func (r *TaskWaitReconciler) markResumed(ctx context.Context, task *corev1alpha1.Task, continuation *corev1alpha1.Task) (ctrl.Result, error) {
	_, result, err := r.linkContinuation(ctx, task, continuation)
	return result, err
}

// linkContinuation records the continuation on the parked Task. It reports
// false when the fenced status write lost a race.
func (r *TaskWaitReconciler) linkContinuation(ctx context.Context, task, continuation *corev1alpha1.Task) (bool, ctrl.Result, error) {
	wait := task.Status.Wait
	now := r.now()
	done, err := r.transition(ctx, task, func(next *corev1alpha1.TaskWaitStatus) {
		next.State = corev1alpha1.TaskWaitStateResumed
		next.ContinuationTaskUID = string(continuation.UID)
		next.Message = boundTaskWaitText("resumed as "+continuation.Name+": "+next.Message, maxTaskWaitMessageLength)
	})
	if err != nil || !done {
		return false, taskWaitRetry(done), err
	}
	metrics.RecordTaskWaitTransition(string(wait.Kind), string(corev1alpha1.TaskWaitStateResumed), taskWaitedFor(wait, now))
	// The UID is durable now; the continuation may be deleted like any Task.
	if err := r.releaseContinuationLink(ctx, continuation.Namespace, continuation.Name); err != nil {
		return true, ctrl.Result{RequeueAfter: taskWaitStepInterval}, nil //nolint:nilerr // The continuation's own reconcile retries the release.
	}
	return true, ctrl.Result{}, nil
}

// releaseContinuationLink removes the link finalizer once the predecessor no
// longer needs it: it recorded this continuation, ended its wait without it,
// or no longer exists. While the predecessor is still Resuming with this
// name, the finalizer stays so that recovery adopts this Task instead of
// creating another one.
func (r *TaskWaitReconciler) releaseContinuationLink(ctx context.Context, namespace, name string) error {
	continuation := &corev1alpha1.Task{}
	if err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, continuation); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.ContainsFinalizer(continuation, labels.ContinuationLinkFinalizer) {
		return nil
	}
	predecessorName := strings.TrimSpace(continuation.Annotations[labels.AnnotationContinuationOfName])
	predecessorUID := strings.TrimSpace(continuation.Annotations[labels.AnnotationContinuationOfUID])
	if predecessorName != "" {
		predecessor := &corev1alpha1.Task{}
		err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: predecessorName}, predecessor)
		switch {
		case err == nil && string(predecessor.UID) == predecessorUID && predecessor.Status.Wait != nil &&
			predecessor.Status.Wait.State == corev1alpha1.TaskWaitStateResuming &&
			predecessor.Status.Wait.ContinuationTaskName == continuation.Name:
			return nil
		case err != nil && !apierrors.IsNotFound(err):
			return err
		}
	}
	base := continuation.DeepCopy()
	controllerutil.RemoveFinalizer(continuation, labels.ContinuationLinkFinalizer)
	if err := r.Patch(ctx, continuation, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// cancel ends a non-terminal wait. A continuation created in the same window
// is deleted, which cancels it like any other agent Task.
func (r *TaskWaitReconciler) cancel(ctx context.Context, task *corev1alpha1.Task, reason string) (ctrl.Result, error) {
	wait := task.Status.Wait
	if wait.State == corev1alpha1.TaskWaitStateResuming && strings.TrimSpace(wait.ContinuationTaskName) != "" {
		// Read authoritatively: a continuation created moments ago may not be
		// cached yet, and missing it would leave it running after cancellation.
		existing := &corev1alpha1.Task{}
		err := r.authoritativeReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: wait.ContinuationTaskName}, existing)
		switch {
		case err == nil && existing.Annotations[labels.AnnotationContinuationOfUID] == string(task.UID):
			uid := existing.UID
			if err := r.Delete(ctx, existing, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("cancel continuation task: %w", err)
			}
		case err != nil && !apierrors.IsNotFound(err):
			return ctrl.Result{}, err
		}
	}
	return r.finishWithoutResume(ctx, task, corev1alpha1.TaskWaitStateCancelled, reason)
}

// finishWithoutResume moves a wait to a final state in which nothing resumes,
// closing an undecided parked approval first.
func (r *TaskWaitReconciler) finishWithoutResume(
	ctx context.Context,
	task *corev1alpha1.Task,
	state corev1alpha1.TaskWaitState,
	message string,
) (ctrl.Result, error) {
	wait := task.Status.Wait
	if wait.Kind == corev1alpha1.TaskWaitKindApproval {
		if _, err := r.closeParkedApproval(ctx, task, state == corev1alpha1.TaskWaitStateExpired, message); err != nil {
			return ctrl.Result{}, err
		}
	}
	now := r.now()
	done, err := r.transition(ctx, task, func(next *corev1alpha1.TaskWaitStatus) {
		next.State = state
		next.Message = boundTaskWaitText(message, maxTaskWaitMessageLength)
	})
	if err != nil || !done {
		return taskWaitRetry(done), err
	}
	metrics.RecordTaskWaitTransition(string(wait.Kind), string(state), taskWaitedFor(wait, now))
	return ctrl.Result{}, nil
}

// closeParkedApproval appends the terminal event for an undecided parked
// approval. It reports false when a human decision landed first; the caller
// then re-evaluates so that the decision wins.
func (r *TaskWaitReconciler) closeParkedApproval(ctx context.Context, task *corev1alpha1.Task, expired bool, reason string) (bool, error) {
	wait := task.Status.Wait
	if r.ExecutionEvents == nil || len(wait.References) != 1 {
		return true, nil
	}
	if wait.State == corev1alpha1.TaskWaitStateRequested {
		// A request that never armed may not have its event yet; ensure it
		// exists so the closing event has a request to close.
		if err := taskwait.EnsureParkedApprovalRequested(ctx, r.ExecutionEvents, task, wait); err != nil {
			return false, err
		}
	}
	event, key, err := taskwait.ParkedApprovalTerminalEvent(task, wait, expired, reason)
	if err != nil {
		return false, err
	}
	persisted, appended, err := r.ExecutionEvents.AppendExecutionEventIfAbsent(ctx, event, key)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return false, nil
		}
		return false, fmt.Errorf("close parked approval: %w", err)
	}
	if !appended && persisted != nil && persisted.Type != event.Type {
		return false, nil
	}
	return true, nil
}

// transition applies one wait mutation fenced on the Task resourceVersion and
// the exact prior wait state. It reports false on a lost race.
func (r *TaskWaitReconciler) transition(
	ctx context.Context,
	task *corev1alpha1.Task,
	mutate func(*corev1alpha1.TaskWaitStatus),
) (bool, error) {
	if task.Status.Wait == nil {
		return false, nil
	}
	base := task.DeepCopy()
	next := task.Status.Wait.DeepCopy()
	previous := next.State
	mutate(next)
	if next.State != previous {
		now := metav1.NewTime(r.now())
		next.LastTransitionTime = &now
	}
	task.Status.Wait = next
	setTaskWaitingCondition(task)
	if err := r.Status().Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("update task wait: %w", err)
	}
	return true, nil
}

func (r *TaskWaitReconciler) syncWaitingCondition(ctx context.Context, task *corev1alpha1.Task) error {
	base := task.DeepCopy()
	if !setTaskWaitingCondition(task) {
		return nil
	}
	if err := r.Status().Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil &&
		!apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// setTaskWaitingCondition mirrors the wait into the Waiting condition and
// reports whether it changed.
func setTaskWaitingCondition(task *corev1alpha1.Task) bool {
	wait := task.Status.Wait
	if wait == nil {
		return false
	}
	condition := metav1.Condition{
		Type: corev1alpha1.TaskConditionWaiting, ObservedGeneration: task.Generation,
		Status: metav1.ConditionTrue, Reason: taskWaitReasonPrefix + string(wait.Kind),
		Message: boundTaskWaitText(wait.Message, maxTaskWaitMessageLength),
	}
	if wait.State.IsTerminal() {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "Wait" + string(wait.State)
	}
	existing := meta.FindStatusCondition(task.Status.Conditions, corev1alpha1.TaskConditionWaiting)
	if existing != nil && existing.Status == condition.Status && existing.Reason == condition.Reason && existing.Message == condition.Message {
		return false
	}
	meta.SetStatusCondition(&task.Status.Conditions, condition)
	return true
}

func taskWaitedFor(wait *corev1alpha1.TaskWaitStatus, now time.Time) time.Duration {
	if wait == nil || wait.WaitingSince == nil {
		return 0
	}
	return max(now.Sub(wait.WaitingSince.Time), time.Nanosecond)
}

// taskWaitChainDeadline is the deadline of the parked Task. Each continuation
// is created with only the remaining time, so it is also the chain deadline.
func taskWaitChainDeadline(task *corev1alpha1.Task) time.Time {
	if deadline, ok := acpTaskDeadline(task, time.Now()); ok {
		return deadline
	}
	return taskwait.Deadline(task)
}

// Labels and annotations a continuation inherits so its frozen runtime
// policy matches the parked turn. Controller runtime state is never copied.
var (
	taskContinuationInheritedLabels = []string{
		labels.LabelAgentName, labels.LabelAgentUID, labels.LabelTaskType, labels.LabelParentTask,
		labels.LabelCoordinator, labels.LabelDelegatedAgent, labels.LabelCreatedBy, labels.LabelChatSession,
		labels.LabelScheduledRun, labels.LabelPurpose, labels.LabelManaged,
	}
	taskContinuationInheritedAnnotations = []string{
		labels.AnnotationParentTaskName, labels.AnnotationCoordinationDepth,
		labels.AnnotationDisableCoordinationToolInject, labels.AnnotationAgentReadOnly,
		labels.AnnotationAgentRuntimeAuthOnly, labels.AnnotationWorkspaceInitContainer,
	}
)

// buildTaskContinuation copies the parked Task's execution inputs. Only the
// prompt, the Session reference flags, and the remaining timeout change. The
// chain's absolute deadline is recorded too, so creation latency or clock
// skew cannot extend it.
func buildTaskContinuation(parked *corev1alpha1.Task, name string, remaining time.Duration, chainDeadline time.Time) *corev1alpha1.Task {
	spec := *parked.Spec.DeepCopy()
	spec.Prompt = taskContinuationPrompt(parked)
	spec.PriorTaskRef = nil
	spec.Suspend = nil
	spec.Timeout = &metav1.Duration{Duration: remaining}
	if workspace := spec.Workspace; workspace != nil && taskWaitTurnPublished(parked) {
		// The parked turn moved the branch, and its Session recorded the
		// published commit as the verified baseline that now governs the
		// next publication. The pre-publication precondition would conflict
		// with that baseline and fail the continuation before it resumes.
		workspace.ExpectedRemoteSHA = ""
	}
	if ref := parked.Spec.SessionRef; ref != nil {
		spec.SessionRef = &corev1alpha1.SessionReference{
			Name: ref.Name, Create: false, Append: true, MaxMessages: ref.MaxMessages,
		}
	}
	continuation := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: parked.Namespace,
			Labels: map[string]string{labels.LabelContinuationOf: labels.SelectorValue(parked.Name)},
			Annotations: map[string]string{
				labels.AnnotationContinuationOfName: parked.Name,
				labels.AnnotationContinuationOfUID:  string(parked.UID),
				labels.AnnotationContinuationIndex:  strconv.Itoa(int(taskwait.ChainIndex(parked)) + 1),
				// RFC 3339 drops sub-second precision, which only shortens the deadline.
				labels.AnnotationContinuationChainDeadline: chainDeadline.UTC().Format(time.RFC3339),
			},
			Finalizers: []string{labels.ContinuationLinkFinalizer},
			// A non-controller owner: deleting the parked Task cancels its
			// continuation without making it a coordination child.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskResourceKind,
				Name: parked.Name, UID: parked.UID,
			}},
		},
		Spec: spec,
	}
	for _, key := range taskContinuationInheritedLabels {
		if value, ok := parked.Labels[key]; ok {
			continuation.Labels[key] = value
		}
	}
	for _, key := range taskContinuationInheritedAnnotations {
		if value, ok := parked.Annotations[key]; ok {
			continuation.Annotations[key] = value
		}
	}
	return continuation
}

// taskWaitTurnPublished reports whether the parked turn published a verified
// commit to its write workspace's branch.
func taskWaitTurnPublished(parked *corev1alpha1.Task) bool {
	delivery := parked.Status.Delivery
	if delivery == nil || strings.TrimSpace(delivery.VerifiedRemoteSHA) == "" {
		return false
	}
	switch delivery.State {
	case corev1alpha1.TaskDeliveryStateVerifiedExact, corev1alpha1.TaskDeliveryStateDeliveredSuperseded:
		return true
	default:
		return false
	}
}

// taskContinuationPrompt is controller-authored. It describes the event and
// never repeats the parked prompt, which the Session transcript already holds.
func taskContinuationPrompt(parked *corev1alpha1.Task) string {
	wait := parked.Status.Wait
	var b strings.Builder
	b.WriteString("[Orka] Your previous turn parked to wait (")
	b.WriteString(strings.ToLower(string(wait.Kind)))
	b.WriteString("). Your runtime was restarted from this Session's history; processes, shells, and unsaved in-memory state from that turn are gone. ")
	b.WriteString(wait.Message)
	b.WriteString(" Continue the original task from where you left off.")
	return b.String()
}

// continuationRequesterSealable reports whether the parked Task carries a
// verified requester that its continuation must be sealed with.
func continuationRequesterSealable(parked *corev1alpha1.Task) bool {
	return len(requesterStampKey) >= connectors.MinRequesterStampKeyBytes && parked != nil &&
		parked.Spec.RequestedBy != nil && connectors.RequesterStampValid(requesterStampKey, parked)
}

// ensureContinuationRequesterSealed seals the continuation's requester stamp
// unless it is already valid, re-reading the continuation when a concurrent
// write fences the patch.
func (r *TaskWaitReconciler) ensureContinuationRequesterSealed(ctx context.Context, parked, continuation *corev1alpha1.Task) error {
	if !continuationRequesterSealable(parked) {
		return nil
	}
	current := continuation
	var err error
	for attempt := range acpChildSealAttempts {
		if attempt > 0 {
			current = &corev1alpha1.Task{}
			if err = r.authoritativeReader().Get(ctx, client.ObjectKeyFromObject(continuation), current); err != nil {
				return err
			}
			if current.UID != continuation.UID {
				return fmt.Errorf("%w: the continuation was replaced", ErrChildSealRefused)
			}
		}
		if connectors.RequesterStampValid(requesterStampKey, current) {
			return nil
		}
		if err = sealContinuationRequesterStamp(ctx, r.Client, requesterStampKey, parked, current); err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}

// sealContinuationRequesterStamp binds the parked Task's verified requester to
// its continuation. It mirrors SealChildRequesterStamp, but proves the link by
// the parked Task's owner reference and continuation annotation.
func sealContinuationRequesterStamp(ctx context.Context, c client.Client, key []byte, parked, continuation *corev1alpha1.Task) error {
	if len(key) < connectors.MinRequesterStampKeyBytes || continuation.Spec.RequestedBy == nil {
		return nil
	}
	if !connectors.RequesterStampValid(key, parked) {
		return fmt.Errorf("%w: the parked task carries no verified requester", ErrChildSealRefused)
	}
	if continuation.Annotations[labels.AnnotationContinuationOfUID] != string(parked.UID) ||
		continuation.Spec.RequestedBy.Issuer != parked.Spec.RequestedBy.Issuer ||
		continuation.Spec.RequestedBy.Subject != parked.Spec.RequestedBy.Subject {
		return fmt.Errorf("%w: the continuation does not match the parked task", ErrChildSealRefused)
	}
	stamp := connectors.RequesterStamp(key, continuation.UID, parked.Spec.RequestedBy.Issuer, parked.Spec.RequestedBy.Subject)
	if stamp == "" {
		return errors.New("the continuation has no UID to bind the requester stamp to")
	}
	original := continuation.DeepCopy()
	if continuation.Annotations == nil {
		continuation.Annotations = map[string]string{}
	}
	continuation.Annotations[labels.AnnotationRequestedBySource] = labels.RequestedBySourceAPI
	continuation.Annotations[labels.AnnotationRequestedByStamp] = stamp
	return c.Patch(ctx, continuation, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func boundTaskWaitText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	cut := limit - 3
	for cut > 0 && (value[cut]&0xC0) == 0x80 {
		cut--
	}
	return value[:cut] + "..."
}

// SetupWithManager watches Tasks. A change to any Task also wakes the parked
// Task waiting on it: its coordination parent for child waits, and the parked
// Task a continuation resumes, so chained child waits settle promptly.
func (r *TaskWaitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("task-wait").
		Watches(&corev1alpha1.Task{}, handler.EnqueueRequestsFromMapFunc(taskWaitRequestsFor)).
		Complete(r)
}

func taskWaitRequestsFor(_ context.Context, object client.Object) []reconcile.Request {
	task, ok := object.(*corev1alpha1.Task)
	if !ok {
		return nil
	}
	names := map[string]struct{}{}
	if task.Status.Wait != nil || controllerutil.ContainsFinalizer(task, labels.ContinuationLinkFinalizer) {
		names[task.Name] = struct{}{}
	}
	if task.Status.Wait != nil {
		if next := strings.TrimSpace(task.Status.Wait.ContinuationTaskName); next != "" {
			names[next] = struct{}{}
		}
	}
	if parent := labels.ParentTaskName(task.Labels, task.Annotations); parent != "" {
		names[parent] = struct{}{}
	}
	if parked := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfName]); parked != "" {
		names[parked] = struct{}{}
	}
	requests := make([]reconcile.Request, 0, len(names))
	for name := range names {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: task.Namespace, Name: name}})
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Name < requests[j].Name })
	return requests
}
