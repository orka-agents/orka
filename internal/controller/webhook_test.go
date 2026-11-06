/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/taskwebhook"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type webhookReceipt struct {
	header http.Header
	body   []byte
}

// webhookReceiver records every request and answers with the next scripted
// status (the last one repeats).
type webhookReceiver struct {
	server   *httptest.Server
	mu       sync.Mutex
	receipts []webhookReceipt
	statuses []int
}

func newWebhookReceiver(t *testing.T, statuses ...int) *webhookReceiver {
	t.Helper()
	receiver := &webhookReceiver{statuses: statuses}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receiver.mu.Lock()
		receiver.receipts = append(receiver.receipts, webhookReceipt{header: r.Header.Clone(), body: body})
		status := http.StatusOK
		if len(receiver.statuses) > 0 {
			index := min(len(receiver.receipts)-1, len(receiver.statuses)-1)
			status = receiver.statuses[index]
		}
		receiver.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

func (r *webhookReceiver) all() []webhookReceipt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]webhookReceipt(nil), r.receipts...)
}

type webhookTestClock struct{ now time.Time }

func (c *webhookTestClock) Now() time.Time { return c.now }

func newWebhookTestKeys(t *testing.T) taskwebhook.KeySet {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := taskwebhook.NewSigningKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return taskwebhook.KeySet{Current: key}
}

// newTestWebhookNotifier admits only 127.0.0.1 so tests can reach httptest
// servers while every other non-public address stays forbidden.
func newTestWebhookNotifier(t *testing.T, keys taskwebhook.KeySet, clock *webhookTestClock) *WebhookNotifier {
	t.Helper()
	loopback := netip.MustParseAddr("127.0.0.1")
	notifier := newWebhookNotifier(&taskwebhook.Dialer{Policy: func(address netip.Addr) bool { return address == loopback }})
	if keys.Current != nil {
		notifier.SetKeySource(taskwebhook.StaticKeySource{Keys: keys})
	}
	if clock != nil {
		notifier.now = clock.Now
	}
	return notifier
}

func newWebhookTask(webhookURL string) *corev1alpha1.Task {
	start := metav1.NewTime(time.Unix(1_800_000_000, 0))
	completion := metav1.NewTime(time.Unix(1_800_000_060, 0))
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "test-task", Namespace: defaultNS, UID: types.UID("5f1d1c2e-1111-4a4a-8b8b-0123456789ab")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, WebhookURL: webhookURL},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseSucceeded,
			Message:        "Task completed successfully",
			Attempts:       1,
			StartTime:      &start,
			CompletionTime: &completion,
			ResultRef:      &corev1alpha1.ResultReference{Available: true},
		},
	}
}

func newWebhookTestClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(objects...).
		Build()
}

func reloadWebhookTask(t *testing.T, client ctrlclient.Client, task *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	current := &corev1alpha1.Task{}
	if err := client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	return current
}

func verifyReceipt(t *testing.T, keys taskwebhook.KeySet, audience string, receipt webhookReceipt, now time.Time) *taskwebhook.Claims {
	t.Helper()
	claims, err := taskwebhook.Verify(receipt.header.Get(taskwebhook.HeaderSignature), keys.JWKS(),
		taskwebhook.VerifyOptions{Audience: audience, Body: receipt.body, Now: now})
	if err != nil {
		t.Fatalf("receiver signature verification failed: %v", err)
	}
	return claims
}

func TestWebhookDeliverySignsAndRecordsSuccess(t *testing.T) {
	t.Parallel()
	receiver := newWebhookReceiver(t, http.StatusAccepted)
	task := newWebhookTask(receiver.server.URL + "/hook?token=abc")
	client := newWebhookTestClient(t, task)
	keys := newWebhookTestKeys(t)
	clock := &webhookTestClock{now: time.Unix(1_800_000_100, 0)}
	notifier := newTestWebhookNotifier(t, keys, clock)

	requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), task)
	if err != nil || requeue != 0 {
		t.Fatalf("reconcileDelivery() = %v, %v; want 0, nil", requeue, err)
	}
	receipts := receiver.all()
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}
	receipt := receipts[0]
	claims := verifyReceipt(t, keys, task.Spec.WebhookURL, receipt, clock.now)

	stored := reloadWebhookTask(t, client, task)
	assertDeliveredRecord(t, stored, http.StatusAccepted)
	assertSignedDelivery(t, task, stored.Status.WebhookDelivery, receipt, claims)

	// A delivered notice is settled: nothing more is sent.
	if requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), stored); err != nil || requeue != 0 {
		t.Fatalf("settled reconcileDelivery() = %v, %v", requeue, err)
	}
	if got := len(receiver.all()); got != 1 {
		t.Fatalf("receipts after settle = %d, want 1", got)
	}
}

