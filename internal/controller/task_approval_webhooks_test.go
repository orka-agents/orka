/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	storesqlite "github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/taskwebhook"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	approvalWebhookTestUID   = types.UID("6a2e3b4c-2222-4b4b-9c9c-0123456789ab")
	approvalWebhookOtherUID  = types.UID("7b3f4c5d-3333-4c4c-8d8d-0123456789ab")
	approvalWebhookTestTask  = "approval-task"
	approvalWebhookSecretArg = "sentinel-secret-argument"
)

var approvalWebhookTestNow = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// approvalWebhookHarness drives approval notices over a fake API, the
// fenced storage client over SQLite, and a recording receiver. Every
// reconcile uses a fresh notifier, as a restarted controller would: all state
// comes from status.
type approvalWebhookHarness struct {
	t      *testing.T
	now    time.Time
	client ctrlclient.Client
	db     *storesqlite.Store
	// storage is the fenced client; events is what the controller reads and
	// writes through, storage unless a test intercepts it.
	storage  *storageclient.Client
	events   store.ExecutionEventStore
	receiver *webhookReceiver
	keys     taskwebhook.KeySet
	// maxAttempts overrides the approval attempt bound when non-zero.
	maxAttempts int32
	// signingUnavailable configures a key source whose keys are missing.
	signingUnavailable bool
}

func newApprovalWebhookHarness(t *testing.T, task *corev1alpha1.Task, statuses ...int) *approvalWebhookHarness {
	t.Helper()
	h := &approvalWebhookHarness{t: t, now: approvalWebhookTestNow, receiver: newWebhookReceiver(t, statuses...), keys: newWebhookTestKeys(t)}
	connection, err := storesqlite.NewDB(filepath.Join(t.TempDir(), "approval-webhooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	h.db = storesqlite.NewStore(connection, "approval-webhooks-test")
	h.storage = approvalStorageClient(t, h.db)
	h.events = h.storage
	if task != nil {
		task.Spec.WebhookURL = h.receiver.server.URL + "/hook"
		// The Task reconciler records usage in the store, keyed by namespace UID.
		h.client = newWebhookTestClient(t, task, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNS, UID: "namespace-uid"}})
	}
	return h
}

// newApprovalWebhookTask returns a Task that has run. A Task that selects
// approval events has tracking set, as status initialization records it.
func newApprovalWebhookTask(phase corev1alpha1.TaskPhase, selected ...corev1alpha1.TaskWebhookEvent) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: approvalWebhookTestTask, Namespace: defaultNS, UID: approvalWebhookTestUID},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, WebhookEvents: selected},
		Status:     corev1alpha1.TaskStatus{Phase: phase, Attempts: 1},
	}
	if len(selected) > 0 {
		task.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true}
	}
	return task
}

var bothApprovalEvents = []corev1alpha1.TaskWebhookEvent{
	corev1alpha1.TaskWebhookEventApprovalRequested, corev1alpha1.TaskWebhookEventApprovalResolved,
}

func (h *approvalWebhookHarness) task() *corev1alpha1.Task {
	h.t.Helper()
	task := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), ctrlclient.ObjectKey{Namespace: defaultNS, Name: approvalWebhookTestTask}, task); err != nil {
		h.t.Fatal(err)
	}
	return task
}

func (h *approvalWebhookHarness) notifier() *WebhookNotifier {
	notifier := newTestWebhookNotifier(h.t, h.keys, nil)
	notifier.now = func() time.Time { return h.now }
	if h.maxAttempts > 0 {
		notifier.approvalMaxAttempts = h.maxAttempts
	}
	if h.signingUnavailable {
		notifier.SetKeySource(taskwebhook.StaticKeySource{})
	}
	return notifier
}

// reconcileApprovalNotices runs one approval pass with no phase work between
// recording notices and sending them, as the Task reconciler does around it.
func (w *WebhookNotifier) reconcileApprovalNotices(
	ctx context.Context,
	status taskStatusWriter,
	eventStore store.ExecutionEventStore,
	task *corev1alpha1.Task,
) (time.Duration, error) {
	prepared, err := w.prepareApprovalNotices(ctx, status, eventStore, task)
	if err != nil {
		return 0, err
	}
	sent, err := w.sendApprovalNotices(ctx, status, task)
	if err != nil {
		return 0, err
	}
	return minPositiveDuration(prepared, sent), nil
}

// reconcile runs one approval pass for the stored Task.
func (h *approvalWebhookHarness) reconcile() time.Duration {
	h.t.Helper()
	requeue, err := h.notifier().reconcileApprovalNotices(context.Background(), h.client.Status(), h.events, h.task())
	if err != nil {
		h.t.Fatalf("reconcileApprovalNotices: %v", err)
	}
	return requeue
}

func (h *approvalWebhookHarness) append(event *store.ExecutionEvent) *store.ExecutionEvent {
	h.t.Helper()
	event.Namespace, event.StreamType, event.StreamID, event.TaskName = defaultNS, events.ExecutionEventStreamTypeTask, approvalWebhookTestTask, approvalWebhookTestTask
	if event.Severity == "" {
		event.Severity = events.ExecutionEventSeverityInfo
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = h.now
	}
	appended, err := h.storage.AppendExecutionEvent(context.Background(), event)
	if err != nil {
		h.t.Fatal(err)
	}
	return appended
}

