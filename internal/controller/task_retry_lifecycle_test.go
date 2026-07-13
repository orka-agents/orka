/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const (
	retryOutputResultOperation = "result"
	retryLifecycleEchoCommand  = "echo"
	retryCancellationMessage   = "cancelled by user"
	retrySessionQuestion       = "question"
	retryOldTaskUID            = "old-task-uid"
	retryNewTaskUID            = "new-task-uid"
)

func retryLifecycleTask(name string, phase corev1alpha1.TaskPhase, attempts int32, jobName string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  defaultNS,
			UID:        types.UID(name + "-uid"),
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeContainer,
			Image:       testBusyboxImage,
			Command:     []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 2},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    phase,
			Attempts: attempts,
			JobName:  jobName,
		},
	}
}

func markRetryCleanupCompleteForTest(task *corev1alpha1.Task) {
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:   ConditionTypeRetryCleanup,
		Status: metav1.ConditionFalse,
		Reason: retryCleanupCompleteReason,
	})
}

type resultReadObserver struct {
	store.ResultStore
	reader         client.Reader
	key            client.ObjectKey
	reads          int
	observedPhases []corev1alpha1.TaskPhase
	observedClaims []bool
}

type failingTaskSessionFinalizerStore struct {
	store.SessionStore
	err error
}

type failingResultReadStore struct {
	store.ResultStore
	err error
}

type policyRemovingResultStore struct {
	store.ResultStore
	client client.Client
	key    client.ObjectKey
	done   bool
}

type staleSameNameLockRaceStore struct {
	store.SessionStore
}

func (s staleSameNameLockRaceStore) IsTaskLocked(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func (s staleSameNameLockRaceStore) AcquireTaskLock(context.Context, string, string, string, string) error {
	return store.ErrConflict
}

func (s staleSameNameLockRaceStore) ReleaseTaskLock(ctx context.Context, namespace, name, taskName, _ string) error {
	return s.ReleaseLock(ctx, namespace, name, taskName)
}

func (s *policyRemovingResultStore) DeleteResult(ctx context.Context, namespace, taskName string) error {
	if !s.done {
		latest := &corev1alpha1.Task{}
		if err := s.client.Get(ctx, s.key, latest); err != nil {
			return err
		}
		latest.Spec.RetryPolicy = nil
		if err := s.client.Update(ctx, latest); err != nil {
			return err
		}
		s.done = true
	}
	return s.ResultStore.DeleteResult(ctx, namespace, taskName)
}

func (s failingResultReadStore) GetResult(context.Context, string, string) ([]byte, error) {
	return nil, s.err
}

func (s failingTaskSessionFinalizerStore) FinalizeTaskSession(context.Context, string, string, string, string, []store.SessionMessage) error {
	return s.err
}

func (s *resultReadObserver) GetResult(ctx context.Context, namespace, taskName string) ([]byte, error) {
	s.reads++
	task := &corev1alpha1.Task{}
	if err := s.reader.Get(ctx, s.key, task); err == nil {
		s.observedPhases = append(s.observedPhases, task.Status.Phase)
		s.observedClaims = append(s.observedClaims, meta.IsStatusConditionTrue(task.Status.Conditions, ConditionTypeTerminalTransition))
	}
	return s.ResultStore.GetResult(ctx, namespace, taskName)
}

func TestCompleteTaskDoesNotClobberNewerRetryAttempt(t *testing.T) {
	scheme := newTestScheme()
	latest := retryLifecycleTask("audit-stale-completion", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	stale := latest.DeepCopy()
	stale.Status.Attempts = 1
	stale.Status.JobName = "attempt-1-job"
	r := newUnitReconciler(scheme, latest)
	observer := &resultReadObserver{
		ResultStore: r.ResultStore,
		reader:      r.Client,
		key:         client.ObjectKeyFromObject(latest),
	}
	r.ResultStore = observer
	if err := r.PlanStore.SavePlan(context.Background(), latest.Namespace, latest.Name, &store.PlanState{Summary: "newer attempt plan"}); err != nil {
		t.Fatalf("SavePlan() error = %v", err)
	}

	if _, err := r.completeTask(context.Background(), stale, corev1alpha1.TaskPhaseFailed, "stale attempt failed"); err != nil {
		t.Fatalf("completeTask() error = %v", err)
	}

	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(latest), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseRunning || got.Status.Attempts != 2 || got.Status.JobName != "attempt-2-job" {
		t.Fatalf("newer attempt was clobbered: status = %#v", got.Status)
	}
	if observer.reads != 0 {
		t.Fatalf("stale completion read result %d times", observer.reads)
	}
	if _, err := r.PlanStore.GetPlan(context.Background(), latest.Namespace, latest.Name); err != nil {
		t.Fatalf("stale completion deleted newer attempt plan: %v", err)
	}
}

func TestCompleteTaskClaimsTerminalTransitionBeforeResultCollection(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-claim-order", corev1alpha1.TaskPhaseRunning, 1, "attempt-1-job")
	r := newUnitReconciler(scheme, task)
	observer := &resultReadObserver{
		ResultStore: r.ResultStore,
		reader:      r.Client,
		key:         client.ObjectKeyFromObject(task),
	}
	r.ResultStore = observer

	if _, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseSucceeded, "done"); err != nil {
		t.Fatalf("completeTask() error = %v", err)
	}
	if observer.reads == 0 {
		t.Fatal("completeTask() did not attempt result collection")
	}
	for i, claimed := range observer.observedClaims {
		if !claimed {
			t.Fatalf("result collection %d observed no durable terminal claim (phase %q)", i, observer.observedPhases[i])
		}
	}
}

func TestTerminalFinalizationResumesWithoutDuplicatingSessionMessages(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-finalization-resume", corev1alpha1.TaskPhaseRunning, 1, "attempt-1-job")
	task.Spec.Prompt = retrySessionQuestion
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "terminal-session", Create: true, Append: true}
	r := newUnitReconciler(scheme, task)
	if err := r.SessionManager.AcquireLock(context.Background(), task); err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("answer")); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	base, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("unit client does not implement client.WithWatch")
	}
	statusUpdates := 0
	finalizeErr := errors.New("injected terminal status finalization failure")
	fc := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && subResourceName == testStatusSubresource {
				statusUpdates++
				if statusUpdates == 2 {
					return finalizeErr
				}
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
	r.Client = fc
	r.APIReader = fc

	if _, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseSucceeded, "done"); !errors.Is(err, finalizeErr) {
		t.Fatalf("completeTask() error = %v, want %v", err, finalizeErr)
	}
	intermediate := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), intermediate); err != nil {
		t.Fatalf("Get(intermediate Task) error = %v", err)
	}
	if intermediate.Status.Phase != corev1alpha1.TaskPhaseRunning || !meta.IsStatusConditionTrue(intermediate.Status.Conditions, ConditionTypeTerminalTransition) {
		t.Fatalf("terminal claim was not left resumable: %#v", intermediate.Status)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	final := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), final); err != nil {
		t.Fatalf("Get(final Task) error = %v", err)
	}
	if final.Status.Phase != corev1alpha1.TaskPhaseSucceeded || final.Status.ResultRef == nil || !final.Status.ResultRef.Available {
		t.Fatalf("terminal finalization did not recover: %#v", final.Status)
	}
	messages, err := r.SessionManager.LoadTranscript(context.Background(), task)
	if err != nil {
		t.Fatalf("LoadTranscript() error = %v", err)
	}
	if len(messages) != 2 || messages[0].Content != retrySessionQuestion || messages[1].Content != "answer" {
		t.Fatalf("session messages = %#v, want one prompt/result pair", messages)
	}
	session, err := r.SessionManager.GetSession(context.Background(), task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.ActiveTask != "" {
		t.Fatalf("session lock remained held by %q", session.ActiveTask)
	}
}

