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

func TestClientPreservesEscapedTurnID(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, err := NewClient(server.URL + "/adapter")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.StreamFrames(context.Background(), "turn 1", 0, func(HarnessEventFrame) error { return nil }); err != nil {
		t.Fatalf("StreamFrames() error = %v", err)
	}
	if requestedPath != "/adapter/v1/turns/turn%201/events" {
		t.Fatalf("requested path = %q", requestedPath)
	}
}

func TestReadSSEFramesRejectsOversizedMultiLineEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", 64*1024) + "\n"
	payload := strings.Repeat(line, maxHarnessSSEEventBytes/(64*1024)+1) + "\n"
	err := readSSEFrames(strings.NewReader(payload), func(HarnessEventFrame) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exceeds harness frame limit") {
		t.Fatalf("readSSEFrames() error = %v, want cumulative size rejection", err)
	}
}

func TestReadSSEFramesAcceptsAdvertisedResultAfterJSONExpansion(t *testing.T) {
	frame := HarnessEventFrame{
		Version:          ProtocolVersion,
		Type:             FrameTurnCompleted,
		RuntimeSessionID: "runtime-1",
		TurnID:           "turn-1",
		CorrelationID:    "correlation-1",
		Seq:              1,
		Completed:        &TurnCompleted{Result: strings.Repeat("\x00", 1<<20)},
	}
	payload := httptest.NewRecorder()
	if err := WriteSSEFrame(payload, frame); err != nil {
		t.Fatalf("WriteSSEFrame() error = %v", err)
	}
	var got HarnessEventFrame
	if err := readSSEFrames(payload.Body, func(value HarnessEventFrame) error {
		got = value
		return nil
	}); err != nil {
		t.Fatalf("readSSEFrames() error = %v", err)
	}
	if len(got.Completed.Result) != 1<<20 {
		t.Fatalf("result length = %d", len(got.Completed.Result))
	}
}

func TestClientRejectsOversizedJSONControlResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", maxHarnessControlResponseBytes)))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	var response map[string]any
	if err := client.getJSON(context.Background(), "/oversized", &response); err == nil ||
		!strings.Contains(err.Error(), "exceeds harness control limit") {
		t.Fatalf("getJSON() error = %v, want response size rejection", err)
	}
	if err := client.postJSON(context.Background(), "/oversized", map[string]string{"input": "safe"}, &response); err == nil ||
		!strings.Contains(err.Error(), "exceeds harness control limit") {
		t.Fatalf("postJSON() error = %v, want response size rejection", err)
	}
}

func TestClientControlTimeoutOverridesLaterParentDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		WriteJSON(w, http.StatusOK, HealthResponse{
			Version: ProtocolVersion,
			Status:  HealthStatusOK,
			Ready:   true,
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithControlTimeout(10*time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	started := time.Now()
	_, err = client.Health(ctx)
	if err == nil {
		t.Fatal("Health() error = nil, want control timeout")
	}
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("Health() elapsed = %s, control timeout was not enforced", elapsed)
	}
}

func TestReadSSEFramesDoesNotEmitBufferedDataAfterScannerFailure(t *testing.T) {
	frame := `{"version":"` + ProtocolVersion + `","type":"TurnStarted","runtimeSessionID":"r","turnID":"t","correlationID":"c","seq":1}`
	payload := "data: " + frame + "\n" + strings.Repeat("x", maxHarnessSSELineBytes+1)
	emitted := false
	err := readSSEFrames(strings.NewReader(payload), func(HarnessEventFrame) error {
		emitted = true
		return nil
	})
	if err == nil {
		t.Fatal("readSSEFrames() error = nil, want scanner failure")
	}
	if emitted {
		t.Fatal("readSSEFrames() emitted a partial event after scanner failure")
	}
}
