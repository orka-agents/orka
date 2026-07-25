package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestClientSanitizesConfiguredBearerFromErrors(t *testing.T) {
	token := strings.ToLower(t.Name())
	assertRedacted := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("error = nil, want sanitized client error")
		}
		if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[REDACTED]") {
			t.Fatalf("error = %v, want configured bearer redacted", err)
		}
	}

	t.Run("status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteError(w, http.StatusBadRequest, "remote reflected "+token)
		}))
		defer server.Close()
		client, err := NewClient(server.URL, WithBearerToken(token))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		_, err = client.Health(context.Background())
		assertRedacted(t, err)
	})

	t.Run("transport", func(t *testing.T) {
		httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport reflected " + token)
		})}
		client, err := NewClient("https://adapter.example", WithHTTPClient(httpClient), WithBearerToken(token))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		_, err = client.Health(context.Background())
		assertRedacted(t, err)
	})

	t.Run("decode", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusOK, map[string]any{
				"version":   ProtocolVersion,
				"status":    HealthStatusOK,
				"ready":     true,
				"checkedAt": token,
			})
		}))
		defer server.Close()
		client, err := NewClient(server.URL, WithBearerToken(token))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		_, err = client.Health(context.Background())
		assertRedacted(t, err)
	})

	t.Run("validation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusOK, HealthResponse{
				Version:   token,
				Status:    HealthStatusOK,
				Ready:     true,
				CheckedAt: time.Now().UTC(),
			})
		}))
		defer server.Close()
		client, err := NewClient(server.URL, WithBearerToken(token))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		_, err = client.Health(context.Background())
		assertRedacted(t, err)
	})
}

func TestClientCapabilitiesRejectsBearerInStructuralField(t *testing.T) {
	value := strings.ToLower(t.Name())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, CapabilitiesResponse{
			Version:                 ProtocolVersion,
			ProtocolVersion:         ProtocolVersion,
			Transport:               HTTPTransport,
			RuntimeName:             "runtime-" + value,
			ProviderKind:            ProviderKindRemote,
			ToolExecutionModes:      []ToolExecutionMode{ToolExecutionModeObserved},
			SupportsCancel:          true,
			SupportsRuntimeSessions: true,
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithBearerToken(value))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.Capabilities(context.Background())
	if err == nil || strings.Contains(err.Error(), value) {
		t.Fatalf("Capabilities() error = %v, want sanitized structural collision", err)
	}
}

func TestClientCapabilitiesSanitizesPresentationFields(t *testing.T) {
	value := strings.ToLower(t.Name())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, CapabilitiesResponse{
			Version:                 ProtocolVersion,
			ProtocolVersion:         ProtocolVersion,
			Transport:               HTTPTransport,
			RuntimeName:             "runtime-safe",
			RuntimeVersion:          value,
			ProviderKind:            ProviderKindRemote,
			ToolExecutionModes:      []ToolExecutionMode{ToolExecutionModeObserved},
			SupportsCancel:          true,
			SupportsRuntimeSessions: true,
			Metadata:                map[string]string{"reflected": value, "api_key": "opaque"},
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithBearerToken(value))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	encoded, _ := json.Marshal(caps)
	if strings.Contains(string(encoded), value) || strings.Contains(string(encoded), "opaque") {
		t.Fatalf("Capabilities() leaked sensitive data: %s", encoded)
	}
}

func TestClientHealthSanitizesPresentationFields(t *testing.T) {
	value := strings.ToLower(t.Name())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, HealthResponse{
			Version:   ProtocolVersion,
			Status:    HealthStatusOK,
			Ready:     true,
			Message:   value,
			CheckedAt: time.Now().UTC(),
			Metadata:  map[string]string{"reflected": value, "api_key": "opaque"},
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithBearerToken(value))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	encoded, _ := json.Marshal(health)
	if strings.Contains(string(encoded), value) || strings.Contains(string(encoded), "opaque") {
		t.Fatalf("Health() leaked sensitive data: %s", encoded)
	}
}

