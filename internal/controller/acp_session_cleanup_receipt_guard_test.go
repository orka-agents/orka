package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSessionCleanupReceiptUsesOriginalFence(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		name := "current owner"
		if takeover {
			name = "takeover before write"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newExternalACPDispatchFixture(t)
			owner, err := fixture.epochs.CurrentFence(fixture.ctx)
			if err != nil {
				t.Fatal(err)
			}
			// The receipt boundary receives an already verified terminal target.
			// Construct it directly so this test does not execute prompt planning.
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "receipt-task", UID: types.UID("receipt-task-uid")},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, SessionRef: &corev1alpha1.SessionReference{Name: "receipt-conversation"}},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1,
					Execution: &corev1alpha1.TaskExecutionStatus{
						State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
						Attempt: 1, PromptID: "receipt-prompt", RequestDigest: testControlDigestForDispatcher("receipt-prompt"),
						ControllerEpoch: owner.Epoch, AgentRuntimeName: "receipt-runtime", AgentRuntimeUID: "receipt-runtime-uid",
						RuntimeInstanceID: "receipt-instance", RuntimeSessionSupervisorBootID: "receipt-boot",
						RuntimeSessionProfileDigest: testControlDigestForDispatcher("receipt-profile"),
						RuntimeSessionUID:           "receipt-session-uid", RuntimeSessionGeneration: 1,
					},
				},
			}
			if err := fixture.client.Create(fixture.ctx, task); err != nil {
				t.Fatal(err)
			}
			target := &sessionRuntimeCleanupTarget{
				task: task.DeepCopy(), taskUID: task.UID,
				identity: sessionRuntimeCleanupIdentityForExecution(task.Status.Execution),
			}
			guard := &sessionCleanupReceiptTakeoverStore{DurableControlStore: fixture.controlStore, takeover: takeover}
			fixture.dispatcher.Store = guard
			err = fixture.dispatcher.recordSessionRuntimeCleanupForTask(fixture.ctx, target, owner)
			if guard.calls != 1 {
				t.Fatalf("receipt bypassed its epoch mutation guard: calls=%d", guard.calls)
			}
			if takeover {
				if !errors.Is(err, store.ErrConflict) {
					t.Fatalf("stale owner receipt write = %v", err)
				}
			} else if err != nil {
				t.Fatalf("current owner receipt write: %v", err)
			}
			current := &corev1alpha1.Task{}
			if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil {
				t.Fatal(err)
			}
			if takeover {
				if current.Status.Execution.RuntimeSessionCleanupDigest != "" {
					t.Fatal("stale cleanup owner wrote a Task receipt")
				}
			} else if !runtimeSessionCleanupCompleteForUID(current, task.UID) {
				t.Fatal("current cleanup owner did not persist the exact receipt")
			}
			current.Status.Execution.RuntimeSessionCleanupDigest = ""
			if current.UID != task.UID || !reflect.DeepEqual(current.Spec, task.Spec) || !reflect.DeepEqual(current.Status, task.Status) {
				t.Fatal("receipt changed the original Task identity or execution")
			}
			if fixture.createCalls.Load() != 0 || fixture.deleteCalls.Load() != 0 {
				t.Fatal("receipt write performed runtime I/O")
			}
		})
	}
}

type sessionCleanupReceiptTakeoverStore struct {
	store.DurableControlStore
	takeover bool
	calls    int
}

func (s *sessionCleanupReceiptTakeoverStore) WithControllerEpochMutation(ctx context.Context, fence store.ControllerEpochFence, fn func(context.Context) error) error {
	s.calls++
	if s.takeover {
		current, err := s.GetControllerEpoch(ctx, fence.Name)
		if err != nil {
			return err
		}
		holder := "receipt-takeover"
		_, err = s.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{
			Name: current.Name, ExpectedVersion: current.Version, ExpectedEpoch: current.Epoch, NewEpoch: current.Epoch + 1,
			HolderID: holder, UpdatedAt: time.Now().UTC(),
			RequestDigest: controllerEpochDigest(current.Name, holder, current.Version, current.Epoch, current.Epoch+1),
		})
		if err != nil {
			return err
		}
	}
	return s.DurableControlStore.(store.ControllerEpochMutationStore).WithControllerEpochMutation(ctx, fence, fn)
}