func assertDeliveredRecord(t *testing.T, stored *corev1alpha1.Task, statusCode int32) {
	t.Helper()
	delivery := stored.Status.WebhookDelivery
	if !stored.Status.WebhookDelivered || delivery == nil || delivery.State != corev1alpha1.TaskWebhookDeliveryDelivered ||
		delivery.Attempts != 1 || delivery.LastStatusCode != statusCode || delivery.LastError != "" || delivery.NextAttemptTime != nil {
		t.Fatalf("stored delivery = %#v (delivered=%v)", delivery, stored.Status.WebhookDelivered)
	}
}

func assertSignedDelivery(t *testing.T, task *corev1alpha1.Task, delivery *corev1alpha1.TaskWebhookDelivery, receipt webhookReceipt, claims *taskwebhook.Claims) {
	t.Helper()
	var payload WebhookPayload
	if err := json.Unmarshal(receipt.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DeliveryID != delivery.DeliveryID || claims.DeliveryID != delivery.DeliveryID ||
		receipt.header.Get(taskwebhook.HeaderDeliveryID) != delivery.DeliveryID ||
		receipt.header.Get(taskwebhook.HeaderIdempotencyKey) != delivery.DeliveryID {
		t.Fatalf("delivery ID mismatch: payload=%q claims=%q headers=%v stored=%q", payload.DeliveryID, claims.DeliveryID, receipt.header, delivery.DeliveryID)
	}
	if claims.Task != (taskwebhook.TaskClaim{UID: string(task.UID), Namespace: task.Namespace, Name: task.Name}) ||
		claims.EventType != "task.succeeded" || claims.Attempt != 1 || claims.PayloadSHA256 != delivery.PayloadSHA256 {
		t.Fatalf("claims = %#v", claims)
	}
	if payload.TaskName != task.Name || payload.TaskNamespace != defaultNS || payload.TaskUID != string(task.UID) ||
		payload.Phase != corev1alpha1.TaskPhaseSucceeded || payload.EventType != "task.succeeded" ||
		payload.Message != "Task completed successfully" || payload.Attempts != 1 || payload.RuntimeType != "agent" ||
		payload.StartTime == nil || payload.CompletionTime == nil || payload.ResultRef == nil || !payload.ResultRef.Available {
		t.Fatalf("payload = %#v", payload)
	}
	for header, want := range map[string]string{
		"Content-Type":            "application/json",
		"User-Agent":              "Orka-Controller/1.0",
		"X-Orka-Task":             task.Name,
		"X-Orka-Namespace":        defaultNS,
		taskwebhook.HeaderEvent:   "task.succeeded",
		taskwebhook.HeaderAttempt: "1",
	} {
		if got := receipt.header.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
}

func TestWebhookDeliveryRetriesWithBackoffThenFails(t *testing.T) {
	t.Parallel()
	receiver := newWebhookReceiver(t, http.StatusServiceUnavailable)
	task := newWebhookTask(receiver.server.URL)
	client := newWebhookTestClient(t, task)
	keys := newWebhookTestKeys(t)
	clock := &webhookTestClock{now: time.Unix(1_800_000_100, 0)}
	notifier := newTestWebhookNotifier(t, keys, clock)
	notifier.maxAttempts = 3

	wantBackoff := []time.Duration{10 * time.Second, 20 * time.Second, 0}
	for attempt := 1; attempt <= 3; attempt++ {
		current := reloadWebhookTask(t, client, task)
		requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), current)
		if err != nil {
			t.Fatal(err)
		}
		if requeue != wantBackoff[attempt-1] {
			t.Fatalf("attempt %d requeue = %v, want %v", attempt, requeue, wantBackoff[attempt-1])
		}
		if attempt < 3 {
			// Calling early sends nothing and reports the remaining backoff.
			early := reloadWebhookTask(t, client, task)
			clock.now = clock.now.Add(time.Second)
			remaining, err := notifier.reconcileDelivery(context.Background(), client.Status(), early)
			if err != nil || remaining != requeue-time.Second {
				t.Fatalf("early reconcile = %v, %v; want %v", remaining, err, requeue-time.Second)
			}
			clock.now = clock.now.Add(requeue)
		}
	}

	receipts := receiver.all()
	if len(receipts) != 3 {
		t.Fatalf("receipts = %d, want 3", len(receipts))
	}
	stored := reloadWebhookTask(t, client, task)
	delivery := stored.Status.WebhookDelivery
	if stored.Status.WebhookDelivered || delivery.State != corev1alpha1.TaskWebhookDeliveryFailed || delivery.Attempts != 3 ||
		delivery.LastStatusCode != http.StatusServiceUnavailable || !strings.Contains(delivery.LastError, "503") {
		t.Fatalf("stored delivery = %#v", delivery)
	}
	for index, receipt := range receipts {
		if receipt.header.Get(taskwebhook.HeaderDeliveryID) != delivery.DeliveryID {
			t.Fatalf("attempt %d delivery ID = %q, want stable %q", index+1, receipt.header.Get(taskwebhook.HeaderDeliveryID), delivery.DeliveryID)
		}
		if got := receipt.header.Get(taskwebhook.HeaderAttempt); got != strconv.Itoa(index+1) {
			t.Fatalf("attempt header = %q, want %d", got, index+1)
		}
		if string(receipt.body) != string(receipts[0].body) {
			t.Fatal("retry body differs from the first attempt")
		}
	}

	clock.now = clock.now.Add(time.Hour)
	if requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), stored); err != nil || requeue != 0 {
		t.Fatalf("exhausted reconcile = %v, %v", requeue, err)
	}
	if got := len(receiver.all()); got != 3 {
		t.Fatalf("receipts after exhaustion = %d, want 3", got)
	}
}

