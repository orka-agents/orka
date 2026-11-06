// Copyright (c) 2026. MIT License - see LICENSE file for details.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

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
	)
	for name, want := range map[string]int{
		"done": http.StatusConflict, "scheduled": http.StatusConflict, "cancelled": http.StatusOK,
		"missing": http.StatusNotFound,
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

func TestCancelSessionCancelsAChatCreatedTurn(t *testing.T) {
	chatTurn := cancelTestTask("chat-turn", corev1alpha1.TaskPhaseRunning, "")
	chatTurn.Labels = map[string]string{labels.LabelChatSession: "conversation"}
	mislabeled := cancelTestTask("mislabeled", corev1alpha1.TaskPhaseRunning, "")
	mislabeled.Labels = map[string]string{labels.LabelChatSession: "task-session"}
	h, app := setupCancelTest(t, chatTurn, mislabeled)
	for name, sessionType := range map[string]string{"conversation": store.SessionTypeChat, "task-session": "task"} {
		if err := h.sessionStore.CreateSession(context.Background(), &store.SessionRecord{Namespace: "default", Name: name, SessionType: sessionType}); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := postCancel(t, app, "/sessions/conversation/cancel", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if got, _ := cancelAnnotation(t, h, "chat-turn"); got != "cancelled by alice" {
		t.Fatalf("chat turn cancel request = %q", got)
	}
	// The chat label names a Session only when that Session is a chat.
	if resp, _ := postCancel(t, app, "/sessions/task-session/cancel", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a chat label on a task Session: status = %d, want 409", resp.StatusCode)
	}
	if _, ok := cancelAnnotation(t, h, "mislabeled"); ok {
		t.Fatal("a chat label must not tie a Task to a non-chat Session")
	}
}
