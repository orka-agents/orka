/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/orka-agents/orka/internal/taskwait"
)

func parkTestTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "coord", Namespace: defaultNamespace, UID: "uid-coord",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAgent,
			Timeout:    &metav1.Duration{Duration: 2 * time.Hour},
			SessionRef: &corev1alpha1.SessionReference{Name: "s", Append: true},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt-uid-coord-1",
			},
		},
	}
}

func parkToolContext(c client.Client, operationID string) *ToolContext {
	return &ToolContext{
		Client: c, PolicyReader: c, Namespace: defaultNamespace, TaskID: "coord", TaskUID: "uid-coord",
		OperationID: operationID, Brokered: true, ExecutionEvents: storetest.NewFakeExecutionEventStore(),
	}
}

func executePark(t *testing.T, toolCtx *ToolContext, args string) (string, error) {
	t.Helper()
	return NewParkUntilTool().Execute(WithToolContext(context.Background(), toolCtx), json.RawMessage(args))
}

func loadParkTask(t *testing.T, c client.Client) *corev1alpha1.Task {
	t.Helper()
	task := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: defaultNamespace, Name: "coord"}, task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestParkUntilRequiresTheBroker(t *testing.T) {
	c := newFakeClient(parkTestTask())
	toolCtx := parkToolContext(c, "op-1")
	toolCtx.Brokered = false
	if _, err := executePark(t, toolCtx, `{"kind":"timer","reason":"backoff","duration":"5m"}`); err == nil {
		t.Fatal("park_until must be unavailable outside the controller broker")
	}
}

func TestParkUntilRecordsATimerWaitBoundToTheRunningTurn(t *testing.T) {
	c := newFakeClient(parkTestTask())
	out, err := executePark(t, parkToolContext(c, "op-1"), `{"kind":"timer","reason":"rate limited","duration":"10m"}`)
	if err != nil {
		t.Fatalf("park_until: %v", err)
	}
	if !strings.Contains(out, `"status":"parked"`) || !strings.Contains(out, "End your turn now") {
		t.Fatalf("unexpected response %s", out)
	}
	wait := loadParkTask(t, c).Status.Wait
	if wait == nil || wait.State != corev1alpha1.TaskWaitStateRequested || wait.Kind != corev1alpha1.TaskWaitKindTimer ||
		wait.Attempt != 1 || wait.PromptID != "prompt-uid-coord-1" || wait.ResumeAt == nil || wait.OperationDigest == "" {
		t.Fatalf("unexpected wait %+v", wait)
	}
	if !wait.Deadline.After(wait.ResumeAt.Time) {
		t.Fatal("the wait deadline must follow the timer")
	}
}

func TestParkUntilReplayIsIdempotentAndASecondRequestIsRejected(t *testing.T) {
	c := newFakeClient(parkTestTask())
	if _, err := executePark(t, parkToolContext(c, "op-1"), `{"kind":"timer","reason":"r","duration":"10m"}`); err != nil {
		t.Fatal(err)
	}
	first := loadParkTask(t, c).Status.Wait.DeepCopy()
	if _, err := executePark(t, parkToolContext(c, "op-1"), `{"kind":"timer","reason":"r","duration":"10m"}`); err != nil {
		t.Fatalf("an exact replay must succeed: %v", err)
	}
	if got := loadParkTask(t, c).Status.Wait; got.OperationDigest != first.OperationDigest || !got.RequestedAt.Equal(&first.RequestedAt) {
		t.Fatal("an exact replay must not rewrite the recorded wait")
	}
	if _, err := executePark(t, parkToolContext(c, "op-2"), `{"kind":"timer","reason":"r","duration":"10m"}`); err == nil {
		t.Fatal("a second wait in the same turn must be rejected")
	}
}