// failingResultWrites fails every status write after the first, simulating a
// controller that dies after claiming an attempt and sending it but before
// recording the result.
type failingResultWrites struct {
	ctrlclient.SubResourceWriter
	writes int
}

func (w *failingResultWrites) Update(ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
	w.writes++
	if w.writes > 1 {
		return errors.New("controller crashed")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestWebhookDeliverySurvivesControllerRestart(t *testing.T) {
	t.Parallel()
	receiver := newWebhookReceiver(t, http.StatusOK)
	task := newWebhookTask(receiver.server.URL)
	client := newWebhookTestClient(t, task)
	keys := newWebhookTestKeys(t)
	clock := &webhookTestClock{now: time.Unix(1_800_000_100, 0)}

	first := newTestWebhookNotifier(t, keys, clock)
	if _, err := first.reconcileDelivery(context.Background(), &failingResultWrites{SubResourceWriter: client.Status()}, task); err == nil {
		t.Fatal("expected the simulated crash to surface")
	}
	claimed := reloadWebhookTask(t, client, task).Status.WebhookDelivery
	if claimed == nil || claimed.Attempts != 1 || claimed.State != corev1alpha1.TaskWebhookDeliveryPending || claimed.NextAttemptTime == nil {
		t.Fatalf("claimed delivery = %#v, want a durable lease for attempt 1", claimed)
	}

	// A fresh controller honors the lease of the interrupted attempt.
	restarted := newTestWebhookNotifier(t, keys, clock)
	clock.now = clock.now.Add(30 * time.Second)
	requeue, err := restarted.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task))
	if err != nil || requeue != defaultWebhookAttemptLease-30*time.Second {
		t.Fatalf("reconcile during lease = %v, %v", requeue, err)
	}
	if got := len(receiver.all()); got != 1 {
		t.Fatalf("receipts during lease = %d, want 1", got)
	}

	clock.now = claimed.NextAttemptTime.Add(time.Second)
	if requeue, err := restarted.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task)); err != nil || requeue != 0 {
		t.Fatalf("reconcile after lease = %v, %v", requeue, err)
	}
	receipts := receiver.all()
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipts))
	}
	if receipts[0].header.Get(taskwebhook.HeaderDeliveryID) != receipts[1].header.Get(taskwebhook.HeaderDeliveryID) ||
		receipts[1].header.Get(taskwebhook.HeaderDeliveryID) != claimed.DeliveryID {
		t.Fatal("redelivery after restart must reuse the delivery ID for receiver deduplication")
	}
	if receipts[1].header.Get(taskwebhook.HeaderAttempt) != "2" {
		t.Fatalf("attempt header = %q, want 2", receipts[1].header.Get(taskwebhook.HeaderAttempt))
	}
	stored := reloadWebhookTask(t, client, task)
	if !stored.Status.WebhookDelivered || stored.Status.WebhookDelivery.State != corev1alpha1.TaskWebhookDeliveryDelivered || stored.Status.WebhookDelivery.Attempts != 2 {
		t.Fatalf("stored delivery = %#v", stored.Status.WebhookDelivery)
	}
}

