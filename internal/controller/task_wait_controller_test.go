/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/orka-agents/orka/internal/taskwait"
)

const (
	taskWaitTestNamespace = "default"
	taskWaitTestPrompt    = "Ship the release once CI is green."
)

var taskWaitTestNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type taskWaitHarness struct {
	t       *testing.T
	client  client.Client
	events  *storetest.FakeExecutionEventStore
	r       *TaskWaitReconciler
	created int
}

func newTaskWaitHarness(t *testing.T, objs ...client.Object) *taskWaitHarness {
	t.Helper()
	h := &taskWaitHarness{t: t, events: storetest.NewFakeExecutionEventStoreWithClock(func() time.Time { return taskWaitTestNow })}
	h.client = fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				h.created++
				obj.SetUID(types.UID(fmt.Sprintf("created-uid-%d", h.created)))
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	h.r = &TaskWaitReconciler{Client: h.client, APIReader: h.client, ExecutionEvents: h.events, Now: func() time.Time { return taskWaitTestNow }}
	return h
}

func (h *taskWaitHarness) reconcile(name string) ctrl.Result {
	h.t.Helper()
	result, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: taskWaitTestNamespace, Name: name}})
	if err != nil {
		h.t.Fatalf("reconcile %s: %v", name, err)
	}
	return result
}

// settle reconciles until the wait stops requesting an immediate retry.
func (h *taskWaitHarness) settle(name string) {
	h.t.Helper()
	for range 8 {
		if result := h.reconcile(name); result.RequeueAfter != taskWaitStepInterval {
			return
		}
	}
	h.t.Fatalf("wait on %s did not settle", name)
}

func (h *taskWaitHarness) task(name string) *corev1alpha1.Task {
	h.t.Helper()
	task := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: taskWaitTestNamespace, Name: name}, task); err != nil {
		h.t.Fatalf("get %s: %v", name, err)
	}
	return task
}

func (h *taskWaitHarness) tasks() []corev1alpha1.Task {
	h.t.Helper()
	list := &corev1alpha1.TaskList{}
	if err := h.client.List(context.Background(), list, client.InNamespace(taskWaitTestNamespace)); err != nil {
		h.t.Fatal(err)
	}
	return list.Items
}

func (h *taskWaitHarness) parkedApprovals() []approvals.Approval {
	h.t.Helper()
	listed, err := approvals.ListEvents(context.Background(), h.events, taskWaitTestNamespace, "parked")
	if err != nil {
		h.t.Fatal(err)
	}
	return approvals.Derive(listed, time.Time{})
}

func (h *taskWaitHarness) decide(task *corev1alpha1.Task, approvalID, eventType, actor, reason string) {
	h.t.Helper()
	content, _ := json.Marshal(map[string]string{"approvalID": approvalID, "actor": actor, "reason": reason, "taskUID": string(task.UID)})
	if _, err := h.events.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: task.Name, TaskName: task.Name,
		Type: eventType, Severity: events.ExecutionEventSeverityInfo, ToolCallID: approvalID, Content: content,
	}); err != nil {
		h.t.Fatal(err)
	}
}

