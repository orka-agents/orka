/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// evalRunFinalizer deletes an EvalRun's trial records with it.
	evalRunFinalizer = "orka.ai/evalrun-trials"
	// evalRunConditionCompleted is True once the run is terminal; its
	// reason says why.
	evalRunConditionCompleted = "Completed"
	// evalRunRequeueInterval bounds how long a running EvalRun waits before
	// it checks its snapshot and trial deadlines again.
	evalRunRequeueInterval = 30 * time.Second

	evalRunReasonRunning                 = "Running"
	evalRunReasonSucceeded               = "Succeeded"
	evalRunReasonSnapshotChanged         = "SnapshotChanged"
	evalRunReasonInfraErrorBudget        = "InfraErrorBudgetExceeded"
	evalRunReasonTrialTaskConflict       = "TrialTaskConflict"
	evalRunReasonCasesInvalid            = "CasesInvalid"
	evalRunReasonCaseTrialCapExceeded    = "CaseTrialCapExceeded"
	evalRunReasonSubjectInvalid          = "SubjectInvalid"
	evalRunReasonSubjectUnsupported      = "SubjectUnsupported"
	evalRunReasonGradersInvalid          = "GradersInvalid"
	evalRunReasonRuntimeImageUnavailable = "RuntimeImageUnavailable"

	evalRunDefaultTrialTimeout = 20 * time.Minute
	// evalTrialSettleGrace is how long a trial Task past its deadline gets
	// to settle itself. Its own spec.timeout ends at the same deadline: the
	// Task path cancels the prompt and flushes the assistant text the journal
	// still buffers. Recording the timeout before that would miss model
	// output, so the controller forces the timeout only after this grace.
	evalTrialSettleGrace              = acpPromptCancellationTimeout + acpInterruptedOutputFlushTimeout + 30*time.Second
	evalRunDefaultMaxInfraErrorPct    = 10
	evalRunDefaultMaxConcurrentTrials = 1
)

// evalRunConfigError is a run configuration that cannot be evaluated as is.
// Before the snapshot is frozen it fails the run; afterwards any such change
// invalidates it.
type evalRunConfigError struct {
	reason  string
	message string
}

func (e *evalRunConfigError) Error() string { return e.message }

func evalRunConfigErrorf(reason, format string, args ...any) error {
	return &evalRunConfigError{reason: reason, message: fmt.Sprintf(format, args...)}
}

// EvalRunReconciler runs EvalRun trials as Tasks, grades them with code and
// records one trial record per trial. It never writes to a forge.
type EvalRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads case ConfigMaps uncached, so the controller does not
	// cache every ConfigMap in its namespaces, and confirms trial Tasks the
	// cache has not seen yet.
	APIReader  client.Reader
	TrialStore store.EvalTrialStore
	// ExperimentStore holds experiment attempt reservations, which outlive
	// their EvalRuns. Nil fails every experiment closed.
	ExperimentStore store.EvalExperimentStore
	ResultStore     store.ResultStore
	EventStore      store.ExecutionEventStore
	RuntimeImages   ACPRuntimeImages
	Graders         *evals.Registry
	Now             func() time.Time
	// GateConfigMap names the protected-set ConfigMap whose gate section
	// judges experiments. Empty fails every experiment closed.
	GateConfigMap types.NamespacedName
}

// evalRunObservation is what one reconcile resolves from the cluster.
type evalRunObservation struct {
	Snapshot corev1alpha1.EvalRunSnapshot
	Cases    []evals.Case
	Plan     []evals.PlannedTrial
	Monitor  *corev1alpha1.RepositoryMonitor
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=evalruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=evalruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=evalruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=repositorymonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get

func (r *EvalRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	run := &corev1alpha1.EvalRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalizeEvalRun(ctx, run)
	}
	if controllerutil.AddFinalizer(run, evalRunFinalizer) {
		// The update triggers the next reconcile.
		return ctrl.Result{}, r.Update(ctx, run)
	}
	if evalRunFinished(run.Status.Phase) {
		return r.reconcileFinishedEvalRun(ctx, run)
	}
	return r.reconcileActiveEvalRun(ctx, run)
}