func TestTerminalSessionFailureDoesNotBlockCancellationJobCleanup(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-session-cleanup", corev1alpha1.TaskPhaseRunning, 1, "terminal-session-job")
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "terminal-session"}
	job := retryTestJob(task, task.Status.JobName, "terminal-session-job-uid")
	job.Status.Active = 1
	r := newUnitReconciler(scheme, task, job)
	baseSessionStore := r.SessionManager.store
	if err := baseSessionStore.CreateSession(context.Background(), &store.SessionRecord{
		Namespace:     task.Namespace,
		Name:          task.Spec.SessionRef.Name,
		SessionType:   taskSessionType,
		ActiveTask:    task.Name,
		ActiveTaskUID: string(task.UID),
	}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	sessionErr := errors.New("injected session finalization failure")
	r.SessionManager = NewSessionManager(failingTaskSessionFinalizerStore{
		SessionStore: baseSessionStore,
		err:          sessionErr,
	})

	if _, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseCancelled, retryCancellationMessage); !errors.Is(err, sessionErr) {
		t.Fatalf("completeTask() error = %v, want %v", err, sessionErr)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cancelled Job still exists after session failure: %v", err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseRunning || !meta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeTerminalTransition) {
		t.Fatalf("terminal claim was not left resumable after session failure: %#v", got.Status)
	}
}

func TestTerminalNoResultCheckpointSurvivesCleanupAndResume(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-no-result-resume", corev1alpha1.TaskPhaseRunning, 1, "terminal-no-result-job")
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "terminal-no-result-session"}
	job := retryTestJob(task, task.Status.JobName, "terminal-no-result-job-uid")
	job.Status.Active = 1
	r := newUnitReconciler(scheme, task, job)
	baseSessionStore := r.SessionManager.store
	if err := baseSessionStore.CreateSession(context.Background(), &store.SessionRecord{
		Namespace:     task.Namespace,
		Name:          task.Spec.SessionRef.Name,
		SessionType:   taskSessionType,
		ActiveTask:    task.Name,
		ActiveTaskUID: string(task.UID),
	}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	sessionErr := errors.New("injected session finalization failure")
	r.SessionManager = NewSessionManager(failingTaskSessionFinalizerStore{
		SessionStore: baseSessionStore,
		err:          sessionErr,
	})

	if _, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseCancelled, retryCancellationMessage); !errors.Is(err, sessionErr) {
		t.Fatalf("completeTask() error = %v, want %v", err, sessionErr)
	}
	intermediate := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), intermediate); err != nil {
		t.Fatalf("Get(intermediate Task) error = %v", err)
	}
	if !meta.IsStatusConditionTrue(intermediate.Status.Conditions, ConditionTypeTerminalResultCollection) {
		t.Fatalf("terminal result collection was not checkpointed: %#v", intermediate.Status.Conditions)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cancelled Job still exists after session failure: %v", err)
	}

	readErr := errors.New("result collection should not rerun after checkpoint")
	r.ResultStore = failingResultReadStore{ResultStore: r.ResultStore, err: readErr}
	r.SessionManager = NewSessionManager(baseSessionStore)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	final := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), final); err != nil {
		t.Fatalf("Get(final Task) error = %v", err)
	}
	if final.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		t.Fatalf("terminal transition did not recover: %#v", final.Status)
	}
}

func TestAcquireSessionLockDoesNotAcceptSameNameDifferentUIDAfterRace(t *testing.T) {
	task := retryLifecycleTask("session-lock-race", corev1alpha1.TaskPhasePending, 0, "")
	task.UID = retryNewTaskUID
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session-lock-race-session"}
	r := newUnitReconciler(newTestScheme())
	baseStore := r.SessionManager.store
	if err := baseStore.CreateSession(context.Background(), &store.SessionRecord{
		Namespace:     task.Namespace,
		Name:          task.Spec.SessionRef.Name,
		SessionType:   taskSessionType,
		ActiveTask:    task.Name,
		ActiveTaskUID: retryOldTaskUID,
	}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	r.SessionManager = NewSessionManager(staleSameNameLockRaceStore{SessionStore: baseStore})

	result, err, done := r.acquireSessionLock(context.Background(), task)
	if err != nil {
		t.Fatalf("acquireSessionLock() error = %v", err)
	}
	if !done || result.RequeueAfter == 0 {
		t.Fatalf("same-name different-UID lock was accepted: result=%#v done=%t", result, done)
	}
}

func TestTerminalResultReadFailureKeepsSessionLockForRetry(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-result-read-failure", corev1alpha1.TaskPhaseRunning, 1, "")
	task.Spec.Prompt = retrySessionQuestion
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "result-read-session", Create: true, Append: true}
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	r := newUnitReconciler(scheme, task)
	if err := r.SessionManager.AcquireLock(context.Background(), task); err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	readErr := errors.New("injected result read failure")
	r.ResultStore = failingResultReadStore{ResultStore: r.ResultStore, err: readErr}

	if _, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseSucceeded, "done"); !errors.Is(err, readErr) {
		t.Fatalf("completeTask() error = %v, want %v", err, readErr)
	}
	session, err := r.SessionManager.GetSession(context.Background(), task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.ActiveTask != task.Name || session.MessageCount != 0 {
		t.Fatalf("session finalized with incomplete transcript: %#v", session)
	}
}