// request appends an AI-worker style approval request with free text that
// must never reach the receiver.
func (h *approvalWebhookHarness) request(uid types.UID, approvalID string) *store.ExecutionEvent {
	h.t.Helper()
	content, _ := json.Marshal(map[string]any{
		"approvalID": approvalID, "taskUID": string(uid), "action": "sentinel-action deploy prod",
		"riskSummary": "sentinel-risk drops the table", "targetTool": "deploy_service", "severity": "warning",
		"targetArgsDigest": "sha256:sentinel-digest", "targetArgsPreview": map[string]string{"token": approvalWebhookSecretArg},
	})
	return h.append(&store.ExecutionEvent{
		Type: events.ExecutionEventTypeApprovalRequested, ToolName: "deploy_service", ToolCallID: approvalID,
		Summary: "sentinel-summary", Content: content,
	})
}

// boundRequest appends a brokered review whose expiry the model enforces.
func (h *approvalWebhookHarness) boundRequest(approvalID string, expiresAt time.Time) {
	h.t.Helper()
	content, _ := json.Marshal(map[string]any{
		"approvalID": approvalID, "taskUID": string(approvalWebhookTestUID), "action": "sentinel-action",
		"targetTool": "github.create_pr", "severity": "warning", "expiresAt": expiresAt,
		"binding": approvals.CallBinding{TaskAttempt: 1, PromptID: "prompt-1", OperationIDDigest: "sha256:op", RequestDigest: "sha256:req"},
	})
	h.append(&store.ExecutionEvent{
		Type: events.ExecutionEventTypeApprovalRequested, ToolName: "call_tool", ToolCallID: approvalID, Content: content,
	})
}

// decide appends a decision as the approval API does, with reviewer text.
func (h *approvalWebhookHarness) decide(uid types.UID, approvalID, eventType string) *store.ExecutionEvent {
	h.t.Helper()
	content, _ := json.Marshal(map[string]string{
		"approvalID": approvalID, "decision": "x", "reason": "sentinel-reason looks fine", "actor": "sentinel-actor@example.com", "taskUID": string(uid),
	})
	return h.append(&store.ExecutionEvent{Type: eventType, ToolCallID: approvalID, Summary: "sentinel-decision", Content: content})
}

type approvalReceipt struct {
	payload ApprovalWebhookPayload
	raw     map[string]any
	claims  *taskwebhook.Claims
	receipt webhookReceipt
}

// receipts verifies every received approval notice and returns them in
// arrival order.
func (h *approvalWebhookHarness) receipts() []approvalReceipt {
	h.t.Helper()
	task := h.task()
	all := h.receiver.all()
	out := make([]approvalReceipt, 0, len(all))
	for _, receipt := range all {
		claims := verifyReceipt(h.t, h.keys, task.Spec.WebhookURL, receipt, receiptIssuedAt(h.t, receipt))
		var payload ApprovalWebhookPayload
		if err := json.Unmarshal(receipt.body, &payload); err != nil {
			h.t.Fatal(err)
		}
		var raw map[string]any
		_ = json.Unmarshal(receipt.body, &raw)
		if claims.EventType != payload.EventType || receipt.header.Get(taskwebhook.HeaderEvent) != payload.EventType ||
			claims.DeliveryID != payload.DeliveryID || receipt.header.Get(taskwebhook.HeaderDeliveryID) != payload.DeliveryID {
			h.t.Fatalf("claims %+v and headers disagree with body %+v", claims, payload)
		}
		if claims.PayloadSHA256 != taskwebhook.PayloadDigest(receipt.body) {
			h.t.Fatalf("payload_sha256 = %s, want digest of the body", claims.PayloadSHA256)
		}
		if claims.Task.UID != string(task.UID) || claims.Task.Name != task.Name || claims.Task.Namespace != task.Namespace {
			h.t.Fatalf("task claim = %+v", claims.Task)
		}
		out = append(out, approvalReceipt{payload: payload, raw: raw, claims: claims, receipt: receipt})
	}
	return out
}

// receiptIssuedAt reads the unverified issue time of a receipt's token, so a
// receipt recorded earlier is verified as of when it was received.
func receiptIssuedAt(t *testing.T, receipt webhookReceipt) time.Time {
	t.Helper()
	parts := strings.Split(receipt.header.Get(taskwebhook.HeaderSignature), ".")
	if len(parts) != 3 {
		t.Fatal("receipt carries no compact JWS")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims taskwebhook.Claims
	if err := json.Unmarshal(decoded, &claims); err != nil {
		t.Fatal(err)
	}
	return time.Unix(claims.IssuedAt, 0)
}

func eventTypes(receipts []approvalReceipt) []string {
	out := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		out = append(out, receipt.payload.EventType)
	}
	return out
}