func TestParkUntilRejectsIneligibleTasksAndSettledTurns(t *testing.T) {
	noSession := parkTestTask()
	noSession.Spec.SessionRef = nil
	if _, err := executePark(t, parkToolContext(newFakeClient(noSession), "op"), `{"kind":"timer","reason":"r","duration":"5m"}`); err == nil {
		t.Fatal("a Task without a Session cannot park")
	}
	settling := parkTestTask()
	settling.Status.Execution.State = corev1alpha1.TaskExecutionStateSettling
	if _, err := executePark(t, parkToolContext(newFakeClient(settling), "op"), `{"kind":"timer","reason":"r","duration":"5m"}`); err == nil {
		t.Fatal("only a running turn can park")
	}
	tight := parkTestTask()
	tight.Spec.Timeout = &metav1.Duration{Duration: 2 * time.Minute}
	if _, err := executePark(t, parkToolContext(newFakeClient(tight), "op"), `{"kind":"timer","reason":"r","duration":"5s"}`); err == nil {
		t.Fatal("a turn too close to its deadline cannot park")
	}
}

func TestParkUntilApprovalRecordsAParkedSignOffRequest(t *testing.T) {
	c := newFakeClient(parkTestTask())
	toolCtx := parkToolContext(c, "op-1")
	out, err := executePark(t, toolCtx, `{"kind":"approval","reason":"May I merge the release PR?"}`)
	if err != nil {
		t.Fatalf("park_until: %v", err)
	}
	wait := loadParkTask(t, c).Status.Wait
	if wait.Kind != corev1alpha1.TaskWaitKindApproval || len(wait.References) != 1 || !strings.Contains(out, wait.References[0]) {
		t.Fatalf("unexpected approval wait %+v / %s", wait, out)
	}
	listed, err := approvals.ListEvents(context.Background(), toolCtx.ExecutionEvents, defaultNamespace, "coord")
	if err != nil {
		t.Fatal(err)
	}
	derived := approvals.Derive(listed, time.Time{})
	if len(derived) != 1 || derived[0].ID != wait.References[0] || derived[0].Status != approvals.StatusPending ||
		derived[0].Binding != nil || derived[0].Action != taskwait.ParkedApprovalAction || derived[0].TaskUID != "uid-coord" {
		t.Fatalf("unexpected parked approval %+v", derived)
	}
}

func TestParkUntilTasksRequiresAuthorizedChildren(t *testing.T) {
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "child", Namespace: defaultNamespace, UID: "uid-child",
		Labels:      map[string]string{labels.LabelParentTask: "coord"},
		Annotations: map[string]string{labels.AnnotationParentTaskUID: "uid-coord"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "coord", UID: "uid-coord", Controller: new(true),
		}},
	}}
	stranger := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "stranger", Namespace: defaultNamespace, UID: "uid-s"}}
	c := newFakeClient(parkTestTask(), child, stranger)
	toolCtx := parkToolContext(c, "op-1")
	toolCtx.TaskProvenanceProtected = true
	if _, err := executePark(t, toolCtx, `{"kind":"tasks","reason":"waiting for review","tasks":["stranger"]}`); err == nil {
		t.Fatal("a Task outside the caller's children must be rejected")
	}
	if _, err := executePark(t, toolCtx, `{"kind":"tasks","reason":"waiting for review","tasks":["child","child"]}`); err == nil {
		t.Fatal("duplicate child names must be rejected")
	}
	if _, err := executePark(t, toolCtx, `{"kind":"tasks","reason":"waiting for review","tasks":["child"]}`); err != nil {
		t.Fatalf("park on an authorized child: %v", err)
	}
	if wait := loadParkTask(t, c).Status.Wait; len(wait.References) != 1 || wait.References[0] != "child" ||
		len(wait.ReferenceUIDs) != 1 || wait.ReferenceUIDs[0] != "uid-child" {
		t.Fatalf("unexpected references %+v %+v", wait.References, wait.ReferenceUIDs)
	}
}