func TestSanitizeHarnessFrameRedactsConfiguredBearer(t *testing.T) {
	value := strings.ToLower(t.Name())
	client := &Client{authBearerValue: value}
	frame := HarnessEventFrame{
		Version:          ProtocolVersion,
		Type:             FrameTurnCompleted,
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Seq:              1,
		Severity:         value,
		Summary:          "summary " + value,
		Content:          json.RawMessage(`{"` + value + `":{"nested":["` + value + `"]}}`),
		ContentText:      "content " + value,
		ToolName:         "tool-safe",
		ToolCallID:       "call-safe",
		ApprovalID:       "approval-safe",
		Metadata:         map[string]string{"reflected": "metadata-" + value, "api_key": "opaque"},
		Completed: &TurnCompleted{
			Result:    "result " + value,
			Data:      map[string]any{value: []any{value}},
			OutputRef: "output-safe",
			Artifacts: []ArtifactRef{{Filename: "result.txt", ContentType: "text/plain", Description: value}},
		},
		Failed: &TurnFailed{
			Reason:    "failed_reason",
			Message:   "failed " + value,
			Result:    value,
			Data:      map[string]any{value: value},
			OutputRef: "failed-output",
			Artifacts: []ArtifactRef{{Filename: "failed.txt", ContentType: "text/plain", Description: value}},
		},
		Error: &ErrorInfo{Code: "remote_error", Message: value},
	}
	sanitized, err := client.sanitizeHarnessFrame(frame)
	if err != nil {
		t.Fatalf("sanitizeHarnessFrame() error = %v", err)
	}
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), value) {
		t.Fatalf("sanitized frame leaked configured bearer: %s", encoded)
	}
	if !strings.Contains(frame.Summary, value) {
		t.Fatal("sanitizeHarnessFrame mutated the input summary")
	}
	if !strings.Contains(frame.Metadata["reflected"], value) || frame.Metadata["api_key"] != "opaque" {
		t.Fatal("sanitizeHarnessFrame mutated the input metadata")
	}
}

func TestClientStreamFramesSanitizesConfiguredBearerFromFrame(t *testing.T) {
	value := strings.ToLower(t.Name())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_ = WriteSSEFrame(w, HarnessEventFrame{
			Version:          ProtocolVersion,
			Type:             FrameRuntimeOutput,
			RuntimeSessionID: "runtime-a",
			TurnID:           "turn-a",
			CorrelationID:    "corr-a",
			Seq:              1,
			Summary:          value,
			Content:          json.RawMessage(`{"message":"` + value + `"}`),
			ContentText:      value,
			Metadata:         map[string]string{"reflected": value},
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithBearerToken(value))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	var got HarnessEventFrame
	err = client.StreamFrames(context.Background(), "turn-a", 0, func(frame HarnessEventFrame) error {
		got = frame
		return nil
	})
	if err != nil {
		t.Fatalf("StreamFrames() error = %v", err)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), value) {
		t.Fatalf("callback frame leaked configured bearer: %s", encoded)
	}
}

func TestSanitizeHarnessFramePreservesBrokeredToolArguments(t *testing.T) {
	client := &Client{authBearerValue: "mock-token"}
	original := json.RawMessage(` {"password":"keep-me", "pageToken":"cursor"} `)
	sanitized, err := client.sanitizeHarnessFrame(HarnessEventFrame{
		Type:    FrameToolCallRequested,
		Content: original,
	})
	if err != nil {
		t.Fatalf("sanitizeHarnessFrame() error = %v", err)
	}
	if !bytes.Equal(sanitized.Content, original) {
		t.Fatalf("brokered tool Content = %q, want original bytes %q", sanitized.Content, original)
	}
}

