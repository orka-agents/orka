package harness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientSanitizesTransportErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusBadRequest, "Authorization: Bearer bearer-value-for-redaction")
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.Health(context.Background())
	if err == nil {
		t.Fatal("Health() error = nil, want status error")
	}
	if strings.Contains(err.Error(), "bearer-value-for-redaction") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("Health() error = %v, want sanitized", err)
	}
}

func TestClientStartTurnMismatchedResponseIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusAccepted, StartTurnResponse{
			Version:          ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: "other-runtime",
			TurnID:           "turn-a",
			CorrelationID:    "corr-a",
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.StartTurn(context.Background(), StartTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     AuthIdentity{Subject: "user:test"},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime session") {
		t.Fatalf("StartTurn() error = %v, want identity mismatch", err)
	}
}

func TestClientStreamFramesRejectsUnsafeTurnID(t *testing.T) {
	client, err := NewClient("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	for _, turnID := range []HarnessTurnID{"..", "turn/one", `turn\one`} {
		err = client.StreamFrames(context.Background(), turnID, 0, func(frame HarnessEventFrame) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "single safe path segment") {
			t.Fatalf("StreamFrames(%q) error = %v, want unsafe segment rejection", turnID, err)
		}
	}
}

func TestClientCancelTurnMismatchedResponseIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, CancelTurnResponse{
			Version:          ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: "other-runtime",
			TurnID:           "turn-a",
			CorrelationID:    "corr-a",
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.CancelTurn(context.Background(), CancelTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
	})
	if err == nil || !strings.Contains(err.Error(), "runtime session") {
		t.Fatalf("CancelTurn() error = %v, want identity mismatch", err)
	}
}

func TestClientCancelTurnRejectedResponseIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, CancelTurnResponse{
			Version:  ProtocolVersion,
			Accepted: false,
			TurnID:   "turn-a",
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.CancelTurn(context.Background(), CancelTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
	})
	if err == nil || !strings.Contains(err.Error(), "did not accept cancellation") {
		t.Fatalf("CancelTurn() error = %v, want rejected cancellation", err)
	}
}

func TestClientContinueTurnPostsToContinuePath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		WriteJSON(w, http.StatusAccepted, ContinueTurnResponse{
			Version:          ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: "runtime-a",
			TurnID:           "turn-a",
			CorrelationID:    "corr-a",
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.ContinueTurn(context.Background(), ContinueTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		ToolResults: []ToolCallResult{{
			Version:          ProtocolVersion,
			RuntimeSessionID: "runtime-a",
			TurnID:           "turn-a",
			ToolCallID:       "tool-1",
			IdempotencyKey:   "runtime-a:turn-a:tool-1",
			Output:           []byte(`{"success":true}`),
		}},
	})
	if err != nil {
		t.Fatalf("ContinueTurn() error = %v", err)
	}
	if gotPath != "/v1/turns/turn-a/continue" {
		t.Fatalf("ContinueTurn path = %q", gotPath)
	}
}

func TestClientContinueTurnMismatchedResponseIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusAccepted, ContinueTurnResponse{
			Version:          ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: "other-runtime",
			TurnID:           "turn-a",
			CorrelationID:    "corr-a",
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.ContinueTurn(context.Background(), ContinueTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		ToolResults: []ToolCallResult{{
			Version:          ProtocolVersion,
			RuntimeSessionID: "runtime-a",
			TurnID:           "turn-a",
			ToolCallID:       "tool-1",
			IdempotencyKey:   "runtime-a:turn-a:tool-1",
			Output:           []byte(`{"success":true}`),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime session") {
		t.Fatalf("ContinueTurn() error = %v, want identity mismatch", err)
	}
}

func TestNewClientDefaultDoesNotSetTotalHTTPTimeout(t *testing.T) {
	client, err := NewClient("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client.httpClient.Timeout != 0 {
		t.Fatalf("default http client timeout = %v, want no total stream timeout", client.httpClient.Timeout)
	}
	if client.controlTimeout <= 0 {
		t.Fatalf("control timeout = %v, want positive control call timeout", client.controlTimeout)
	}
}

func TestClientRejectsInvalidBaseURL(t *testing.T) {
	if _, err := NewClient("localhost:8080"); err == nil {
		t.Fatal("NewClient() error = nil, want invalid base URL")
	}
}

func TestReadSSEFramesStopsOnDoneSentinel(t *testing.T) {
	raw := "data: [DONE]\n\n" +
		"data: not-json\n\n"
	if err := readSSEFrames(strings.NewReader(raw), func(frame HarnessEventFrame) error {
		t.Fatalf("unexpected frame after done: %#v", frame)
		return nil
	}); err != nil {
		t.Fatalf("readSSEFrames() error = %v", err)
	}
}

func TestReadSSEFramesAllowsLargeFrameEnvelope(t *testing.T) {
	largeText := strings.Repeat("x", 200*1024)
	frame := HarnessEventFrame{
		Version:          ProtocolVersion,
		Type:             FrameRuntimeOutput,
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Seq:              1,
		ContentText:      largeText,
	}
	recorder := httptest.NewRecorder()
	if err := WriteSSEFrame(recorder, frame); err != nil {
		t.Fatalf("WriteSSEFrame() error = %v", err)
	}
	var got []HarnessEventFrame
	if err := readSSEFrames(strings.NewReader(recorder.Body.String()), func(frame HarnessEventFrame) error {
		got = append(got, frame)
		return nil
	}); err != nil {
		t.Fatalf("readSSEFrames() error = %v", err)
	}
	if len(got) != 1 || len(got[0].ContentText) != len(largeText) {
		t.Fatalf("frames=%d contentTextLen=%d, want one large frame", len(got), func() int {
			if len(got) == 0 {
				return 0
			}
			return len(got[0].ContentText)
		}())
	}
}

func TestReadSSEFrames(t *testing.T) {
	raw := "data: {\"version\":\"" + ProtocolVersion + "\",\"type\":\"TurnStarted\",\"runtimeSessionID\":\"r\",\"turnID\":\"t\",\"correlationID\":\"c\",\"seq\":1}\n\n" +
		"data: [DONE]\n\n"
	var frames []HarnessEventFrame
	if err := readSSEFrames(strings.NewReader(raw), func(frame HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("readSSEFrames() error = %v", err)
	}
	if len(frames) != 1 || frames[0].Type != FrameTurnStarted {
		t.Fatalf("frames = %#v, want one turn started", frames)
	}
}

func TestClientTimeoutOnStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(&http.Client{Timeout: 1 * time.Millisecond}))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	err = client.StreamFrames(context.Background(), "turn-a", 0, func(frame HarnessEventFrame) error { return nil })
	if err == nil {
		t.Fatal("StreamFrames() error = nil, want timeout")
	}
}