func TestWebhookDeliveryNeverClaimsPastTheBoundAfterRestart(t *testing.T) {
	t.Parallel()
	receiver := newWebhookReceiver(t, http.StatusServiceUnavailable)
	task := newWebhookTask(receiver.server.URL)
	client := newWebhookTestClient(t, task)
	keys := newWebhookTestKeys(t)
	clock := &webhookTestClock{now: time.Unix(1_800_000_100, 0)}
	notifier := newTestWebhookNotifier(t, keys, clock)
	notifier.maxAttempts = 2

	if _, err := notifier.reconcileDelivery(context.Background(), client.Status(), task); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Hour)
	// The final attempt is claimed and sent, but its result is never recorded.
	if _, err := notifier.reconcileDelivery(context.Background(), &failingResultWrites{SubResourceWriter: client.Status()}, reloadWebhookTask(t, client, task)); err == nil {
		t.Fatal("expected the simulated crash to surface")
	}
	if got := reloadWebhookTask(t, client, task).Status.WebhookDelivery; got.Attempts != 2 || got.State != corev1alpha1.TaskWebhookDeliveryPending {
		t.Fatalf("interrupted delivery = %#v", got)
	}

	clock.now = clock.now.Add(time.Hour)
	restarted := newTestWebhookNotifier(t, keys, clock)
	restarted.maxAttempts = 2
	if requeue, err := restarted.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task)); err != nil || requeue != 0 {
		t.Fatalf("reconcile after exhausted lease = %v, %v", requeue, err)
	}
	if got := len(receiver.all()); got != 2 {
		t.Fatalf("receipts = %d, want exactly the 2 allowed attempts", got)
	}
	if got := reloadWebhookTask(t, client, task).Status.WebhookDelivery; got.State != corev1alpha1.TaskWebhookDeliveryFailed || got.Attempts != 2 {
		t.Fatalf("exhausted delivery = %#v, want Failed after 2 attempts", got)
	}
}

func TestWebhookDeliveryClaimConflictSendsNothing(t *testing.T) {
	t.Parallel()
	receiver := newWebhookReceiver(t, http.StatusOK)
	task := newWebhookTask(receiver.server.URL)
	client := newWebhookTestClient(t, task)
	keys := newWebhookTestKeys(t)
	notifier := newTestWebhookNotifier(t, keys, &webhookTestClock{now: time.Unix(1_800_000_100, 0)})

	stale := reloadWebhookTask(t, client, task)
	winner := reloadWebhookTask(t, client, task)
	winner.Status.Message = "updated by another writer"
	if err := client.Status().Update(context.Background(), winner); err != nil {
		t.Fatal(err)
	}
	if _, err := notifier.reconcileDelivery(context.Background(), client.Status(), stale); err == nil {
		t.Fatal("expected a stale claim to conflict")
	}
	if got := len(receiver.all()); got != 0 {
		t.Fatalf("receipts after lost claim = %d, want 0", got)
	}
}

