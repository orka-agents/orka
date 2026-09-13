package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type gatewayCleanupPendingStore struct {
	store.GatewayEventStore
	pending bool
}

func (s *gatewayCleanupPendingStore) AdmitGatewayEvent(ctx context.Context, request store.GatewayEventAdmission) (*store.GatewayEvent, bool, error) {
	if s.pending {
		return nil, false, store.ErrGatewaySessionCleanupPending
	}
	return s.GatewayEventStore.AdmitGatewayEvent(ctx, request)
}

func TestGatewayIngressRetriesPendingSessionCleanupWithoutConsumingEvent(t *testing.T) {
	service, db, _ := newGatewayServiceFixture(t)
	ctx := context.Background()
	events := &gatewayCleanupPendingStore{GatewayEventStore: db, pending: true}
	service.EventStore = events
	body := gatewayEventBody(t, "cleanup-pending-event", "user-1")
	_, err := service.AdmitEvent(ctx, "default", "chat", "Bearer inbound-token", body)
	var responseError *HTTPError
	if !errors.As(err, &responseError) || responseError.Code != http.StatusServiceUnavailable {
		t.Fatalf("pending cleanup must be retryable: %v", err)
	}
	records, err := db.ListGatewayEvents(ctx, store.GatewayEventFilter{Namespace: "default"})
	if err != nil || len(records) != 0 {
		t.Fatalf("pending cleanup consumed external event identity: %+v, %v", records, err)
	}
	events.pending = false
	accepted, err := service.AdmitEvent(ctx, "default", "chat", "Bearer inbound-token", body)
	if err != nil || accepted.Status != ingressStatusAccepted {
		t.Fatalf("same event could not be retried after cleanup: %+v, %v", accepted, err)
	}
}

type gatewayCleanupTestCoordinator struct {
	candidates []store.GatewaySessionCleanupCandidate
	requests   []store.ReclaimGatewaySessionRequest
	fence      store.ControllerEpochFence
	listError  error
	fenceError error
}

func (s *gatewayCleanupTestCoordinator) ListGatewaySessionCleanupCandidates(_ context.Context, filter store.GatewaySessionCleanupFilter) (store.GatewaySessionCleanupPage, error) {
	if s.listError != nil {
		return store.GatewaySessionCleanupPage{}, s.listError
	}
	var candidates []store.GatewaySessionCleanupCandidate
	for _, candidate := range s.candidates {
		if (filter.Namespace == "" || candidate.Namespace == filter.Namespace) &&
			(candidate.Namespace > filter.AfterNamespace || (candidate.Namespace == filter.AfterNamespace && candidate.SessionName > filter.AfterSessionName)) {
			candidate.Proof.TerminalCutoff = filter.TerminalCutoff
			candidates = append(candidates, candidate)
		}
	}
	slices.SortFunc(candidates, func(a, b store.GatewaySessionCleanupCandidate) int {
		if a.Namespace != b.Namespace {
			return strings.Compare(a.Namespace, b.Namespace)
		}
		return strings.Compare(a.SessionName, b.SessionName)
	})
	page := store.GatewaySessionCleanupPage{Complete: len(candidates) < filter.Limit}
	page.Candidates = candidates[:min(len(candidates), filter.Limit)]
	if len(page.Candidates) > 0 {
		last := page.Candidates[len(page.Candidates)-1]
		page.NextNamespace, page.NextSessionName = last.Namespace, last.SessionName
	}
	return page, nil
}

func (s *gatewayCleanupTestCoordinator) CurrentFence(context.Context) (store.ControllerEpochFence, error) {
	return s.fence, s.fenceError
}

func (s *gatewayCleanupTestCoordinator) ReclaimGatewaySession(_ context.Context, request store.ReclaimGatewaySessionRequest) error {
	s.requests = append(s.requests, request)
	if request.Session.SessionName == "blocked" {
		return store.ErrConflict
	}
	return nil
}

func TestGatewaySessionRetentionContinuesPastBlockedSession(t *testing.T) {
	coordinator := &gatewayCleanupTestCoordinator{
		candidates: []store.GatewaySessionCleanupCandidate{
			{Namespace: "default", SessionName: "blocked"}, {Namespace: "default", SessionName: "ready"}, {Namespace: "unrelated", SessionName: "other"},
		},
		fence: store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 3, HolderID: "controller"},
	}
	service := &Service{Config: Config{Namespace: "default", BatchSize: 2}, SessionCleanup: coordinator, SessionCleanupCandidates: coordinator, SessionCleanupEpochs: coordinator}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Hour)
	if err := service.cleanupRetainedGatewaySessions(context.Background(), now, cutoff); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("blocked Session error lost: %v", err)
	}
	if len(coordinator.requests) != 2 || coordinator.requests[1].Session.SessionName != "ready" {
		t.Fatalf("one blocked Session stopped independent cleanup or crossed namespace: %+v", coordinator.requests)
	}
	for _, request := range coordinator.requests {
		if request.Fence != coordinator.fence || !request.RequestedAt.Equal(now) || !request.Session.Proof.TerminalCutoff.Equal(cutoff) {
			t.Fatalf("cleanup lost current epoch or retention boundary: %+v", request)
		}
	}
}

