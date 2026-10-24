/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/labels"
)

var (
	cancelRequester = &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "alice", Username: "alice"}
	cancelStranger  = &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "bob", Username: "bob"}
	cancelOperator  = &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:ops:operator"}
)

func cancelTestTask(name string, taskType corev1alpha1.TaskType, phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID("uid-" + name),
			Annotations: map[string]string{labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        taskType,
			RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: "alice"},
		},
		Status: corev1alpha1.TaskStatus{Phase: phase},
	}
}

func setupCancelHandlers(t *testing.T, tasks ...*corev1alpha1.Task) (*Handlers, *fiber.App) {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(newTestScheme())
	for _, task := range tasks {
		builder = builder.WithObjects(task)
	}
	h := NewHandlers(HandlersConfig{Client: builder.Build(), ExecutionMode: executionmode.HarnessV2})
	callers := map[string]*UserInfo{"alice": cancelRequester, "bob": cancelStranger, "operator": cancelOperator}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ui := callers[c.Get("X-Test-Caller")]; ui != nil {
			c.Locals(UserInfoContextKey, ui)
		}
		return c.Next()
	})
	app.Post("/api/v1/tasks/:id/cancel", h.CancelTask)
	return h, app
}

func postCancel(t *testing.T, app *fiber.App, task, caller string) (int, CancelTaskResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+task+"/cancel?namespace=default", nil)
	req.Header.Set("X-Test-Caller", caller)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var body CancelTaskResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
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

func TestCancelTaskRecordsTheRequestersCancel(t *testing.T) {
	h, app := setupCancelHandlers(t,
		cancelTestTask("running", corev1alpha1.TaskTypeAgent, corev1alpha1.TaskPhaseRunning))

	status, body := postCancel(t, app, "running", "alice")
	if status != http.StatusAccepted || body.Status != cancellationRequestedStatus {
		t.Fatalf("cancel = %d %+v, want 202 %q", status, body, cancellationRequestedStatus)
	}
	if value, ok := cancelAnnotation(t, h, "running"); !ok || value != requestedCancellationMessage {
		t.Fatalf("cancel annotation = %q, %v", value, ok)
	}
	// Asking again is idempotent.
	if status, _ := postCancel(t, app, "running", "alice"); status != http.StatusAccepted {
		t.Fatalf("second cancel = %d, want 202", status)
	}
}

func TestCancelTaskRefusesAnotherPerson(t *testing.T) {
	h, app := setupCancelHandlers(t,
		cancelTestTask("running", corev1alpha1.TaskTypeAgent, corev1alpha1.TaskPhaseRunning))

	if status, _ := postCancel(t, app, "running", "bob"); status != http.StatusForbidden {
		t.Fatalf("cancel by another person = %d, want 403", status)
	}
	if _, ok := cancelAnnotation(t, h, "running"); ok {
		t.Fatal("a refused cancel recorded a request")
	}
}

func TestCancelTaskRefusesAnUnstampedRequester(t *testing.T) {
	task := cancelTestTask("forged", corev1alpha1.TaskTypeAgent, corev1alpha1.TaskPhaseRunning)
	delete(task.Annotations, labels.AnnotationRequestedBySource)
	_, app := setupCancelHandlers(t, task)

	if status, _ := postCancel(t, app, "forged", "alice"); status != http.StatusForbidden {
		t.Fatalf("cancel of a Task without an API requester stamp = %d, want 403", status)
	}
}

func TestCancelTaskAllowsServiceAccountsWithRBAC(t *testing.T) {
	h, app := setupCancelHandlers(t,
		cancelTestTask("queued", corev1alpha1.TaskTypeAgent, corev1alpha1.TaskPhasePending))

	if status, _ := postCancel(t, app, "queued", "operator"); status != http.StatusAccepted {
		t.Fatalf("cancel by a ServiceAccount = %d, want 202", status)
	}
	if _, ok := cancelAnnotation(t, h, "queued"); !ok {
		t.Fatal("cancel annotation missing")
	}
}

func TestCancelTaskReportsFinishedTasks(t *testing.T) {
	_, app := setupCancelHandlers(t,
		cancelTestTask("done", corev1alpha1.TaskTypeAgent, corev1alpha1.TaskPhaseSucceeded),
		cancelTestTask("stopped", corev1alpha1.TaskTypeAgent, corev1alpha1.TaskPhaseCancelled))

	if status, _ := postCancel(t, app, "done", "alice"); status != http.StatusConflict {
		t.Fatalf("cancel of a succeeded Task = %d, want 409", status)
	}
	status, body := postCancel(t, app, "stopped", "alice")
	if status != http.StatusAccepted || body.Status != string(corev1alpha1.TaskPhaseCancelled) {
		t.Fatalf("cancel of a cancelled Task = %d %+v, want 202 Cancelled", status, body)
	}
}

func TestCancelTaskRefusesAScheduledTask(t *testing.T) {
	scheduled := cancelTestTask("nightly", corev1alpha1.TaskTypeAgent, corev1alpha1.TaskPhaseScheduled)
	scheduled.Spec.Schedule = "0 3 * * *"
	pending := cancelTestTask("nightly-new", corev1alpha1.TaskTypeAgent, corev1alpha1.TaskPhasePending)
	pending.Spec.Schedule = "0 3 * * *"
	h, app := setupCancelHandlers(t, scheduled, pending)
	for _, name := range []string{"nightly", "nightly-new"} {
		if status, _ := postCancel(t, app, name, "alice"); status != http.StatusConflict {
			t.Fatalf("cancel of scheduled Task %s = %d, want 409", name, status)
		}
		if _, ok := cancelAnnotation(t, h, name); ok {
			t.Fatalf("scheduled Task %s got a cancel request", name)
		}
	}
}

func TestCancelTaskRecordsAContainerTask(t *testing.T) {
	h, app := setupCancelHandlers(t,
		cancelTestTask("job", corev1alpha1.TaskTypeContainer, corev1alpha1.TaskPhaseRunning))

	// The Task reconciler completes a container Task's recorded cancel request.
	if status, _ := postCancel(t, app, "job", "alice"); status != http.StatusAccepted {
		t.Fatalf("cancel of a container Task = %d, want 202", status)
	}
	if _, ok := cancelAnnotation(t, h, "job"); !ok {
		t.Fatal("cancel annotation missing")
	}
}
