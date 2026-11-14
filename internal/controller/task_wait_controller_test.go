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
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/connectors"
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
	h.r = &TaskWaitReconciler{
		Client: h.client, APIReader: h.client, ExecutionEvents: h.events, TaskProvenanceProtected: true,
		Now: func() time.Time { return taskWaitTestNow },
	}
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
	for _, reference := range references {
		if kind == corev1alpha1.TaskWaitKindTasks {
			task.Status.Wait.ReferenceUIDs = append(task.Status.Wait.ReferenceUIDs, "uid-"+reference)
		}
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
	if want := taskWaitTestNow.Add(3*time.Hour + 50*time.Minute).Format(time.RFC3339); continuation.Annotations[labels.AnnotationContinuationChainDeadline] != want {
		t.Fatalf("continuation must record the chain's absolute deadline %s, got %q", want, continuation.Annotations[labels.AnnotationContinuationChainDeadline])
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
	existing := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
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
	continuation := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	continuation.UID = "uid-cont"
	h := newTaskWaitHarness(t, task, continuation)
	if err := h.client.Delete(context.Background(), h.task("parked")); err != nil {
		t.Fatal(err)
	}
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateCancelled {
		t.Fatalf("deleting the parked Task must cancel its wait, got %s", got)
	}
	// The cancelled continuation's link is released, so its deletion completes.
	h.settle(continuation.Name)
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

func TestTaskWaitCancellationFindsAContinuationTheCacheHasNotSeen(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Annotations[labels.AnnotationCancelWait] = "stop"
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	continuation := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	continuation.UID = "uid-cont"

	deleted := ""
	cached := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = obj.GetName()
			return nil
		}}).Build()
	authoritative := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(task.DeepCopy(), continuation).Build()
	r := &TaskWaitReconciler{
		Client: cached, APIReader: authoritative, TaskProvenanceProtected: true, Now: func() time.Time { return taskWaitTestNow },
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: taskWaitTestNamespace, Name: "parked"}}); err != nil {
		t.Fatal(err)
	}
	if deleted != continuation.Name {
		t.Fatalf("cancellation must delete the continuation seen by the authoritative reader, deleted %q", deleted)
	}
}

func TestTaskWaitCancellationNeverDeletesAnUnprovenContinuation(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Annotations[labels.AnnotationCancelWait] = "stop"
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	// Without provenance admission, a Task holding the reserved name may
	// carry a forged lineage annotation.
	forged := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	forged.UID, forged.Finalizers = "uid-forged", nil
	h := newTaskWaitHarness(t, task, forged)
	h.r.TaskProvenanceProtected = false
	h.settle("parked")
	if wait := h.task("parked").Status.Wait; wait.State != corev1alpha1.TaskWaitStateCancelled {
		t.Fatalf("the wait must be cancelled, got %+v", wait)
	}
	if got := h.task(forged.Name); !got.DeletionTimestamp.IsZero() || got.UID != "uid-forged" {
		t.Fatal("a Task whose only claim is a forgeable annotation must not be deleted")
	}
}

func TestHistoryLimitsKeepPredecessorsOfRunningContinuations(t *testing.T) {
	scheduled := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: taskWaitTestNamespace, UID: "uid-nightly"},
		Spec: corev1alpha1.TaskSpec{Schedule: "@daily", SuccessfulRunsHistoryLimit: new(int32(0))}}
	run := func(name string, age time.Duration) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: taskWaitTestNamespace, UID: types.UID("uid-" + name),
				CreationTimestamp: metav1.NewTime(taskWaitTestNow.Add(-age)),
				Labels:            map[string]string{labels.LabelParentTask: "nightly"},
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		}
	}
	parked := run("parked-run", 3*time.Hour)
	parked.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, ContinuationTaskName: "parked-run-next", ContinuationTaskUID: "uid-parked-run-next",
	}
	next := run("parked-run-next", time.Hour)
	next.Annotations = map[string]string{labels.AnnotationContinuationOfUID: "uid-parked-run"}
	next.Status.Phase = corev1alpha1.TaskPhaseRunning
	done := run("done-run", 2*time.Hour)

	r := newUnitReconciler(newTestScheme(), scheduled, parked, next, done)
	if err := r.enforceHistoryLimits(context.Background(), scheduled); err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	list := &corev1alpha1.TaskList{}
	if err := r.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		remaining[item.Name] = true
	}
	if !remaining["parked-run"] || !remaining["parked-run-next"] {
		t.Fatalf("a predecessor with a running continuation must not be pruned: %v", remaining)
	}
	if remaining["done-run"] {
		t.Fatalf("settled runs beyond the history limit are still pruned: %v", remaining)
	}
}