// parkedTestTask is a Task whose turn requested a wait. Its phase and
// execution describe where that turn is.
func parkedTestTask(kind corev1alpha1.TaskWaitKind, phase corev1alpha1.TaskPhase, references ...string) *corev1alpha1.Task {
	state := corev1alpha1.TaskExecutionStateRunning
	outcome := corev1alpha1.TaskExecutionOutcome("")
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded:
		state, outcome = corev1alpha1.TaskExecutionStateSucceeded, corev1alpha1.TaskExecutionOutcomeSucceeded
	case corev1alpha1.TaskPhaseFailed:
		state, outcome = corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parked", Namespace: taskWaitTestNamespace, UID: "uid-parked",
			CreationTimestamp: metav1.NewTime(taskWaitTestNow.Add(-10 * time.Minute)),
			Labels:            map[string]string{labels.LabelAgentName: "coder", acpRuntimeTaskPoolLabel: "pool-a"},
			Annotations:       map[string]string{labels.AnnotationCancelRequested: "ignored", "orka.ai/acp-queued-at": "x"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: taskWaitTestPrompt,
			AgentRef:   &corev1alpha1.AgentReference{Name: "coder"},
			Timeout:    &metav1.Duration{Duration: 4 * time.Hour},
			SessionRef: &corev1alpha1.SessionReference{Name: "release", Create: true, Append: true, MaxMessages: 80},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: phase,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: state, Outcome: outcome, Attempt: 1, PromptID: "prompt-uid-parked-1",
			},
			Wait: &corev1alpha1.TaskWaitStatus{
				State: corev1alpha1.TaskWaitStateRequested, Kind: kind, References: references, Reason: "need sign-off",
				Attempt: 1, PromptID: "prompt-uid-parked-1",
				RequestedAt: metav1.NewTime(taskWaitTestNow.Add(-time.Minute)),
				Deadline:    metav1.NewTime(taskWaitTestNow.Add(2 * time.Hour)),
			},
		},
	}
	if kind == corev1alpha1.TaskWaitKindApproval && len(references) == 0 {
		task.Status.Wait.References = []string{"approval-1"}
	}
	return task
}

func TestTaskWaitDoesNotArmBeforeTheTurnReachesItsIdleBoundary(t *testing.T) {
	h := newTaskWaitHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseRunning))
	result := h.reconcile("parked")
	if result.RequeueAfter <= 0 {
		t.Fatal("a requested wait must be re-checked until its turn settles")
	}
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateRequested {
		t.Fatalf("wait armed while the turn was still running: %s", got)
	}
	if len(h.tasks()) != 1 {
		t.Fatal("no continuation may exist before the turn settles")
	}
}

func TestTaskWaitAbandonsWhenTheTurnDidNotProveSuccess(t *testing.T) {
	h := newTaskWaitHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseFailed))
	h.settle("parked")
	parked := h.task("parked")
	if parked.Status.Wait.State != corev1alpha1.TaskWaitStateAbandoned {
		t.Fatalf("state = %s, want Abandoned", parked.Status.Wait.State)
	}
	if len(h.tasks()) != 1 {
		t.Fatal("a failed turn must never resume")
	}
	if got := h.parkedApprovals(); len(got) != 1 || got[0].Status != approvals.StatusCancelled {
		t.Fatalf("the undecided sign-off must be closed, got %+v", got)
	}
	condition := meta.FindStatusCondition(parked.Status.Conditions, corev1alpha1.TaskConditionWaiting)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "WaitAbandoned" {
		t.Fatalf("unexpected Waiting condition %+v", condition)
	}
}

func TestTaskWaitAbandonsASupersededAttempt(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseRunning)
	task.Status.Execution.Attempt, task.Status.Execution.PromptID = 2, "prompt-uid-parked-2"
	h := newTaskWaitHarness(t, task)
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateAbandoned {
		t.Fatalf("a wait from an earlier attempt must not arm, got %s", got)
	}
}