func TestStaleTaskUIDCannotFinalizeReusedSessionLock(t *testing.T) {
	scheme := newTestScheme()
	oldTask := retryLifecycleTask("reused-session-task", corev1alpha1.TaskPhaseRunning, 1, "")
	oldTask.UID = retryOldTaskUID
	oldTask.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "reused-session", Create: true}
	r := newUnitReconciler(scheme)
	if err := r.SessionManager.AcquireLock(context.Background(), oldTask); err != nil {
		t.Fatalf("AcquireLock(old) error = %v", err)
	}
	if err := r.SessionManager.ReleaseLock(context.Background(), oldTask); err != nil {
		t.Fatalf("ReleaseLock(old) error = %v", err)
	}
	newTask := oldTask.DeepCopy()
	newTask.UID = retryNewTaskUID
	if err := r.SessionManager.AcquireLock(context.Background(), newTask); err != nil {
		t.Fatalf("AcquireLock(new) error = %v", err)
	}

	if err := r.SessionManager.FinalizeTask(context.Background(), oldTask, r.ResultStore); err != nil {
		t.Fatalf("FinalizeTask(stale) error = %v", err)
	}
	session, err := r.SessionManager.GetSession(context.Background(), oldTask.Namespace, oldTask.Spec.SessionRef.Name)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.ActiveTask != newTask.Name || session.ActiveTaskUID != string(newTask.UID) {
		t.Fatalf("stale Task finalized new lock owner: %#v", session)
	}
}

func TestLegacySessionLockBackfillsTaskUIDDuringFinalization(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("legacy-session-owner", corev1alpha1.TaskPhaseRunning, 1, "")
	task.UID = "legacy-session-task-uid"
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "legacy-session"}
	r := newUnitReconciler(scheme)
	now := time.Now()
	if err := r.SessionManager.store.CreateSession(context.Background(), &store.SessionRecord{
		Namespace:   task.Namespace,
		Name:        task.Spec.SessionRef.Name,
		SessionType: taskSessionType,
		ActiveTask:  task.Name,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	if err := r.SessionManager.FinalizeTask(context.Background(), task, r.ResultStore); err != nil {
		t.Fatalf("FinalizeTask() error = %v", err)
	}
	session, err := r.SessionManager.GetSession(context.Background(), task.Namespace, task.Spec.SessionRef.Name)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if session.ActiveTask != "" || session.ActiveTaskUID != "" {
		t.Fatalf("legacy session lock was not finalized: %#v", session)
	}
}

func TestHandleCompletedCleansCrashWindowRetryReplacement(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("audit-cancelled-orphan", corev1alpha1.TaskPhaseCancelled, 1, "")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "old-source-uid",
	}
	replacement := retryTestJob(task, buildTaskJobName(task), "replacement-uid")
	replacement.Status.Active = 1
	r := newUnitReconciler(scheme, task, replacement)

	if _, err := r.handleCompleted(context.Background(), task); err != nil {
		t.Fatalf("handleCompleted() error = %v", err)
	}

	got := &batchv1.Job{}
	err := r.Get(context.Background(), client.ObjectKeyFromObject(replacement), got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("untracked retry replacement still exists after cancellation: err=%v job=%#v", err, got)
	}
}

func TestHandleCompletedCleansCrashWindowRetryReplacementAfterGateClear(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("cancelled-gate-cleared-orphan", corev1alpha1.TaskPhaseCancelled, 1, "")
	replacement := retryTestJob(task, buildTaskJobName(task), "replacement-uid")
	replacement.Status.Active = 1
	r := newUnitReconciler(scheme, task, replacement)

	if _, err := r.handleCompleted(context.Background(), task); err != nil {
		t.Fatalf("handleCompleted() error = %v", err)
	}

	err := r.Get(context.Background(), client.ObjectKeyFromObject(replacement), &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("gate-cleared retry replacement still exists after cancellation: err=%v", err)
	}
}

func TestHandleCompletedWaitsForRecreatedRetryReplacement(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("cancelled-recreated-replacement", corev1alpha1.TaskPhaseCancelled, 1, "")
	replacement := retryTestJob(task, buildTaskJobName(task), "replacement-uid")
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, replacement).Build()
	recreated := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			job, ok := obj.(*batchv1.Job)
			if !ok || recreated {
				return c.Delete(ctx, obj, opts...)
			}
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			if err := c.Create(ctx, retryTestJob(task, job.Name, "recreated-replacement-uid")); err != nil {
				return err
			}
			recreated = true
			return nil
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.APIReader = fc

	result, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("handleCompleted() error = %v", err)
	}
	if !recreated || result.RequeueAfter != 2*time.Second {
		t.Fatalf("handleCompleted() result = %#v, recreated=%t; want cleanup wait", result, recreated)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: replacement.Name, Namespace: replacement.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("recreated replacement was not preserved for the next cleanup pass: %v", err)
	}
}

func TestHandleCompletedIgnoresUncontrolledReplacementNameCollision(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-foreign-collision", corev1alpha1.TaskPhaseCancelled, 1, "")
	foreignTask := task.DeepCopy()
	foreignTask.Name = "foreign-owner"
	foreignTask.UID = "foreign-owner-uid"
	foreign := retryTestJob(foreignTask, buildTaskJobName(task), "foreign-collision-uid")
	r := newUnitReconciler(scheme, task, foreign)

	if _, err := r.handleCompleted(context.Background(), task); err != nil {
		t.Fatalf("handleCompleted() error = %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &batchv1.Job{}); err != nil {
		t.Fatalf("uncontrolled collision Job was deleted: %v", err)
	}
}

func TestRetryClearsPriorAttemptOutputs(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("audit-stale-result", corev1alpha1.TaskPhaseRunning, 1, "missing-source-job")
	r := newUnitReconciler(scheme, task)
	artifactStore, ok := r.ResultStore.(store.ArtifactStore)
	if !ok {
		t.Fatal("unit ResultStore does not implement ArtifactStore")
	}
	r.ArtifactStore = artifactStore
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("attempt-1 partial error")); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
	if err := r.ArtifactStore.SaveArtifact(context.Background(), task.Namespace, task.Name, "attempt-1.txt", "text/plain", []byte("stale artifact")); err != nil {
		t.Fatalf("SaveArtifact() error = %v", err)
	}

	if _, err := r.retryTask(context.Background(), task); err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}

	pending := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), pending); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	pending.Status.Phase = corev1alpha1.TaskPhaseRunning
	pending.Status.Attempts = 2
	pending.Status.JobName = "attempt-2-job"
	pending.Status.ResultRef = nil
	if err := r.collectResult(context.Background(), pending); err != nil {
		t.Fatalf("collectResult() error = %v", err)
	}
	if pending.Status.ResultRef != nil && pending.Status.ResultRef.Available {
		data, getErr := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name)
		t.Fatalf("retry adopted a prior-attempt result: result=%q err=%v", data, getErr)
	}
	if _, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("prior-attempt result was not cleared: err=%v", err)
	}
	artifacts, err := r.ArtifactStore.ListArtifacts(context.Background(), task.Namespace, task.Name)
	if err != nil {
		t.Fatalf("ListArtifacts() error = %v", err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("prior-attempt artifacts were not cleared: %#v", artifacts)
	}
}