func (r *EvalRunReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func evalRunFinished(phase corev1alpha1.EvalRunPhase) bool {
	switch phase {
	case corev1alpha1.EvalRunPhaseSucceeded, corev1alpha1.EvalRunPhaseInvalid, corev1alpha1.EvalRunPhaseFailed:
		return true
	default:
		return false
	}
}

func (r *EvalRunReconciler) finalizeEvalRun(ctx context.Context, run *corev1alpha1.EvalRun) error {
	if !controllerutil.ContainsFinalizer(run, evalRunFinalizer) {
		return nil
	}
	if err := r.TrialStore.DeleteEvalTrials(ctx, run.Namespace, run.Name); err != nil {
		return fmt.Errorf("delete trial records of EvalRun %s/%s: %w", run.Namespace, run.Name, err)
	}
	controllerutil.RemoveFinalizer(run, evalRunFinalizer)
	return r.Update(ctx, run)
}

// reconcileFinishedEvalRun stops trials still running after the run ended
// and deletes the run once ttlSecondsAfterFinished has passed.
func (r *EvalRunReconciler) reconcileFinishedEvalRun(ctx context.Context, run *corev1alpha1.EvalRun) (ctrl.Result, error) {
	tasks, err := r.listEvalTrialTasks(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, task := range tasks {
		if evalTrialTaskTerminal(task) {
			continue
		}
		if err := r.deleteEvalTrialTask(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Judge an experiment before the TTL can delete the candidate's records.
	if experiment := run.Status.Experiment; experiment != nil && experiment.Result == corev1alpha1.EvalExperimentPending {
		return r.decideExperiment(ctx, run)
	}
	if run.Spec.TTLSecondsAfterFinished == nil || run.Status.CompletionTime == nil {
		return ctrl.Result{}, nil
	}
	// A candidate keeps its whole retention window after the decision, however
	// long it waited for its baseline.
	finished := run.Status.CompletionTime.Time
	if experiment := run.Status.Experiment; experiment != nil && experiment.DecidedAt != nil && experiment.DecidedAt.After(finished) {
		finished = experiment.DecidedAt.Time
	}
	expiry := finished.Add(time.Duration(*run.Spec.TTLSecondsAfterFinished) * time.Second)
	if remaining := expiry.Sub(r.now()); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	// A baseline outlives its TTL while a candidate still needs its trials.
	if waiting, err := r.hasUndecidedCandidates(ctx, run); err != nil || waiting {
		return ctrl.Result{RequeueAfter: evalRunRequeueInterval}, err
	}
	logf.FromContext(ctx).Info("Deleting EvalRun after ttlSecondsAfterFinished", "evalRun", run.Name)
	propagation := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, run, &client.DeleteOptions{
		PropagationPolicy: &propagation,
		Preconditions:     &metav1.Preconditions{UID: &run.UID},
	}); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *EvalRunReconciler) reconcileActiveEvalRun(ctx context.Context, run *corev1alpha1.EvalRun) (ctrl.Result, error) {
	observed, err := r.observeEvalRun(ctx, run)
	if err != nil {
		var configErr *evalRunConfigError
		if !errors.As(err, &configErr) {
			return ctrl.Result{}, err
		}
		if run.Status.Snapshot == nil {
			return ctrl.Result{}, r.finishEvalRun(ctx, run, nil, corev1alpha1.EvalRunPhaseFailed, configErr.reason, configErr.message)
		}
		return ctrl.Result{}, r.finishEvalRun(ctx, run, nil, corev1alpha1.EvalRunPhaseInvalid, evalRunReasonSnapshotChanged,
			"the run configuration changed after it started: "+configErr.message)
	}
	if run.Status.Snapshot == nil {
		if run.Spec.Experiment == nil && evalRunAgentOverride(run) {
			if err := r.refuseUnpairedHeldOutCandidate(ctx, run); err != nil {
				if configErr, ok := errors.AsType[*evalRunConfigError](err); ok {
					return ctrl.Result{}, r.finishEvalRun(ctx, run, nil, corev1alpha1.EvalRunPhaseFailed, configErr.reason, configErr.message)
				}
				return ctrl.Result{}, err
			}
		}
		if run.Spec.Experiment != nil {
			experiment, err := r.prepareExperiment(ctx, run, observed)
			if err != nil {
				if configErr, ok := errors.AsType[*evalRunConfigError](err); ok {
					return ctrl.Result{}, r.finishEvalRun(ctx, run, nil, corev1alpha1.EvalRunPhaseFailed, configErr.reason, configErr.message)
				}
				return ctrl.Result{}, err
			}
			run.Status.Experiment = experiment
		}
		return ctrl.Result{}, r.startEvalRun(ctx, run, observed)
	}
	if changed := evalRunSnapshotChanges(*run.Status.Snapshot, observed.Snapshot); len(changed) > 0 {
		return ctrl.Result{}, r.finishEvalRun(ctx, run, nil, corev1alpha1.EvalRunPhaseInvalid, evalRunReasonSnapshotChanged,
			"the run snapshot changed after it started: "+strings.Join(changed, ", "))
	}
	return r.runEvalTrials(ctx, run, observed)
}

// observeEvalRun resolves the cases, the subject and the snapshot. A
// configuration that can never run returns *evalRunConfigError.
func (r *EvalRunReconciler) observeEvalRun(ctx context.Context, run *corev1alpha1.EvalRun) (*evalRunObservation, error) {
	if run.Spec.Subject.Type != corev1alpha1.EvalSubjectRepositoryMonitorReview || run.Spec.Subject.RepositoryMonitorReview == nil {
		return nil, evalRunConfigErrorf(evalRunReasonSubjectUnsupported, "subject type %q is not supported", run.Spec.Subject.Type)
	}
	graders := r.graders()
	graderSetDigest, err := graders.GraderSetDigest(run.Spec.Graders)
	if err != nil {
		return nil, evalRunConfigErrorf(evalRunReasonGradersInvalid, "%v", err)
	}
	cases, err := r.loadEvalCases(ctx, run)
	if err != nil {
		return nil, err
	}
	caseSetDigest, err := evals.CaseSetDigest(cases)
	if err != nil {
		return nil, err
	}
	plan := evals.TrialPlan(cases, run.Spec.Trials)
	planDigest, err := evals.TrialPlanDigest(plan)
	if err != nil {
		return nil, err
	}
	subject, err := r.resolveEvalReviewSubject(ctx, run)
	if err != nil {
		return nil, err
	}
	snapshot := subject.snapshot()
	snapshot.CaseSetDigest = caseSetDigest
	snapshot.GraderSetDigest = graderSetDigest
	snapshot.TrialPlanDigest = planDigest
	return &evalRunObservation{Snapshot: snapshot, Cases: cases, Plan: plan, Monitor: subject.Monitor}, nil
}

func (r *EvalRunReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *EvalRunReconciler) graders() *evals.Registry {
	if r.Graders != nil {
		return r.Graders
	}
	return evals.DefaultRegistry()
}

// loadEvalCases reads, parses and selects the run's cases, and checks the
// cases × trials cap that admission cannot check without spec.cases.ids.
func (r *EvalRunReconciler) loadEvalCases(ctx context.Context, run *corev1alpha1.EvalRun) ([]evals.Case, error) {
	ref := run.Spec.Cases.ConfigMapRef
	if ref == nil {
		return nil, evalRunConfigErrorf(evalRunReasonCasesInvalid, "spec.cases.configMapRef is required")
	}
	configMap := &corev1.ConfigMap{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: ref.Name}, configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, evalRunConfigErrorf(evalRunReasonCasesInvalid, "case ConfigMap %q was not found", ref.Name)
		}
		return nil, err
	}
	data, ok := configMap.Data[ref.Key]
	if !ok {
		return nil, evalRunConfigErrorf(evalRunReasonCasesInvalid, "case ConfigMap %q has no key %q", ref.Name, ref.Key)
	}
	parsed, err := evals.ParseCases([]byte(data), run.Spec.Subject.Type)
	if err != nil {
		return nil, evalRunConfigErrorf(evalRunReasonCasesInvalid, "case ConfigMap %q key %q: %v", ref.Name, ref.Key, err)
	}
	cases, err := evals.SelectCases(parsed, run.Spec.Cases.IDs)
	if err != nil {
		return nil, evalRunConfigErrorf(evalRunReasonCasesInvalid, "%v", err)
	}
	if total := int64(len(cases)) * int64(run.Spec.Trials); run.Spec.Trials < 1 || total > corev1alpha1.EvalRunMaxCaseTrials {
		return nil, evalRunConfigErrorf(evalRunReasonCaseTrialCapExceeded,
			"%d cases × %d trials exceeds the limit of %d trials per run", len(cases), run.Spec.Trials, corev1alpha1.EvalRunMaxCaseTrials)
	}
	return cases, nil
}

func (r *EvalRunReconciler) startEvalRun(ctx context.Context, run *corev1alpha1.EvalRun, observed *evalRunObservation) error {
	now := metav1.NewTime(r.now())
	snapshot := observed.Snapshot
	run.Status.Phase = corev1alpha1.EvalRunPhaseRunning
	run.Status.Snapshot = &snapshot
	run.Status.StartTime = &now
	run.Status.Counts = corev1alpha1.EvalRunCounts{Planned: int32(len(observed.Plan))}
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               evalRunConditionCompleted,
		Status:             metav1.ConditionFalse,
		Reason:             evalRunReasonRunning,
		Message:            "the snapshot is frozen and trials are running",
		ObservedGeneration: run.Generation,
	})
	return r.Status().Update(ctx, run)
}