func TestTaskWaitContinuationWaitsOnItsPredecessorsChild(t *testing.T) {
	predecessor := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	predecessor.Name, predecessor.UID = "first", "uid-first"
	predecessor.Status.Wait.State = corev1alpha1.TaskWaitStateResumed
	predecessor.Status.Wait.ContinuationTaskName, predecessor.Status.Wait.ContinuationTaskUID = "parked", "uid-parked"
	parked := parkedTestTask(corev1alpha1.TaskWaitKindTasks, corev1alpha1.TaskPhaseSucceeded, "child")
	parked.Annotations[labels.AnnotationContinuationOfName] = "first"
	parked.Annotations[labels.AnnotationContinuationOfUID] = "uid-first"
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: taskWaitTestNamespace, UID: "uid-child",
			Annotations: map[string]string{labels.AnnotationParentTaskName: "first", labels.AnnotationParentTaskUID: "uid-first"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	h := newTaskWaitHarness(t, predecessor, parked, child)
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateWaiting {
		t.Fatalf("the predecessor's running child must keep the continuation waiting, got %s", got)
	}
	current := h.task("child")
	current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := h.client.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed || !strings.Contains(h.task(wait.ContinuationTaskName).Spec.Prompt, "child: Succeeded") {
		t.Fatalf("unexpected resume %+v", wait)
	}
}

func TestTaskWaitAdoptsAContinuationDeletedBeforeItWasLinked(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	h := newTaskWaitHarness(t, task)

	// The create landed but the controller lost the response before linking it.
	continuation := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	if err := h.client.Create(context.Background(), continuation); err != nil {
		t.Fatal(err)
	}
	// History pruning or a person deletes it before the link is recorded.
	if err := h.client.Delete(context.Background(), h.task(continuation.Name)); err != nil {
		t.Fatal(err)
	}
	h.settle(continuation.Name)
	if deleting := h.task(continuation.Name); deleting.DeletionTimestamp.IsZero() {
		t.Fatal("the unlinked continuation must stay visible while its predecessor is Resuming")
	}

	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed || wait.ContinuationTaskUID != string(continuation.UID) {
		t.Fatalf("recovery must adopt the continuation that already ran, got %+v", wait)
	}
	h.settle(continuation.Name)
	if names := h.tasks(); len(names) != 1 {
		t.Fatalf("no second continuation may be created, have %d Tasks", len(names))
	}
}

func TestTaskWaitReleasesTheLinkOnceResumed(t *testing.T) {
	h := newTaskWaitHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded))
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("state = %s", wait.State)
	}
	if finalizers := h.task(wait.ContinuationTaskName).Finalizers; len(finalizers) != 0 {
		t.Fatalf("a linked continuation must not keep the link finalizer: %v", finalizers)
	}
}

func TestTaskWaitRefusesToAdoptWithoutProvenanceAdmission(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	// Without provenance admission this lineage annotation may be forged.
	forged := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	forged.UID, forged.Finalizers = "uid-forged", nil
	h := newTaskWaitHarness(t, task, forged)
	h.r.TaskProvenanceProtected = false
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateAbandoned || wait.ContinuationTaskUID != "" {
		t.Fatalf("an unprovable continuation must never be linked, got %+v", wait)
	}
	if got := h.task(forged.Name); !got.DeletionTimestamp.IsZero() {
		t.Fatal("a Task whose only claim is a forgeable annotation must not be deleted")
	}
}