func TestApprovalWebhooksLegacyTaskGetsTerminalOnly(t *testing.T) {
	task := newApprovalWebhookTask(corev1alpha1.TaskPhaseSucceeded)
	h := newApprovalWebhookHarness(t, task)
	h.request(approvalWebhookTestUID, "approval-legacy")
	h.decide(approvalWebhookTestUID, "approval-legacy", events.ExecutionEventTypeApprovalApproved)

	for range 3 {
		if requeue := h.reconcile(); requeue != 0 {
			t.Fatalf("legacy Task requeued %v for approval notices", requeue)
		}
	}
	if stored := h.task(); stored.Status.ApprovalWebhooks != nil || len(h.receiver.all()) != 0 {
		t.Fatalf("legacy Task projected approval notices: %+v, %d receipts", stored.Status.ApprovalWebhooks, len(h.receiver.all()))
	}

	if _, err := h.notifier().reconcileDelivery(context.Background(), h.client.Status(), h.task()); err != nil {
		t.Fatal(err)
	}
	receipts := h.receiver.all()
	if len(receipts) != 1 || receipts[0].header.Get(taskwebhook.HeaderEvent) != "task.succeeded" {
		t.Fatalf("want exactly the terminal notice, got %d receipts", len(receipts))
	}
}

func TestApprovalWebhooksNotifyEachTransitionExactlyOnceAcrossRestarts(t *testing.T) {
	for _, tc := range []struct {
		eventType  string
		resolution string
	}{
		{events.ExecutionEventTypeApprovalApproved, approvals.StatusApproved},
		{events.ExecutionEventTypeApprovalDeclined, approvals.StatusDeclined},
		{events.ExecutionEventTypeApprovalCancelled, approvals.StatusCancelled},
	} {
		t.Run(tc.resolution, func(t *testing.T) {
			h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...))
			requested := h.request(approvalWebhookTestUID, "approval-1")
			h.reconcile()
			h.reconcile()
			h.now = h.now.Add(90 * time.Second)
			decided := h.decide(approvalWebhookTestUID, "approval-1", tc.eventType)
			for range 3 {
				h.reconcile()
			}

			receipts := h.receipts()
			if got := eventTypes(receipts); !slices.Equal(got, []string{"task.approval_requested", "task.approval_resolved"}) {
				t.Fatalf("events = %v", got)
			}
			first, second := receipts[0].payload, receipts[1].payload
			if first.DeliveryID != approvalWebhookDeliveryID(string(approvalWebhookTestUID), "approval-1", corev1alpha1.TaskApprovalWebhookRequested) ||
				second.DeliveryID != approvalWebhookDeliveryID(string(approvalWebhookTestUID), "approval-1", corev1alpha1.TaskApprovalWebhookResolved) {
				t.Fatalf("delivery IDs are not deterministic: %s, %s", first.DeliveryID, second.DeliveryID)
			}
			if first.StreamSeq != requested.Seq || second.StreamSeq != decided.Seq || first.StreamSeq >= second.StreamSeq {
				t.Fatalf("streamSeq = %d, %d; want %d < %d", first.StreamSeq, second.StreamSeq, requested.Seq, decided.Seq)
			}
			if first.OccurredAt != approvalWebhookTestNow.Format(time.RFC3339) ||
				second.OccurredAt != approvalWebhookTestNow.Add(90*time.Second).Format(time.RFC3339) {
				t.Fatalf("occurredAt = %s, %s; want the recording events' times", first.OccurredAt, second.OccurredAt)
			}
			if first.Resolution != "" || second.Resolution != tc.resolution || second.ApprovalID != "approval-1" ||
				second.TargetTool != "deploy_service" || second.Severity != "warning" || second.Parked || second.ExpiresAt != "" {
				t.Fatalf("resolved payload = %+v", second)
			}
			if first.SchemaVersion != ApprovalWebhookSchemaVersion || first.TaskUID != string(approvalWebhookTestUID) ||
				first.TaskName != approvalWebhookTestTask || first.TaskNamespace != defaultNS {
				t.Fatalf("requested payload = %+v", first)
			}
			state := h.task().Status.ApprovalWebhooks
			if state.ProjectedSeq != decided.Seq || len(state.Notices) != 2 {
				t.Fatalf("state = %+v", state)
			}
			for _, notice := range state.Notices {
				if notice.State != corev1alpha1.TaskWebhookDeliveryDelivered || notice.Attempts != 1 {
					t.Fatalf("notice %s = %s after %d attempts", notice.DeliveryID, notice.State, notice.Attempts)
				}
			}
		})
	}
}

func TestApprovalWebhooksRecordBrokeredExpiryOnceBeforeNotifying(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...))
	expiresAt := approvalWebhookTestNow.Add(10 * time.Minute)
	h.boundRequest("approval-bound", expiresAt)
	if requeue := h.reconcile(); requeue != 10*time.Minute+approvalExpiryGrace {
		t.Fatalf("requeue = %v, want the expiry plus grace", requeue)
	}

	// Past expiresAt but within the grace the broker owns the expiry.
	h.now = expiresAt.Add(time.Second)
	h.reconcile()
	if got := len(h.receipts()); got != 1 {
		t.Fatalf("receipts = %d, want only the request before the expiry is recorded", got)
	}

	h.now = expiresAt.Add(approvalExpiryGrace)
	for range 3 {
		h.reconcile()
	}
	listed, err := approvals.ListEvents(context.Background(), h.events, defaultNS, approvalWebhookTestTask)
	if err != nil {
		t.Fatal(err)
	}
	expiries := slices.DeleteFunc(slices.Clone(listed), func(event store.ExecutionEvent) bool {
		return event.Type != events.ExecutionEventTypeApprovalExpired
	})
	if len(expiries) != 1 {
		t.Fatalf("expiry events = %d, want exactly one durable expiry", len(expiries))
	}
	receipts := h.receipts()
	if got := eventTypes(receipts); !slices.Equal(got, []string{"task.approval_requested", "task.approval_resolved"}) {
		t.Fatalf("events = %v", got)
	}
	resolved := receipts[1].payload
	if resolved.Resolution != approvals.StatusExpired || resolved.StreamSeq != expiries[0].Seq ||
		resolved.ExpiresAt != expiresAt.Format(time.RFC3339) || resolved.TargetTool != "github.create_pr" {
		t.Fatalf("resolved payload = %+v", resolved)
	}
}