// runEvalTrials grades finished trials, enforces trial deadlines, starts
// trials in plan order up to maxConcurrentTrials and updates the aggregates.
func (r *EvalRunReconciler) runEvalTrials(ctx context.Context, run *corev1alpha1.EvalRun, observed *evalRunObservation) (ctrl.Result, error) {
	tasks, err := r.listEvalTrialTasks(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	records, err := r.TrialStore.ListEvalTrials(ctx, run.Namespace, run.Name, string(run.UID))
	if err != nil {
		return ctrl.Result{}, err
	}
	recorded := make(map[evals.PlannedTrial]struct{}, len(records))
	for _, record := range records {
		recorded[evals.PlannedTrial{CaseID: record.CaseID, Trial: record.Trial}] = struct{}{}
	}
	casesByID := make(map[string]evals.Case, len(observed.Cases))
	for _, c := range observed.Cases {
		casesByID[c.ID] = c
	}
	timeout := evalRunTrialTimeout(run)
	now := r.now()
	requeue := evalRunRequeueInterval
	active := 0
	for _, planned := range observed.Plan {
		task := tasks[evalTrialTaskName(run, planned.CaseID, planned.Trial)]
		if _, done := recorded[planned]; done {
			// A trial recorded at its deadline keeps running until its Task
			// is gone: deletion only requests cancellation, and the Task's
			// finalizer holds it while the prompt stops. It holds its slot
			// until then.
			if task != nil && !evalTrialTaskTerminal(task) {
				if err := r.stopEvalTrialTask(ctx, task); err != nil {
					return ctrl.Result{}, err
				}
				active++
			}
			continue
		}
		if task == nil {
			continue
		}
		task, err = r.confirmOverdueEvalTrialTask(ctx, task, task.CreationTimestamp.Add(timeout).Add(evalTrialSettleGrace), now)
		if err != nil {
			return ctrl.Result{}, err
		}
		c := casesByID[planned.CaseID]
		if err := r.validateEvalTrialTask(run, observed.Monitor, c, planned.Trial, task); err != nil {
			// Trials recorded earlier in this loop are immutable, and the
			// run is terminal after this, so its aggregates count them now.
			aggregate := aggregateEvalRun(observed.Cases, run.Spec.Trials, run.Spec.Graders, records)
			return ctrl.Result{}, r.finishEvalRun(ctx, run, &aggregate, corev1alpha1.EvalRunPhaseFailed, evalRunReasonTrialTaskConflict, err.Error())
		}
		deadline := task.CreationTimestamp.Add(timeout)
		forceDeadline := deadline.Add(evalTrialSettleGrace)
		terminal := evalTrialTaskTerminal(task)
		deadlineExceeded := !terminal && !now.Before(forceDeadline)
		if !terminal && !deadlineExceeded {
			active++
			next := deadline
			if !now.Before(deadline) {
				next = forceDeadline
			}
			requeue = min(requeue, next.Sub(now))
			continue
		}
		record, err := r.gradeEvalTrial(ctx, run, observed, c, planned.Trial, task, deadlineExceeded, deadline)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.TrialStore.CreateEvalTrial(ctx, record); err != nil {
			return ctrl.Result{}, fmt.Errorf("record trial %d of case %q: %w", planned.Trial, planned.CaseID, err)
		}
		records = append(records, *record)
		recorded[planned] = struct{}{}
		if deadlineExceeded {
			if err := r.stopEvalTrialTask(ctx, task); err != nil {
				return ctrl.Result{}, err
			}
			active++
		}
	}

	aggregate := aggregateEvalRun(observed.Cases, run.Spec.Trials, run.Spec.Graders, records)
	if evalRunInfraErrorBudgetExceeded(run, aggregate.Counts) {
		return ctrl.Result{}, r.finishEvalRun(ctx, run, &aggregate, corev1alpha1.EvalRunPhaseInvalid, evalRunReasonInfraErrorBudget,
			fmt.Sprintf("%d of %d planned trials ended in an infra error, more than maxInfraErrorPercent %d%%",
				aggregate.Counts.InfraError, aggregate.Counts.Planned, evalRunMaxInfraErrorPercent(run)))
	}
	if len(recorded) == len(observed.Plan) {
		return ctrl.Result{}, r.finishEvalRun(ctx, run, &aggregate, corev1alpha1.EvalRunPhaseSucceeded, evalRunReasonSucceeded,
			fmt.Sprintf("%d trials graded, %d infra errors excluded", aggregate.Counts.Passed+aggregate.Counts.Failed, aggregate.Counts.InfraError))
	}

	maxConcurrent := evalRunMaxConcurrentTrials(run)
	for _, planned := range observed.Plan {
		if active >= maxConcurrent {
			break
		}
		if _, done := recorded[planned]; done {
			continue
		}
		if tasks[evalTrialTaskName(run, planned.CaseID, planned.Trial)] != nil {
			continue
		}
		if err := r.createEvalTrialTask(ctx, run, observed.Monitor, casesByID[planned.CaseID], planned.Trial, timeout); err != nil {
			if configErr, ok := errors.AsType[*evalRunConfigError](err); ok {
				return ctrl.Result{}, r.finishEvalRun(ctx, run, &aggregate, corev1alpha1.EvalRunPhaseFailed, configErr.reason, configErr.message)
			}
			return ctrl.Result{}, err
		}
		active++
	}

	if err := r.updateEvalRunAggregate(ctx, run, aggregate); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: max(requeue, time.Second)}, nil
}