func TestTaskWaitIgnoresASameNamedReplacementChild(t *testing.T) {
	// The authorized child was deleted and an unrelated Task took its name,
	// carrying only the parent label that admission allows on any create.
	replacement := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: taskWaitTestNamespace, UID: "uid-replacement",
			Labels: map[string]string{labels.LabelParentTask: "parked"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed},
	}
	h := newTaskWaitHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindTasks, corev1alpha1.TaskPhaseSucceeded, "child"), replacement)
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("state = %s, want Resumed", wait.State)
	}
	prompt := h.task(wait.ContinuationTaskName).Spec.Prompt
	if strings.Contains(prompt, "child: Failed") || !strings.Contains(prompt, "child: is no longer the task this wait was authorized for") {
		t.Fatalf("a replacement child must not supply the outcome, got %s", prompt)
	}
}

// requesterParkedTestTask is a parked Task whose requester the API sealed.
func requesterParkedTestTask(t *testing.T) *corev1alpha1.Task {
	t.Helper()
	SetRequesterStampKey(testRequesterStampKey)
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	task.Annotations[labels.AnnotationRequestedBySource] = labels.RequestedBySourceAPI
	task.Annotations[labels.AnnotationRequestedByStamp] = connectors.RequesterStamp(testRequesterStampKey, task.UID, "https://issuer.example.test", "alice")
	return task
}

func TestTaskWaitContinuationWaitsForItsRequesterSeal(t *testing.T) {
	h := newTaskWaitHarness(t, requesterParkedTestTask(t))
	// Sealing keeps failing, as if the API server were briefly unavailable.
	h.r.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return errors.New("api unavailable")
		},
	})
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("an unsealed continuation must still be linked, got %s", wait.State)
	}
	continuation := h.task(wait.ContinuationTaskName)
	if continuation.Annotations[labels.AnnotationRequestedByStamp] != "" ||
		continuation.Annotations[labels.AnnotationRequestedBySource] != labels.RequestedBySourceAPI {
		t.Fatalf("the continuation must be created marked as awaiting its seal: %v", continuation.Annotations)
	}
	continuation.CreationTimestamp = metav1.NewTime(taskWaitTestNow)
	if !requesterStampPending(continuation, taskWaitTestNow.Add(time.Second)) {
		t.Fatal("admission must wait for the requester seal instead of freezing without connectors")
	}
}

func TestTaskWaitSealsTheRequesterOfAnAdoptedContinuation(t *testing.T) {
	task := requesterParkedTestTask(t)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	// The controller restarted after creating the continuation, before sealing it.
	existing := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	existing.UID = "uid-existing"
	h := newTaskWaitHarness(t, task, existing)
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("state = %s, want Resumed", got)
	}
	if adopted := h.task(existing.Name); !connectors.RequesterStampValid(testRequesterStampKey, adopted) {
		t.Fatalf("adoption must seal the continuation's requester: %v", adopted.Annotations)
	}
}

func TestContinuationDeadlineDoesNotDriftWithCreationTime(t *testing.T) {
	parked := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	chainDeadline := taskwait.Deadline(parked)
	remaining := chainDeadline.Sub(taskWaitTestNow)
	continuation := buildTaskContinuation(parked, taskwait.ContinuationTaskName(parked), remaining, chainDeadline)
	// The API server assigned the creation time three seconds after the
	// remaining budget was computed.
	continuation.CreationTimestamp = metav1.NewTime(taskWaitTestNow.Add(3 * time.Second))
	if got, ok := acpTaskDeadline(continuation, taskWaitTestNow); !ok || !got.Equal(chainDeadline) {
		t.Fatalf("enforced deadline = %v, want the chain deadline %v", got, chainDeadline)
	}
	if got := taskwait.Deadline(continuation); !got.Equal(chainDeadline) {
		t.Fatalf("wait deadline = %v, want the chain deadline %v", got, chainDeadline)
	}
}