func TestWebhookDeliverySupersedesPendingNoticeWithNewerState(t *testing.T) {
	t.Parallel()
	receiver := newWebhookReceiver(t, http.StatusServiceUnavailable, http.StatusOK)
	task := newWebhookTask(receiver.server.URL)
	client := newWebhookTestClient(t, task)
	keys := newWebhookTestKeys(t)
	clock := &webhookTestClock{now: time.Unix(1_800_000_100, 0)}
	notifier := newTestWebhookNotifier(t, keys, clock)

	requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), task)
	if err != nil || requeue == 0 {
		t.Fatalf("first attempt = %v, %v", requeue, err)
	}
	original := reloadWebhookTask(t, client, task)
	originalID := original.Status.WebhookDelivery.DeliveryID

	original.Status.Message = "result published"
	if err := client.Status().Update(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	// Newer state never accelerates the pending backoff.
	if early, err := notifier.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task)); err != nil || early == 0 {
		t.Fatalf("reconcile during backoff = %v, %v", early, err)
	}
	clock.now = clock.now.Add(requeue)
	if requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task)); err != nil || requeue != 0 {
		t.Fatalf("superseding attempt = %v, %v", requeue, err)
	}

	receipts := receiver.all()
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2", len(receipts))
	}
	stored := reloadWebhookTask(t, client, task).Status.WebhookDelivery
	if stored.DeliveryID == originalID || stored.Superseded != 1 || stored.Attempts != 1 || stored.State != corev1alpha1.TaskWebhookDeliveryDelivered {
		t.Fatalf("superseding delivery = %#v, original ID %q", stored, originalID)
	}
	if receipts[1].header.Get(taskwebhook.HeaderDeliveryID) != stored.DeliveryID {
		t.Fatal("the superseded notice was redelivered instead of the newer one")
	}
	var payload WebhookPayload
	if err := json.Unmarshal(receipts[1].body, &payload); err != nil || payload.Message != "result published" {
		t.Fatalf("superseding payload = %#v (%v)", payload, err)
	}
	verifyReceipt(t, keys, task.Spec.WebhookURL, receipts[1], clock.now)
}

func TestWebhookDeliveryRejectsPolicyViolationsWithoutNetwork(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{
		"http://0.0.0.0/hook",
		"http://10.0.0.1/hook",
		"http://100.64.0.1/hook",
		"http://[fd00::1]/hook",
		"http://[fe80::1]/hook",
		"http://[::ffff:10.0.0.1]/hook",
		"http://receiver.other.svc/hook",
		"ftp://hooks.example.com/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			t.Parallel()
			task := newWebhookTask(rawURL)
			client := newWebhookTestClient(t, task)
			notifier := newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)
			notifier.policy = taskwebhook.PublicAddressPolicy
			if requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), task); err != nil || requeue != 0 {
				t.Fatalf("reconcileDelivery() = %v, %v", requeue, err)
			}
			delivery := reloadWebhookTask(t, client, task).Status.WebhookDelivery
			if delivery == nil || delivery.State != corev1alpha1.TaskWebhookDeliveryFailed || delivery.Attempts != 0 || delivery.LastError == "" {
				t.Fatalf("delivery = %#v, want Failed without attempts", delivery)
			}
		})
	}
}

func TestWebhookDeliveryNeverSendsUnsigned(t *testing.T) {
	t.Parallel()
	receiver := newWebhookReceiver(t, http.StatusOK)

	// Disabled signing fails the notice terminally rather than requeueing a
	// finished Task forever.
	disabledTask := newWebhookTask(receiver.server.URL)
	disabledClient := newWebhookTestClient(t, disabledTask)
	disabled := newTestWebhookNotifier(t, taskwebhook.KeySet{}, nil)
	requeue, err := disabled.reconcileDelivery(context.Background(), disabledClient.Status(), reloadWebhookTask(t, disabledClient, disabledTask))
	if err != nil || requeue != 0 {
		t.Fatalf("disabled: reconcileDelivery() = %v, %v", requeue, err)
	}
	if delivery := reloadWebhookTask(t, disabledClient, disabledTask).Status.WebhookDelivery; delivery == nil ||
		delivery.State != corev1alpha1.TaskWebhookDeliveryFailed || delivery.Attempts != 0 || delivery.LastError == "" {
		t.Fatalf("disabled: delivery = %#v, want Failed without attempts", delivery)
	}

	// A configured key source whose keys are temporarily unavailable retries.
	task := newWebhookTask(receiver.server.URL)
	client := newWebhookTestClient(t, task)
	unavailable := newTestWebhookNotifier(t, taskwebhook.KeySet{}, nil)
	unavailable.SetKeySource(taskwebhook.StaticKeySource{})
	requeue, err = unavailable.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task))
	if err != nil || requeue != webhookSigningRetryInterval {
		t.Fatalf("unavailable: reconcileDelivery() = %v, %v", requeue, err)
	}
	if delivery := reloadWebhookTask(t, client, task).Status.WebhookDelivery; delivery == nil ||
		delivery.State != corev1alpha1.TaskWebhookDeliveryPending || delivery.Attempts != 0 || delivery.LastError == "" {
		t.Fatalf("unavailable: delivery = %#v", delivery)
	}
	if got := len(receiver.all()); got != 0 {
		t.Fatalf("receipts = %d, want 0", got)
	}
}