func TestGatewaySessionRetentionPagesPastBlockedCandidatesAndWraps(t *testing.T) {
	coordinator := &gatewayCleanupTestCoordinator{
		candidates: []store.GatewaySessionCleanupCandidate{
			{Namespace: "default", SessionName: "blocked"}, {Namespace: "default", SessionName: "ready"}, {Namespace: "unrelated", SessionName: "other"},
		},
		fence: store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 3, HolderID: "controller"},
	}
	service := &Service{Config: Config{Namespace: "default", BatchSize: 1}, SessionCleanup: coordinator, SessionCleanupCandidates: coordinator, SessionCleanupEpochs: coordinator}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Hour)
	for _, failedRead := range []*error{&coordinator.listError, &coordinator.fenceError} {
		*failedRead = store.ErrConflict
		if err := service.cleanupRetainedGatewaySessions(t.Context(), now, cutoff); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("read failure lost: %v", err)
		}
		*failedRead = nil
		if len(coordinator.requests) != 0 {
			t.Fatal("cleanup continued without its candidate page and current fence")
		}
	}
	if err := service.cleanupRetainedGatewaySessions(t.Context(), now, cutoff); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("blocked Session error lost: %v", err)
	}
	if len(coordinator.requests) != 1 || coordinator.requests[0].Session.SessionName != "blocked" {
		t.Fatalf("first bounded pass = %+v", coordinator.requests)
	}
	if err := service.cleanupRetainedGatewaySessions(t.Context(), now, cutoff); err != nil {
		t.Fatal(err)
	}
	if len(coordinator.requests) != 2 || coordinator.requests[1].Session.SessionName != "ready" {
		t.Fatalf("blocked Session starved the next page: %+v", coordinator.requests)
	}
	if err := service.cleanupRetainedGatewaySessions(t.Context(), now, cutoff); err != nil {
		t.Fatal(err)
	}
	if len(coordinator.requests) != 2 {
		t.Fatalf("cleanup crossed its namespace: %+v", coordinator.requests)
	}
	if err := service.cleanupRetainedGatewaySessions(t.Context(), now, cutoff); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("blocked Session was not retried after the scan wrapped: %v", err)
	}
	if len(coordinator.requests) != 3 || coordinator.requests[2].Session.SessionName != "blocked" {
		t.Fatalf("wrapped pass = %+v", coordinator.requests)
	}
}

type gatewayArchiveResultStore struct {
	store.ResultStore
	receipt *store.SessionTurnCleanupReceipt
}

func (s gatewayArchiveResultStore) GetSessionTurnCleanupReceipt(_ context.Context, namespace, session, attempt string) (*store.SessionTurnCleanupReceipt, error) {
	if s.receipt.Namespace != namespace || s.receipt.SessionName != session || s.receipt.PromptAttemptID != attempt {
		return nil, store.ErrNotFound
	}
	return s.receipt, nil
}

func TestGatewayTaskRetentionUsesArchiveAfterEventTombstoneExpires(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact Task", true: "replacement Task"}[replacement], func(t *testing.T) {
			service, _, _ := newGatewayServiceFixture(t)
			ctx := context.Background()
			now := time.Now().UTC()
			old := now.Add(-3 * time.Hour)
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "archived-task", UID: "archived-task-uid", CreationTimestamp: metav1.NewTime(old), Labels: map[string]string{TaskGatewayEventLabel: "old-event"}},
				Spec: corev1alpha1.TaskSpec{
					RequestedBy: &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/default/namespace-uid/chat/gateway-uid"},
					SessionRef:  &corev1alpha1.SessionReference{Name: "old-session"},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, Execution: &corev1alpha1.TaskExecutionStatus{
					RuntimeSessionUID: "old-session-uid", RuntimeSessionGeneration: 1, Attempt: 1, PromptID: "old-prompt",
				}},
			}
			key := store.SessionTurnKey{SessionUID: "old-session-uid", LeaseGeneration: 1, TaskUID: string(task.UID), Attempt: 1, PromptID: "old-prompt"}
			turnID, err := key.CanonicalID()
			if err != nil {
				t.Fatal(err)
			}
			attemptID, err := (store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: key.TaskUID, Attempt: key.Attempt, PromptID: key.PromptID}).CanonicalID()
			if err != nil {
				t.Fatal(err)
			}
			operationID, operationDigest := store.GatewaySessionCleanupOperation(task.Namespace, task.Spec.SessionRef.Name, key.SessionUID, "gateway-uid", "binding-uid")
			payload := []byte(`{"phase":"Succeeded"}`)
			receipt := &store.SessionTurnCleanupReceipt{
				Namespace: task.Namespace, SessionName: task.Spec.SessionRef.Name, OperationID: operationID, OperationDigest: operationDigest,
				TurnID: turnID, Key: key, PromptAttemptID: attemptID, TerminalKind: store.SessionTurnAssistantResult, FinalizedAt: old,
				ProjectionID: store.CanonicalControlID("outbox", turnID, "TaskTerminalStatus"), ProjectionKind: "TaskTerminalStatus", ProjectionDigest: store.CanonicalBytesDigest(payload),
				AggregateKind: "SessionTurn", AggregateID: turnID, Payload: payload, PayloadDigest: store.CanonicalBytesDigest(payload),
				ProjectionState: store.OutboxProjectionDelivered, DeliveryDigest: store.CanonicalBytesDigest([]byte("delivered")), DeliveredAt: &old,
			}
			service.ResultStore = gatewayArchiveResultStore{ResultStore: service.ResultStore, receipt: receipt}
			if replacement {
				task.UID = "replacement-task-uid"
			}
			if err := service.Client.Create(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := service.cleanupRetainedGatewayTasks(ctx, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			var tasks corev1alpha1.TaskList
			if err := service.Client.List(ctx, &tasks); err != nil {
				t.Fatal(err)
			}
			want := 0
			if replacement {
				want = 1
			}
			if len(tasks.Items) != want {
				t.Fatalf("remaining Tasks=%d, want %d", len(tasks.Items), want)
			}
		})
	}
}
