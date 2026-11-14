// Copyright (c) 2026. MIT License - see LICENSE file for details.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func cancelTestTask(name string, phase corev1alpha1.TaskPhase, session string) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
		Status:     corev1alpha1.TaskStatus{Phase: phase},
	}
	if session != "" {
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: session}
	}
	return task
}

func setupCancelTest(t *testing.T, objects ...runtime.Object) (*Handlers, *fiber.App) {
	t.Helper()
	handlers, app := setupTestHandlersWithObjects(objects...)
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{Username: "alice"})
		return c.Next()
	})
	app.Post("/tasks/:id/cancel", handlers.CancelTask)
	app.Post("/sessions/:id/cancel", handlers.CancelSession)
	return handlers, app
}

func postCancel(t *testing.T, app *fiber.App, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path+"?namespace=default", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp, decoded
}

func cancelAnnotation(t *testing.T, h *Handlers, name string) (string, bool) {
	t.Helper()
	task := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, task); err != nil {
		t.Fatal(err)
	}
	value, ok := task.Annotations[labels.AnnotationCancelRequested]
	return value, ok
}

func TestCancelTaskRecordsTheCallerAndReasonOnce(t *testing.T) {
	h, app := setupCancelTest(t, cancelTestTask("running", corev1alpha1.TaskPhaseRunning, ""))
	resp, body := postCancel(t, app, "/tasks/running/cancel", `{"reason":"wrong\nrepository","requestedBy":"mallory"}`)
	if resp.StatusCode != http.StatusOK || body["requested"] != true {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if got, _ := cancelAnnotation(t, h, "running"); got != "cancelled by alice: wrong repository" {
		t.Fatalf("cancel request = %q", got)
	}
	resp, body = postCancel(t, app, "/tasks/running/cancel", `{"reason":"again"}`)
	if resp.StatusCode != http.StatusOK || body["requested"] != false {
		t.Fatalf("second cancel: status = %d, body = %v", resp.StatusCode, body)
	}
	if got, _ := cancelAnnotation(t, h, "running"); got != "cancelled by alice: wrong repository" {
		t.Fatalf("a second cancel must change nothing, got %q", got)
	}
}

func TestCancelTaskRefusesFinishedAndMissingTasks(t *testing.T) {
	h, app := setupCancelTest(t,
		cancelTestTask("done", corev1alpha1.TaskPhaseSucceeded, ""),
		cancelTestTask("cancelled", corev1alpha1.TaskPhaseCancelled, ""),
		cancelTestTask("scheduled", corev1alpha1.TaskPhaseScheduled, ""),
		cancelTestTask("pending", corev1alpha1.TaskPhasePending, ""),
		newCronTemplate("new-cron", ""),
		newCronTemplate("pending-cron", corev1alpha1.TaskPhasePending),
	)
	for name, want := range map[string]int{
		"done": http.StatusConflict, "scheduled": http.StatusConflict, "cancelled": http.StatusOK,
		"missing": http.StatusNotFound, "new-cron": http.StatusConflict, "pending-cron": http.StatusConflict,
	} {
		resp, body := postCancel(t, app, "/tasks/"+name+"/cancel", "")
		if resp.StatusCode != want {
			t.Fatalf("%s: status = %d, want %d (%v)", name, resp.StatusCode, want, body)
		}
	}
	if _, ok := cancelAnnotation(t, h, "done"); ok {
		t.Fatal("a finished Task must not get a cancel request")
	}
	if _, ok := cancelAnnotation(t, h, "cancelled"); ok {
		t.Fatal("an already Cancelled Task needs no cancel request")
	}
	for _, name := range []string{"new-cron", "pending-cron"} {
		if _, ok := cancelAnnotation(t, h, name); ok {
			t.Fatalf("%s: a cron template must not get a cancel request before it is Scheduled", name)
		}
	}
	if resp, _ := postCancel(t, app, "/tasks/pending/cancel", `{"uid":"an-older-uid"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a replaced Task: status = %d, want 409", resp.StatusCode)
	}
	if _, ok := cancelAnnotation(t, h, "pending"); ok {
		t.Fatal("a Task replaced since confirmation must not be cancelled")
	}
	if resp, _ := postCancel(t, app, "/tasks/pending/cancel", `{"uid":"pending-uid"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("the confirmed Task: status = %d, want 200", resp.StatusCode)
	}
	resp, _ := postCancel(t, app, "/tasks/pending/cancel", `{"reason":"`+strings.Repeat("x", maxCancelReasonLength+1)+`"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an over-long reason: status = %d", resp.StatusCode)
	}
}

func TestCancelSessionCancelsOnlyItsRunningTurn(t *testing.T) {
	h, app := setupCancelTest(t,
		cancelTestTask("turn", corev1alpha1.TaskPhaseRunning, "chat"),
		cancelTestTask("earlier-turn", corev1alpha1.TaskPhaseSucceeded, "chat"),
		cancelTestTask("other-session", corev1alpha1.TaskPhaseRunning, "other"),
	)
	for _, name := range []string{"chat", "idle"} {
		if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: name, SessionType: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := postCancel(t, app, "/sessions/chat/cancel", `{"reason":"enough"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if tasks, _ := body["tasks"].([]any); len(tasks) != 1 {
		t.Fatalf("cancelled = %v, want only the running turn", body["tasks"])
	}
	if got, _ := cancelAnnotation(t, h, "turn"); got != "cancelled by alice: enough" {
		t.Fatalf("turn cancel request = %q", got)
	}
	for _, name := range []string{"earlier-turn", "other-session"} {
		if _, ok := cancelAnnotation(t, h, name); ok {
			t.Fatalf("%s must not be cancelled", name)
		}
	}
	if resp, _ := postCancel(t, app, "/sessions/idle/cancel", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a Session with no running turn: status = %d, want 409", resp.StatusCode)
	}
	if resp, _ := postCancel(t, app, "/sessions/missing/cancel", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing Session: status = %d, want 404", resp.StatusCode)
	}
}

func TestCancelSessionHidesGatewayOwnedSessions(t *testing.T) {
	h, app := setupCancelTest(t, cancelTestTask("gateway-turn", corev1alpha1.TaskPhaseRunning, "gateway-chat"))
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: "default", Name: "gateway-chat", SessionType: store.SessionTypeGateway,
	}); err != nil {
		t.Fatal(err)
	}
	if resp, body := postCancel(t, app, "/sessions/gateway-chat/cancel", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a gateway-owned Session: status = %d, body = %v, want 404", resp.StatusCode, body)
	}
	if _, ok := cancelAnnotation(t, h, "gateway-turn"); ok {
		t.Fatal("a gateway-owned Session's Task must not be cancelled through the Session API")
	}
}

func TestCancelSessionCancelsAChatCreatedTurn(t *testing.T) {
	chatTurn := cancelTestTask("chat-turn", corev1alpha1.TaskPhaseRunning, "")
	chatTurn.Labels = map[string]string{labels.LabelChatSession: "conversation"}
	// create_ai_task sets both: the conversation label and a continuity Session.
	withContinuity := cancelTestTask("chat-turn-with-continuity", corev1alpha1.TaskPhaseRunning, "continuity")
	withContinuity.Labels = map[string]string{labels.LabelChatSession: "conversation"}
	mislabeled := cancelTestTask("mislabeled", corev1alpha1.TaskPhaseRunning, "")
	mislabeled.Labels = map[string]string{labels.LabelChatSession: "task-session"}
	h, app := setupCancelTest(t, chatTurn, withContinuity, mislabeled)
	// Stopping a turn reads Session metadata, never the transcript.
	sessions := &metadataOnlySessionStore{SessionStore: h.sessionStore, typeReader: h.sessionStore.(transcriptSessionTypeReader)}
	h.sessionStore = sessions
	for name, sessionType := range map[string]string{"conversation": store.SessionTypeChat, "task-session": "task", "continuity": "task"} {
		if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: name, SessionType: sessionType}); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := postCancel(t, app, "/sessions/conversation/cancel", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	for _, name := range []string{"chat-turn", "chat-turn-with-continuity"} {
		if got, _ := cancelAnnotation(t, h, name); got != "cancelled by alice" {
			t.Fatalf("%s cancel request = %q", name, got)
		}
	}
	if sessions.getCalls != 0 {
		t.Fatalf("GetSession calls = %d, want the metadata-only lookup", sessions.getCalls)
	}
	// The chat label names a Session only when that Session is a chat.
	if resp, _ := postCancel(t, app, "/sessions/task-session/cancel", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a chat label on a task Session: status = %d, want 409", resp.StatusCode)
	}
	if _, ok := cancelAnnotation(t, h, "mislabeled"); ok {
		t.Fatal("a chat label must not tie a Task to a non-chat Session")
	}
}

// TestCancelSessionSkipsTurnsOfAnEarlierIncarnation covers a chat deleted and
// recreated under its name while a Task of the old chat still runs: stopping
// the new chat's turn cancels only Tasks pinned to its own incarnation.
func TestCancelSessionSkipsTurnsOfAnEarlierIncarnation(t *testing.T) {
	current := cancelTestTask("current-turn", corev1alpha1.TaskPhaseRunning, "")
	earlier := cancelTestTask("earlier-turn", corev1alpha1.TaskPhaseRunning, "")
	h, app := setupCancelTest(t, current, earlier)
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: "default", Name: "recreated", SessionType: store.SessionTypeChat, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	epoch := chatSessionEpoch(context.Background(), h.sessionStore, "default", "recreated")
	if epoch == "" {
		t.Fatal("the test session store reports no creation time")
	}
	for name, pinned := range map[string]string{"current-turn": epoch, "earlier-turn": "1"} {
		task := &corev1alpha1.Task{}
		if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, task); err != nil {
			t.Fatal(err)
		}
		task.Labels = map[string]string{labels.LabelChatSession: "recreated", labels.LabelChatSessionEpoch: pinned}
		if err := h.client.Update(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := postCancel(t, app, "/sessions/recreated/cancel", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if got, _ := cancelAnnotation(t, h, "current-turn"); got == "" {
		t.Fatal("the current chat's turn must be cancelled")
	}
	if _, ok := cancelAnnotation(t, h, "earlier-turn"); ok {
		t.Fatal("a Task of the chat's earlier incarnation must not be cancelled")
	}
}

// creationFailingSessionStore cannot read a Session's creation time.
type creationFailingSessionStore struct {
	store.SessionStore
	typeReader transcriptSessionTypeReader
	err        error
}

func (s creationFailingSessionStore) GetSessionType(ctx context.Context, namespace, name string) (string, error) {
	return s.typeReader.GetSessionType(ctx, namespace, name)
}

func (s creationFailingSessionStore) GetSessionCreatedAt(context.Context, string, string) (time.Time, error) {
	return time.Time{}, s.err
}

// TestCancelSessionRefusesWithoutTheChatIncarnation covers a creation-time
// read that fails or finds the Session gone: without the incarnation the
// cancel cannot tell the chat's Tasks from an earlier chat's, so it cancels
// none.
func TestCancelSessionRefusesWithoutTheChatIncarnation(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
	}{
		"unreadable": {err: errors.New("database is locked"), status: http.StatusServiceUnavailable},
		"discarded":  {err: store.ErrNotFound, status: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			turn := cancelTestTask("chat-turn", corev1alpha1.TaskPhaseRunning, "")
			turn.Labels = map[string]string{labels.LabelChatSession: "conversation"}
			h, app := setupCancelTest(t, turn)
			if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{
				Namespace: "default", Name: "conversation", SessionType: store.SessionTypeChat, CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			h.sessionStore = creationFailingSessionStore{SessionStore: h.sessionStore, typeReader: h.sessionStore.(transcriptSessionTypeReader), err: tc.err}
			if resp, body := postCancel(t, app, "/sessions/conversation/cancel", ""); resp.StatusCode != tc.status {
				t.Fatalf("status = %d, body = %v, want %d", resp.StatusCode, body, tc.status)
			}
			if _, ok := cancelAnnotation(t, h, "chat-turn"); ok {
				t.Fatal("no Task may be cancelled without the chat's incarnation")
			}
		})
	}
}

func TestCancelSessionStopsAnInFlightChatRequest(t *testing.T) {
	h, app := setupCancelTest(t)
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: "conversation", SessionType: store.SessionTypeChat}); err != nil {
		t.Fatal(err)
	}
	chat := &ChatHandler{}
	h.chatTurns = chat
	if resp, _ := postCancel(t, app, "/sessions/conversation/cancel", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("an idle chat Session: status = %d, want 409", resp.StatusCode)
	}

	// A chat request that has not started a Task yet still counts as a turn.
	reservation, err := chat.reserveActiveChat("default", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		<-reservation.cancelContext.Done()
		close(stopped)
		chat.finishActiveChatReservation(reservation, nil)
	}()
	resp, body := postCancel(t, app, "/sessions/conversation/cancel", "")
	if resp.StatusCode != http.StatusOK || body["chatTurnStopped"] != true {
		t.Fatalf("status = %d, body = %v, want the chat turn stopped", resp.StatusCode, body)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("the chat request was not cancelled")
	}
	// The reservation is released, so the Session accepts the next message.
	next, err := chat.reserveActiveChat("default", "conversation")
	if err != nil {
		t.Fatalf("next message: %v", err)
	}
	chat.finishActiveChatReservation(next, nil)
}

// TestCancelSessionKeepsASessionItsFirstTurnCreated covers a cancel that stops
// the first turn of a new chat Session: a disconnected client's empty Session
// is deleted, but a cancelled one is kept, so the caller can still read it.
func TestCancelSessionKeepsASessionItsFirstTurnCreated(t *testing.T) {
	h, app := setupCancelTest(t)
	committer, ok := h.sessionStore.(store.SessionTurnCommitter)
	if !ok {
		t.Fatal("the test session store commits no chat turns")
	}
	chat := &ChatHandler{sessionTurnCommitter: committer}
	h.chatTurns = chat
	reservation, err := chat.reserveActiveChat("default", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := chat.activateReservedChat(context.Background(), reservation, "default", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-reservation.cancelContext.Done()
		handle.finish()
	}()
	resp, body := postCancel(t, app, "/sessions/fresh/cancel", "")
	if resp.StatusCode != http.StatusOK || body["chatTurnStopped"] != true {
		t.Fatalf("status = %d, body = %v, want the first turn stopped", resp.StatusCode, body)
	}
	if _, err := h.sessionStore.GetSession(context.Background(), "default", "fresh"); err != nil {
		t.Fatalf("the cancelled Session must be kept: %v", err)
	}
}

// blockingTurnCommitter holds ReleaseChatTurn until released, so a test can
// order a turn that is already ending against a cancel.
type blockingTurnCommitter struct {
	store.SessionTurnCommitter
	entered chan bool
	proceed chan struct{}
}

func (c blockingTurnCommitter) ReleaseChatTurn(ctx context.Context, namespace, name, turnID string, deleteEmptyCreatedSession bool) error {
	c.entered <- deleteEmptyCreatedSession
	<-c.proceed
	return c.SessionTurnCommitter.ReleaseChatTurn(ctx, namespace, name, turnID, deleteEmptyCreatedSession)
}

// TestCancelSessionReportsASessionItsEndingTurnDiscarded covers the race in
// which a first turn that is already ending decides to discard its empty
// Session before a cancel could keep it: the cancel then answers 404, never
// a success for a Session that is gone.
func TestCancelSessionReportsASessionItsEndingTurnDiscarded(t *testing.T) {
	h, app := setupCancelTest(t)
	committer, ok := h.sessionStore.(store.SessionTurnCommitter)
	if !ok {
		t.Fatal("the test session store commits no chat turns")
	}
	blocking := blockingTurnCommitter{SessionTurnCommitter: committer, entered: make(chan bool, 1), proceed: make(chan struct{})}
	chat := &ChatHandler{sessionTurnCommitter: blocking}
	h.chatTurns = chat
	reservation, err := chat.reserveActiveChat("default", "ending")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := chat.activateReservedChat(context.Background(), reservation, "default", "ending")
	if err != nil {
		t.Fatal(err)
	}
	go handle.finish() // the turn ends on its own, as after a disconnect
	if discard := <-blocking.entered; !discard {
		t.Fatal("an ending first turn should discard its empty Session")
	}
	type outcome struct {
		status int
		body   map[string]any
	}
	cancelled := make(chan outcome, 1)
	go func() {
		resp, body := postCancel(t, app, "/sessions/ending/cancel", "")
		cancelled <- outcome{resp.StatusCode, body}
	}()
	// Let the cancel reach the turn, then let the discard finish.
	deadline := time.Now().Add(5 * time.Second)
	for {
		chat.activeChatsMu.Lock()
		waiting := reservation.request.deleteWaiters > 0
		chat.activeChatsMu.Unlock()
		if waiting || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(blocking.proceed)
	got := <-cancelled
	if got.status != http.StatusNotFound {
		t.Fatalf("status = %d, body = %v; want 404 for the discarded Session", got.status, got.body)
	}
}

// staleCacheClient lists from a cache that has not seen the newest Tasks, and
// runs onPatch before each patch.
type staleCacheClient struct {
	client.Client
	onPatch func()
}

func (c staleCacheClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if tasks, ok := list.(*corev1alpha1.TaskList); ok {
		tasks.Items = nil
	}
	return nil
}

func (c staleCacheClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.onPatch != nil {
		c.onPatch()
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// A chat request that created a Task just before it was stopped: the Task is
// found although the cache has not seen it, and no next message can start
// until it is cancelled.
func TestCancelSessionCancelsTheStoppedChatTurnsTaskBeforeTheNextMessage(t *testing.T) {
	created := cancelTestTask("just-created", corev1alpha1.TaskPhasePending, "")
	created.Labels = map[string]string{labels.LabelChatSession: "conversation"}
	h, app := setupCancelTest(t, created)
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: "conversation", SessionType: store.SessionTypeChat}); err != nil {
		t.Fatal(err)
	}
	chat := &ChatHandler{}
	h.chatTurns = chat
	live := h.client
	h.apiReader = live
	var nextMessageErr error
	h.client = staleCacheClient{Client: live, onPatch: func() {
		next, err := chat.reserveActiveChat("default", "conversation")
		if err == nil {
			chat.finishActiveChatReservation(next, nil)
		}
		nextMessageErr = err
	}}

	reservation, err := chat.reserveActiveChat("default", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-reservation.cancelContext.Done()
		chat.finishActiveChatReservation(reservation, nil)
	}()
	resp, body := postCancel(t, app, "/sessions/conversation/cancel", "")
	if resp.StatusCode != http.StatusOK || body["chatTurnStopped"] != true {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if got, _ := cancelAnnotation(t, h, "just-created"); got != "cancelled by alice" {
		t.Fatalf("cancel request = %q, want the Task the cache missed cancelled", got)
	}
	if !errors.Is(nextMessageErr, store.ErrConflict) {
		t.Fatalf("next message while cancelling = %v, want it refused", nextMessageErr)
	}
	next, err := chat.reserveActiveChat("default", "conversation")
	if err != nil {
		t.Fatalf("next message after the stop: %v", err)
	}
	chat.finishActiveChatReservation(next, nil)
}

func TestStopActiveChatFencesAnIdleSession(t *testing.T) {
	chat := &ChatHandler{}
	running, release, err := chat.stopActiveChat(context.Background(), "default", "conversation")
	if err != nil || running {
		t.Fatalf("stopActiveChat() = %v, %v; want an idle Session", running, err)
	}
	if _, err := chat.reserveActiveChat("default", "conversation"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("next message before release = %v, want it refused", err)
	}
	release()
	next, err := chat.reserveActiveChat("default", "conversation")
	if err != nil {
		t.Fatalf("next message after release: %v", err)
	}
	chat.finishActiveChatReservation(next, nil)
}

func TestOverlappingIdleCancelsReportNoRunningTurn(t *testing.T) {
	chat := &ChatHandler{}
	running, releaseFirst, err := chat.stopActiveChat(context.Background(), "default", "conversation")
	if err != nil || running {
		t.Fatalf("first stopActiveChat() = %v, %v; want an idle Session", running, err)
	}
	type result struct {
		running bool
		release func()
		err     error
	}
	second := make(chan result, 1)
	go func() {
		running, release, err := chat.stopActiveChat(context.Background(), "default", "conversation")
		second <- result{running, release, err}
	}()
	var overlap result
	select {
	case overlap = <-second:
	case <-time.After(5 * time.Second):
		releaseFirst()
		t.Fatal("second idle cancel waited on the first cancel's gate")
	}
	if overlap.err != nil || overlap.running {
		t.Fatalf("second stopActiveChat() = %v, %v; want no running turn", overlap.running, overlap.err)
	}
	releaseFirst()
	if _, err := chat.reserveActiveChat("default", "conversation"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("next message while the second cancel holds the gate = %v, want it refused", err)
	}
	overlap.release()
	next, err := chat.reserveActiveChat("default", "conversation")
	if err != nil {
		t.Fatalf("next message after both releases: %v", err)
	}
	chat.finishActiveChatReservation(next, nil)
}

// phantomListReader lists Tasks that are gone or replaced by the time they
// are read.
type phantomListReader struct {
	client.Reader
	phantoms []corev1alpha1.Task
}

func (r phantomListReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if tasks, ok := list.(*corev1alpha1.TaskList); ok {
		tasks.Items = append(append([]corev1alpha1.Task{}, r.phantoms...), tasks.Items...)
	}
	return nil
}

func TestCancelSessionSkipsTasksDeletedOrReplacedSinceTheList(t *testing.T) {
	h, app := setupCancelTest(t, cancelTestTask("still-running", corev1alpha1.TaskPhaseRunning, "chat"))
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: "chat", SessionType: "task"}); err != nil {
		t.Fatal(err)
	}
	replaced := cancelTestTask("still-running", corev1alpha1.TaskPhaseRunning, "chat")
	replaced.UID = "an-older-uid"
	h.apiReader = phantomListReader{Reader: h.client, phantoms: []corev1alpha1.Task{
		*cancelTestTask("deleted", corev1alpha1.TaskPhaseRunning, "chat"), *replaced,
	}}
	resp, body := postCancel(t, app, "/sessions/chat/cancel", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if tasks, _ := body["tasks"].([]any); len(tasks) != 1 {
		t.Fatalf("cancelled = %v, want only the Task that still exists", body["tasks"])
	}
	if got, _ := cancelAnnotation(t, h, "still-running"); got != "cancelled by alice" {
		t.Fatalf("cancel request = %q, want the remaining Task cancelled", got)
	}
}

// chatTask is a running Task the chat "conversation" created; its
// chat-session-namespace label, when set, names the chat's namespace.
func chatTask(namespace, name, chatNamespace string) *corev1alpha1.Task {
	task := cancelTestTask(name, corev1alpha1.TaskPhaseRunning, "")
	task.Namespace = namespace
	task.Labels = map[string]string{labels.LabelChatSession: "conversation"}
	if chatNamespace != "" {
		task.Labels[labels.LabelChatSessionNamespace] = chatNamespace
	}
	return task
}

func cancelAnnotationIn(t *testing.T, h *Handlers, namespace, name string) (string, bool) {
	t.Helper()
	task := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, task); err != nil {
		t.Fatal(err)
	}
	value, ok := task.Annotations[labels.AnnotationCancelRequested]
	return value, ok
}

// A chat Task is tied to a Session by its chat's namespace as well as the
// conversation name, and a chat may have created it in another namespace.
func TestCancelSessionMatchesChatTasksByTheChatsNamespace(t *testing.T) {
	h, app := setupCancelTest(t,
		chatTask("default", "same-namespace", "default"),
		chatTask("default", "legacy-same-namespace", ""),
		// Created in this namespace by a chat of the same name in another one.
		chatTask("default", "other-chats-task", "elsewhere"),
		// Created in another namespace by this chat.
		chatTask("elsewhere", "created-elsewhere", "default"),
		// Another namespace's own chat of the same name.
		chatTask("elsewhere", "elsewhere-chat", ""),
	)
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: "conversation", SessionType: store.SessionTypeChat}); err != nil {
		t.Fatal(err)
	}
	resp, body := postCancel(t, app, "/sessions/conversation/cancel", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	for _, task := range []struct {
		namespace, name string
		cancelled       bool
	}{
		{"default", "same-namespace", true},
		{"default", "legacy-same-namespace", true},
		{"default", "other-chats-task", false},
		{"elsewhere", "created-elsewhere", true},
		{"elsewhere", "elsewhere-chat", false},
	} {
		if _, ok := cancelAnnotationIn(t, h, task.namespace, task.name); ok != task.cancelled {
			t.Fatalf("%s/%s cancelled = %v, want %v", task.namespace, task.name, ok, task.cancelled)
		}
	}
}

// With a watch namespace the API acts in no other namespace, so a chat Task
// elsewhere is not looked for.
func TestCancelSessionStaysInTheWatchNamespace(t *testing.T) {
	h, app := setupCancelTest(t,
		chatTask("default", "same-namespace", "default"),
		chatTask("elsewhere", "created-elsewhere", "default"),
	)
	h.watchNamespace = "default"
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: "conversation", SessionType: store.SessionTypeChat}); err != nil {
		t.Fatal(err)
	}
	if resp, body := postCancel(t, app, "/sessions/conversation/cancel", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if _, ok := cancelAnnotationIn(t, h, "elsewhere", "created-elsewhere"); ok {
		t.Fatal("a Task outside the watch namespace must not be cancelled")
	}
}