func TestLegacyPendingRetryGateClearsPriorAttemptOutputs(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("legacy-pending-stale-output", corev1alpha1.TaskPhasePending, 1, "missing-source-job")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore:    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:       "",
		labels.AnnotationRetryCleanupState: "complete",
	}
	r := newUnitReconciler(scheme, task)
	artifactStore, ok := r.ResultStore.(store.ArtifactStore)
	if !ok {
		t.Fatal("unit ResultStore does not implement ArtifactStore")
	}
	r.ArtifactStore = artifactStore
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("legacy result")); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
	if err := r.ArtifactStore.SaveArtifact(context.Background(), task.Namespace, task.Name, "legacy.txt", "text/plain", []byte("legacy artifact")); err != nil {
		t.Fatalf("SaveArtifact() error = %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy prior-attempt result was not cleared: %v", err)
	}
	artifacts, err := r.ArtifactStore.ListArtifacts(context.Background(), task.Namespace, task.Name)
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("legacy prior-attempt artifacts = %#v, %v", artifacts, err)
	}
}

func TestRetryOutputCleanupFailureKeepsCurrentAttemptRunning(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("retry-output-cleanup-failure", corev1alpha1.TaskPhaseRunning, 1, "missing-source-job")
	r := newUnitReconciler(scheme, task)
	artifactStore, ok := r.ResultStore.(store.ArtifactStore)
	if !ok {
		t.Fatal("unit ResultStore does not implement ArtifactStore")
	}
	cleanupErr := errors.New("injected retry output cleanup failure")
	failingStore := &failOnceTaskCleanupStore{
		ResultStore:   r.ResultStore,
		ArtifactStore: artifactStore,
		failOperation: retryOutputResultOperation,
		failErr:       cleanupErr,
	}
	r.ResultStore = failingStore
	r.ArtifactStore = failingStore

	if _, err := r.retryTask(context.Background(), task); !errors.Is(err, cleanupErr) {
		t.Fatalf("retryTask() error = %v, want %v", err, cleanupErr)
	}

	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseRunning || got.Status.Attempts != 1 || got.Status.JobName != "missing-source-job" {
		t.Fatalf("retry advanced despite output cleanup failure: %#v", got.Status)
	}
	if got.Annotations[labels.AnnotationRetryNotBefore] == "" {
		t.Fatal("retry gate was not retained after output cleanup failure")
	}
}

func TestRetryCleanupClaimCompletesAfterPolicyRemoval(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("retry-cleanup-policy-change", corev1alpha1.TaskPhaseRunning, 1, "missing-source-job")
	r := newUnitReconciler(scheme, task)
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("stale")); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
	removingStore := &policyRemovingResultStore{
		ResultStore: r.ResultStore,
		client:      r.Client,
		key:         client.ObjectKeyFromObject(task),
	}
	r.ResultStore = removingStore

	if _, err := r.retryTask(context.Background(), task); err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if !removingStore.done || got.Status.Phase != corev1alpha1.TaskPhasePending || !retryCleanupComplete(got) {
		t.Fatalf("claimed cleanup was stranded after policy removal: status=%#v policyRemoved=%t", got.Status, removingStore.done)
	}
}

func TestConcurrentCancellationBeforeRetryCleanupClaimPreservesOutputs(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("retry-cleanup-cancel-race", corev1alpha1.TaskPhaseRunning, 1, "missing-source-job")
	r := newUnitReconciler(scheme, task)
	artifactStore, ok := r.ResultStore.(store.ArtifactStore)
	if !ok {
		t.Fatal("unit ResultStore does not implement ArtifactStore")
	}
	r.ArtifactStore = artifactStore
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("preserve result")); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
	if err := r.ArtifactStore.SaveArtifact(context.Background(), task.Namespace, task.Name, "preserve.txt", "text/plain", []byte("preserve artifact")); err != nil {
		t.Fatalf("SaveArtifact() error = %v", err)
	}

	base, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("unit client does not implement client.WithWatch")
	}
	cancelled := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && subResourceName == testStatusSubresource && !cancelled {
				cancelled = true
				latest := &corev1alpha1.Task{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
					return err
				}
				latest.Status.Phase = corev1alpha1.TaskPhaseCancelled
				latest.Status.Message = retryCancellationMessage
				if err := c.SubResource(subResourceName).Update(ctx, latest); err != nil {
					return err
				}
				return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: testTaskResource}, obj.GetName(), errors.New("cancelled before cleanup claim"))
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
	r.Client = fc
	r.APIReader = fc

	if _, err := r.retryTask(context.Background(), task); err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if !cancelled {
		t.Fatal("did not inject concurrent cancellation")
	}
	if data, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name); err != nil || string(data) != "preserve result" {
		t.Fatalf("result after cancelled cleanup claim = %q, %v", data, err)
	}
	artifacts, err := r.ArtifactStore.ListArtifacts(context.Background(), task.Namespace, task.Name)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts after cancelled cleanup claim = %#v, %v", artifacts, err)
	}
}

func TestRetryCleanupCannotClaimWhileTerminalTransitionOwnsAttempt(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("retry-vs-terminal-claim", corev1alpha1.TaskPhaseRunning, 1, "missing-source-job")
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:    ConditionTypeTerminalTransition,
		Status:  metav1.ConditionTrue,
		Reason:  taskCancelledReason,
		Message: retryCancellationMessage,
	})
	r := newUnitReconciler(scheme, task)
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("preserve result")); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	if _, err := r.retryTask(context.Background(), task); err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if data, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name); err != nil || string(data) != "preserve result" {
		t.Fatalf("terminal-owned result was deleted: %q, %v", data, err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeRetryCleanup) {
		t.Fatalf("retry cleanup claimed terminal-owned attempt: %#v", got.Status.Conditions)
	}
}

func TestRunningLegacyRetryGateBackfillsCleanupState(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("running-legacy-retry-gate", corev1alpha1.TaskPhaseRunning, 1, "missing-source-job")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	r := newUnitReconciler(scheme, task)

	if _, err := r.retryTask(context.Background(), task); err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhasePending || !retryCleanupComplete(got) {
		t.Fatalf("legacy retry gate did not recover: status=%#v annotations=%#v", got.Status, got.Annotations)
	}
}

func TestRetryResumesWhenPendingTransitionFailsAfterCleanup(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("retry-cleanup-before-pending", corev1alpha1.TaskPhaseRunning, 1, "missing-source-job")
	r := newUnitReconciler(scheme, task)
	base, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("unit client does not implement client.WithWatch")
	}
	statusUpdates := 0
	resetErr := errors.New("injected pending status failure")
	fc := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && subResourceName == testStatusSubresource {
				statusUpdates++
				if statusUpdates == 2 {
					return resetErr
				}
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
	r.Client = fc
	r.APIReader = fc

	if _, err := r.retryTask(context.Background(), task); !errors.Is(err, resetErr) {
		t.Fatalf("retryTask() error = %v, want %v", err, resetErr)
	}
	intermediate := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), intermediate); err != nil {
		t.Fatalf("Get(intermediate Task) error = %v", err)
	}
	if intermediate.Status.Phase != corev1alpha1.TaskPhaseRunning ||
		!meta.IsStatusConditionTrue(intermediate.Status.Conditions, ConditionTypeRetryCleanup) {
		t.Fatalf("retry cleanup claim was not left resumable: %#v annotations=%#v", intermediate.Status, intermediate.Annotations)
	}

	if _, err := r.retryTask(context.Background(), intermediate); err != nil {
		t.Fatalf("recovery retryTask() error = %v", err)
	}
	final := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), final); err != nil {
		t.Fatalf("Get(final Task) error = %v", err)
	}
	if final.Status.Phase != corev1alpha1.TaskPhasePending || final.Status.Attempts != 1 {
		t.Fatalf("retry did not resume to Pending: %#v", final.Status)
	}
}

