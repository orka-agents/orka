package common

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
)

func TestEventRecorderFakeCapturesEventsInOrder(t *testing.T) {
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	recorder := NewFakeEventRecorderWithClock(func() time.Time { return now })
	recorder.Record(context.Background(), events.ExecutionEventTypeTaskStarted, WithEventTaskName("task-1"))
	recorder.Record(
		context.Background(),
		events.ExecutionEventTypeToolCallCompleted,
		WithEventToolName("file_read"),
		WithEventToolCallID("call-1"),
	)

	gotTypes := recorder.EventTypes()
	wantTypes := []string{events.ExecutionEventTypeTaskStarted, events.ExecutionEventTypeToolCallCompleted}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("EventTypes() = %#v, want %#v", gotTypes, wantTypes)
	}
	captured := recorder.Events()
	if len(captured) != 2 {
		t.Fatalf("Events() length = %d, want 2", len(captured))
	}
	if captured[0].TaskName != "task-1" || captured[1].ToolName != "file_read" || captured[1].ToolCallID != "call-1" {
		t.Fatalf("captured events = %#v", captured)
	}
	if !captured[0].CreatedAt.Equal(now) || !captured[1].CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt values = %#v", captured)
	}
}

func TestEventRecorderNoopAllowsNilAndEmptyOptions(t *testing.T) {
	recorder := NoopEventRecorder{}
	recorder.Record(context.Background(), events.ExecutionEventTypeTaskCreated)
	recorder.Record(context.Background(), events.ExecutionEventTypeTaskCreated, nil, WithEventSeverity("warning"))
}

func TestEventRecorderFakeSanitizesPayloadOptions(t *testing.T) {
	recorder := NewFakeEventRecorder()
	bearerValue := testDashToken("bearer")
	apiKey := testOpenAIKey()
	recorder.Record(context.Background(), events.ExecutionEventTypeModelMessage,
		WithEventSeverity("ERROR"),
		WithEventSummary("Authorization: Bearer "+bearerValue),
		WithEventContent(mustRawJSON(t, map[string]any{"apiKey": apiKey, "safe": "ok"})),
		WithEventContentText(strings.Repeat("x", events.MaxExecutionEventContentTextChars+10)),
	)
	captured := recorder.Events()
	if len(captured) != 1 {
		t.Fatalf("Events() length = %d, want 1", len(captured))
	}
	event := captured[0]
	if event.Severity != events.ExecutionEventSeverityError {
		t.Fatalf("Severity = %q, want error", event.Severity)
	}
	if strings.Contains(event.Summary, bearerValue) || strings.Contains(string(event.Content), apiKey) {
		t.Fatalf("captured event leaked secret: %#v content=%s", event, event.Content)
	}
	if event.Truncation == nil || !event.Truncation.ContentTextTruncated {
		t.Fatalf("Truncation = %#v, want contentText truncated", event.Truncation)
	}
}

func TestFakeEventRecorderReset(t *testing.T) {
	recorder := NewFakeEventRecorder()
	recorder.Record(context.Background(), events.ExecutionEventTypeTaskStarted)
	recorder.Reset()
	if got := len(recorder.Events()); got != 0 {
		t.Fatalf("Events() length after reset = %d, want 0", got)
	}
}

func TestHTTPEventRecorderFromEnvMissingConfigNoop(t *testing.T) {
	t.Setenv(EnvOrkaControllerURL, "")
	t.Setenv(EnvOrkaTaskNamespace, "default")
	t.Setenv(EnvOrkaTaskName, "task-1")
	if _, ok := NewHTTPEventRecorderFromEnv().(NoopEventRecorder); !ok {
		t.Fatalf("NewHTTPEventRecorderFromEnv() = %T, want NoopEventRecorder", NewHTTPEventRecorderFromEnv())
	}
}

