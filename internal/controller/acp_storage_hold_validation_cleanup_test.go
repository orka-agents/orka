package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// recoveredValidationConflictStatus is the delivery recovery records for a
// workspace validation it could not finish before recording the outcome as
// unknown.
func recoveredValidationConflictStatus() *corev1alpha1.TaskDeliveryStatus {
	return &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
		Reason: acpRecoveredWorkspaceLostReason, Message: acpRecoveredWorkspaceLostMessage,
	}
}

// residentValidatingSupervisor serves a runtime whose RuntimeSession is still
// Validating: it settles the abandoned prompt, validates the workspace, and
// deletes the session only after that, with an authenticated tombstone.
type residentValidatingSupervisor struct {
	t       *testing.T
	fence   harnessv2.Fence
	control *validationRecoveryStore

	mu        sync.Mutex
	validated bool
	deleted   bool
	calls     []string
}

func (s *residentValidatingSupervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath {
		statusFence := s.fence
		statusFence.RuntimeSessionUID = ""
		statusFence.RuntimeSessionGeneration = 0
		now := time.Now().UTC()
		var sessions []harnessv2.RuntimeSessionStatus
		if !s.deleted {
			state := harnessv2.RuntimeSessionStateValidating
			if s.validated {
				state = harnessv2.RuntimeSessionStateIdle
			}
			sessions = []harnessv2.RuntimeSessionStatus{{
				RuntimeSessionID: harnessv2.RuntimeSessionID(runtimeSessionID(s.fence)), RuntimeSessionUID: s.fence.RuntimeSessionUID,
				Generation: s.fence.RuntimeSessionGeneration, State: state, LastTransitionAt: now,
			}}
		}
		writeDispatcherJSON(w, harnessv2.StatusResponse{
			Protocol: harnessv2.ProtocolVersion, Fence: statusFence, Timestamp: now, Sessions: sessions,
			Pressure:  harnessv2.PressureMetadata{ResidentSessions: uint32(len(sessions))},
			Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true},
		})
		return
	}
	if !s.control.inMutation.Load() {
		s.t.Error("runtime cleanup mutation escaped the current-owner interlock")
	}
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	switch {
	case strings.HasSuffix(r.URL.Path, "/cancel"):
		writeDispatcherJSON(w, harnessv2.CancelPromptResponse{
			Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationSettled, Phase: harnessv2.OperationPhaseSettled, TerminalEvent: harnessv2.EventCompleted},
			BarrierState: harnessv2.CancellationBarrierSettled, SettlementProven: true,
			Settlement: harnessv2.PromptSettlement{TerminalEvent: harnessv2.EventCompleted, Outcome: harnessv2.PromptOutcomeSucceeded, StopReason: harnessv2.ACPStopReasonEndTurn, SettledAt: time.Now().UTC()},
		})
	case strings.Contains(r.URL.Path, "/workspace-deltas/"):
		var request harnessv2.CreateWorkspaceDeltaRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			s.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.validated = true
		writeDispatcherJSON(w, harnessv2.CreateWorkspaceDeltaResponse{
			Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
			Delta: harnessv2.WorkspaceDeltaDescriptor{
				DeltaID: request.DeltaID, RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, SessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration,
				State: harnessv2.WorkspaceDeltaNoChange, Intent: request.Intent, VerifiedBaseline: request.VerifiedBaseline,
				NoFollowVerified: true, PublicationSafe: true, FrozenAt: time.Now().UTC(),
			},
		})
	case r.Method == http.MethodDelete:
		if !s.validated {
			s.t.Error("the RuntimeSession was deleted before its validation finished")
		}
		var request harnessv2.DeleteRuntimeSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			s.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.deleted = true
		writeDispatcherJSON(w, harnessv2.DeleteRuntimeSessionResponse{
			Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
			State: harnessv2.RuntimeSessionStateDeleted, Tombstone: testDeleteTombstone(request, time.Now().UTC()),
		})
	default:
		s.t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// A Task whose hold was acknowledged after workspace validation started ends
