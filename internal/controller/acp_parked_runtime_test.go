/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

func TestTurnRequestedParkMatchesOnlyTheSettlingTurn(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default", UID: "uid-t"},
		Status: corev1alpha1.TaskStatus{
			Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 2, PromptID: "prompt-uid-t-2"},
			Wait: &corev1alpha1.TaskWaitStatus{
				State: corev1alpha1.TaskWaitStateRequested, Kind: corev1alpha1.TaskWaitKindTimer, Attempt: 2, PromptID: "prompt-uid-t-2",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	d := &ACPDispatcher{Client: c, APIReader: c}
	if !d.turnRequestedPark(context.Background(), task) {
		t.Fatal("the settling turn's own wait must release its runtime")
	}
	other := task.DeepCopy()
	other.Status.Execution.Attempt, other.Status.Execution.PromptID = 3, "prompt-uid-t-3"
	if d.turnRequestedPark(context.Background(), other) {
		t.Fatal("a wait from another attempt must not release this turn's runtime")
	}
	replaced := task.DeepCopy()
	replaced.UID = "uid-other"
	if d.turnRequestedPark(context.Background(), replaced) {
		t.Fatal("a replaced Task must not match")
	}
}

func TestFailedParkedRuntimeReleaseIsRetriedBeforeTheNextTurnClaimsASlot(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	failing := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		deleted = append(deleted, r.URL.Path)
		if failing {
			writeDispatcherJSONStatus(w, http.StatusServiceUnavailable, harnessv2.ErrorResponse{
				Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeInvalidRequest, Message: "supervisor unavailable",
			})
			return
		}
		// Not found is a completed deletion.
		writeDispatcherJSONStatus(w, http.StatusNotFound, harnessv2.ErrorResponse{
			Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeInvalidRequest, Message: "runtime session not found",
		})
	}))
	defer server.Close()
	deletes := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), deleted...)
	}
	runtimeClient, err := harnessv2.NewClient(
		server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("t", 32)),
		harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("s", 32))),
	)
	if err != nil {
		t.Fatal(err)
	}
	parked := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "parked", UID: types.UID("99999999-9999-9999-9999-999999999999")},
		Status:     corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-parked"}},
	}
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot-id", ControllerEpoch: 1,
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1, RuntimeProfileDigest: harnessv2.ProfileDigest(testControlDigestForDispatcher("parked-profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion, RuntimeSessionUID: "session-parked", RuntimeSessionGeneration: 2,
	}
	d := &ACPDispatcher{}
	d.setRuntimeSessionBinding(ACPRuntimeSessionBinding{SessionUID: "session-parked", Generation: 2, RecreationRequired: true})
	d.releaseParkedRuntimeSession(context.Background(), runtimeClient, "runtime-session-parked-g2", parked, fence, "session-parked")
	if _, pending := d.pendingParkedRuntimeRelease("session-parked"); !pending {
		t.Fatal("a failed release must be kept for the next turn to retry")
	}
	if d.currentRuntimeSessionBinding("session-parked") == nil {
		t.Fatal("a failed release must keep the binding that requires recreation")
	}

	// The next turn would reuse generation 2: never delete it.
	calls := len(deletes())
	d.retryParkedRuntimeRelease(context.Background(), runtimeClient, "session-parked", "runtime-instance", 2)
	if len(deletes()) != calls {
		t.Fatal("the release must not delete a generation the current turn may use")
	}

	// A still-failing retry keeps the release pending.
	d.retryParkedRuntimeRelease(context.Background(), runtimeClient, "session-parked", "runtime-instance", 3)
	if len(deletes()) != calls+1 {
		t.Fatalf("the next turn must retry the release before claiming a slot, got %d calls", len(deletes())-calls)
	}
	if _, pending := d.pendingParkedRuntimeRelease("session-parked"); !pending {
		t.Fatal("a failed retry must stay pending")
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	d.retryParkedRuntimeRelease(context.Background(), runtimeClient, "session-parked", "runtime-instance", 3)
	if got := deletes(); len(got) != calls+2 || !strings.Contains(got[len(got)-1], "runtime-session-parked-g2") {
		t.Fatalf("the retry must delete the parked RuntimeSession, got %v", got)
	}
	if _, pending := d.pendingParkedRuntimeRelease("session-parked"); pending {
		t.Fatal("a completed release must be forgotten")
	}

	// A replaced runtime instance retired its sessions; nothing is deleted.
	d.setPendingParkedRuntimeRelease("session-parked", acpParkedRuntimeRelease{task: parked, runtimeSessionID: "runtime-session-parked-g2", fence: fence})
	calls = len(deletes())
	d.retryParkedRuntimeRelease(context.Background(), runtimeClient, "session-parked", "runtime-instance-2", 3)
	if _, pending := d.pendingParkedRuntimeRelease("session-parked"); pending || len(deletes()) != calls {
		t.Fatal("a release on a replaced runtime instance must be dropped without a delete")
	}
}

func TestParkedRuntimeReleaseKeepsASuccessorsBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDispatcherJSONStatus(w, http.StatusNotFound, harnessv2.ErrorResponse{
			Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeInvalidRequest, Message: "runtime session not found",
		})
	}))
	defer server.Close()
	runtimeClient, err := harnessv2.NewClient(
		server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("t", 32)),
		harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("s", 32))),
	)
	if err != nil {
		t.Fatal(err)
	}
	parked := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "parked", UID: types.UID("99999999-9999-9999-9999-999999999999")},
		Status:     corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-parked"}},
	}
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot-id", ControllerEpoch: 1,
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1, RuntimeProfileDigest: harnessv2.ProfileDigest(testControlDigestForDispatcher("parked-profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion, RuntimeSessionUID: "session-parked", RuntimeSessionGeneration: 2,
	}
	d := &ACPDispatcher{}
	// A later turn of the Session installed its own binding while this
	// deletion was in flight.
	successor := ACPRuntimeSessionBinding{SessionUID: "session-parked", Generation: 3, RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot-id"}
	d.setRuntimeSessionBinding(successor)
	d.releaseParkedRuntimeSession(context.Background(), runtimeClient, "runtime-session-parked-g2", parked, fence, "session-parked")
	if got := d.currentRuntimeSessionBinding("session-parked"); got == nil || *got != successor {
		t.Fatalf("releasing the retired RuntimeSession must keep the successor's binding, got %+v", got)
	}

	retired := ACPRuntimeSessionBinding{SessionUID: "session-parked", Generation: 2, RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot-id", RecreationRequired: true}
	d.setRuntimeSessionBinding(retired)
	d.releaseParkedRuntimeSession(context.Background(), runtimeClient, "runtime-session-parked-g2", parked, fence, "session-parked")
	if got := d.currentRuntimeSessionBinding("session-parked"); got != nil {
		t.Fatalf("the retired RuntimeSession's own binding must be forgotten, got %+v", got)
	}
}

func TestTurnRequestedParkFailsClosedOnALookupError(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default", UID: "uid-t"},
		Status:     corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-uid-t-1"}},
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(task).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("apiserver unavailable")
		}}).Build()
	d := &ACPDispatcher{Client: c, APIReader: c}
	if !d.turnRequestedPark(context.Background(), task) {
		t.Fatal("a turn whose park cannot be ruled out must release its runtime")
	}
}

func TestRecoveredParkedTurnNeverKeepsAReusableRuntimeBinding(t *testing.T) {
	const sessionUID = "acp-session-parked-recovery"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parked", Namespace: "default", UID: "uid-parked"},
		Spec:       corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: "durable", Append: true}},
		Status: corev1alpha1.TaskStatus{
			Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-uid-parked-1"},
			Wait: &corev1alpha1.TaskWaitStatus{
				State: corev1alpha1.TaskWaitStateWaiting, Kind: corev1alpha1.TaskWaitKindTimer, Attempt: 1, PromptID: "prompt-uid-parked-1",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	d := &ACPDispatcher{Client: c, APIReader: c}
	binding := ACPRuntimeSessionBinding{SessionUID: sessionUID, Generation: 1}
	d.setRuntimeSessionBinding(binding)
	attempt := &store.PromptAttempt{ExecutionState: store.PromptExecutionSucceeded, DeliveryState: store.PromptDeliveryNotRequested}
	d.retireRecoveredRuntimeSessionBinding(context.Background(), task, attempt, binding)
	if d.currentRuntimeSessionBinding(sessionUID) != nil {
		t.Fatal("recovery must not keep the live runtime of a turn that parked; its continuation starts from the transcript")
	}
}