func TestApprovalWebhooksBrokeredDecisionBeforeExpiryWins(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskWebhookEventApprovalResolved))
	expiresAt := approvalWebhookTestNow.Add(time.Minute)
	h.boundRequest("approval-bound", expiresAt)
	h.decide(approvalWebhookTestUID, "approval-bound", events.ExecutionEventTypeApprovalApproved)
	h.now = expiresAt.Add(time.Hour)
	h.reconcile()
	receipts := h.receipts()
	if len(receipts) != 1 || receipts[0].payload.Resolution != approvals.StatusApproved {
		t.Fatalf("receipts = %+v, want only the approved resolution", receipts)
	}
}

func TestApprovalWebhooksIsolateSameNameTasksByUID(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...))
	// An earlier Task with the same name left its approvals in the stream.
	h.request(approvalWebhookOtherUID, "approval-old")
	h.decide(approvalWebhookOtherUID, "approval-old", events.ExecutionEventTypeApprovalApproved)
	h.reconcile()
	if got := len(h.receiver.all()); got != 0 {
		t.Fatalf("receipts = %d, want none for another Task's approvals", got)
	}

	h.request(approvalWebhookTestUID, "approval-new")
	h.reconcile()
	receipts := h.receipts()
	if len(receipts) != 1 || receipts[0].payload.ApprovalID != "approval-new" {
		t.Fatalf("receipts = %+v, want only this Task's request", receipts)
	}
}

func TestApprovalWebhooksPayloadCarriesNoFreeText(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...))
	h.request(approvalWebhookTestUID, "approval-1")
	h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalDeclined)
	h.reconcile()
	h.reconcile()

	allowed := []string{
		"schemaVersion", "deliveryId", "eventType", "occurredAt", "taskUID", "taskName", "taskNamespace",
		"streamSeq", "approvalId", "targetTool", "severity", "expiresAt", "resolution", "parked",
	}
	receipts := h.receipts()
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipts))
	}
	for _, receipt := range receipts {
		for key := range receipt.raw {
			if !slices.Contains(allowed, key) {
				t.Fatalf("payload has unexpected field %q", key)
			}
		}
		if strings.Contains(string(receipt.receipt.body), "sentinel") {
			t.Fatalf("payload leaks agent or reviewer text: %s", receipt.receipt.body)
		}
	}
	if strings.Contains(fmt.Sprintf("%+v", h.task().Status.ApprovalWebhooks), "sentinel") {
		t.Fatal("status records agent or reviewer text")
	}
}

func TestApprovalWebhooksSkipUnsafeIdentifiers(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...))
	h.request(approvalWebhookTestUID, "approve me at https://evil.example/?q=1")
	h.reconcile()
	if got := len(h.receiver.all()); got != 0 {
		t.Fatalf("receipts = %d, want none for an unsafe approval ID", got)
	}
	if state := h.task().Status.ApprovalWebhooks; state == nil || state.ProjectedSeq == 0 || len(state.Notices) != 0 {
		t.Fatalf("state = %+v, want the transition consumed without a notice", state)
	}
}

func TestApprovalWebhooksOptInPerEvent(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskWebhookEventApprovalResolved))
	h.request(approvalWebhookTestUID, "approval-1")
	h.reconcile()
	h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)
	h.reconcile()
	if got := eventTypes(h.receipts()); !slices.Equal(got, []string{"task.approval_resolved"}) {
		t.Fatalf("events = %v, want only the selected event", got)
	}
}

func TestApprovalWebhooksRetireNoticesThatAreNoLongerSelected(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...), http.StatusServiceUnavailable)
	h.request(approvalWebhookTestUID, "approval-1")
	h.reconcile()
	if got := len(h.receiver.all()); got != 1 {
		t.Fatalf("receipts = %d, want one failed attempt", got)
	}
	task := h.task()
	task.Spec.WebhookEvents = nil
	if err := h.client.Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Hour)
	if requeue := h.reconcile(); requeue != 0 {
		t.Fatalf("requeue = %v after opting out", requeue)
	}
	notice := h.task().Status.ApprovalWebhooks.Notices[0]
	if notice.State != corev1alpha1.TaskWebhookDeliveryFailed || len(h.receiver.all()) != 1 {
		t.Fatalf("notice = %s with %d receipts, want retired unsent", notice.State, len(h.receiver.all()))
	}
}