// A chat Task in another namespace is cancelled only with tasks/cancel in
// that namespace; the route authorized the Session's namespace alone. A
// denial cancels none of the Session's Tasks, including those in its own
// namespace.
func TestCancelSessionAuthorizesChatTasksInOtherNamespaces(t *testing.T) {
	for _, allow := range []bool{false, true} {
		h, app := setupTestHandlersWithObjects(
			chatTask("default", "same-namespace", "default"),
			chatTask("elsewhere", "created-elsewhere", "default"),
		)
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview})
			return c.Next()
		})
		app.Post("/sessions/:id/cancel", h.CancelSession)
		clientset, seen, _ := identityBindClientsetWithSubjects(allow)
		h.clientset = clientset
		if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: "conversation", SessionType: store.SessionTypeChat}); err != nil {
			t.Fatal(err)
		}
		resp, body := postCancel(t, app, "/sessions/conversation/cancel", "")
		want := http.StatusForbidden
		if allow {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Fatalf("allow=%v: status = %d, body = %v, want %d", allow, resp.StatusCode, body, want)
		}
		for _, task := range [][2]string{{"default", "same-namespace"}, {"elsewhere", "created-elsewhere"}} {
			if _, ok := cancelAnnotationIn(t, h, task[0], task[1]); ok != allow {
				t.Fatalf("allow=%v: %s/%s cancelled = %v", allow, task[0], task[1], ok)
			}
		}
		found := false
		for _, attributes := range *seen {
			if attributes.Namespace == "elsewhere" && attributes.Verb == apiVerbUpdate && attributes.Resource == "tasks" &&
				attributes.Subresource == "cancel" && attributes.Name == "created-elsewhere" {
				found = true
			}
		}
		if !found {
			t.Fatalf("allow=%v: access reviews = %+v, want update tasks/cancel in the Task's namespace", allow, *seen)
		}
	}
}