func TestTaskWaitResumesOnApprovalWithANewPromptAndExactlyOneContinuation(t *testing.T) {
	before := metrics.CounterVecValue(metrics.TaskWaitTransitionsTotal, "Approval", "Resumed")
	h := newTaskWaitHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded))
	h.settle("parked")

	parked := h.task("parked")
	if parked.Status.Wait.State != corev1alpha1.TaskWaitStateWaiting || parked.Status.Wait.WaitingSince == nil {
		t.Fatalf("a succeeded turn must arm its wait, got %+v", parked.Status.Wait)
	}
	condition := meta.FindStatusCondition(parked.Status.Conditions, corev1alpha1.TaskConditionWaiting)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "AwaitingApproval" {
		t.Fatalf("unexpected Waiting condition %+v", condition)
	}
	pending := h.parkedApprovals()
	if len(pending) != 1 || pending[0].ID != "approval-1" || pending[0].Status != approvals.StatusPending {
		t.Fatalf("the parked sign-off must be requested when the wait arms, got %+v", pending)
	}
	h.settle("parked")
	if len(h.tasks()) != 1 {
		t.Fatal("nothing resumes while the approval is pending")
	}

	h.decide(parked, "approval-1", events.ExecutionEventTypeApprovalApproved, "alice", "ship it")
	h.settle("parked")
	parked = h.task("parked")
	wait := parked.Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed || wait.ContinuationTaskName != taskwait.ContinuationTaskName(parked) || wait.ContinuationTaskUID == "" {
		t.Fatalf("unexpected resumed wait %+v", wait)
	}
	continuation := h.task(wait.ContinuationTaskName)
	assertTaskWaitContinuation(t, continuation)

	h.settle("parked")
	h.settle(continuation.Name)
	if len(h.tasks()) != 2 {
		t.Fatal("repeated reconciles must not create another continuation")
	}
	if got := metrics.CounterVecValue(metrics.TaskWaitTransitionsTotal, "Approval", "Resumed"); got != before+1 {
		t.Fatalf("resume metric = %v, want %v", got, before+1)
	}
}

// assertTaskWaitContinuation checks that a continuation resumes the Session
// with a controller-authored prompt and inherits only safe metadata.
func assertTaskWaitContinuation(t *testing.T, continuation *corev1alpha1.Task) {
	t.Helper()
	if continuation.Spec.Prompt == taskWaitTestPrompt || strings.Contains(continuation.Spec.Prompt, taskWaitTestPrompt) {
		t.Fatal("the continuation must never replay the parked prompt")
	}
	for _, want := range []string{"approved by alice", "ship it", "processes"} {
		if !strings.Contains(continuation.Spec.Prompt, want) {
			t.Errorf("continuation prompt lacks %q: %s", want, continuation.Spec.Prompt)
		}
	}
	ref := continuation.Spec.SessionRef
	if ref == nil || ref.Name != "release" || ref.Create || !ref.Append || ref.MaxMessages != 80 {
		t.Fatalf("continuation must resume the same Session, got %+v", ref)
	}
	if continuation.Spec.Timeout == nil || continuation.Spec.Timeout.Duration > 3*time.Hour+50*time.Minute ||
		continuation.Spec.Timeout.Duration < 3*time.Hour+49*time.Minute {
		t.Fatalf("continuation timeout must be the chain's remaining time, got %v", continuation.Spec.Timeout)
	}
	if continuation.Annotations[labels.AnnotationContinuationOfUID] != "uid-parked" ||
		continuation.Annotations[labels.AnnotationContinuationIndex] != "1" ||
		continuation.Labels[labels.LabelAgentName] != "coder" {
		t.Fatalf("unexpected continuation metadata %v %v", continuation.Labels, continuation.Annotations)
	}
	for _, leaked := range []string{acpRuntimeTaskPoolLabel} {
		if _, ok := continuation.Labels[leaked]; ok {
			t.Errorf("controller runtime label %s must not be inherited", leaked)
		}
	}
	if _, ok := continuation.Annotations[labels.AnnotationCancelRequested]; ok {
		t.Error("controller-managed annotations must not be inherited")
	}
	if len(continuation.OwnerReferences) != 1 || continuation.OwnerReferences[0].UID != "uid-parked" || metav1.GetControllerOf(continuation) != nil {
		t.Fatalf("continuation must be owned, not controlled, by the parked Task: %+v", continuation.OwnerReferences)
	}
}

func TestTaskWaitAdoptsAContinuationCreatedBeforeARestart(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	existing := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour)
	existing.UID = "uid-existing"
	h := newTaskWaitHarness(t, task, existing)
	h.settle("parked")
	if wait := h.task("parked").Status.Wait; wait.State != corev1alpha1.TaskWaitStateResumed || wait.ContinuationTaskUID != "uid-existing" {
		t.Fatalf("the reserved continuation must be adopted, got %+v", wait)
	}
	if len(h.tasks()) != 2 {
		t.Fatal("adoption must not create a second continuation")
	}
}

