package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestClientOmitsBearerFromHealthAndCapabilities(t *testing.T) {
	var discoveryAuthHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case HealthPath:
			discoveryAuthHeaders = append(discoveryAuthHeaders, r.Header.Get("Authorization"))
			WriteJSON(w, http.StatusOK, HealthResponse{
				Version:   ProtocolVersion,
				Status:    HealthStatusOK,
				Ready:     true,
				CheckedAt: time.Now().UTC(),
			})
		case CapabilitiesPath:
			discoveryAuthHeaders = append(discoveryAuthHeaders, r.Header.Get("Authorization"))
			WriteJSON(w, http.StatusOK, CapabilitiesResponse{
				Version:                 ProtocolVersion,
				ProtocolVersion:         ProtocolVersion,
				Transport:               HTTPTransport,
				RuntimeName:             "runtime-a",
				ProviderKind:            ProviderKindRemote,
				ToolExecutionModes:      []ToolExecutionMode{ToolExecutionModeObserved},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
			})
		case TurnsPath:
			if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Fatalf("StartTurn Authorization = %q, want bearer token", got)
			}
			var request StartTurnRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode StartTurn request: %v", err)
			}
			eventsPath, _ := EventStreamPath(request.TurnID)
			WriteJSON(w, http.StatusAccepted, StartTurnResponse{
				Version:          ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: request.RuntimeSessionID,
				TurnID:           request.TurnID,
				CorrelationID:    request.CorrelationID,
				EventStreamPath:  eventsPath,
			})
		default:
			WriteError(w, http.StatusNotFound, "not found")
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, WithBearerToken("secret-token"), WithPublicDiscovery())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if _, err := client.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if _, err := client.StartTurn(context.Background(), validClientStartTurnRequest()); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	if len(discoveryAuthHeaders) != 2 || discoveryAuthHeaders[0] != "" || discoveryAuthHeaders[1] != "" {
		t.Fatalf("discovery Authorization headers = %#v, want both empty", discoveryAuthHeaders)
	}
}

func TestClientKeepsDiscoveryAuthByDefault(t *testing.T) {
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		WriteJSON(w, http.StatusOK, HealthResponse{
			Version:   ProtocolVersion,
			Status:    HealthStatusOK,
			Ready:     true,
			CheckedAt: time.Now().UTC(),
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, WithBearerToken("secret-token"))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if authHeader != "Bearer secret-token" {
		t.Fatalf("Health Authorization = %q, want default bearer behavior", authHeader)
	}
}

func TestAgentRuntimeHTTPClientRejectsRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		WriteError(w, http.StatusInternalServerError, "redirect should not be followed")
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client, err := NewClient(
		redirector.URL,
		WithHTTPClient(NewAgentRuntimeHTTPClient(true)),
		WithBearerToken("secret-token"),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.StartTurn(context.Background(), validClientStartTurnRequest()); err == nil || !strings.Contains(err.Error(), "redirects are not allowed") {
		t.Fatalf("StartTurn() error = %v, want redirect rejection", err)
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
}

func TestAgentRuntimeHTTPClientDisablesProxyForInsecureHTTP(t *testing.T) {
	insecureClient := NewAgentRuntimeHTTPClient(true)
	insecureTransport, ok := insecureClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("insecure transport = %T, want *http.Transport", insecureClient.Transport)
	}
	if insecureTransport.Proxy != nil {
		t.Fatal("insecure AgentRuntime transport retained proxy configuration")
	}

	tlsClient := NewAgentRuntimeHTTPClient(false)
	tlsTransport, ok := tlsClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("TLS transport = %T, want *http.Transport", tlsClient.Transport)
	}
	if tlsTransport.Proxy == nil {
		t.Fatal("TLS AgentRuntime transport unexpectedly disabled proxies")
	}
	if NewAgentRuntimeHTTPClient(true).Transport != insecureClient.Transport {
		t.Fatal("insecure AgentRuntime clients do not reuse the shared transport")
	}
	if NewAgentRuntimeHTTPClient(false).Transport != tlsClient.Transport {
		t.Fatal("TLS AgentRuntime clients do not reuse the shared transport")
	}
}

func TestRootAgentRuntimeDialAddressRootsDNSOnly(t *testing.T) {
	got, err := rootAgentRuntimeDialAddress("runtime.default.svc.corp.internal:8080")
	if err != nil {
		t.Fatalf("rootAgentRuntimeDialAddress(DNS) error = %v", err)
	}
	if got != "runtime.default.svc.corp.internal.:8080" {
		t.Fatalf("rooted DNS address = %q", got)
	}
	got, err = rootAgentRuntimeDialAddress("127.0.0.1:8080")
	if err != nil {
		t.Fatalf("rootAgentRuntimeDialAddress(IP) error = %v", err)
	}
	if got != "127.0.0.1:8080" {
		t.Fatalf("rooted IP address = %q, want unchanged", got)
	}
}

func TestPinnedAgentRuntimeHTTPClientDialsPodIPAndPreservesServiceHost(t *testing.T) {
	var requestHost string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHost = r.Host
		WriteJSON(w, http.StatusOK, HealthResponse{
			Version:   ProtocolVersion,
			Status:    HealthStatusOK,
			Ready:     true,
			CheckedAt: time.Now().UTC(),
		})
	}))
	defer server.Close()

	httpClient, err := NewPinnedAgentRuntimeHTTPClient(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("NewPinnedAgentRuntimeHTTPClient() error = %v", err)
	}
	client, err := NewClient(
		"http://runtime.default.svc.cluster.local",
		WithHTTPClient(httpClient),
		WithPublicDiscovery(),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if requestHost != "runtime.default.svc.cluster.local" {
		t.Fatalf("request Host = %q, want Service authority", requestHost)
	}
	if _, err := NewPinnedAgentRuntimeHTTPClient("runtime.default.svc.cluster.local:8080"); err == nil {
		t.Fatal("NewPinnedAgentRuntimeHTTPClient(DNS) error = nil, want IP-literal rejection")
	}
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