func TestContinuationKeepsItsChatSessionIncarnation(t *testing.T) {
	parked := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	if parked.Labels == nil {
		parked.Labels = map[string]string{}
	}
	parked.Labels[labels.LabelChatSession] = "chat"
	parked.Labels[labels.LabelChatSessionNamespace] = "chat-home"
	parked.Labels[labels.LabelChatSessionEpoch] = "1700000000000000000"
	continuation := buildTaskContinuation(parked, taskwait.ContinuationTaskName(parked), time.Hour, taskWaitTestNow.Add(time.Hour))
	for _, key := range []string{labels.LabelChatSession, labels.LabelChatSessionNamespace, labels.LabelChatSessionEpoch} {
		if got, want := continuation.Labels[key], parked.Labels[key]; got != want {
			t.Fatalf("continuation label %s = %q, want %q", key, got, want)
		}
	}
}

func TestHistoryLimitsPruneAContinuationChainAsOneRun(t *testing.T) {
	scheduled := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: taskWaitTestNamespace, UID: "uid-nightly"},
		Spec: corev1alpha1.TaskSpec{Schedule: "@daily", SuccessfulRunsHistoryLimit: new(int32(1)), FailedRunsHistoryLimit: new(int32(0))}}
	run := func(name string, age time.Duration, phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: taskWaitTestNamespace, UID: types.UID("uid-" + name),
				CreationTimestamp: metav1.NewTime(taskWaitTestNow.Add(-age)),
				Labels:            map[string]string{labels.LabelParentTask: "nightly"},
			},
			Status: corev1alpha1.TaskStatus{Phase: phase},
		}
	}
	// The newest run parked and its continuation succeeded; an older run
	// succeeded without parking.
	older := run("older-run", 5*time.Hour, corev1alpha1.TaskPhaseSucceeded)
	parked := run("parked-run", 3*time.Hour, corev1alpha1.TaskPhaseSucceeded)
	parked.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, ContinuationTaskName: "parked-run-next", ContinuationTaskUID: "uid-parked-run-next",
	}
	next := run("parked-run-next", time.Hour, corev1alpha1.TaskPhaseSucceeded)
	next.Annotations = map[string]string{
		labels.AnnotationContinuationOfName: "parked-run", labels.AnnotationContinuationOfUID: "uid-parked-run",
	}
	// A run whose continuation failed counts as a failed run.
	failedHead := run("failed-head", 2*time.Hour, corev1alpha1.TaskPhaseSucceeded)
	failedHead.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, ContinuationTaskName: "failed-head-next", ContinuationTaskUID: "uid-failed-head-next",
	}
	failedNext := run("failed-head-next", 90*time.Minute, corev1alpha1.TaskPhaseFailed)
	failedNext.Annotations = map[string]string{
		labels.AnnotationContinuationOfName: "failed-head", labels.AnnotationContinuationOfUID: "uid-failed-head",
	}

	r := newUnitReconciler(newTestScheme(), scheduled, older, parked, next, failedHead, failedNext)
	if err := r.enforceHistoryLimits(context.Background(), scheduled); err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	list := &corev1alpha1.TaskList{}
	if err := r.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		remaining[item.Name] = true
	}
	if !remaining["parked-run"] || !remaining["parked-run-next"] {
		t.Fatalf("the newest run's chain must be retained as a unit: %v", remaining)
	}
	if remaining["older-run"] {
		t.Fatalf("the older run beyond the success limit must be pruned: %v", remaining)
	}
	if remaining["failed-head"] {
		t.Fatalf("a chain is pruned through its head by its tail's outcome: %v", remaining)
	}
}