func TestTaskWaitRefusesAContinuationNameHeldByAnotherTask(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	squatter := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: task.Status.Wait.ContinuationTaskName, Namespace: taskWaitTestNamespace, UID: "uid-squat"}}
	h := newTaskWaitHarness(t, task, squatter)
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateAbandoned {
		t.Fatalf("an unrelated Task must not be adopted, got %s", got)
	}
}

func TestTaskWaitResumesWhenChildTasksSettleThroughTheirChains(t *testing.T) {
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: taskWaitTestNamespace, UID: "uid-child",
			Labels:      map[string]string{labels.LabelParentTask: "parked"},
			Annotations: map[string]string{labels.AnnotationParentTaskName: "parked", labels.AnnotationParentTaskUID: "uid-parked"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	h := newTaskWaitHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindTasks, corev1alpha1.TaskPhaseSucceeded, "child"), child)
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateWaiting {
		t.Fatalf("a running child must keep the parent waiting, got %s", got)
	}

	// The child parks too: its Succeeded phase alone must not wake the parent.
	current := h.task("child")
	current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	current.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, Kind: corev1alpha1.TaskWaitKindTimer,
		ContinuationTaskName: "child-next", ContinuationTaskUID: "uid-child-next",
	}
	if err := h.client.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	next := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-next", Namespace: taskWaitTestNamespace, UID: "uid-child-next",
			Annotations: map[string]string{labels.AnnotationContinuationOfUID: "uid-child"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	if err := h.client.Create(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Status().Update(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateWaiting {
		t.Fatalf("a child whose continuation still runs must keep the parent waiting, got %s", got)
	}

	next = h.task("child-next")
	next.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := h.client.Status().Update(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("settled children must resume the parent, got %s", wait.State)
	}
	prompt := h.task(wait.ContinuationTaskName).Spec.Prompt
	if !strings.Contains(prompt, "child: Succeeded (continued as child-next)") || !strings.Contains(prompt, "wait_for_tasks") {
		t.Fatalf("unexpected continuation prompt %s", prompt)
	}
}

func TestTaskWaitCancellationWhileWaiting(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded)
	h := newTaskWaitHarness(t, task)
	h.settle("parked")
	current := h.task("parked")
	current.Annotations[labels.AnnotationCancelWait] = "no longer needed"
	if err := h.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	h.settle("parked")
	parked := h.task("parked")
	if parked.Status.Wait.State != corev1alpha1.TaskWaitStateCancelled || !strings.Contains(parked.Status.Wait.Message, "no longer needed") {
		t.Fatalf("unexpected wait %+v", parked.Status.Wait)
	}
	if len(h.tasks()) != 1 {
		t.Fatal("a cancelled wait must not resume")
	}
	if got := h.parkedApprovals(); len(got) != 1 || got[0].Status != approvals.StatusCancelled {
		t.Fatalf("cancellation must close the sign-off, got %+v", got)
	}
	// A late decision is refused and cannot revive the wait.
	content, _ := json.Marshal(map[string]string{"approvalID": "approval-1", "taskUID": "uid-parked"})
	if _, err := h.events.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: taskWaitTestNamespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: "parked", TaskName: "parked",
		Type: events.ExecutionEventTypeApprovalApproved, Severity: events.ExecutionEventSeverityInfo, ToolCallID: "approval-1", Content: content,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a late decision must conflict with the closed sign-off, got %v", err)
	}
	h.settle("parked")
	if len(h.tasks()) != 1 {
		t.Fatal("a late decision must not revive a cancelled wait")
	}
}

func TestTaskWaitDeletionCancelsAContinuationBeingCreated(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Finalizers = []string{labels.TaskFinalizer}
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	continuation := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour)
	continuation.UID = "uid-cont"
	h := newTaskWaitHarness(t, task, continuation)
	if err := h.client.Delete(context.Background(), h.task("parked")); err != nil {
		t.Fatal(err)
	}
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateCancelled {
		t.Fatalf("deleting the parked Task must cancel its wait, got %s", got)
	}
	if len(h.tasks()) != 1 {
		t.Fatal("the continuation created in the same window must be cancelled")
	}
}

