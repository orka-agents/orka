package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

func TestInternalSubmitExecutionEvent(t *testing.T) {
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	eventStore := store.NewFakeExecutionEventStoreWithClock(func() time.Time { return now })
	app := setupInternalExecutionEventApp(eventStore, &UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"})

	redactionValue := strings.Join([]string{"bearer", "value", "for", "redaction"}, "-")
	body := map[string]any{
		"id":          "client-id-is-ignored",
		"seq":         999,
		"createdAt":   "2000-01-01T00:00:00Z",
		"type":        events.ExecutionEventTypeModelMessage,
		"severity":    "ERROR",
		"summary":     "Authorization: Bearer " + redactionValue,
		"content":     map[string]any{"token": redactionValue, "safe": "ok"},
		"contentText": strings.Repeat("x", events.MaxExecutionEventContentTextChars+5),
	}
	resp := doJSONRequest(t, app, http.MethodPost, "/internal/v1/events/default/task/task-1", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var submitted SubmitExecutionEventResponse
	if err := json.NewDecoder(resp.Body).Decode(&submitted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if submitted.ID == "" || submitted.ID == "client-id-is-ignored" || submitted.Seq != 1 || !submitted.CreatedAt.Equal(now) {
		t.Fatalf("response = %#v, want assigned id seq createdAt", submitted)
	}

	stored, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-1",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored len = %d, want 1", len(stored))
	}
	event := stored[0]
	if event.ID == "client-id-is-ignored" || event.Seq != 1 || !event.CreatedAt.Equal(now) {
		t.Fatalf("stored assignment = %#v", event)
	}
	if event.TaskName != "task-1" || event.Type != events.ExecutionEventTypeModelMessage || event.Severity != events.ExecutionEventSeverityError {
		t.Fatalf("stored event fields = %#v", event)
	}
	if strings.Contains(event.Summary, redactionValue) || strings.Contains(event.ContentText, redactionValue) || len([]rune(event.ContentText)) != events.MaxExecutionEventContentTextChars {
		t.Fatalf("stored text not sanitized/truncated: summary=%q contentTextLen=%d", event.Summary, len([]rune(event.ContentText)))
	}
	var content map[string]string
	if err := json.Unmarshal(event.Content, &content); err != nil {
		t.Fatalf("unmarshal stored content: %v", err)
	}
	if content["token"] != events.ExecutionEventRedactedValue || content["safe"] != "ok" {
		t.Fatalf("stored content = %#v, want redacted token", content)
	}
	if event.Truncation == nil || !event.Truncation.ContentTextTruncated {
		t.Fatalf("truncation = %#v, want content text truncation", event.Truncation)
	}
}

func TestInternalSubmitExecutionEventValidationAndAuth(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	authenticatedApp := setupInternalExecutionEventApp(eventStore, &UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"})

	tests := []struct {
		name string
		path string
		body map[string]any
		want int
	}{
		{
			name: "invalid stream type",
			path: "/internal/v1/events/default/session/session-1",
			body: map[string]any{"type": events.ExecutionEventTypeTaskStarted},
			want: http.StatusBadRequest,
		},
		{
			name: "missing event type",
			path: "/internal/v1/events/default/task/task-1",
			body: map[string]any{"summary": "missing type"},
			want: http.StatusBadRequest,
		},
		{
			name: "unknown event type",
			path: "/internal/v1/events/default/task/task-1",
			body: map[string]any{"type": "UnknownEvent"},
			want: http.StatusBadRequest,
		},
		{
			name: "cross namespace service account denied",
			path: "/internal/v1/events/other/task/task-1",
			body: map[string]any{"type": events.ExecutionEventTypeTaskStarted},
			want: http.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := doJSONRequest(t, authenticatedApp, http.MethodPost, tt.path, tt.body)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}

	t.Run("payload too large", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/events/default/task/task-1", strings.NewReader(strings.Repeat("x", maxSubmitExecutionEventRequestBytes+1)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := authenticatedApp.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		app := setupInternalExecutionEventApp(eventStore, nil)
		resp := doJSONRequest(t, app, http.MethodPost, "/internal/v1/events/default/task/task-1", map[string]any{"type": events.ExecutionEventTypeTaskStarted})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}

func setupInternalExecutionEventApp(eventStore store.ExecutionEventStore, userInfo *UserInfo) *fiber.App {
	h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{ExecutionEventStore: eventStore})
	app := fiber.New()
	if userInfo != nil {
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, userInfo)
			return c.Next()
		})
	}
	app.Post("/internal/v1/events/:namespace/:streamType/:streamID", h.SubmitExecutionEvent)
	return app
}

func doJSONRequest(t *testing.T, app *fiber.App, method, target string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}