func TestContinuationKeepsAuthorityOverItsPredecessorsChildren(t *testing.T) {
	predecessor := parkTestTask()
	predecessor.Name, predecessor.UID = "coord-prev", "uid-prev"
	predecessor.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	predecessor.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, ContinuationTaskName: "coord", ContinuationTaskUID: "uid-coord",
	}
	continuation := parkTestTask()
	continuation.Annotations = map[string]string{
		labels.AnnotationContinuationOfName: "coord-prev", labels.AnnotationContinuationOfUID: "uid-prev",
	}
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "child", Namespace: defaultNamespace, UID: "uid-child",
		Labels:      map[string]string{labels.LabelParentTask: "coord-prev"},
		Annotations: map[string]string{labels.AnnotationParentTaskUID: "uid-prev"},
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(predecessor, corev1alpha1.GroupVersion.WithKind("Task")),
		},
	}}
	c := newFakeClient(predecessor, continuation, child)
	toolCtx := parkToolContext(c, "op-1")
	toolCtx.TaskProvenanceProtected = true
	if _, err := executePark(t, toolCtx, `{"kind":"tasks","reason":"r","tasks":["child"]}`); err != nil {
		t.Fatalf("a continuation must keep authority over its predecessor's children: %v", err)
	}

	// The annotation alone proves nothing: without the predecessor's resumed
	// status naming this Task, the child stays out of scope.
	forgedPredecessor := predecessor.DeepCopy()
	forgedPredecessor.ResourceVersion = ""
	forgedPredecessor.Status.Wait.ContinuationTaskUID = "uid-someone-else"
	fresh := parkTestTask()
	fresh.Annotations = continuation.Annotations
	c = newFakeClient(forgedPredecessor, fresh, child.DeepCopy())
	toolCtx = parkToolContext(c, "op-1")
	toolCtx.TaskProvenanceProtected = true
	if _, err := executePark(t, toolCtx, `{"kind":"tasks","reason":"r","tasks":["child"]}`); err == nil {
		t.Fatal("an unverified predecessor link must not grant child authority")
	}
}

func TestParkUntilBoundsTheReasonInCharacters(t *testing.T) {
	// A schema-valid multibyte reason is accepted even though it exceeds
	// MaxReasonLength bytes.
	multibyte := strings.Repeat("é", taskwait.MaxReasonLength)
	c := newFakeClient(parkTestTask())
	args, _ := json.Marshal(map[string]string{"kind": "timer", "reason": multibyte, "duration": "10m"})
	if _, err := executePark(t, parkToolContext(c, "op-1"), string(args)); err != nil {
		t.Fatalf("a %d-character multibyte reason must be accepted: %v", taskwait.MaxReasonLength, err)
	}
	if got := loadParkTask(t, c).Status.Wait.Reason; got != multibyte {
		t.Fatalf("the multibyte reason must be kept intact, got %d characters", utf8.RuneCountInString(got))
	}

	tooLong, _ := json.Marshal(map[string]string{"kind": "timer", "reason": multibyte + "é", "duration": "10m"})
	if _, err := executePark(t, parkToolContext(newFakeClient(parkTestTask()), "op-1"), string(tooLong)); err == nil {
		t.Fatal("a reason over the character limit must be rejected")
	}

	// Redaction placeholders are longer than the short secrets they replace,
	// so a valid reason can grow past the limit; the persisted value stays bounded.
	secrets := strings.Repeat("password=x ", taskwait.MaxReasonLength/len("password=x "))
	expanding, _ := json.Marshal(map[string]string{"kind": "timer", "reason": secrets, "duration": "10m"})
	c = newFakeClient(parkTestTask())
	if _, err := executePark(t, parkToolContext(c, "op-1"), string(expanding)); err != nil {
		t.Fatalf("a reason within the limit must be accepted after redaction: %v", err)
	}
	got := loadParkTask(t, c).Status.Wait.Reason
	if n := utf8.RuneCountInString(got); n > taskwait.MaxReasonLength {
		t.Fatalf("the redacted reason must stay within %d characters, got %d", taskwait.MaxReasonLength, n)
	}
	if strings.Contains(got, "password=x") {
		t.Fatal("the persisted reason must be redacted")
	}
}