// An opt-out observed while the head notice waits out its retry backoff
// retires every deselected notice at once, so selecting the event again
// before the retry is due never sends a notice from before the opt-out.
func TestApprovalWebhooksOptOutDuringBackoffRetiresNotices(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...), http.StatusServiceUnavailable)
	h.request(approvalWebhookTestUID, "approval-1")
	h.request(approvalWebhookTestUID, "approval-2")
	h.reconcile()
	if got := len(h.receiver.all()); got != 1 {
		t.Fatalf("receipts = %d, want one failed attempt", got)
	}
	head := h.task().Status.ApprovalWebhooks.Notices[0]
	if head.NextAttemptTime == nil || !h.now.Before(head.NextAttemptTime.Time) {
		t.Fatalf("head notice = %+v, want it waiting out a retry backoff", head)
	}

	// Still before NextAttemptTime: deselect requests, then select them again.
	h.setWebhookEvents(corev1alpha1.TaskWebhookEventApprovalResolved)
	h.reconcile()
	for _, notice := range h.task().Status.ApprovalWebhooks.Notices {
		if notice.State != corev1alpha1.TaskWebhookDeliveryFailed {
			t.Fatalf("notice %s = %s, want retired while the head was in backoff", notice.DeliveryID, notice.State)
		}
	}
	h.setWebhookEvents(bothApprovalEvents...)
	h.now = h.now.Add(time.Hour)
	h.reconcile()
	if got := len(h.receiver.all()); got != 1 {
		t.Fatalf("receipts = %d after re-selecting, want no notice from before the opt-out", got)
	}
}

func TestApprovalWebhooksRetryBoundIsShorterThanTerminal(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...), http.StatusBadGateway)
	h.request(approvalWebhookTestUID, "approval-1")
	for range 20 {
		if requeue := h.reconcile(); requeue > defaultApprovalWebhookMaxBackoff {
			t.Fatalf("requeue %v exceeds the approval backoff cap", requeue)
		}
		h.now = h.now.Add(defaultApprovalWebhookMaxBackoff)
	}
	notice := h.task().Status.ApprovalWebhooks.Notices[0]
	if notice.State != corev1alpha1.TaskWebhookDeliveryFailed || notice.Attempts != defaultApprovalWebhookMaxAttempts ||
		len(h.receiver.all()) != defaultApprovalWebhookMaxAttempts || defaultApprovalWebhookMaxAttempts >= defaultWebhookMaxAttempts {
		t.Fatalf("notice = %s after %d attempts (%d receipts)", notice.State, notice.Attempts, len(h.receiver.all()))
	}
}

func TestApprovalWebhooksOverflowIsVisibleAndKeepsPendingNotices(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskWebhookEventApprovalRequested), http.StatusServiceUnavailable)
	h.maxAttempts = 100
	limit := corev1alpha1.MaxTaskApprovalWebhookNotices - 1
	seqs := make([]int64, 0, limit+3)
	for i := range limit + 3 {
		seqs = append(seqs, h.request(approvalWebhookTestUID, fmt.Sprintf("approval-%02d", i)).Seq)
	}
	h.reconcile()

	state := h.task().Status.ApprovalWebhooks
	if len(state.Notices) != corev1alpha1.MaxTaskApprovalWebhookNotices || !state.Overflowed || state.Omitted != 3 || state.ProjectedSeq != seqs[len(seqs)-1] {
		t.Fatalf("state = %d notices, overflowed=%v omitted=%d cursor=%d", len(state.Notices), state.Overflowed, state.Omitted, state.ProjectedSeq)
	}
	overflow := state.Notices[len(state.Notices)-1]
	if overflow.Transition != corev1alpha1.TaskApprovalWebhookOverflow || overflow.StreamSeq != seqs[limit] ||
		overflow.EventType != "task.approval_overflow" || overflow.ApprovalID != "" {
		t.Fatalf("overflow notice = %+v", overflow)
	}
	for _, notice := range state.Notices {
		if notice.State != corev1alpha1.TaskWebhookDeliveryPending {
			t.Fatalf("notice %s = %s, want pending", notice.DeliveryID, notice.State)
		}
	}

	// Later transitions are counted, never notified or regenerated.
	h.request(approvalWebhookTestUID, "approval-late")
	h.reconcile()
	if state := h.task().Status.ApprovalWebhooks; state.Omitted != 4 || len(state.Notices) != corev1alpha1.MaxTaskApprovalWebhookNotices {
		t.Fatalf("state after overflow = %+v", state)
	}

	// The receiver recovers: every recorded notice is delivered in order.
	h.receiver.mu.Lock()
	h.receiver.statuses = []int{http.StatusOK}
	h.receiver.receipts = nil
	h.receiver.mu.Unlock()
	for range 2 * corev1alpha1.MaxTaskApprovalWebhookNotices {
		h.now = h.now.Add(time.Hour)
		h.reconcile()
	}
	receipts := h.receipts()
	if len(receipts) != corev1alpha1.MaxTaskApprovalWebhookNotices {
		t.Fatalf("receipts = %d, want every recorded notice", len(receipts))
	}
	for i, receipt := range receipts[:limit] {
		if receipt.payload.StreamSeq != seqs[i] {
			t.Fatalf("receipt %d has streamSeq %d, want %d", i, receipt.payload.StreamSeq, seqs[i])
		}
	}
	last := receipts[limit].payload
	if last.EventType != "task.approval_overflow" || last.StreamSeq != seqs[limit] || last.ApprovalID != "" {
		t.Fatalf("overflow payload = %+v", last)
	}
}