func TestWebhookDeliveryRefusesRedirects(t *testing.T) {
	t.Parallel()
	target := newWebhookReceiver(t, http.StatusOK)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.server.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	task := newWebhookTask(redirector.URL)
	client := newWebhookTestClient(t, task)
	notifier := newTestWebhookNotifier(t, newWebhookTestKeys(t), &webhookTestClock{now: time.Unix(1_800_000_100, 0)})

	if requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), task); err != nil || requeue == 0 {
		t.Fatalf("reconcileDelivery() = %v, %v", requeue, err)
	}
	if got := len(target.all()); got != 0 {
		t.Fatalf("redirect target receipts = %d, want 0", got)
	}
	delivery := reloadWebhookTask(t, client, task).Status.WebhookDelivery
	if delivery.LastError != taskwebhook.ErrRedirectRefused.Error() || delivery.State != corev1alpha1.TaskWebhookDeliveryPending {
		t.Fatalf("delivery = %#v", delivery)
	}
}

type webhookTestResolver func(context.Context, string) ([]net.IPAddr, error)

func (f webhookTestResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func TestWebhookDeliveryFailsClosedOnDNSAndPrivateAnswers(t *testing.T) {
	t.Parallel()
	for name, resolver := range map[string]webhookTestResolver{
		"dns failure": func(context.Context, string) ([]net.IPAddr, error) {
			return nil, &net.DNSError{Err: "server misbehaving", Name: "hooks.example.com", IsTemporary: true}
		},
		"loopback answer": func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		"unspecified": func(context.Context, string) ([]net.IPAddr, error) { return []net.IPAddr{{IP: net.IPv4zero}}, nil },
		"cgnat answer": func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("100.100.0.1")}}, nil
		},
		"mapped ipv4": func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("::ffff:192.168.0.1")}}, nil
		},
		"ipv6 ula": func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("fd00:ec2::254")}}, nil
		},
		"ipv6 link-local": func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("fe80::1")}}, nil
		},
		"public and secret": func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("10.0.0.1")}}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			task := newWebhookTask("https://hooks.example.com/orka")
			client := newWebhookTestClient(t, task)
			notifier := newWebhookNotifier(&taskwebhook.Dialer{Resolver: resolver, Policy: taskwebhook.PublicAddressPolicy})
			notifier.SetKeySource(taskwebhook.StaticKeySource{Keys: newWebhookTestKeys(t)})
			if requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), task); err != nil || requeue == 0 {
				t.Fatalf("reconcileDelivery() = %v, %v", requeue, err)
			}
			stored := reloadWebhookTask(t, client, task)
			delivery := stored.Status.WebhookDelivery
			if stored.Status.WebhookDelivered || delivery.State != corev1alpha1.TaskWebhookDeliveryPending || delivery.Attempts != 1 {
				t.Fatalf("delivery = %#v", delivery)
			}
			if delivery.LastError != taskwebhook.ErrResolution.Error() && delivery.LastError != taskwebhook.ErrAddressNotAllowed.Error() {
				t.Fatalf("LastError = %q", delivery.LastError)
			}
			if strings.Contains(delivery.LastError, "hooks.example.com") {
				t.Fatalf("LastError leaks the destination: %q", delivery.LastError)
			}
		})
	}
}