func TestTaskWaitExpiresAtTheChainDeadline(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded)
	task.Spec.Timeout = &metav1.Duration{Duration: 10*time.Minute + 30*time.Second}
	h := newTaskWaitHarness(t, task)
	h.settle("parked")
	parked := h.task("parked")
	if parked.Status.Wait.State != corev1alpha1.TaskWaitStateWaiting || !parked.Status.Wait.Deadline.Time.Equal(taskWaitTestNow.Add(30*time.Second)) {
		t.Fatalf("the wait deadline must be clamped to the Task deadline, got %+v", parked.Status.Wait)
	}
	h.r.Now = func() time.Time { return taskWaitTestNow.Add(time.Minute) }
	h.settle("parked")
	parked = h.task("parked")
	if parked.Status.Wait.State != corev1alpha1.TaskWaitStateExpired {
		t.Fatalf("state = %s, want Expired", parked.Status.Wait.State)
	}
	if len(h.tasks()) != 1 {
		t.Fatal("an expired wait must not resume")
	}
	if got := h.parkedApprovals(); len(got) != 1 || got[0].Status != approvals.StatusExpired {
		t.Fatalf("expiry must close the sign-off as expired, got %+v", got)
	}
}

func TestTaskWaitTimeoutResumesWhenTheChainHasTimeLeft(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.Deadline = metav1.NewTime(taskWaitTestNow.Add(5 * time.Minute))
	h := newTaskWaitHarness(t, task)
	h.settle("parked")
	h.r.Now = func() time.Time { return taskWaitTestNow.Add(6 * time.Minute) }
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("a wait timeout with budget left must resume so the agent can react, got %s", wait.State)
	}
	if prompt := h.task(wait.ContinuationTaskName).Spec.Prompt; !strings.Contains(prompt, "deadline") {
		t.Fatalf("continuation must report the timeout, got %s", prompt)
	}
	if got := h.parkedApprovals(); len(got) != 1 || got[0].Status != approvals.StatusExpired {
		t.Fatalf("the unanswered sign-off must expire, got %+v", got)
	}
}

func TestTaskWaitDecisionBeforeTimeoutWins(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.Deadline = metav1.NewTime(taskWaitTestNow.Add(5 * time.Minute))
	h := newTaskWaitHarness(t, task)
	h.settle("parked")
	h.decide(h.task("parked"), "approval-1", events.ExecutionEventTypeApprovalDeclined, "carol", "not today")
	h.r.Now = func() time.Time { return taskWaitTestNow.Add(6 * time.Minute) }
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if prompt := h.task(wait.ContinuationTaskName).Spec.Prompt; !strings.Contains(prompt, "declined by carol") {
		t.Fatalf("a recorded decision must win over the timeout, got %s", prompt)
	}
}

func TestTaskWaitTimerResumesAtItsTime(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	resumeAt := metav1.NewTime(taskWaitTestNow.Add(10 * time.Minute))
	task.Status.Wait.ResumeAt = &resumeAt
	h := newTaskWaitHarness(t, task)
	h.settle("parked")
	result := h.reconcile("parked")
	if result.RequeueAfter <= 0 || result.RequeueAfter > 10*time.Minute+time.Second {
		t.Fatalf("timer wait must requeue for its resume time, got %v", result.RequeueAfter)
	}
	h.r.Now = func() time.Time { return taskWaitTestNow.Add(11 * time.Minute) }
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("state = %s, want Resumed", got)
	}
}

func TestTaskWaitRequestsMapChildrenAndContinuationsToTheParkedTask(t *testing.T) {
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "c", Namespace: "ns", Annotations: map[string]string{
			labels.AnnotationParentTaskName: "parent", labels.AnnotationContinuationOfName: "parked",
		},
	}}
	got := taskWaitRequestsFor(context.Background(), child)
	if len(got) != 2 || got[0].Name != "parent" || got[1].Name != "parked" {
		t.Fatalf("unexpected requests %+v", got)
	}
}