func TestApprovalWebhooksPruneOnlySettledNotices(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...))
	total := 2 * corev1alpha1.MaxTaskApprovalWebhookNotices
	for i := range total {
		h.request(approvalWebhookTestUID, fmt.Sprintf("approval-%02d", i))
		h.reconcile()
	}
	state := h.task().Status.ApprovalWebhooks
	if state.Overflowed || len(state.Notices) >= corev1alpha1.MaxTaskApprovalWebhookNotices ||
		int(state.Pruned)+len(state.Notices) != total || len(h.receiver.all()) != total {
		t.Fatalf("state = %d notices, pruned=%d overflowed=%v, %d receipts", len(state.Notices), state.Pruned, state.Overflowed, len(h.receiver.all()))
	}
	// A pruned transition is never regenerated.
	h.reconcile()
	if got := len(h.receiver.all()); got != total {
		t.Fatalf("receipts = %d after another pass, want %d", got, total)
	}
}

func TestApprovalWebhooksKeepTerminalDeliveryUnchanged(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseSucceeded, bothApprovalEvents...))
	h.request(approvalWebhookTestUID, "approval-1")
	h.reconcile()
	if _, err := h.notifier().reconcileDelivery(context.Background(), h.client.Status(), h.task()); err != nil {
		t.Fatal(err)
	}
	all := h.receiver.all()
	if len(all) != 2 || all[1].header.Get(taskwebhook.HeaderEvent) != "task.succeeded" {
		t.Fatalf("receipts = %d, want the approval notice then the terminal notice", len(all))
	}
	var terminal map[string]any
	if err := json.Unmarshal(all[1].body, &terminal); err != nil {
		t.Fatal(err)
	}
	if _, ok := terminal["schemaVersion"]; ok || terminal["phase"] != "Succeeded" {
		t.Fatalf("terminal payload changed: %v", terminal)
	}
	stored := h.task()
	if !stored.Status.WebhookDelivered || stored.Status.WebhookDelivery == nil ||
		stored.Status.WebhookDelivery.Attempts != 1 || stored.Status.ApprovalWebhooks.Notices[0].Attempts != 1 {
		t.Fatalf("terminal %+v and approval %+v delivery records interfere", stored.Status.WebhookDelivery, stored.Status.ApprovalWebhooks)
	}
}

func TestApprovalWebhooksParkedSignOffNotifiesAfterTheTurnSucceeded(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded)
	task.Spec.WebhookEvents = bothApprovalEvents
	task.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true}
	h := newParkedWebhookHarness(t, task)
	approvalNotices := func() {
		t.Helper()
		notifier := newTestWebhookNotifier(t, h.keys, nil)
		if _, err := notifier.reconcileApprovalNotices(context.Background(), h.client.Status(), h.events, h.task("parked")); err != nil {
			t.Fatal(err)
		}
	}

	h.settle("parked")
	parked := h.task("parked")
	if parked.Status.Wait.State != corev1alpha1.TaskWaitStateWaiting {
		t.Fatalf("wait state = %s, want Waiting", parked.Status.Wait.State)
	}
	approvalNotices()
	// The terminal notice stays deferred while the sign-off is pending.
	h.deliver("parked")
	if stored := h.task("parked"); stored.Status.WebhookDelivered || stored.Status.WebhookDelivery != nil {
		t.Fatalf("parked Task recorded a terminal notice: %+v", stored.Status.WebhookDelivery)
	}

	h.decide(parked, "approval-1", events.ExecutionEventTypeApprovalApproved, "alice", "ship it")
	h.settle("parked")
	if state := h.task("parked").Status.Wait.State; state != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("wait state = %s, want Resumed", state)
	}
	approvalNotices()
	approvalNotices()
	h.deliver("parked")

	receipts := h.receiver.all()
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want requested and resolved only", len(receipts))
	}
	var requested, resolved ApprovalWebhookPayload
	_ = json.Unmarshal(receipts[0].body, &requested)
	_ = json.Unmarshal(receipts[1].body, &resolved)
	deadline := parked.Status.Wait.Deadline.UTC().Format(time.RFC3339)
	if requested.EventType != "task.approval_requested" || !requested.Parked || requested.ExpiresAt != deadline ||
		resolved.EventType != "task.approval_resolved" || !resolved.Parked || resolved.Resolution != approvals.StatusApproved {
		t.Fatalf("payloads = %+v, %+v", requested, resolved)
	}
	if strings.Contains(string(receipts[0].body), "need sign-off") || strings.Contains(string(receipts[1].body), "ship it") ||
		strings.Contains(string(receipts[1].body), "alice") {
		t.Fatal("parked approval notices leak the wait reason or the reviewer")
	}
	for _, receipt := range receipts {
		verifyReceipt(t, h.keys, task.Spec.WebhookURL, receipt, time.Now())
	}

	continuation := h.task(h.task("parked").Status.Wait.ContinuationTaskName)
	if !slices.Equal(continuation.Spec.WebhookEvents, bothApprovalEvents) {
		t.Fatalf("continuation webhookEvents = %v, want the parked Task's", continuation.Spec.WebhookEvents)
	}
	// The resumed parked Task still sends no terminal notice of its own.
	if stored := h.task("parked"); stored.Status.WebhookDelivered || stored.Status.WebhookDelivery != nil {
		t.Fatalf("resumed parked Task recorded a terminal notice: %+v", stored.Status.WebhookDelivery)
	}
}