func evalRunTrialTimeout(run *corev1alpha1.EvalRun) time.Duration {
	if run.Spec.TrialTimeout != nil && run.Spec.TrialTimeout.Duration > 0 {
		return run.Spec.TrialTimeout.Duration
	}
	return evalRunDefaultTrialTimeout
}

func evalRunMaxConcurrentTrials(run *corev1alpha1.EvalRun) int {
	if run.Spec.MaxConcurrentTrials > 0 {
		return int(run.Spec.MaxConcurrentTrials)
	}
	return evalRunDefaultMaxConcurrentTrials
}

func evalRunMaxInfraErrorPercent(run *corev1alpha1.EvalRun) int32 {
	if run.Spec.MaxInfraErrorPercent != nil {
		return *run.Spec.MaxInfraErrorPercent
	}
	return evalRunDefaultMaxInfraErrorPct
}

// evalRunInfraErrorBudgetExceeded compares infra errors with the planned
// trial count, so a run stops as soon as the budget is spent.
func evalRunInfraErrorBudgetExceeded(run *corev1alpha1.EvalRun, counts corev1alpha1.EvalRunCounts) bool {
	return int64(counts.InfraError)*100 > int64(evalRunMaxInfraErrorPercent(run))*int64(counts.Planned)
}

// evalTrialTaskTerminal reports a trial Task that will not change its result.
// Only the Task phase decides: the dispatcher records a Succeeded execution
// outcome before delivery and cleanup finish, and either can still fail the
// Task.
// confirmOverdueEvalTrialTask re-reads, uncached, a Task the cache shows
// running past its force deadline. Trial records are immutable, so a timeout
// is recorded only if the API server agrees; the cache can lag a terminal
// status. A Task already gone keeps its cached state.
func (r *EvalRunReconciler) confirmOverdueEvalTrialTask(ctx context.Context, task *corev1alpha1.Task, forceDeadline, now time.Time) (*corev1alpha1.Task, error) {
	if evalTrialTaskTerminal(task) || now.Before(forceDeadline) {
		return task, nil
	}
	fresh := &corev1alpha1.Task{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(task), fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return task, nil
		}
		return nil, err
	}
	return fresh, nil
}

