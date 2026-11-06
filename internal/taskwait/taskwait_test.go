/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskwait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(s)
	return s
}

func parkedTask(name, uid string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid)},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAgent,
			SessionRef: &corev1alpha1.SessionReference{Name: "s", Append: true},
		},
	}
}

func TestContinuationTaskNameIsDeterministicBoundedAndIncarnationScoped(t *testing.T) {
	first := parkedTask("work", "uid-1")
	if a, b := ContinuationTaskName(first), ContinuationTaskName(first.DeepCopy()); a != b || !strings.HasPrefix(a, "work-resume-") {
		t.Fatalf("name is not deterministic: %q %q", a, b)
	}
	reused := parkedTask("work", "uid-2")
	if ContinuationTaskName(first) == ContinuationTaskName(reused) {
		t.Fatal("a reused Task name must not collide with the earlier incarnation's continuation")
	}
	long := parkedTask(strings.Repeat("a", 253), "uid-3")
	if got := ContinuationTaskName(long); len(got) > 253 {
		t.Fatalf("continuation name length %d exceeds the object name limit", len(got))
	}
}

func TestResolveTailFollowsVerifiedContinuations(t *testing.T) {
	root := parkedTask("root", "uid-root")
	root.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	root.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, ContinuationTaskName: "next", ContinuationTaskUID: "uid-next",
	}
	next := parkedTask("next", "uid-next")
	next.Annotations = map[string]string{labels.AnnotationContinuationOfUID: "uid-root"}
	next.Status.Phase = corev1alpha1.TaskPhaseRunning

	reader := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(root, next).Build()
	tail, err := ResolveTail(context.Background(), reader, root)
	if err != nil || tail.Name != "next" {
		t.Fatalf("ResolveTail = %v, %v; want next", tail, err)
	}
	if Settled(tail) {
		t.Fatal("a running continuation must not settle the chain")
	}

	forged := parkedTask("next", "uid-other")
	forged.Annotations = map[string]string{labels.AnnotationContinuationOfUID: "uid-root"}
	reader = fake.NewClientBuilder().WithScheme(scheme()).WithObjects(root, forged).Build()
	if _, err := ResolveTail(context.Background(), reader, root); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("a replaced continuation must break the chain, got %v", err)
	}
}

func TestSettledRequiresTerminalPhaseWithoutActiveWait(t *testing.T) {
	task := parkedTask("t", "u")
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if !Settled(task) {
		t.Fatal("a succeeded Task without a wait is settled")
	}
	for _, state := range []corev1alpha1.TaskWaitState{
		corev1alpha1.TaskWaitStateRequested, corev1alpha1.TaskWaitStateWaiting, corev1alpha1.TaskWaitStateResuming,
	} {
		task.Status.Wait = &corev1alpha1.TaskWaitStatus{State: state}
		if Settled(task) {
			t.Fatalf("a Task with a %s wait must not settle", state)
		}
	}
	task.Status.Wait = &corev1alpha1.TaskWaitStatus{State: corev1alpha1.TaskWaitStateExpired}
	if !Settled(task) {
		t.Fatal("an expired wait settles the Task")
	}
}

func TestEligibilityRejectsInputsAContinuationCannotInherit(t *testing.T) {
	base := parkedTask("t", "u")
	if err := Eligibility(base); err != nil {
		t.Fatalf("eligible Task rejected: %v", err)
	}
	cases := map[string]func(*corev1alpha1.Task){
		"no session":  func(task *corev1alpha1.Task) { task.Spec.SessionRef = nil },
		"no append":   func(task *corev1alpha1.Task) { task.Spec.SessionRef.Append = false },
		"gateway":     func(task *corev1alpha1.Task) { task.Spec.SessionRef.ThroughMessageID = "gateway:e:user" },
		"transaction": func(task *corev1alpha1.Task) { task.Spec.Transaction = &corev1alpha1.TaskTransaction{} },
		"schedule":    func(task *corev1alpha1.Task) { task.Spec.Schedule = "* * * * *" },
		"prior diff":  func(task *corev1alpha1.Task) { task.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{Name: "p"} },
		"chain at max": func(task *corev1alpha1.Task) {
			task.Annotations = map[string]string{labels.AnnotationContinuationIndex: "32"}
		},
		"bad index": func(task *corev1alpha1.Task) {
			task.Annotations = map[string]string{labels.AnnotationContinuationIndex: "x"}
		},
	}
	for name, mutate := range cases {
		task := base.DeepCopy()
		mutate(task)
		if err := Eligibility(task); !errors.Is(err, ErrIneligible) {
			t.Errorf("%s: Eligibility = %v, want ErrIneligible", name, err)
		}
	}
}

func TestDeadlineUsesTimeoutOrHarnessDefault(t *testing.T) {
	task := parkedTask("t", "u")
	created := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	task.CreationTimestamp = metav1.NewTime(created)
	if got := Deadline(task); !got.Equal(created.Add(DefaultAgentTaskTimeout)) {
		t.Fatalf("default deadline = %v", got)
	}
	task.Spec.Timeout = &metav1.Duration{Duration: 4 * time.Hour}
	if got := Deadline(task); !got.Equal(created.Add(4 * time.Hour)) {
		t.Fatalf("timeout deadline = %v", got)
	}
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	// An inherited chain deadline can only shorten the deadline.
	task.Annotations[labels.AnnotationContinuationChainDeadline] = created.Add(time.Hour).Format(time.RFC3339)
	if got := Deadline(task); !got.Equal(created.Add(time.Hour)) {
		t.Fatalf("chain deadline = %v", got)
	}
	task.Annotations[labels.AnnotationContinuationChainDeadline] = created.Add(8 * time.Hour).Format(time.RFC3339)
	if got := Deadline(task); !got.Equal(created.Add(4 * time.Hour)) {
		t.Fatalf("a later chain deadline must not extend the Task, got %v", got)
	}
}

func TestAcceptsApprovalDecisionOnlyForTheActiveParkedApproval(t *testing.T) {
	task := parkedTask("t", "u")
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateWaiting, Kind: corev1alpha1.TaskWaitKindApproval, References: []string{"a1"},
	}
	if !AcceptsApprovalDecision(task, "a1") {
		t.Fatal("the active parked approval must accept a decision")
	}
	if AcceptsApprovalDecision(task, "a2") {
		t.Fatal("another approval ID must not be accepted")
	}
	task.Status.Wait.State = corev1alpha1.TaskWaitStateRequested
	if !AcceptsApprovalDecision(task, "a1") {
		t.Fatal("a requested wait whose turn succeeded must accept a decision")
	}
	for _, phase := range []corev1alpha1.TaskPhase{corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled} {
		task.Status.Phase = phase
		if AcceptsApprovalDecision(task, "a1") {
			t.Fatalf("a requested wait after a %s turn is abandoned and must not accept a decision", phase)
		}
	}
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.Wait.State = corev1alpha1.TaskWaitStateExpired
	if AcceptsApprovalDecision(task, "a1") {
		t.Fatal("an ended wait must not accept a late decision")
	}
	task.Status.Wait.State = corev1alpha1.TaskWaitStateWaiting
	task.Status.Wait.Kind = corev1alpha1.TaskWaitKindTimer
	if AcceptsApprovalDecision(task, "a1") {
		t.Fatal("a non-approval wait must not accept decisions")
	}
}