// The Task reconciler drives approval notices in every phase, here for a
// finished Task, alongside its terminal notice.
func TestTaskReconcilerDeliversApprovalNoticesForAFinishedTask(t *testing.T) {
	task := newApprovalWebhookTask(corev1alpha1.TaskPhaseSucceeded, bothApprovalEvents...)
	task.Spec.Type = corev1alpha1.TaskTypeContainer
	task.Finalizers = []string{labels.TaskFinalizer}
	h := newApprovalWebhookHarness(t, task)
	h.request(approvalWebhookTestUID, "approval-1")
	h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)
	notifier := newTestWebhookNotifier(t, h.keys, nil)
	reconciler := &TaskReconciler{Client: h.client, Scheme: h.client.Scheme(), ExecutionEventStore: h.events, WebhookNotifier: notifier}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: defaultNS, Name: approvalWebhookTestTask}}
	for range 4 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	all := h.receiver.all()
	var approvalEvents []string
	terminal := 0
	for _, receipt := range all {
		switch event := receipt.header.Get(taskwebhook.HeaderEvent); event {
		case "task.succeeded":
			terminal++
		default:
			approvalEvents = append(approvalEvents, event)
		}
	}
	// Approval and terminal notices are independent; approval notices keep
	// their own order.
	if terminal != 1 || !slices.Equal(approvalEvents, []string{"task.approval_requested", "task.approval_resolved"}) {
		t.Fatalf("terminal notices = %d, approval events = %v", terminal, approvalEvents)
	}
}

// approvalExpiryCleanupStore runs cleanup once, just before the controller's
// first ApprovalExpired append reaches the store: after the approval history
// was listed and, under the fence, after the fenced read of the request.
type approvalExpiryCleanupStore struct {
	*storageclient.Client
	cleanup func(context.Context) error
	once    sync.Once
	appends atomic.Int32
}

func (s *approvalExpiryCleanupStore) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	if event.Type == events.ExecutionEventTypeApprovalExpired {
		s.appends.Add(1)
		var err error
		s.once.Do(func() { err = s.cleanup(context.Background()) })
		if err != nil {
			return nil, err
		}
	}
	return s.Client.AppendExecutionEvent(ctx, event)
}

// Timeline or Session cleanup that commits after the controller read an
// expired brokered review must refuse the controller's expiry append, so it
// neither recreates the cleaned request's history nor restores the cleaned
// Session association.
func TestApprovalWebhooksExpiryCleanupAfterListRecreatesNothing(t *testing.T) {
	const session = "approval-session"
	for _, cleanup := range []string{"timeline", "session"} {
		t.Run(cleanup, func(t *testing.T) {
			h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...))
			expiresAt := approvalWebhookTestNow.Add(10 * time.Minute)
			content, _ := json.Marshal(map[string]any{
				"approvalID": "approval-bound", "taskUID": string(approvalWebhookTestUID), "targetTool": "github.create_pr",
				"expiresAt": expiresAt, "binding": approvals.CallBinding{TaskAttempt: 1, PromptID: "prompt-1", OperationIDDigest: "sha256:op", RequestDigest: "sha256:req"},
			})
			h.append(&store.ExecutionEvent{
				Type: events.ExecutionEventTypeApprovalRequested, ToolName: "call_tool", ToolCallID: "approval-bound",
				SessionName: session, Content: content,
			})
			h.reconcile()

			wrapped := &approvalExpiryCleanupStore{Client: h.storage, cleanup: func(ctx context.Context) error {
				if cleanup == "timeline" {
					return h.db.DeleteExecutionEvents(ctx, defaultNS, events.ExecutionEventStreamTypeTask, approvalWebhookTestTask)
				}
				return h.db.DeleteSession(ctx, defaultNS, session, time.Now().UTC())
			}}
			h.events = wrapped
			h.now = expiresAt.Add(approvalExpiryGrace)
			for range 3 {
				h.reconcile()
			}
			if wrapped.appends.Load() == 0 {
				t.Fatal("the controller never reached its expiry append")
			}

			listed, err := h.storage.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
				Namespace: defaultNS, StreamType: events.ExecutionEventStreamTypeTask, StreamID: approvalWebhookTestTask,
			})
			if err != nil {
				t.Fatal(err)
			}
			if cleanup == "timeline" {
				if len(listed) != 0 {
					t.Fatalf("cleaned history has %d events; the expiry recreated an orphan %s", len(listed), listed[0].Type)
				}
				if got := eventTypes(h.receipts()); !slices.Equal(got, []string{"task.approval_requested"}) {
					t.Fatalf("events = %v, want no resolution for a cleaned request", got)
				}
				return
			}
			expiries := 0
			for _, event := range listed {
				if event.SessionName != "" {
					t.Fatalf("%s restored the cleaned Session association %q", event.Type, event.SessionName)
				}
				if event.Type == events.ExecutionEventTypeApprovalExpired {
					expiries++
				}
			}
			if expiries != 1 {
				t.Fatalf("expiry events = %d, want one recorded from a fresh read", expiries)
			}
			sessionEvents, _, err := h.storage.ListSessionExecutionEvents(context.Background(), store.SessionExecutionEventFilter{
				Namespace: defaultNS, SessionName: session,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(sessionEvents) != 0 {
				t.Fatalf("the cleaned Session's stream has %d events", len(sessionEvents))
			}
			if got := eventTypes(h.receipts()); !slices.Equal(got, []string{"task.approval_requested", "task.approval_resolved"}) {
				t.Fatalf("events = %v", got)
			}
		})
	}
}