func TestContinuationOfAPublishedWriteTurnUsesTheSessionBaseline(t *testing.T) {
	const preparedSHA, publishedSHA = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	parked := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	parked.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		GitRepo: "https://github.com/orka-agents/example", Intent: corev1alpha1.WorkspaceIntentWrite,
		PushBranch: "orka/release", ExpectedRemoteSHA: preparedSHA,
	}
	chainDeadline := taskwait.Deadline(parked)
	unpublished := buildTaskContinuation(parked, taskwait.ContinuationTaskName(parked), time.Hour, chainDeadline)
	if got := unpublished.Spec.Workspace.ExpectedRemoteSHA; got != preparedSHA {
		t.Fatalf("a turn that published nothing keeps its precondition, got %q", got)
	}

	parked.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateVerifiedExact, Outcome: corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
		ExpectedCommitSHA: publishedSHA, VerifiedRemoteSHA: publishedSHA,
	}
	continuation := buildTaskContinuation(parked, taskwait.ContinuationTaskName(parked), time.Hour, chainDeadline)
	if parked.Spec.Workspace.ExpectedRemoteSHA != preparedSHA {
		t.Fatal("building the continuation must not mutate the parked Task")
	}
	// The Session records the published commit as its verified baseline. The
	// continuation must start from it instead of failing on the old SHA.
	baseline := &store.VerifiedBranchBaseline{RepositoryID: "github.com/orka-agents/example", Ref: "refs/heads/orka/release", SHA: publishedSHA}
	if err := validateSessionExpectedRemoteSHA(unpublished.Spec.Workspace, baseline); err == nil {
		t.Fatal("the pre-publication precondition conflicts with the published baseline")
	}
	if err := validateSessionExpectedRemoteSHA(continuation.Spec.Workspace, baseline); err != nil {
		t.Fatalf("the continuation of a published turn must accept the Session baseline: %v", err)
	}
	workspace, err := runtimeWorkspaceForSessionContinuation(continuation.Spec.Workspace, baseline)
	if err != nil || workspace.Ref != baseline.Ref {
		t.Fatalf("the continuation must prepare its workspace from the published baseline, got %+v, %v", workspace, err)
	}
}