// A context token whose Task type covers only some of a Session's Tasks
// cancels none of them, whatever order they are listed in.
func TestCancelSessionChecksEveryTaskAgainstAContextTokenFirst(t *testing.T) {
	agentTask := chatTask("default", "agent-turn", "default")
	aiTask := chatTask("default", "ai-turn", "default")
	aiTask.Spec.Type = corev1alpha1.TaskTypeAI
	h, app := setupTestHandlersWithObjects(agentTask, aiTask)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatal(err)
	}
	h.contextTokenAuthorization = authz
	token := &ContextToken{Subject: "alice", Issuer: "https://issuer.example.test",
		Scopes:             append(slices.Clone(authz.SessionWriteScopes), authz.TaskUpdateScopes...),
		TransactionContext: map[string]any{"taskType": string(corev1alpha1.TaskTypeAgent)}}
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{Username: "alice", Subject: "alice", Issuer: token.Issuer, AuthType: AuthTypeContextToken, ContextToken: token})
		return c.Next()
	})
	app.Post("/sessions/:id/cancel", h.CancelSession)
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: "conversation", SessionType: store.SessionTypeChat}); err != nil {
		t.Fatal(err)
	}
	if resp, body := postCancel(t, app, "/sessions/conversation/cancel", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body = %v, want 403", resp.StatusCode, body)
	}
	for _, name := range []string{"agent-turn", "ai-turn"} {
		if _, ok := cancelAnnotationIn(t, h, "default", name); ok {
			t.Fatalf("%s was cancelled although the token does not cover every Task", name)
		}
	}
}