// setWebhookEvents changes the stored Task's selection.
func (h *approvalWebhookHarness) setWebhookEvents(selected ...corev1alpha1.TaskWebhookEvent) {
	h.t.Helper()
	task := h.task()
	task.Spec.WebhookEvents = selected
	if err := h.client.Update(context.Background(), task); err != nil {
		h.t.Fatal(err)
	}
}

// Selecting approval events on a Task whose spec is still mutable, or
// selecting them again after opting out, never notifies earlier history and
// cannot fill the queue with it; later transitions are notified exactly once.
func TestApprovalWebhooksLateSelectionDoesNotReplayHistory(t *testing.T) {
	task := newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning)
	task.Spec.Type = corev1alpha1.TaskTypeAI
	h := newApprovalWebhookHarness(t, task)
	// More history than the queue holds, so a replay would overflow it.
	for i := range corev1alpha1.MaxTaskApprovalWebhookNotices + 4 {
		id := fmt.Sprintf("approval-old-%02d", i)
		h.request(approvalWebhookTestUID, id)
		h.decide(approvalWebhookTestUID, id, events.ExecutionEventTypeApprovalApproved)
	}
	h.request(approvalWebhookTestUID, "approval-open")
	h.reconcile()

	h.setWebhookEvents(bothApprovalEvents...)
	h.reconcile()
	h.reconcile()
	state := h.task().Status.ApprovalWebhooks
	if got := len(h.receiver.all()); got != 0 || state == nil || !state.Tracking || state.Overflowed || len(state.Notices) != 0 {
		t.Fatalf("late selection replayed history: %d receipts, state %+v", got, state)
	}

	// A transition after the selection is notified once, even for an
	// approval requested before it.
	h.decide(approvalWebhookTestUID, "approval-open", events.ExecutionEventTypeApprovalDeclined)
	h.request(approvalWebhookTestUID, "approval-new")
	h.reconcile()
	h.reconcile()
	h.reconcile()
	receipts := h.receipts()
	if got := eventTypes(receipts); !slices.Equal(got, []string{"task.approval_resolved", "task.approval_requested"}) ||
		receipts[0].payload.ApprovalID != "approval-open" || receipts[1].payload.ApprovalID != "approval-new" {
		t.Fatalf("events = %v, want only the transitions after the selection", got)
	}

	// While opted out, transitions are not tracked, and selecting again does
	// not replay them.
	h.setWebhookEvents()
	h.reconcile()
	if state := h.task().Status.ApprovalWebhooks; state.Tracking {
		t.Fatal("opting out kept tracking")
	}
	h.decide(approvalWebhookTestUID, "approval-new", events.ExecutionEventTypeApprovalApproved)
	h.request(approvalWebhookTestUID, "approval-unselected")
	h.reconcile()
	h.setWebhookEvents(bothApprovalEvents...)
	h.reconcile()
	h.reconcile()
	if got := len(h.receiver.all()); got != 2 {
		t.Fatalf("receipts = %d after selecting again, want no replay of the opted-out period", got)
	}
	h.request(approvalWebhookTestUID, "approval-after")
	h.reconcile()
	h.reconcile()
	receipts = h.receipts()
	if len(receipts) != 3 || receipts[2].payload.ApprovalID != "approval-after" || receipts[2].payload.EventType != "task.approval_requested" {
		t.Fatalf("receipts = %v, want only the new request after selecting again", eventTypes(receipts))
	}
	if state := h.task().Status.ApprovalWebhooks; state.Overflowed || state.Omitted != 0 {
		t.Fatalf("state = %+v, want no overflow from history", state)
	}
}

// Status initialization records that a Task selected approval events from
// the start, so an approval it requests before its first projection is still
// notified. A Task that selects none gets no approval state.
func TestTaskReconcilerInitializesApprovalTrackingWithStatus(t *testing.T) {
	for _, selected := range [][]corev1alpha1.TaskWebhookEvent{nil, bothApprovalEvents} {
		t.Run(fmt.Sprintf("selected=%d", len(selected)), func(t *testing.T) {
			task := newApprovalWebhookTask("", selected...)
			task.Status = corev1alpha1.TaskStatus{}
			task.Finalizers = []string{labels.TaskFinalizer}
			h := newApprovalWebhookHarness(t, task)
			reconciler := &TaskReconciler{Client: h.client, Scheme: h.client.Scheme(), ExecutionEventStore: h.events}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: defaultNS, Name: approvalWebhookTestTask}}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			stored := h.task()
			if stored.Status.Phase != corev1alpha1.TaskPhasePending {
				t.Fatalf("phase = %q, want status initialized", stored.Status.Phase)
			}
			if len(selected) == 0 {
				if stored.Status.ApprovalWebhooks != nil {
					t.Fatalf("legacy Task recorded approval state %+v", stored.Status.ApprovalWebhooks)
				}
				return
			}
			if state := stored.Status.ApprovalWebhooks; state == nil || !state.Tracking || state.ProjectedSeq != 0 {
				t.Fatalf("state = %+v, want tracking from the start", state)
			}
			h.request(approvalWebhookTestUID, "approval-1")
			h.reconcile()
			if got := eventTypes(h.receipts()); !slices.Equal(got, []string{"task.approval_requested"}) {
				t.Fatalf("events = %v, want the first request notified", got)
			}
		})
	}
}