func TestHTTPEventRecorderPostsEventWithBearerToken(t *testing.T) {
	bearerValue := strings.Join([]string{"service", "account", "value"}, "-")
	bearerPath := writeTestSAToken(t, bearerValue)
	redactionValue := strings.Join([]string{"value", "for", "redaction"}, "-")
	requests := make(chan *http.Request, 1)
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- r
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"evt-1","seq":1,"createdAt":"2026-06-11T10:00:00Z"}`))
	}))
	defer server.Close()

	recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
		ControllerURL: server.URL,
		Namespace:     "default",
		TaskName:      "task-1",
		BearerPath:    bearerPath,
		Timeout:       time.Second,
	})
	recorder.Record(context.Background(), events.ExecutionEventTypeToolCallCompleted,
		WithEventSeverity("warning"),
		WithEventToolName("file_read"),
		WithEventToolCallID("call-1"),
		WithEventSummary("done"),
		WithEventContent(mustRawJSON(t, map[string]any{"token": redactionValue, "safe": "ok"})),
	)

	req := <-requests
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if req.URL.Path != "/internal/v1/events/default/task/task-1" {
		t.Fatalf("path = %s, want /internal/v1/events/default/task/task-1", req.URL.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+bearerValue {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
	body := <-bodies
	if body["type"] != events.ExecutionEventTypeToolCallCompleted ||
		body["severity"] != events.ExecutionEventSeverityWarning {
		t.Fatalf("body type/severity = %#v", body)
	}
	if body["taskName"] != "task-1" || body["toolName"] != "file_read" || body["toolCallID"] != "call-1" {
		t.Fatalf("body task/tool fields = %#v", body)
	}
	content, ok := body["content"].(map[string]any)
	if !ok {
		t.Fatalf("content = %#v, want object", body["content"])
	}
	if content["token"] != events.ExecutionEventRedactedValue || content["safe"] != "ok" {
		t.Fatalf("content = %#v, want redacted token and safe value", content)
	}
}

func TestHTTPEventRecorderWarningOnlyOnServerFailureAndTimeout(t *testing.T) {
	t.Run("server 500", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()
		recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
			ControllerURL: server.URL,
			Namespace:     "default",
			TaskName:      "task-1",
			BearerPath:    writeTestSAToken(t, "value"),
			Timeout:       time.Second,
		})
		recorder.Record(context.Background(), events.ExecutionEventTypeWorkerFailed)
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
		}))
		defer server.Close()
		recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
			ControllerURL: server.URL,
			Namespace:     "default",
			TaskName:      "task-1",
			BearerPath:    writeTestSAToken(t, "value"),
			Timeout:       10 * time.Millisecond,
		})
		start := time.Now()
		recorder.Record(context.Background(), events.ExecutionEventTypeWorkerFailed)
		if elapsed := time.Since(start); elapsed > 80*time.Millisecond {
			t.Fatalf("Record elapsed = %v, want timeout before server completes", elapsed)
		}
	})
}

func TestHTTPEventRecorderSanitizesAndTruncatesPayload(t *testing.T) {
	gotBody := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotBody <- body
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	secret := testDashToken("bearer")
	recorder := NewHTTPEventRecorder(HTTPEventRecorderConfig{
		ControllerURL: server.URL,
		Namespace:     "default",
		TaskName:      "task-1",
		BearerPath:    writeTestSAToken(t, "value"),
		Timeout:       time.Second,
	})
	recorder.Record(context.Background(), events.ExecutionEventTypeModelMessage,
		WithEventSummary("Authorization: Bearer "+secret),
		WithEventContentText(strings.Repeat("x", events.MaxExecutionEventContentTextChars+10)),
	)
	body := <-gotBody
	if strings.Contains(body["summary"].(string), secret) {
		t.Fatalf("summary leaked secret: %q", body["summary"])
	}
	if got := len([]rune(body["contentText"].(string))); got != events.MaxExecutionEventContentTextChars {
		t.Fatalf("contentText chars = %d, want %d", got, events.MaxExecutionEventContentTextChars)
	}
	truncation, ok := body["truncation"].(map[string]any)
	if !ok || truncation["contentTextTruncated"] != true {
		t.Fatalf("truncation = %#v, want contentTextTruncated metadata", body["truncation"])
	}
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw JSON: %v", err)
	}
	return json.RawMessage(data)
}

func writeTestSAToken(t *testing.T, token string) string {
	t.Helper()
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func testDashToken(prefix string) string {
	return strings.Join([]string{prefix, "value", "for", "redaction"}, "-")
}

func testOpenAIKey() string {
	return strings.Join([]string{"sk", "test12345678901234567890"}, "-")
}