func TestSanitizeHarnessFrameRejectsBearerInBrokeredToolArguments(t *testing.T) {
	client := &Client{authBearerValue: "opaque-bearer"}
	_, err := client.sanitizeHarnessFrame(HarnessEventFrame{
		Type:    FrameToolCallRequested,
		Content: json.RawMessage(`{"value":"opaque-bearer"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "brokered tool content") {
		t.Fatalf("sanitizeHarnessFrame() error = %v, want bearer rejection", err)
	}
}

func TestSanitizeHarnessFrameSanitizesSensitiveMetadataMarkerCollision(t *testing.T) {
	value := strings.Trim("[REDACTED]", "[]")
	client := &Client{authBearerValue: value}
	sanitized, err := client.sanitizeHarnessFrame(HarnessEventFrame{
		Metadata: map[string]string{"api_key": "opaque"},
	})
	if err != nil {
		t.Fatalf("sanitizeHarnessFrame() error = %v", err)
	}
	encoded, _ := json.Marshal(sanitized)
	if strings.Contains(string(encoded), value) || strings.Contains(string(encoded), "opaque") {
		t.Fatalf("sanitized metadata leaked marker collision or secret: %s", encoded)
	}
}

func TestSanitizeHarnessFramePreservesUnchangedRawContent(t *testing.T) {
	client := &Client{authBearerValue: "mock-token"}
	original := json.RawMessage(` {"b":1, "a":2} `)
	sanitized, err := client.sanitizeHarnessFrame(HarnessEventFrame{Content: original})
	if err != nil {
		t.Fatalf("sanitizeHarnessFrame() error = %v", err)
	}
	if !bytes.Equal(sanitized.Content, original) {
		t.Fatalf("Content = %q, want original bytes %q", sanitized.Content, original)
	}
}

func TestSanitizeHarnessFramePreservesSensitiveKeyNameUsedAsJSONValue(t *testing.T) {
	client := &Client{authBearerValue: "mock-token"}
	original := json.RawMessage(` {"kind":"api_key"} `)
	sanitized, err := client.sanitizeHarnessFrame(HarnessEventFrame{Content: original})
	if err != nil {
		t.Fatalf("sanitizeHarnessFrame() error = %v", err)
	}
	if !bytes.Equal(sanitized.Content, original) {
		t.Fatalf("Content = %q, want original bytes %q", sanitized.Content, original)
	}
}

func TestSanitizeHarnessFrameSanitizesDuplicateAndSensitiveJSONKeys(t *testing.T) {
	value := strings.ToLower(t.Name())
	client := &Client{authBearerValue: value}
	content := json.RawMessage(`{"duplicate":"` + value + `","duplicate":"safe","api_key":"opaque"}`)
	sanitized, err := client.sanitizeHarnessFrame(HarnessEventFrame{Content: content})
	if err != nil {
		t.Fatalf("sanitizeHarnessFrame() error = %v", err)
	}
	if strings.Contains(string(sanitized.Content), value) || strings.Contains(string(sanitized.Content), "opaque") {
		t.Fatalf("Content leaked configured or sensitive value: %s", sanitized.Content)
	}
}

func TestSanitizeHarnessFrameRejectsBearerInStructuralField(t *testing.T) {
	client := &Client{authBearerValue: "turn-a"}
	_, err := client.sanitizeHarnessFrame(HarnessEventFrame{TurnID: "turn-a"})
	if err == nil || !strings.Contains(err.Error(), "structural field") {
		t.Fatalf("sanitizeHarnessFrame() error = %v, want structural bearer conflict", err)
	}
}

func TestSanitizeHarnessFrameRejectsGenericSecretInStructuralField(t *testing.T) {
	client := &Client{}
	_, err := client.sanitizeHarnessFrame(HarnessEventFrame{
		CorrelationID: "Authorization: Bearer mock-token",
	})
	if err == nil || !strings.Contains(err.Error(), "structural field") {
		t.Fatalf("sanitizeHarnessFrame() error = %v, want generic secret rejection", err)
	}
}

func TestSanitizeHarnessFrameRejectsNumericBearerInStructuralFields(t *testing.T) {
	client := &Client{authBearerValue: "12345678"}
	for name, frame := range map[string]HarnessEventFrame{
		"seq":      {Seq: 12345678},
		"finalSeq": {Completed: &TurnCompleted{FinalEventSeq: 12345678}},
		"size":     {Completed: &TurnCompleted{Artifacts: []ArtifactRef{{Size: 12345678}}}},
	} {
		if _, err := client.sanitizeHarnessFrame(frame); err == nil {
			t.Fatalf("%s structural numeric bearer was not rejected", name)
		}
	}
}

func TestSanitizeHarnessFrameSanitizesNumericBearer(t *testing.T) {
	client := &Client{authBearerValue: "12345678"}
	frame := HarnessEventFrame{
		Content:   json.RawMessage(`{"value":12345678}`),
		Completed: &TurnCompleted{Data: map[string]any{"value": float64(12345678)}},
	}
	sanitized, err := client.sanitizeHarnessFrame(frame)
	if err != nil {
		t.Fatalf("sanitizeHarnessFrame() error = %v", err)
	}
	encoded, _ := json.Marshal(sanitized)
	if strings.Contains(string(encoded), "12345678") {
		t.Fatalf("sanitized frame leaked numeric bearer: %s", encoded)
	}
}

func TestSanitizeHarnessFrameRejectsRawEscapeBearerBypass(t *testing.T) {
	client := &Client{authBearerValue: "token123"}
	content := json.RawMessage(`{"value":"\token123"}`)
	if _, err := client.sanitizeHarnessFrame(HarnessEventFrame{Content: content}); err == nil {
		t.Fatal("sanitizeHarnessFrame() error = nil, want raw bearer postcondition failure")
	}
	if _, err := client.sanitizeHarnessFrame(HarnessEventFrame{Type: FrameToolCallRequested, Content: content}); err == nil {
		t.Fatal("brokered sanitizeHarnessFrame() error = nil, want raw bearer rejection")
	}
}

func TestSanitizeHarnessFrameRejectsInvalidContentJSON(t *testing.T) {
	client := &Client{authBearerValue: "mock-token"}
	_, err := client.sanitizeHarnessFrame(HarnessEventFrame{Content: json.RawMessage(`{"invalid"`)})
	if err == nil || !strings.Contains(err.Error(), "invalid harness frame content JSON") {
		t.Fatalf("sanitizeHarnessFrame() error = %v, want invalid content JSON", err)
	}
}

func TestClientErrorSanitizesRenderedFields(t *testing.T) {
	for _, tt := range []struct {
		name            string
		configuredValue string
		op              string
		status          int
	}{
		{name: "operation", configuredValue: "stream_frames", op: "stream_frames", status: http.StatusBadGateway},
		{name: "status", configuredValue: "401", op: "get", status: http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{authBearerValue: tt.configuredValue}
			err := client.sanitizeClientError(ClientError{
				Op:         tt.op,
				StatusCode: tt.status,
				Message:    "remote failure",
			})
			if strings.Contains(err.Error(), tt.configuredValue) {
				t.Fatalf("sanitized error = %q, configured bearer leaked from rendered fields", err.Error())
			}
			var clientErr ClientError
			if !errors.As(err, &clientErr) {
				t.Fatalf("sanitized error = %T, want ClientError", err)
			}
			if clientErr.Op != tt.op || clientErr.StatusCode != tt.status {
				t.Fatalf("structured fields = op:%q status:%d, want op:%q status:%d", clientErr.Op, clientErr.StatusCode, tt.op, tt.status)
			}
		})
	}
}

func TestClientStreamFramesPreservesContextCancellationCause(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})}
	client, err := NewClient(
		"https://adapter.example",
		WithHTTPClient(httpClient),
		WithBearerToken("context canceled"),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	err = client.StreamFrames(context.Background(), "turn-a", 0, func(HarnessEventFrame) error { return nil })
	if err == nil {
		t.Fatal("StreamFrames() error = nil, want context cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StreamFrames() error = %v, want context.Canceled cause", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("StreamFrames() error = %v, configured bearer leaked", err)
	}
}

func TestClientStreamFramesPreservesCallbackClientError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_ = WriteSSEFrame(w, HarnessEventFrame{
			Version:          ProtocolVersion,
			Type:             FrameTurnStarted,
			RuntimeSessionID: "runtime-a",
			TurnID:           "turn-a",
			CorrelationID:    "corr-a",
			Seq:              1,
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithBearerToken("mock-token"))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	callbackErr := &ClientError{Op: "callback", Message: "caller-owned error"}
	err = client.StreamFrames(context.Background(), "turn-a", 0, func(HarnessEventFrame) error {
		return callbackErr
	})
	if err != callbackErr {
		t.Fatalf("StreamFrames() error = %#v, want original callback error %#v", err, callbackErr)
	}
}

func TestRedactExactBearerValue(t *testing.T) {
	t.Run("redacts embedded short token text", func(t *testing.T) {
		got := RedactExactBearerValue("turn already exists; reflected x", "x")
		want := "turn already e[REDACTED]ists; reflected [REDACTED]"
		if got != want {
			t.Fatalf("RedactExactBearerValue() = %q, want %q", got, want)
		}
	})
	t.Run("redacts key value assignment", func(t *testing.T) {
		token := strings.ToLower(t.Name())
		got := RedactExactBearerValue("id="+token, token)
		want := "id=[REDACTED]"
		if got != want {
			t.Fatalf("RedactExactBearerValue() = %q, want %q", got, want)
		}
	})
	t.Run("redacts sentence punctuation", func(t *testing.T) {
		token := strings.ToLower(t.Name())
		got := RedactExactBearerValue("remote reflected "+token+".", token)
		want := "remote reflected [REDACTED]."
		if got != want {
			t.Fatalf("RedactExactBearerValue() = %q, want %q", got, want)
		}
	})
	t.Run("redacts path adjacency", func(t *testing.T) {
		token := strings.ToLower(t.Name())
		got := RedactExactBearerValue("/runtime/"+token+"/events", token)
		want := "/runtime/[REDACTED]/events"
		if got != want {
			t.Fatalf("RedactExactBearerValue() = %q, want %q", got, want)
		}
	})
	t.Run("avoids marker collision", func(t *testing.T) {
		token := strings.Trim("[REDACTED]", "[]")
		client := &Client{authBearerValue: token}
		got := client.sanitizeClientMessage("Authorization: Bearer placeholder")
		if strings.Contains(got, token) {
			t.Fatalf("sanitizeClientMessage() = %q, configured bearer reproduced by marker", got)
		}
	})
	t.Run("removes reconstructed collision", func(t *testing.T) {
		token := strings.Trim("[REDACTED]", "[]")
		got := RedactExactBearerValue("RED"+token+"ACTED", token)
		if strings.Contains(got, token) {
			t.Fatalf("RedactExactBearerValue() = %q, configured bearer reconstructed across replacement", got)
		}
	})
}

func TestClientErrorClassificationIncludesOperation(t *testing.T) {
	err := safeClientError("decode harness frame", 0, "unexpected EOF")
	var clientErr ClientError
	if !errors.As(err, &clientErr) {
		t.Fatalf("safeClientError() = %T, want ClientError", err)
	}
	if !clientErr.IsProtocolViolation() {
		t.Fatal("operation-carried protocol violation was not classified")
	}
}

func TestClientDuplicateTurnReasonRequiresConflictStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusInternalServerError} {
		err := safeClientError("post", status, "turn already exists")
		var clientErr ClientError
		if !errors.As(err, &clientErr) {
			t.Fatalf("safeClientError(%d) = %T, want ClientError", status, err)
		}
		if clientErr.IsDuplicateTurn() {
			t.Fatalf("safeClientError(%d) was classified as duplicate turn", status)
		}
	}
	err := safeClientError("post", http.StatusConflict, "turn already exists")
	var clientErr ClientError
	if !errors.As(err, &clientErr) || !clientErr.IsDuplicateTurn() {
		t.Fatalf("safeClientError(409) = %#v, want duplicate turn", err)
	}
}

func TestClientStartTurnRejectsSensitiveEventStreamPath(t *testing.T) {
	value := strings.ToLower(t.Name())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusAccepted, StartTurnResponse{
			Version:          ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: "runtime-a",
			TurnID:           "turn-a",
			CorrelationID:    "corr-a",
			EventStreamPath:  "/v1/turns/" + value + "/events",
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithBearerToken(value))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.StartTurn(context.Background(), validClientStartTurnRequest())
	if err == nil || strings.Contains(err.Error(), value) {
		t.Fatalf("StartTurn() error = %v, want sanitized eventStreamPath rejection", err)
	}
	var clientErr ClientError
	if !errors.As(err, &clientErr) || !clientErr.RemoteAccepted {
		t.Fatalf("StartTurn() error = %#v, want accepted remote marker", err)
	}
}

func TestClientSanitizesSuccessfulMutationMessages(t *testing.T) {
	value := strings.ToLower(t.Name())
	t.Run("cancel", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusAccepted, CancelTurnResponse{
				Version:          ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: "runtime-a",
				TurnID:           "turn-a",
				CorrelationID:    "corr-a",
				Message:          value,
			})
		}))
		defer server.Close()
		client, err := NewClient(server.URL, WithBearerToken(value))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		response, err := client.CancelTurn(context.Background(), CancelTurnRequest{
			Version:          ProtocolVersion,
			Namespace:        "default",
			TaskName:         "task-a",
			SessionName:      "session-a",
			RuntimeSessionID: "runtime-a",
			TurnID:           "turn-a",
			CorrelationID:    "corr-a",
		})
		if err != nil {
			t.Fatalf("CancelTurn() error = %v", err)
		}
		if strings.Contains(response.Message, value) {
			t.Fatalf("CancelTurn() message = %q, configured bearer leaked", response.Message)
		}
	})
	t.Run("continue", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteJSON(w, http.StatusAccepted, ContinueTurnResponse{
				Version:          ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: "runtime-a",
				TurnID:           "turn-a",
				CorrelationID:    "corr-a",
				Message:          value,
			})
		}))
		defer server.Close()
		client, err := NewClient(server.URL, WithBearerToken(value))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		response, err := client.ContinueTurn(context.Background(), ContinueTurnRequest{
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
				ToolCallID:       "call-a",
				IdempotencyKey:   ToolRequestIdempotencyKey("runtime-a", "turn-a", "call-a"),
				Approved:         true,
				Output:           json.RawMessage(`{"ok":true}`),
			}},
		})
		if err != nil {
			t.Fatalf("ContinueTurn() error = %v", err)
		}
		if strings.Contains(response.Message, value) {
			t.Fatalf("ContinueTurn() message = %q, configured bearer leaked", response.Message)
		}
	})
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
	_, err = client.StartTurn(context.Background(), validClientStartTurnRequest())
	if err == nil || !strings.Contains(err.Error(), "runtime session") {
		t.Fatalf("StartTurn() error = %v, want identity mismatch", err)
	}
	var clientErr ClientError
	if !errors.As(err, &clientErr) || !clientErr.RemoteAccepted {
		t.Fatalf("StartTurn() error = %#v, want accepted remote response marker", err)
	}
}

func TestClientStartTurnVersionErrorAcceptanceMarker(t *testing.T) {
	for _, tt := range []struct {
		name               string
		includeAccepted    bool
		acceptedValue      any
		wantRemoteAccepted bool
		wantUnknown        bool
	}{
		{name: "rejected", includeAccepted: true, acceptedValue: false},
		{name: "accepted", includeAccepted: true, acceptedValue: true, wantRemoteAccepted: true},
		{name: "omitted", wantUnknown: true},
		{name: "null", includeAccepted: true, acceptedValue: nil, wantUnknown: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				response := map[string]any{"version": "orka.harness.invalid"}
				if tt.includeAccepted {
					response["accepted"] = tt.acceptedValue
				}
				WriteJSON(w, http.StatusAccepted, response)
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			_, err = client.StartTurn(context.Background(), validClientStartTurnRequest())
			if err == nil || !strings.Contains(err.Error(), "unsupported version") {
				t.Fatalf("StartTurn() error = %v, want unsupported version", err)
			}
			var clientErr ClientError
			if !errors.As(err, &clientErr) {
				t.Fatalf("StartTurn() error = %T, want ClientError", err)
			}
			if clientErr.RemoteAccepted != tt.wantRemoteAccepted {
				t.Fatalf("RemoteAccepted = %t, want %t", clientErr.RemoteAccepted, tt.wantRemoteAccepted)
			}
			if clientErr.RemoteAcceptanceUnknown != tt.wantUnknown {
				t.Fatalf("RemoteAcceptanceUnknown = %t, want %t", clientErr.RemoteAcceptanceUnknown, tt.wantUnknown)
			}
		})
	}
}

func TestClientStartTurnMissingAcceptedMarksAcceptanceUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusAccepted, map[string]any{
			"version":          ProtocolVersion,
			"runtimeSessionID": "runtime-a",
			"turnID":           "turn-a",
			"correlationID":    "corr-a",
		})
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.StartTurn(context.Background(), validClientStartTurnRequest())
	if err == nil || !strings.Contains(err.Error(), "did not include accepted") {
		t.Fatalf("StartTurn() error = %v, want missing accepted", err)
	}
	var clientErr ClientError
	if !errors.As(err, &clientErr) {
		t.Fatalf("StartTurn() error = %T, want ClientError", err)
	}
	if !clientErr.RemoteAcceptanceUnknown || clientErr.RemoteAccepted {
		t.Fatalf("acceptance markers = accepted:%t unknown:%t, want false/true", clientErr.RemoteAccepted, clientErr.RemoteAcceptanceUnknown)
	}
}

func TestClientStartTurnTransportFailureMarksAcceptanceUnknown(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport failed")
	})}
	client, err := NewClient("https://adapter.example", WithHTTPClient(httpClient))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.StartTurn(context.Background(), validClientStartTurnRequest())
	if err == nil {
		t.Fatal("StartTurn() error = nil, want transport failure")
	}
	var clientErr ClientError
	if !errors.As(err, &clientErr) {
		t.Fatalf("StartTurn() error = %T, want ClientError", err)
	}
	if !clientErr.RemoteAcceptanceUnknown || clientErr.RemoteAccepted {
		t.Fatalf("acceptance markers = accepted:%t unknown:%t, want false/true", clientErr.RemoteAccepted, clientErr.RemoteAcceptanceUnknown)
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
	client := &Client{authBearerValue: "stream_frames"}
	err = readSSEFramesWithSanitizer(
		strings.NewReader(payload),
		func(HarnessEventFrame) error { return nil },
		client.sanitizeClientError,
	)
	if err == nil || strings.Contains(err.Error(), client.authBearerValue) {
		t.Fatalf("sanitized readSSEFrames() error = %v, want operation bearer redacted", err)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func validClientStartTurnRequest() StartTurnRequest {
	return StartTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     AuthIdentity{Subject: "user:test"},
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