func TestAmbiguousRetryJobCreateAdoptsCreatedJob(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("audit-ambiguous-create", corev1alpha1.TaskPhasePending, 1, "")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	injected := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok && !injected {
				injected = true
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				return apierrors.NewTimeoutError("ambiguous create response", 1)
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.APIReader = fc
	r.JobBuilder = NewJobBuilder(fc)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !injected {
		t.Fatal("did not inject ambiguous Job create")
	}

	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseRunning || got.Status.Attempts != 2 || got.Status.JobName != buildTaskJobName(task) {
		var jobs batchv1.JobList
		_ = r.List(context.Background(), &jobs, client.InNamespace(task.Namespace))
		t.Fatalf("ambiguous create was not adopted: status=%#v jobs=%#v", got.Status, jobs.Items)
	}
	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: got.Status.JobName, Namespace: got.Namespace}, job); err != nil {
		t.Fatalf("Get(adopted Job) error = %v", err)
	}
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		t.Fatal("adopted retry Job remained suspended after the Task tracked it")
	}
}

func TestAmbiguousRetryJobCreateWithoutVisibleJobDoesNotFailTask(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("ambiguous-create-not-visible", corev1alpha1.TaskPhasePending, 1, "")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return apierrors.NewTimeoutError("ambiguous create response", 1)
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.APIReader = fc
	r.JobBuilder = NewJobBuilder(fc)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err == nil {
		t.Fatal("Reconcile() error = nil, want ambiguous create error for retry")
	}

	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhasePending || got.Status.Attempts != 1 || got.Status.JobName != "" {
		t.Fatalf("ambiguous create failed or advanced task: %#v", got.Status)
	}
}

func TestRetryGenerationIsRevalidatedImmediatelyBeforeCreate(t *testing.T) {
	scheme := newTestScheme()
	stale := retryLifecycleTask("retry-generation-preflight", corev1alpha1.TaskPhasePending, 1, "")
	stale.Generation = 1
	stale.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	markRetryCleanupCompleteForTest(stale)
	latest := stale.DeepCopy()
	latest.Generation = 2
	latest.Spec.Command = []string{retryLifecycleEchoCommand, "updated"}

	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(latest).Build()
	createCalled := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				createCalled = true
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.APIReader = fc
	expected, ok := retryGateExpectationFromTask(stale)
	if !ok {
		t.Fatal("retryGateExpectationFromTask() did not find gate")
	}
	job := retryTestJob(stale, buildTaskJobName(stale), "")

	_, _, result, proceed, err := r.createOrAdoptTaskJob(context.Background(), stale, &expected, job, false)
	if err != nil {
		t.Fatalf("createOrAdoptTaskJob() error = %v", err)
	}
	if proceed || result.RequeueAfter == 0 {
		t.Fatalf("createOrAdoptTaskJob() = proceed %t, result %#v; want requeue without create", proceed, result)
	}
	if createCalled {
		t.Fatal("retry Job Create was called with a stale Task generation")
	}
}

func TestRetryPreflightRejectionReleasesUnusedPoolReservation(t *testing.T) {
	scheme := newTestScheme()
	stale := retryLifecycleTask("retry-preflight-release", corev1alpha1.TaskPhasePending, 1, "")
	stale.Generation = 1
	stale.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	latest := stale.DeepCopy()
	latest.Generation = 2
	lease := newSubstratePoolActorLease(latest, latest.Namespace, testSubstrateActorID, testSubstrateActorID)
	r := newUnitReconciler(scheme, latest, lease)
	expected, ok := retryGateExpectationFromTask(stale)
	if !ok {
		t.Fatal("retryGateExpectationFromTask() did not find gate")
	}
	job := retryTestJob(stale, buildTaskJobName(stale), "")

	_, _, result, proceed, err := r.createOrAdoptTaskJob(context.Background(), stale, &expected, job, true)
	if err != nil {
		t.Fatalf("createOrAdoptTaskJob() error = %v", err)
	}
	if proceed || result.RequeueAfter == 0 {
		t.Fatalf("createOrAdoptTaskJob() = proceed %t, result %#v; want requeue without create", proceed, result)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: lease.Name, Namespace: lease.Namespace}, lease); !apierrors.IsNotFound(err) {
		t.Fatalf("unused pool reservation still exists: %v", err)
	}
}

func TestRetryPolicyRemovalDuringCreatePreventsAttempt(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("audit-policy-toctou", corev1alpha1.TaskPhasePending, 1, "")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	removed := false
	createdSuspended := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if job, ok := obj.(*batchv1.Job); ok && !removed {
				createdSuspended = job.Spec.Suspend != nil && *job.Spec.Suspend
				latest := &corev1alpha1.Task{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
					return err
				}
				latest.Spec.RetryPolicy = nil
				if err := c.Update(ctx, latest); err != nil {
					return err
				}
				removed = true
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.APIReader = fc
	r.JobBuilder = NewJobBuilder(fc)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !removed {
		t.Fatal("retry policy was not removed before Job creation")
	}
	if !createdSuspended {
		t.Fatal("retry Job was not created suspended across the policy revalidation window")
	}

	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(task.Namespace)); err != nil {
		t.Fatalf("List(Jobs) error = %v", err)
	}
	if got.Status.Attempts > 1 || len(jobs.Items) != 0 {
		t.Fatalf("retry started after policy removal: status=%#v jobs=%d", got.Status, len(jobs.Items))
	}
}