// TestWebhookDeliveryPinsSameNamespaceServiceToClusterIP proves the in-cluster
// path keeps working under the public-only policy: the connection goes to the
// validated Service's ClusterIP without consulting DNS.
func TestWebhookDeliveryPinsSameNamespaceServiceToClusterIP(t *testing.T) {
	t.Parallel()
	receiver := newWebhookReceiver(t, http.StatusOK)
	parsed, err := url.Parse(receiver.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "receiver", Namespace: defaultNS},
		Spec: corev1.ServiceSpec{
			Type:       corev1.ServiceTypeClusterIP,
			ClusterIP:  "127.0.0.1",
			ClusterIPs: []string{"127.0.0.1"},
			Selector:   map[string]string{"app": "receiver"},
		},
	}
	task := newWebhookTask("http://receiver." + defaultNS + ".svc.cluster.local:" + parsed.Port() + "/hook")
	client := newWebhookTestClient(t, task, service)
	notifier := newWebhookNotifier(&taskwebhook.Dialer{
		Policy: taskwebhook.PublicAddressPolicy,
		Resolver: webhookTestResolver(func(context.Context, string) ([]net.IPAddr, error) {
			t.Error("a pinned Service destination must not be resolved")
			return nil, errors.New("unexpected lookup")
		}),
	})
	notifier.SetKubeClient(client)
	keys := newWebhookTestKeys(t)
	notifier.SetKeySource(taskwebhook.StaticKeySource{Keys: keys})

	if requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), task); err != nil || requeue != 0 {
		t.Fatalf("reconcileDelivery() = %v, %v (delivery %#v)", requeue, err, reloadWebhookTask(t, client, task).Status.WebhookDelivery)
	}
	receipts := receiver.all()
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}
	verifyReceipt(t, keys, task.Spec.WebhookURL, receipts[0], time.Now())
}

func TestHandleCompletedSkipsTasksWithoutWebhook(t *testing.T) {
	t.Parallel()
	notifier := NewWebhookNotifier()
	task := newWebhookTask("")
	client := newWebhookTestClient(t, task)
	if requeue, err := notifier.reconcileDelivery(context.Background(), client.Status(), task); err != nil || requeue != 0 {
		t.Fatalf("reconcileDelivery() = %v, %v", requeue, err)
	}
	if task.Status.WebhookDelivery != nil {
		t.Fatal("a Task without webhookURL must not record a delivery")
	}
}

func TestWebhookBackoffIsBounded(t *testing.T) {
	t.Parallel()
	notifier := NewWebhookNotifier()
	if notifier.client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("webhook transport must not use a proxy")
	}
	for attempts, want := range map[int32]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 4: 80 * time.Second, 7: 640 * time.Second, 20: 15 * time.Minute} {
		if got := notifier.backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
	if notifier.attemptLease <= taskwebhook.DefaultRequestTimeout {
		t.Fatal("the attempt lease must outlast one request")
	}
}

// slowWebhookKeySource advances the clock while loading keys, as a slow Secret
// read or key-source lock would.
type slowWebhookKeySource struct {
	taskwebhook.StaticKeySource
	clock *webhookTestClock
	delay time.Duration
}

func (s slowWebhookKeySource) SigningKeys(ctx context.Context) (taskwebhook.KeySet, error) {
	s.clock.now = s.clock.now.Add(s.delay)
	return s.StaticKeySource.SigningKeys(ctx)
}

func TestWebhookDeliveryLeaseStartsAfterKeyLoading(t *testing.T) {
	t.Parallel()
	clock := &webhookTestClock{now: time.Unix(1_800_000_100, 0)}
	var (
		mu       sync.Mutex
		leaseEnd *metav1.Time
		client   ctrlclient.Client
		task     *corev1alpha1.Task
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The attempt is claimed before sending; record the claimed lease.
		claimed := &corev1alpha1.Task{}
		if err := client.Get(r.Context(), ctrlclient.ObjectKeyFromObject(task), claimed); err == nil && claimed.Status.WebhookDelivery != nil {
			mu.Lock()
			leaseEnd = claimed.Status.WebhookDelivery.NextAttemptTime
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	task = newWebhookTask(server.URL)
	client = newWebhookTestClient(t, task)
	notifier := newTestWebhookNotifier(t, taskwebhook.KeySet{}, clock)
	notifier.SetKeySource(slowWebhookKeySource{StaticKeySource: taskwebhook.StaticKeySource{Keys: newWebhookTestKeys(t)}, clock: clock, delay: 3 * notifier.attemptLease})
	if _, err := notifier.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if leaseEnd == nil {
		t.Fatal("the attempt was sent without a claimed lease")
	}
	if want := clock.now.Add(notifier.attemptLease); !leaseEnd.Time.Equal(want) {
		t.Fatalf("claimed lease ends at %v, want %v (a full lease after slow key loading)", leaseEnd.Time, want)
	}
}