func evalTrialTaskTerminal(task *corev1alpha1.Task) bool {
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	}
	return false
}

// listEvalTrialTasks returns the run's trial Tasks by name. Only Tasks this
// run controls count, so a Task with copied labels is ignored.
func (r *EvalRunReconciler) listEvalTrialTasks(ctx context.Context, run *corev1alpha1.EvalRun) (map[string]*corev1alpha1.Task, error) {
	var list corev1alpha1.TaskList
	if err := r.List(ctx, &list, client.InNamespace(run.Namespace),
		client.MatchingLabels{labels.LabelEvalRun: labels.SelectorValue(run.Name)}); err != nil {
		return nil, err
	}
	tasks := make(map[string]*corev1alpha1.Task, len(list.Items))
	for i := range list.Items {
		task := &list.Items[i]
		if owner := metav1.GetControllerOf(task); owner == nil || owner.UID != run.UID {
			continue
		}
		tasks[task.Name] = task
	}
	return tasks, nil
}

// stopEvalTrialTask requests deletion of a running trial Task once.
func (r *EvalRunReconciler) stopEvalTrialTask(ctx context.Context, task *corev1alpha1.Task) error {
	if !task.DeletionTimestamp.IsZero() {
		return nil
	}
	return r.deleteEvalTrialTask(ctx, task)
}