func TestTerminalClaimDuringCreatePreventsRetryAttempt(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-claim-create-race", corev1alpha1.TaskPhasePending, 1, "")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	claimed := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok && !claimed {
				latest := &corev1alpha1.Task{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
					return err
				}
				meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
					Type:    ConditionTypeTerminalTransition,
					Status:  metav1.ConditionTrue,
					Reason:  taskCancelledReason,
					Message: retryCancellationMessage,
				})
				if err := c.SubResource(testStatusSubresource).Update(ctx, latest); err != nil {
					return err
				}
				claimed = true
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.APIReader = fc
	r.JobBuilder = NewJobBuilder(fc)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !claimed {
		t.Fatal("did not inject terminal claim during Create")
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(task.Namespace)); err != nil {
		t.Fatalf("List(Jobs) error = %v", err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Attempts != 1 || got.Status.Phase != corev1alpha1.TaskPhasePending || len(jobs.Items) != 0 {
		t.Fatalf("retry advanced after terminal claim: status=%#v jobs=%d", got.Status, len(jobs.Items))
	}
}

func TestMismatchedSourceJobUIDBlocksConcurrentRetryReplacement(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("audit-reused-source", corev1alpha1.TaskPhasePending, 1, "source-job")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "original-source-uid",
	}
	reused := retryTestJob(task, task.Status.JobName, "reused-source-uid")
	reused.Status.Active = 1
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "reused-source-pod",
			Namespace: task.Namespace,
			Labels: map[string]string{
				batchv1.JobNameLabel: task.Status.JobName,
				labels.LabelTask:     labels.SelectorValue(task.Name),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: jobOwnerKind, Name: reused.Name, UID: reused.UID, Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r := newUnitReconciler(scheme, task, reused, pod)
	replacementName := buildTaskJobName(task)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	replacement := &batchv1.Job{}
	err := r.Get(context.Background(), types.NamespacedName{Name: replacementName, Namespace: task.Namespace}, replacement)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("replacement %q was created while reused source Job/Pod remained active: err=%v", replacementName, err)
	}
}

func TestMismatchedNonterminalSourceJobWithoutVisiblePodBlocksReplacement(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("mismatched-source-no-pod", corev1alpha1.TaskPhasePending, 1, "source-job")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "original-source-uid",
	}
	reused := retryTestJob(task, task.Status.JobName, "reused-source-uid")
	r := newUnitReconciler(scheme, task, reused)
	replacementName := buildTaskJobName(task)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	err := r.Get(context.Background(), types.NamespacedName{Name: replacementName, Namespace: task.Namespace}, &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("replacement %q was created while mismatched source Job remained nonterminal: %v", replacementName, err)
	}
}

func TestRecreatedNonterminalSourceJobBlocksCleanupCompletion(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("recreated-source-job", corev1alpha1.TaskPhaseRunning, 1, "source-job")
	original := retryTestJob(task, task.Status.JobName, "original-source-uid")
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, original).Build()
	recreated := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			job, ok := obj.(*batchv1.Job)
			if !ok || recreated {
				return c.Delete(ctx, obj, opts...)
			}
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			replacement := retryTestJob(task, job.Name, "recreated-source-uid")
			if err := c.Create(ctx, replacement); err != nil {
				return err
			}
			recreated = true
			return nil
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.APIReader = fc

	result, err := r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if !recreated || result.RequeueAfter != 2*time.Second {
		t.Fatalf("retryTask() result = %#v, recreated=%t; want overlap wait", result, recreated)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseRunning || !meta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeRetryCleanup) {
		t.Fatalf("retry cleanup advanced past recreated source: %#v", got.Status)
	}
}

func TestCompleteTaskDoesNotOverwriteConcurrentCancellation(t *testing.T) {
	scheme := newTestScheme()
	latest := retryLifecycleTask("audit-cancel-race", corev1alpha1.TaskPhaseCancelled, 1, "attempt-1-job")
	latest.Status.Message = retryCancellationMessage
	stale := latest.DeepCopy()
	stale.Status.Phase = corev1alpha1.TaskPhaseRunning
	stale.Status.Message = ""
	r := newUnitReconciler(scheme, latest)

	if _, err := r.completeTask(context.Background(), stale, corev1alpha1.TaskPhaseSucceeded, "stale success"); err != nil {
		t.Fatalf("completeTask() error = %v", err)
	}

	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(latest), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseCancelled || got.Status.Message != retryCancellationMessage {
		t.Fatalf("concurrent cancellation was overwritten: status=%#v", got.Status)
	}
}

func TestCompleteTaskDoesNotRewriteExistingTerminalState(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("existing-terminal-state", corev1alpha1.TaskPhaseCancelled, 1, "attempt-1-job")
	task.Status.Message = retryCancellationMessage
	r := newUnitReconciler(scheme, task)

	if _, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseSucceeded, "late success"); err != nil {
		t.Fatalf("completeTask() error = %v", err)
	}

	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseCancelled || got.Status.Message != retryCancellationMessage {
		t.Fatalf("existing terminal state was rewritten: %#v", got.Status)
	}
}

func TestTerminalTransitionCannotClaimWhileRetryCleanupOwnsAttempt(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-vs-retry-claim", corev1alpha1.TaskPhaseRunning, 1, "attempt-1-job")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore:    time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:       "attempt-1-uid",
		labels.AnnotationRetryCleanupState: retryCleanupStatePending,
	}
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:   ConditionTypeRetryCleanup,
		Status: metav1.ConditionTrue,
		Reason: "RetryCleanupClaimed",
	})
	r := newUnitReconciler(scheme, task)

	if _, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseCancelled, retryCancellationMessage); err != nil {
		t.Fatalf("completeTask() error = %v", err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseRunning || meta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeTerminalTransition) {
		t.Fatalf("terminal transition claimed retry-owned attempt: %#v", got.Status)
	}
}

func TestTerminalTransitionCannotClaimWhileJobStartOwnsAttempt(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("terminal-vs-job-start", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:   ConditionTypeJobStart,
		Status: metav1.ConditionTrue,
		Reason: jobStartClaimedReason,
	})
	r := newUnitReconciler(scheme, task)

	if _, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseCancelled, retryCancellationMessage); err != nil {
		t.Fatalf("completeTask() error = %v", err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhaseRunning || meta.IsStatusConditionTrue(got.Status.Conditions, ConditionTypeTerminalTransition) {
		t.Fatalf("terminal transition claimed job-start-owned attempt: %#v", got.Status)
	}
}

func TestJobStartClaimRecoversAfterUnsuspendStatusFailure(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("job-start-recovery", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	job := retryTestJob(task, task.Status.JobName, "attempt-2-uid")
	suspend := true
	job.Spec.Suspend = &suspend
	job.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "2",
	}
	r := newUnitReconciler(scheme, task, job)
	base, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("unit client does not implement client.WithWatch")
	}
	statusUpdates := 0
	completeErr := errors.New("injected job-start completion failure")
	fc := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && subResourceName == testStatusSubresource {
				statusUpdates++
				if statusUpdates == 2 {
					return completeErr
				}
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
	r.Client = fc
	r.APIReader = fc

	if _, err := r.ensureTaskJobStarted(context.Background(), task, job); !errors.Is(err, completeErr) {
		t.Fatalf("ensureTaskJobStarted() error = %v, want %v", err, completeErr)
	}
	intermediate := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), intermediate); err != nil {
		t.Fatalf("Get(intermediate Task) error = %v", err)
	}
	if !meta.IsStatusConditionTrue(intermediate.Status.Conditions, ConditionTypeJobStart) {
		t.Fatalf("JobStart claim was not left resumable: %#v", intermediate.Status.Conditions)
	}
	currentJob := &batchv1.Job{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), currentJob); err != nil {
		t.Fatalf("Get(Job) error = %v", err)
	}
	if currentJob.Spec.Suspend != nil && *currentJob.Spec.Suspend {
		t.Fatal("Job remained suspended after successful unsuspend patch")
	}

	if _, err := r.handleRunning(context.Background(), intermediate); err != nil {
		t.Fatalf("handleRunning() recovery error = %v", err)
	}
	final := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), final); err != nil {
		t.Fatalf("Get(final Task) error = %v", err)
	}
	if meta.IsStatusConditionTrue(final.Status.Conditions, ConditionTypeJobStart) {
		t.Fatalf("JobStart claim was not completed: %#v", final.Status.Conditions)
	}
}