func TestTaskWaitLinksTheContinuationItCreatedDespiteAStatusConflict(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	h := newTaskWaitHarness(t, task)
	created, conflicts := 0, 0
	h.client = fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				created++
				obj.SetUID(types.UID(fmt.Sprintf("created-uid-%d", created)))
				return c.Create(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				// A concurrent writer wins the first link attempt.
				if obj.GetName() == "parked" && created > 0 && conflicts == 0 {
					conflicts++
					return apierrors.NewConflict(schema.GroupResource{Group: "core.orka.ai", Resource: "tasks"}, obj.GetName(), errors.New("modified"))
				}
				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	h.r.Client, h.r.APIReader = h.client, h.client
	// Without provenance admission a later reconcile cannot prove an existing
	// continuation is Orka's, so the reconcile that created it must link it.
	h.r.TaskProvenanceProtected = false
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if conflicts != 1 || created != 1 {
		t.Fatalf("expected one create and one lost race, got %d and %d", created, conflicts)
	}
	if wait.State != corev1alpha1.TaskWaitStateResumed || wait.ContinuationTaskUID != "created-uid-1" {
		t.Fatalf("the created continuation must be linked after a status conflict, got %+v", wait)
	}
	if finalizers := h.task(wait.ContinuationTaskName).Finalizers; len(finalizers) != 0 {
		t.Fatalf("a linked continuation must not keep the link finalizer: %v", finalizers)
	}
}

func TestTaskWaitCancelsTheContinuationItCreatedWhenCancelledDuringALostLink(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	h := newTaskWaitHarness(t, task)
	created := 0
	h.client = fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				created++
				obj.SetUID(types.UID(fmt.Sprintf("created-uid-%d", created)))
				return c.Create(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if obj.GetName() == "parked" && created > 0 {
					current := &corev1alpha1.Task{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
						return err
					}
					if current.Annotations[labels.AnnotationCancelWait] == "" {
						// The user cancels the wait while the link is being written.
						current.Annotations[labels.AnnotationCancelWait] = "no longer needed"
						if err := c.Update(ctx, current); err != nil {
							return err
						}
						return apierrors.NewConflict(schema.GroupResource{Group: "core.orka.ai", Resource: "tasks"}, obj.GetName(), errors.New("modified"))
					}
				}
				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	h.r.Client, h.r.APIReader = h.client, h.client
	h.r.TaskProvenanceProtected = false
	h.settle("parked")
	if wait := h.task("parked").Status.Wait; wait.State != corev1alpha1.TaskWaitStateCancelled {
		t.Fatalf("a cancellation that raced the link must cancel the wait, got %+v", wait)
	}
	for _, existing := range h.tasks() {
		if existing.Name != "parked" && existing.DeletionTimestamp.IsZero() {
			t.Fatalf("the continuation created for a cancelled wait must be deleted, found %s", existing.Name)
		}
	}
}

func TestTaskWaitLinksItsContinuationAfterExhaustingLinkRetries(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	h := newTaskWaitHarness(t, task)
	created, conflicts := 0, 0
	h.client = fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				created++
				obj.SetUID(types.UID(fmt.Sprintf("created-uid-%d", created)))
				return c.Create(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				// Concurrent writers win every link attempt of the reconcile
				// that created the continuation.
				if obj.GetName() == "parked" && created > 0 && conflicts < taskWaitLinkAttempts {
					conflicts++
					return apierrors.NewConflict(schema.GroupResource{Group: "core.orka.ai", Resource: "tasks"}, obj.GetName(), errors.New("modified"))
				}
				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	h.r.Client, h.r.APIReader = h.client, h.client
	h.r.TaskProvenanceProtected = false
	if result := h.reconcile("parked"); result.RequeueAfter != taskWaitStepInterval {
		t.Fatalf("exhausted link retries must requeue, got %+v", result)
	}
	if conflicts != taskWaitLinkAttempts {
		t.Fatalf("expected %d lost link attempts, got %d", taskWaitLinkAttempts, conflicts)
	}
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed || wait.ContinuationTaskUID != "created-uid-1" || created != 1 {
		t.Fatalf("the continuation this controller created must be linked after retries ran out, got %+v (created %d)", wait, created)
	}

	// After a restart nothing proves the same Task was created here: the
	// wait is abandoned and the unproven Task left alone. Execution holds it
	// until a link that will now never be recorded, so it never runs.
	restarted := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	restarted.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	restarted.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(restarted)
	orphan := buildTaskContinuation(restarted, restarted.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	orphan.UID = "created-before-restart"
	fresh := newTaskWaitHarness(t, restarted, orphan)
	fresh.r.TaskProvenanceProtected = false
	fresh.settle("parked")
	if wait := fresh.task("parked").Status.Wait; wait.State != corev1alpha1.TaskWaitStateAbandoned || wait.ContinuationTaskUID != "" {
		t.Fatalf("an unproven continuation must not be linked after a restart, got %+v", wait)
	}
	if got := fresh.task(orphan.Name); !got.DeletionTimestamp.IsZero() {
		t.Fatal("an unproven continuation must not be deleted on a forgeable claim")
	}
}

func TestTaskWaitConfirmsAMissingChildAuthoritativelyBeforeResuming(t *testing.T) {
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: taskWaitTestNamespace, UID: "uid-child",
			Labels:      map[string]string{labels.LabelParentTask: "parked"},
			Annotations: map[string]string{labels.AnnotationParentTaskName: "parked", labels.AnnotationParentTaskUID: "uid-parked"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	h := newTaskWaitHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindTasks, corev1alpha1.TaskPhaseSucceeded, "child"), child)
	// The informer cache has not observed the just-created child yet.
	h.r.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "child" {
				return apierrors.NewNotFound(schema.GroupResource{Group: "core.orka.ai", Resource: "tasks"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateWaiting {
		t.Fatalf("a child the cache has not seen must not resume the parent while it still runs, got %s", got)
	}
	if len(h.tasks()) != 2 {
		t.Fatal("no continuation may be created while the child still runs")
	}
}

func TestContinuationExecutionWaitsForItsPredecessorsLink(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	continuation := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	continuation.UID = "uid-continuation"
	continuation.Finalizers = nil
	h := newTaskWaitHarness(t, task, continuation)
	r := &TaskReconciler{Client: h.client, APIReader: h.client, Scheme: newTestScheme()}
	queue := func() (ctrl.Result, error) {
		t.Helper()
		current := h.task(continuation.Name)
		// The binding was frozen; queueing would start the prompt.
		current.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{}
		return r.queueACPRuntimeTask(context.Background(), current, nil)
	}

	// The predecessor reserved the name but has not linked this Task yet.
	result, err := queue()
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("an unlinked continuation must be held, got %+v, %v", result, err)
	}
	if got := h.task(continuation.Name); got.Status.Execution != nil || got.Status.Phase != "" {
		t.Fatalf("a held continuation must not be queued, got %+v", got.Status)
	}

	// After a restart without provenance admission the wait is abandoned
	// without linking it. The continuation never ran and now never will.
	h.r.TaskProvenanceProtected = false
	h.settle("parked")
	if wait := h.task("parked").Status.Wait; wait.State != corev1alpha1.TaskWaitStateAbandoned {
		t.Fatalf("the unproven continuation must not be linked, got %+v", wait)
	}
	if _, err := queue(); err != nil {
		t.Fatal(err)
	}
	got := h.task(continuation.Name)
	if got.Status.Phase != corev1alpha1.TaskPhaseCancelled || got.Status.Execution == nil ||
		got.Status.Execution.Reason != acpContinuationUnlinkedReason || got.Status.Execution.Attempt != 0 {
		t.Fatalf("an orphaned continuation must be cancelled without running, got %+v", got.Status)
	}
}

func TestLinkedContinuationIsNotHeld(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResumed
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	task.Status.Wait.ContinuationTaskUID = "uid-continuation"
	continuation := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	continuation.UID = "uid-continuation"
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, continuation).Build()
	r := &TaskReconciler{Client: c, APIReader: c}
	if pending, orphaned, err := r.continuationLinkPending(context.Background(), continuation); err != nil || pending || orphaned {
		t.Fatalf("a linked continuation must run, got pending=%v orphaned=%v err=%v", pending, orphaned, err)
	}
	plain := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	if pending, orphaned, err := r.continuationLinkPending(context.Background(), plain); err != nil || pending || orphaned {
		t.Fatalf("an ordinary Task must not be held, got pending=%v orphaned=%v err=%v", pending, orphaned, err)
	}
}

func TestDeletingAParkedTaskErasesHistoryOnlyAfterItsWaitEnds(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded)
	task.Finalizers = []string{labels.TaskFinalizer}
	h := newTaskWaitHarness(t, task)
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateWaiting {
		t.Fatalf("state = %s", got)
	}
	if err := h.client.Delete(context.Background(), h.task("parked")); err != nil {
		t.Fatal(err)
	}
	deleting := h.task("parked")
	r := &TaskReconciler{Client: h.client, APIReader: h.client, ExecutionEventStore: h.events}
	result, err := r.handleDeletion(context.Background(), deleting)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("deletion must wait for the parked wait to end, got %+v, %v", result, err)
	}
	if len(h.parkedApprovals()) != 1 {
		t.Fatal("history must not be erased while the wait reconciler may still write to it")
	}
	// The wait reconciler ends the wait of the deleting Task and closes its
	// sign-off; after that it never writes history again.
	h.settle("parked")
	if wait := h.task("parked").Status.Wait; wait.State != corev1alpha1.TaskWaitStateCancelled {
		t.Fatalf("a deleting Task's wait must be cancelled, got %+v", wait)
	}
	if active, err := r.parkedWaitActive(context.Background(), h.task("parked")); err != nil || active {
		t.Fatalf("an ended wait must not hold deletion, got %v, %v", active, err)
	}
}

func TestContinuationLinkedBetweenReadsIsNeverOrphaned(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Status.Wait.State = corev1alpha1.TaskWaitStateResuming
	task.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(task)
	continuation := buildTaskContinuation(task, task.Status.Wait.ContinuationTaskName, time.Hour, taskWaitTestNow.Add(time.Hour))
	continuation.UID = "uid-continuation"
	reads := 0
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, continuation).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if key.Name == "parked" {
				reads++
				if reads == 1 {
					// The wait reconciler links the continuation right after
					// this read returns.
					linked := obj.(*corev1alpha1.Task).DeepCopy()
					linked.Status.Wait.State = corev1alpha1.TaskWaitStateResumed
					linked.Status.Wait.ContinuationTaskUID = "uid-continuation"
					if err := c.Status().Update(ctx, linked); err != nil {
						return err
					}
				}
			}
			return nil
		}}).Build()
	r := &TaskReconciler{Client: c, APIReader: c}
	pending, orphaned, err := r.continuationLinkPending(context.Background(), continuation)
	if err != nil || orphaned || !pending {
		t.Fatalf("a continuation linked between reads must be held, not orphaned: pending=%v orphaned=%v err=%v", pending, orphaned, err)
	}
	if pending, orphaned, err = r.continuationLinkPending(context.Background(), continuation); err != nil || orphaned || pending {
		t.Fatalf("the linked continuation must run, got pending=%v orphaned=%v err=%v", pending, orphaned, err)
	}
}