func (r *EvalRunReconciler) deleteEvalTrialTask(ctx context.Context, task *corev1alpha1.Task) error {
	propagation := metav1.DeletePropagationBackground
	err := r.Delete(ctx, task, &client.DeleteOptions{
		PropagationPolicy: &propagation,
		Preconditions:     &metav1.Preconditions{UID: &task.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete trial Task %s/%s: %w", task.Namespace, task.Name, err)
	}
	return nil
}

func (r *EvalRunReconciler) createEvalTrialTask(ctx context.Context, run *corev1alpha1.EvalRun, monitor *corev1alpha1.RepositoryMonitor, c evals.Case, trial int32, timeout time.Duration) error {
	task, err := buildEvalReviewTrialTask(run, monitor, c, trial, metav1.Duration{Duration: timeout})
	if err != nil {
		return evalRunConfigErrorf(evalRunReasonSubjectInvalid, "build trial %d of case %q: %v", trial, c.ID, err)
	}
	if err := controllerutil.SetControllerReference(run, task, r.Scheme); err != nil {
		return err
	}
	err = r.Create(ctx, task)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create trial %d of case %q: %w", trial, c.ID, err)
	}
	// The cached listing can miss a trial this run created moments ago, so
	// read the Task uncached and adopt it only when it is exactly that trial.
	existing := &corev1alpha1.Task{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(task), existing); err != nil {
		return fmt.Errorf("read existing trial %d of case %q: %w", trial, c.ID, err)
	}
	if owner := metav1.GetControllerOf(existing); owner == nil || owner.UID != run.UID {
		return evalRunConfigErrorf(evalRunReasonTrialTaskConflict,
			"Task %s/%s already exists and is not a trial of this EvalRun", task.Namespace, task.Name)
	}
	if err := r.validateEvalTrialTask(run, monitor, c, trial, existing); err != nil {
		return evalRunConfigErrorf(evalRunReasonTrialTaskConflict, "%v", err)
	}
	return nil
}

// validateEvalTrialTask checks that a Task the run controls is exactly the
// trial the controller renders. Anyone who can create Tasks can set an owner
// reference, so ownership alone proves nothing.
func (r *EvalRunReconciler) validateEvalTrialTask(run *corev1alpha1.EvalRun, monitor *corev1alpha1.RepositoryMonitor, c evals.Case, trial int32, task *corev1alpha1.Task) error {
	expected, err := buildEvalReviewTrialTask(run, monitor, c, trial, metav1.Duration{Duration: evalRunTrialTimeout(run)})
	if err != nil {
		return err
	}
	// The Task path adds its own queue and workspace metadata, so only the
	// keys the trial renders are compared.
	if !evalMapContains(task.Labels, expected.Labels) || !evalMapContains(task.Annotations, expected.Annotations) {
		return fmt.Errorf("trial Task %s/%s metadata does not match the rendered trial", task.Namespace, task.Name)
	}
	// Extra metadata is allowed for the Task path's own keys, but a parent
	// Task marks a delegated child, which is granted messaging tools the
	// rendered trial does not have.
	// The runtime checks the label's presence, not its value, so an empty
	// marker counts too.
	_, parentLabel := task.Labels[labels.LabelParentTask]
	_, parentAnnotation := task.Annotations[labels.AnnotationParentTaskName]
	if parentLabel || parentAnnotation {
		return fmt.Errorf("trial Task %s/%s is marked as a delegated child", task.Namespace, task.Name)
	}
	if task.Spec.Prompt != expected.Spec.Prompt ||
		!reflect.DeepEqual(repositoryMonitorComparableReviewTaskSpec(task.Spec), repositoryMonitorComparableReviewTaskSpec(expected.Spec)) {
		return fmt.Errorf("trial Task %s/%s spec does not match the rendered trial", task.Namespace, task.Name)
	}
	// The run creates every trial after it froze the snapshot, and the
	// snapshot invalidates the run if the image, model or prompt changes
	// after that. A Task the API server created earlier may have run on any
	// configuration, even with the same Agent generation.
	if start := run.Status.StartTime; start != nil && task.CreationTimestamp.Before(start) {
		return fmt.Errorf("trial Task %s/%s was created before the run froze its snapshot", task.Namespace, task.Name)
	}
	return validateEvalTrialBinding(run, task)
}

// validateEvalTrialBinding checks that a bound trial ran on the reviewer the
// snapshot froze. A Task with the rendered spec and metadata can still have
// been bound and run earlier against another reviewer generation, and its
// result must not count for this snapshot. An unbound Task is checked again
// once it binds, before it is graded.
func validateEvalTrialBinding(run *corev1alpha1.EvalRun, task *corev1alpha1.Task) error {
	binding := task.Status.AgentExecutionBinding
	if binding == nil {
		return nil
	}
	frozen := run.Status.Snapshot.ReviewerAgent
	if frozen == nil {
		return fmt.Errorf("trial Task %s/%s is bound but the run froze no reviewer", task.Namespace, task.Name)
	}
	if binding.Task.UID != task.UID {
		return fmt.Errorf("trial Task %s/%s binding belongs to another Task", task.Namespace, task.Name)
	}
	agent := binding.Agent
	if agent == nil || agent.Namespace != task.Namespace || agent.Name != frozen.Name ||
		agent.UID != frozen.UID || agent.Generation != frozen.Generation {
		return fmt.Errorf("trial Task %s/%s is bound to a reviewer other than the frozen %s (generation %d)",
			task.Namespace, task.Name, frozen.Name, frozen.Generation)
	}
	return nil
}

func evalMapContains(actual, expected map[string]string) bool {
	for key, value := range expected {
		if got, ok := actual[key]; !ok || got != value {
			return false
		}
	}
	return true
}

// gradeEvalTrial classifies and grades one finished trial into its record.
func (r *EvalRunReconciler) gradeEvalTrial(ctx context.Context, run *corev1alpha1.EvalRun, observed *evalRunObservation, c evals.Case, trial int32, task *corev1alpha1.Task, deadlineExceeded bool, deadline time.Time) (*store.EvalTrialRecord, error) {
	signals, err := readEvalTrialSignals(ctx, r.EventStore, task)
	if err != nil {
		return nil, fmt.Errorf("read journal of trial Task %s/%s: %w", task.Namespace, task.Name, err)
	}
	result, err := r.readEvalTrialResult(ctx, task, deadlineExceeded)
	if err != nil {
		return nil, err
	}
	outcome := evals.TrialOutcome{InfraClass: classifyEvalTrialInfra(task, signals, result, deadlineExceeded)}
	if outcome.InfraClass == "" {
		outcome.RepositoryMonitorReview = evalReviewTrialOutcome(observed.Monitor, c, task, result, deadlineExceeded)
	}
	grade, err := r.graders().GradeTrial(c, run.Spec.Graders, outcome)
	if err != nil {
		return nil, err
	}

	completedAt := deadline
	if !deadlineExceeded && task.Status.CompletionTime != nil {
		completedAt = task.Status.CompletionTime.Time
	} else if !deadlineExceeded {
		completedAt = r.now()
	}
	startedAt := task.CreationTimestamp.UTC()
	latency := max(completedAt.Sub(startedAt).Milliseconds(), 0)
	snapshot := run.Status.Snapshot
	record := &store.EvalTrialRecord{
		SchemaVersion: store.EvalTrialSchemaVersion,
		Namespace:     run.Namespace,
		EvalRun:       run.Name,
		EvalRunUID:    string(run.UID),
		CaseID:        c.ID,
		Trial:         trial,
		TaskName:      task.Name,
		TaskUID:       string(task.UID),
		Outcome:       string(grade.Outcome),
		InfraClass:    string(grade.InfraClass),
		InputTokens:   signals.InputTokens,
		OutputTokens:  signals.OutputTokens,
		ToolCalls:     &signals.ToolCalls,
		LatencyMillis: &latency,
		Snapshot: store.EvalTrialSnapshot{
			MonitorUID:                 string(snapshot.MonitorUID),
			MonitorGeneration:          snapshot.MonitorGeneration,
			RuntimeImageDigest:         snapshot.RuntimeImageDigest,
			Model:                      snapshot.Model,
			PromptTemplateDigest:       snapshot.PromptTemplateDigest,
			ReviewerSystemPromptDigest: snapshot.ReviewerSystemPromptDigest,
			CaseSetDigest:              snapshot.CaseSetDigest,
			GraderSetDigest:            snapshot.GraderSetDigest,
			TrialPlanDigest:            snapshot.TrialPlanDigest,
		},
		StartedAt:   &startedAt,
		CompletedAt: completedAt.UTC(),
	}
	if snapshot.ReviewerAgent != nil {
		record.Snapshot.ReviewerAgentUID = string(snapshot.ReviewerAgent.UID)
		record.Snapshot.ReviewerGeneration = snapshot.ReviewerAgent.Generation
	}
	if task.Status.Execution != nil {
		record.RuntimeSessionUID = task.Status.Execution.RuntimeSessionUID
	}
	if review := outcome.RepositoryMonitorReview; review != nil {
		record.Verdict = review.Verdict
		record.SkipReason = review.SkipReason
		record.GateReason = review.GateReason
		if review.Result != nil {
			record.ReviewerVerdict = review.Result.Verdict
		}
	}
	for _, result := range grade.Results {
		record.Grades = append(record.Grades, store.EvalTrialGrade{
			Grader: string(result.Grader),
			Status: string(result.Status),
			Score:  result.Score,
			Reason: evalTrialBoundedReason(result.Reason),
		})
	}
	return record, nil
}

// evalTrialMaxReasonBytes keeps grader reasons, which may quote model
// output, from filling a trial record.
const evalTrialMaxReasonBytes = 1024

func evalTrialBoundedReason(reason string) string {
	if len(reason) <= evalTrialMaxReasonBytes {
		return reason
	}
	cut := evalTrialMaxReasonBytes
	for cut > 0 && !utf8RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// readEvalTrialResult reads the stored result of a trial that succeeded
// within its deadline. Other trials are graded without one.
func (r *EvalRunReconciler) readEvalTrialResult(ctx context.Context, task *corev1alpha1.Task, deadlineExceeded bool) (evalTrialResult, error) {
	if deadlineExceeded || task.Status.Phase != corev1alpha1.TaskPhaseSucceeded ||
		task.Status.ResultRef == nil || !task.Status.ResultRef.Available {
		return evalTrialResult{}, nil
	}
	raw, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return evalTrialResult{}, nil
		}
		return evalTrialResult{}, fmt.Errorf("get result of trial Task %s/%s: %w", task.Namespace, task.Name, err)
	}
	return evalTrialResult{Raw: raw, Found: true}, nil
}