func TestTrackedRetryStartsAfterLaterPolicyEdit(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("job-start-policy-race", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 1}
	job := retryTestJob(task, task.Status.JobName, "attempt-2-uid")
	suspend := true
	job.Spec.Suspend = &suspend
	job.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "2",
	}
	r := newUnitReconciler(scheme, task, job)
	base, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("unit client does not implement client.WithWatch")
	}
	removed := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && subResourceName == testStatusSubresource && !removed {
				latest := &corev1alpha1.Task{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
					return err
				}
				latest.Spec.RetryPolicy = nil
				if err := c.Update(ctx, latest); err != nil {
					return err
				}
				removed = true
				return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: testTaskResource}, obj.GetName(), errors.New("policy removed after retry tracking"))
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
	r.Client = fc
	r.APIReader = fc

	started, err := r.ensureTaskJobStarted(context.Background(), task, job)
	if err != nil {
		t.Fatalf("ensureTaskJobStarted() error = %v", err)
	}
	if !started || !removed {
		t.Fatalf("ensureTaskJobStarted() = %t, policyRemoved=%t; want tracked retry start", started, removed)
	}
	current := &batchv1.Job{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), current); err != nil {
		t.Fatalf("tracked retry Job is missing: %v", err)
	}
	if current.Spec.Suspend != nil && *current.Spec.Suspend {
		t.Fatal("tracked retry Job remained suspended")
	}
}

func TestDeletingTaskDoesNotUnsuspendTrackedRetry(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("deleting-job-start", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 1}
	now := metav1.Now()
	task.DeletionTimestamp = &now
	job := retryTestJob(task, task.Status.JobName, "attempt-2-uid")
	suspend := true
	job.Spec.Suspend = &suspend
	job.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "2",
	}
	r := newUnitReconciler(scheme, task, job)

	started, err := r.ensureTaskJobStarted(context.Background(), task, job)
	if err != nil {
		t.Fatalf("ensureTaskJobStarted() error = %v", err)
	}
	if started {
		t.Fatal("deleting Task started its suspended retry Job")
	}
	current := &batchv1.Job{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), current); apierrors.IsNotFound(err) {
		return
	} else if err != nil {
		t.Fatalf("Get(Job) error = %v", err)
	}
	if current.Spec.Suspend == nil || !*current.Spec.Suspend {
		t.Fatal("deleting Task retry Job was unsuspended")
	}
}

func TestDeletionAfterJobStartClaimPreventsUnsuspend(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("job-start-delete-race", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 1}
	job := retryTestJob(task, task.Status.JobName, "attempt-2-uid")
	suspend := true
	job.Spec.Suspend = &suspend
	job.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "2",
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, job).Build()
	taskReads := 0
	deletionInjected := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && key == client.ObjectKeyFromObject(task) {
				taskReads++
				if taskReads == 2 && !deletionInjected {
					latest := &corev1alpha1.Task{}
					if err := c.Get(ctx, key, latest); err != nil {
						return err
					}
					if err := c.Delete(ctx, latest); err != nil {
						return err
					}
					deletionInjected = true
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.APIReader = fc

	started, err := r.ensureTaskJobStarted(context.Background(), task, job)
	if err != nil {
		t.Fatalf("ensureTaskJobStarted() error = %v", err)
	}
	if started || !deletionInjected {
		t.Fatalf("ensureTaskJobStarted() = %t, deletionInjected=%t; want blocked start", started, deletionInjected)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deletion-raced retry Job still exists: %v", err)
	}
}

func TestJobStartClaimRejectsRecreatedSameNameJob(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("job-start-recreated", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 1}
	original := retryTestJob(task, task.Status.JobName, "original-attempt-2-uid")
	suspend := true
	original.Spec.Suspend = &suspend
	original.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "2",
	}
	r := newUnitReconciler(scheme, task, original)
	claimed, err := r.claimTaskJobStart(context.Background(), task, original)
	if err != nil || !claimed {
		t.Fatalf("claimTaskJobStart() = %t, %v", claimed, err)
	}
	if err := r.Delete(context.Background(), original); err != nil {
		t.Fatalf("Delete(original Job) error = %v", err)
	}
	recreated := retryTestJob(task, task.Status.JobName, "recreated-attempt-2-uid")
	recreated.Spec.Suspend = &suspend
	recreated.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "2",
	}
	if err := r.Create(context.Background(), recreated); err != nil {
		t.Fatalf("Create(recreated Job) error = %v", err)
	}

	started, err := r.ensureTaskJobStarted(context.Background(), task, recreated)
	if err != nil {
		t.Fatalf("ensureTaskJobStarted() error = %v", err)
	}
	if started {
		t.Fatal("recreated same-name Job was accepted by the original JobStart claim")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(recreated), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("recreated same-name Job still exists: %v", err)
	}
}

func TestStaleJobStartCompletionCannotClearSuccessorClaim(t *testing.T) {
	scheme := newTestScheme()
	oldTask := retryLifecycleTask("job-start-successor", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	oldTask.UID = retryOldTaskUID
	oldJob := retryTestJob(oldTask, oldTask.Status.JobName, "old-job-uid")
	oldJob.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "2",
	}
	meta.SetStatusCondition(&oldTask.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeJobStart,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 0,
		Reason:             jobStartClaimedReason,
		Message:            jobStartIdentity(oldJob),
	})
	newTask := oldTask.DeepCopy()
	newTask.UID = retryNewTaskUID
	newJob := retryTestJob(newTask, newTask.Status.JobName, "new-job-uid")
	newJob.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "2",
	}
	meta.SetStatusCondition(&newTask.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeJobStart,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 0,
		Reason:             jobStartClaimedReason,
		Message:            jobStartIdentity(newJob),
	})
	r := newUnitReconciler(scheme, newTask, newJob)

	if err := r.completeTaskJobStart(context.Background(), oldTask, oldJob, jobStartedReason, "old completion"); err != nil {
		t.Fatalf("completeTaskJobStart() error = %v", err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(newTask), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	condition := meta.FindStatusCondition(got.Status.Conditions, ConditionTypeJobStart)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Message != jobStartIdentity(newJob) {
		t.Fatalf("successor JobStart claim was altered: %#v", condition)
	}
}

func TestRunningUnsuspendedRetryJobRejectsAttemptMismatch(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("running-unsuspended-stale", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 1}
	job := retryTestJob(task, task.Status.JobName, "attempt-2-uid")
	job.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "0",
		labels.AnnotationRetryAttempt:        "3",
	}
	r := newUnitReconciler(scheme, task, job)

	if _, err := r.handleRunning(context.Background(), task); err != nil {
		t.Fatalf("handleRunning() error = %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("attempt-mismatched unsuspended retry Job still exists: %v", err)
	}
}

func TestStartedRetrySurvivesLaterTaskGenerationChange(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("started-retry-generation-change", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	task.Generation = 2
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 1}
	job := retryTestJob(task, task.Status.JobName, "attempt-2-uid")
	job.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "1",
		labels.AnnotationRetryAttempt:        "2",
	}
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeJobStart,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		Reason:             jobStartedReason,
		Message:            jobStartIdentity(job),
	})
	r := newUnitReconciler(scheme, task, job)

	if _, err := r.handleRunning(context.Background(), task); err != nil {
		t.Fatalf("handleRunning() error = %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
		t.Fatalf("started retry Job was removed after later Task edit: %v", err)
	}
}