func TestForbidConcurrencyCountsAResumedRunUntilItsContinuationSettles(t *testing.T) {
	run := func(name string) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: taskWaitTestNamespace, UID: types.UID("uid-" + name),
				Labels: map[string]string{labels.LabelParentTask: "nightly", labels.LabelScheduledRun: scheduledRunLabelValue},
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		}
	}
	continuation := func(phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
		next := run("parked-run-next")
		next.Annotations = map[string]string{
			labels.AnnotationContinuationOfName: "parked-run", labels.AnnotationContinuationOfUID: "uid-parked-run",
		}
		next.Status.Phase = phase
		return next
	}
	for name, tc := range map[string]struct {
		cached        *corev1alpha1.Task // the continuation as the informer cache holds it
		authoritative *corev1alpha1.Task // the continuation as the API server holds it
		wantNewRun    bool
	}{
		"continuation has no phase yet": {
			cached: continuation(""), authoritative: continuation(""),
		},
		"continuation is not in the cache yet": {
			authoritative: continuation(corev1alpha1.TaskPhaseRunning),
		},
		"continuation is not in the cache and has no phase": {
			authoritative: continuation(""),
		},
		"continuation parked again": {
			cached: continuation(corev1alpha1.TaskPhaseSucceeded),
			authoritative: func() *corev1alpha1.Task {
				next := continuation(corev1alpha1.TaskPhaseSucceeded)
				next.Status.Wait = &corev1alpha1.TaskWaitStatus{State: corev1alpha1.TaskWaitStateWaiting, Kind: corev1alpha1.TaskWaitKindTimer}
				return next
			}(),
		},
		"continuation finished": {
			cached: continuation(corev1alpha1.TaskPhaseSucceeded), authoritative: continuation(corev1alpha1.TaskPhaseSucceeded),
			wantNewRun: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			scheduled := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: taskWaitTestNamespace, UID: "uid-nightly"},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeContainer, Image: "busybox", Schedule: "* * * * *",
					ConcurrencyPolicy: corev1alpha1.ForbidConcurrent,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseScheduled,
					// The next tick after this is already due and inside the starting deadline.
					LastScheduleTime: new(metav1.NewTime(time.Now().Add(-70 * time.Second))),
				},
			}
			parked := run("parked-run")
			parked.Status.Wait = &corev1alpha1.TaskWaitStatus{
				State: corev1alpha1.TaskWaitStateResumed, Kind: corev1alpha1.TaskWaitKindTimer,
				ContinuationTaskName: "parked-run-next", ContinuationTaskUID: "uid-parked-run-next",
			}
			cachedObjects := []client.Object{scheduled, parked}
			if tc.cached != nil {
				cachedObjects = append(cachedObjects, tc.cached)
			}
			r := newUnitReconciler(newTestScheme(), cachedObjects...)
			r.APIReader = fake.NewClientBuilder().WithScheme(newTestScheme()).
				WithObjects(scheduled.DeepCopy(), parked.DeepCopy(), tc.authoritative).Build()

			if _, err := r.handleScheduled(context.Background(), scheduled); err != nil {
				t.Fatal(err)
			}
			list := &corev1alpha1.TaskList{}
			if err := r.List(context.Background(), list, client.InNamespace(taskWaitTestNamespace)); err != nil {
				t.Fatal(err)
			}
			newRun := false
			for _, item := range list.Items {
				if strings.HasPrefix(item.Name, "nightly-") {
					newRun = true
				}
			}
			if newRun != tc.wantNewRun {
				t.Fatalf("new scheduled run created = %v, want %v", newRun, tc.wantNewRun)
			}
		})
	}
}