// OutcomeUnknown with the recovered validation conflict. When its runtime
// still holds that validation, cleanup finishes it like any abandoned
// validation and deletes the RuntimeSession through an authenticated
// tombstone, so Task deletion and pool rollout are not blocked, also when a
// later controller epoch owns the cleanup.
func TestRecoveredValidationConflictCleansUpAResidentValidatingRuntime(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		name := "same epoch"
		if takeover {
			name = "after takeover"
		}
		t.Run(name, func(t *testing.T) {
			testRecoveredValidationConflictCleanup(t, takeover)
		})
	}
}

func testRecoveredValidationConflictCleanup(t *testing.T, takeover bool) {
	d, task, control, fence := validationRecoveryFixture(t, false)
	control.attempt.DeliveryState = store.PromptDeliveryConflict
	task.Status.Delivery = recoveredValidationConflictStatus()
	if takeover {
		// The runtime keeps the epoch it booted under; a successor owns cleanup.
		d.Epochs = readyAgentRuntimeTestEpochManager(2)
		owner, err := d.Epochs.CurrentFence(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		control.fence = owner
	}
	supervisor := &residentValidatingSupervisor{t: t, fence: fence, control: control}
	server := httptest.NewServer(supervisor)
	defer server.Close()
	validationRecoveryPoolForServer(t, d, task, server.URL)

	complete := false
	for range 4 {
		var err error
		complete, err = d.reconcileRecoveredTaskScopedRuntimeSession(t.Context(), task, task.UID, true)
		if err != nil {
			t.Fatalf("runtime cleanup: %v", err)
		}
		if complete {
			break
		}
	}
	supervisor.mu.Lock()
	calls, deleted := append([]string(nil), supervisor.calls...), supervisor.deleted
	supervisor.mu.Unlock()
	if !complete || !deleted {
		t.Fatalf("runtime cleanup complete=%v deleted=%v calls=%v", complete, deleted, calls)
	}
	latest := &corev1alpha1.Task{}
	if err := d.Client.Get(t.Context(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, latest); err != nil {
		t.Fatal(err)
	}
	if !runtimeSessionCleanupCompleteForUID(latest, task.UID) || latest.Status.Execution.RuntimeSessionCleanupDigest == "" {
		t.Fatalf("the Task does not record the runtime cleanup receipt: %#v", latest.Status.Execution)
	}
	if latest.Status.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown || !recoveredValidationConflict(latest.Status.Delivery) {
		t.Fatalf("cleanup changed the recovered outcome: execution %#v delivery %#v", latest.Status.Execution, latest.Status.Delivery)
	}
}

// Only the conflict recovery records for an unknown outcome counts as an
// abandoned validation; other conflicts, and the same conflict on a successful
// or failed attempt, keep their existing cleanup rules.
func TestAbandonedValidationAcceptsOnlyTheRecoveredValidationConflict(t *testing.T) {
	for _, change := range []string{"other conflict reason", "succeeded attempt", "failed attempt", "attempt still validating", "Task without the conflict"} {
		t.Run(change, func(t *testing.T) {
			d, task, control, fence := validationRecoveryFixture(t, false)
			control.attempt.DeliveryState = store.PromptDeliveryConflict
			task.Status.Delivery = recoveredValidationConflictStatus()
			switch change {
			case "other conflict reason":
				task.Status.Delivery.Reason = "BranchMoved"
			case "succeeded attempt":
				control.attempt.ExecutionState = store.PromptExecutionSucceeded
			case "failed attempt":
				control.attempt.ExecutionState = store.PromptExecutionFailed
				task.Status.Execution.State = corev1alpha1.TaskExecutionStateFailed
				task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeFailed
			case "attempt still validating":
				control.attempt.DeliveryState = store.PromptDeliveryValidating
			case "Task without the conflict":
				task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateValidating}
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
			defer server.Close()
			runtimeClient, err := harnessv2.NewClient(server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("fixture-only-", 3)), harnessv2.WithOperationCapabilitySecret(bytes.Repeat([]byte{0x46}, 32)))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.finishAbandonedWorkspaceValidation(t.Context(), task, task.UID, runtimeClient, fence); err == nil || calls != 0 {
				t.Fatalf("abandoned validation accepted %s: calls=%d err=%v", change, calls, err)
			}
		})
	}
}