// evalReviewTrialOutcome is what production would record for a trial that
// is not an infra error.
func evalReviewTrialOutcome(monitor *corev1alpha1.RepositoryMonitor, c evals.Case, task *corev1alpha1.Task, result evalTrialResult, deadlineExceeded bool) *evals.ReviewTrialOutcome {
	failed := func(skipReason string) *evals.ReviewTrialOutcome {
		return &evals.ReviewTrialOutcome{Verdict: repositoryMonitorReviewVerdictFailed, SkipReason: skipReason}
	}
	switch {
	case deadlineExceeded, task.Status.Phase == corev1alpha1.TaskPhaseFailed:
		return failed(repositoryMonitorReviewSkipReasonTaskFailed)
	case task.Status.Phase == corev1alpha1.TaskPhaseCancelled:
		return failed(repositoryMonitorReviewSkipReasonTaskCancelled)
	case task.Status.Phase != corev1alpha1.TaskPhaseSucceeded:
		return failed(repositoryMonitorReviewSkipReasonTaskFailed)
	case !result.Found:
		return failed(repositoryMonitorReviewSkipReasonMissingResult)
	}
	return evalReviewOutcomeFromResult(result.Raw, monitor, c, task)
}

func (r *EvalRunReconciler) updateEvalRunAggregate(ctx context.Context, run *corev1alpha1.EvalRun, aggregate evalRunAggregate) error {
	if reflect.DeepEqual(run.Status.Counts, aggregate.Counts) &&
		reflect.DeepEqual(run.Status.Results, aggregate.Results) &&
		reflect.DeepEqual(run.Status.SuspectCases, aggregate.SuspectCases) {
		return nil
	}
	run.Status.Counts = aggregate.Counts
	run.Status.Results = aggregate.Results
	run.Status.SuspectCases = aggregate.SuspectCases
	return r.Status().Update(ctx, run)
}