// A Session cancel the caller is refused for one of its Tasks stops no chat
// turn: the Tasks are authorized before the active request is stopped.
func TestRefusedSessionCancelLeavesTheActiveChatRunning(t *testing.T) {
	aiTask := chatTask("default", "ai-turn", "default")
	aiTask.Spec.Type = corev1alpha1.TaskTypeAI
	h, app := setupTestHandlersWithObjects(aiTask)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatal(err)
	}
	h.contextTokenAuthorization = authz
	token := &ContextToken{Subject: "alice", Issuer: "https://issuer.example.test",
		Scopes:             append(slices.Clone(authz.SessionWriteScopes), authz.TaskUpdateScopes...),
		TransactionContext: map[string]any{"taskType": string(corev1alpha1.TaskTypeAgent)}}
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{Username: "alice", Subject: "alice", Issuer: token.Issuer, AuthType: AuthTypeContextToken, ContextToken: token})
		return c.Next()
	})
	app.Post("/sessions/:id/cancel", h.CancelSession)
	if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: "conversation", SessionType: store.SessionTypeChat}); err != nil {
		t.Fatal(err)
	}
	chat := &ChatHandler{}
	h.chatTurns = chat
	reservation, err := chat.reserveActiveChat("default", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	defer chat.finishActiveChatReservation(reservation, nil)

	if resp, body := postCancel(t, app, "/sessions/conversation/cancel", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body = %v, want 403", resp.StatusCode, body)
	}
	if err := reservation.cancelContext.Err(); err != nil {
		t.Fatalf("the active chat request was stopped by a refused cancel: %v", err)
	}
	if _, ok := cancelAnnotationIn(t, h, "default", "ai-turn"); ok {
		t.Fatal("a refused cancel cancelled a Task")
	}
}

// newCronTemplate is a scheduled Task the controller has not yet moved to
// Scheduled: a cron template still, whatever its phase.
func newCronTemplate(name string, phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
	task := cancelTestTask(name, phase, "")
	task.Spec.Schedule = "*/5 * * * *"
	return task
}

// TestCancelMessageStaysWithinTheStatusLimit covers a caller with a very
// long name: the cancel request it records stays within the bound, ends on a
// whole character, and keeps the caller first.
func TestCancelMessageStaysWithinTheStatusLimit(t *testing.T) {
	long := strings.Repeat("é", 400)
	message := cancelMessage(&UserInfo{Username: long}, strings.Repeat("r", 500))
	if len(message) > cancelMessageLimit || !utf8.ValidString(message) || !strings.HasPrefix(message, "cancelled by ") {
		t.Fatalf("cancelMessage() = %d bytes (valid UTF-8 %t), want at most %d starting with the caller", len(message), utf8.ValidString(message), cancelMessageLimit)
	}
	if got := cancelMessage(&UserInfo{Username: "alice"}, "wrong repository"); got != "cancelled by alice: wrong repository" {
		t.Fatalf("a short message changed: %q", got)
	}
}