func TestStaleRetryCleanupNeverDeletesUncontrolledJob(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("uncontrolled-stale-replacement", corev1alpha1.TaskPhaseRunning, 2, "attempt-2-job")
	foreignTask := task.DeepCopy()
	foreignTask.Name = "foreign-task"
	foreignTask.UID = "foreign-task-uid"
	job := retryTestJob(foreignTask, task.Status.JobName, "foreign-job-uid")
	r := newUnitReconciler(scheme, task, job)

	if _, err := r.deleteStaleRetryReplacement(context.Background(), task, job); err == nil {
		t.Fatal("deleteStaleRetryReplacement() error = nil, want ownership rejection")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
		t.Fatalf("uncontrolled Job was deleted: %v", err)
	}
}

func TestRetryDoesNotAdoptDeletingReplacement(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("audit-deleting-adoption", corev1alpha1.TaskPhasePending, 1, "")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	replacement := retryTestJob(task, buildTaskJobName(task), "deleting-replacement-uid")
	now := metav1.Now()
	replacement.DeletionTimestamp = &now
	replacement.Finalizers = []string{"audit.example/finalizer"}
	r := newUnitReconciler(scheme, task, replacement)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhasePending || got.Status.Attempts != 1 || got.Status.JobName != "" {
		t.Fatalf("deleting replacement was adopted: status=%#v", got.Status)
	}
}

func TestCreateErrorFallbackDoesNotAdoptDeletingRetryReplacement(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("deleting-create-fallback", corev1alpha1.TaskPhasePending, 1, "")
	task.Generation = 1
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	markRetryCleanupCompleteForTest(task)
	existing := retryTestJob(task, buildTaskJobName(task), "deleting-fallback-uid")
	now := metav1.Now()
	existing.DeletionTimestamp = &now
	existing.Finalizers = []string{"audit.example/finalizer"}
	r := newUnitReconciler(scheme, task, existing)
	expected, ok := retryGateExpectationFromTask(task)
	if !ok {
		t.Fatal("retryGateExpectationFromTask() did not find gate")
	}
	job := retryTestJob(task, existing.Name, "")

	_, _, result, proceed, err := r.createOrAdoptTaskJob(context.Background(), task, &expected, job, false)
	if err != nil {
		t.Fatalf("createOrAdoptTaskJob() error = %v", err)
	}
	if proceed || result.RequeueAfter != 2*time.Second {
		t.Fatalf("createOrAdoptTaskJob() = proceed %t, result %#v; want deleting Job wait", proceed, result)
	}
}

func TestCreateErrorFallbackRejectsStaleSuspendedRetryReplacement(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("stale-create-fallback", corev1alpha1.TaskPhasePending, 1, "")
	task.Generation = 2
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	markRetryCleanupCompleteForTest(task)
	existing := retryTestJob(task, buildTaskJobName(task), "stale-fallback-uid")
	suspend := true
	existing.Spec.Suspend = &suspend
	existing.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "1",
		labels.AnnotationRetryAttempt:        "2",
	}
	r := newUnitReconciler(scheme, task, existing)
	expected, ok := retryGateExpectationFromTask(task)
	if !ok {
		t.Fatal("retryGateExpectationFromTask() did not find gate")
	}
	job := retryTestJob(task, existing.Name, "")

	_, _, result, proceed, err := r.createOrAdoptTaskJob(context.Background(), task, &expected, job, false)
	if err != nil {
		t.Fatalf("createOrAdoptTaskJob() error = %v", err)
	}
	if proceed || result.RequeueAfter == 0 {
		t.Fatalf("createOrAdoptTaskJob() = proceed %t, result %#v; want stale replacement cleanup", proceed, result)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(existing), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale Create-fallback replacement still exists: %v", err)
	}
}

func TestSuspendedRetryReplacementWithStaleGenerationIsDeleted(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("stale-generation-replacement", corev1alpha1.TaskPhasePending, 1, "")
	task.Generation = 2
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		labels.AnnotationRetryJobUID:    "",
	}
	replacement := retryTestJob(task, buildTaskJobName(task), "stale-generation-uid")
	suspend := true
	replacement.Spec.Suspend = &suspend
	replacement.Annotations = map[string]string{
		labels.AnnotationRetryTaskGeneration: "1",
		labels.AnnotationRetryAttempt:        "2",
	}
	r := newUnitReconciler(scheme, task, replacement)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(replacement), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale suspended replacement still exists: %v", err)
	}
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), got); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if got.Status.Phase != corev1alpha1.TaskPhasePending || got.Status.Attempts != 1 {
		t.Fatalf("stale replacement advanced Task: %#v", got.Status)
	}
}

func TestLegacyRetryGateBlocksConcurrentActiveSource(t *testing.T) {
	scheme := newTestScheme()
	task := retryLifecycleTask("audit-legacy-source", corev1alpha1.TaskPhasePending, 1, "legacy-source-job")
	task.Annotations = map[string]string{
		labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	}
	source := retryTestJob(task, task.Status.JobName, "legacy-source-real-uid")
	source.Status.Active = 1
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-source-pod",
			Namespace: task.Namespace,
			Labels: map[string]string{
				batchv1.JobNameLabel: task.Status.JobName,
				labels.LabelTask:     labels.SelectorValue(task.Name),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: jobOwnerKind, Name: source.Name, UID: source.UID, Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r := newUnitReconciler(scheme, task, source, pod)
	replacementName := buildTaskJobName(task)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	replacement := &batchv1.Job{}
	err := r.Get(context.Background(), types.NamespacedName{Name: replacementName, Namespace: task.Namespace}, replacement)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("replacement %q was created while legacy source Job/Pod remained active: err=%v", replacementName, err)
	}
}