// finishEvalRun records a terminal phase. A nil aggregate keeps the counts
// and results from the last update.
func (r *EvalRunReconciler) finishEvalRun(ctx context.Context, run *corev1alpha1.EvalRun, aggregate *evalRunAggregate, phase corev1alpha1.EvalRunPhase, reason, message string) error {
	if aggregate != nil {
		run.Status.Counts = aggregate.Counts
		run.Status.Results = aggregate.Results
		run.Status.SuspectCases = aggregate.SuspectCases
	}
	now := metav1.NewTime(r.now())
	run.Status.Phase = phase
	run.Status.CompletionTime = &now
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               evalRunConditionCompleted,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            evalRunConditionMessage(message),
		ObservedGeneration: run.Generation,
	})
	logf.FromContext(ctx).Info("EvalRun finished", "evalRun", run.Name, "phase", phase, "reason", reason)
	return r.Status().Update(ctx, run)
}

// evalRunConditionMessage bounds a condition message to the API limit.
func evalRunConditionMessage(message string) string {
	const limit = 32 * 1024
	if len(message) <= limit {
		return message
	}
	return message[:limit]
}

// SetupWithManager registers the EvalRun controller.
func (r *EvalRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.TrialStore == nil || r.ResultStore == nil || r.EventStore == nil {
		return fmt.Errorf("EvalRun controller requires trial, result and execution event stores")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.EvalRun{}).
		Owns(&corev1alpha1.Task{}).
		Named("evalrun").
		Complete(r)
}
