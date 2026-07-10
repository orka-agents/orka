/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpdatePlanTool_Name(t *testing.T) {
	tool := NewUpdatePlanTool()
	if got := tool.Name(); got != updatePlanToolName {
		t.Errorf("Name() = %q, want %q", got, updatePlanToolName)
	}
}

func TestNewUpdatePlanTool_DoesNotAssumeDefaultTransportType(t *testing.T) {
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	tool := NewUpdatePlanTool()
	if tool.client == nil || tool.client.Transport == nil {
		t.Fatal("NewUpdatePlanTool() did not configure an HTTP transport")
	}
}

func TestNewUpdatePlanTool_ConfiguresBoundedClientAndContext(t *testing.T) {
	tool := NewUpdatePlanTool()
	if tool.requestTimeout != updatePlanRequestTimeout {
		t.Fatalf("requestTimeout = %s, want %s", tool.requestTimeout, updatePlanRequestTimeout)
	}
	if tool.client == nil {
		t.Fatal("HTTP client is nil")
	}
	if tool.client.Timeout != 0 {
		t.Fatalf("HTTP client timeout = %s, want request context to own overall timeout", tool.client.Timeout)
	}

	transport, ok := tool.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("HTTP client transport = %T, want *http.Transport", tool.client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("transport DialContext is nil")
	}
	if transport.TLSHandshakeTimeout <= 0 {
		t.Fatalf("TLSHandshakeTimeout = %s, want positive timeout", transport.TLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != updatePlanResponseHeaderTimeout {
		t.Fatalf(
			"ResponseHeaderTimeout = %s, want %s",
			transport.ResponseHeaderTimeout,
			updatePlanResponseHeaderTimeout,
		)
	}
	if transport.IdleConnTimeout <= 0 {
		t.Fatalf("IdleConnTimeout = %s, want positive timeout", transport.IdleConnTimeout)
	}
}

func TestUpdatePlanTool_Description(t *testing.T) {
	tool := NewUpdatePlanTool()
	if got := tool.Description(); got == "" {
		t.Error("Description() should not be empty")
	}
}

func TestUpdatePlanTool_Parameters(t *testing.T) {
	tool := NewUpdatePlanTool()
	params := tool.Parameters()
	if len(params) == 0 {
		t.Fatal("Parameters() should not be empty")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() should be valid JSON: %v", err)
	}

	if schema[jsonSchemaTypeField] != jsonSchemaTypeObject {
		t.Errorf("schema type = %v, want object", schema[jsonSchemaTypeField])
	}

	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("schema should have properties")
	}

	for _, required := range []string{"summary", "progress_pct", "goal_complete", "plan_document"} {
		if _, ok := props[required]; !ok {
			t.Errorf("schema missing property %q", required)
		}
	}
}

func TestUpdatePlanTool_Execute(t *testing.T) {
	tool := NewUpdatePlanTool()

	tests := []struct {
		name       string
		args       string
		envURL     string
		envTask    string
		envNS      string
		envToken   string
		serverCode int
		wantErr    string
		wantResult string
		skipServer bool
	}{
		{
			name:    invalidJSONArgsCaseName,
			args:    invalidJSONText,
			wantErr: invalidArgumentsMessage,
		},
		{
			name:    "empty summary",
			args:    `{"summary":"","plan_document":"# Plan"}`,
			wantErr: "summary is required",
		},
		{
			name:    "empty plan_document",
			args:    `{"summary":"test","plan_document":""}`,
			wantErr: "plan_document is required",
		},
		{
			name:       "missing all env vars",
			args:       testPlanJSON,
			envURL:     "",
			envTask:    "",
			envNS:      "",
			wantErr:    missingControllerTaskEnvMessage,
			skipServer: true,
		},
		{
			name:       "missing ORKA_TASK_NAME",
			args:       testPlanJSON,
			envURL:     localhostURL,
			envTask:    "",
			envNS:      defaultNamespace,
			wantErr:    missingControllerTaskEnvMessage,
			skipServer: true,
		},
		{
			name:       "missing ORKA_TASK_NAMESPACE",
			args:       testPlanJSON,
			envURL:     localhostURL,
			envTask:    testMyTaskName,
			envNS:      "",
			wantErr:    missingControllerTaskEnvMessage,
			skipServer: true,
		},
		{
			name:       "successful update with 204",
			args:       `{"summary":"phase 1 done","progress_pct":50,"goal_complete":false,"plan_document":"# Plan\n## Done"}`,
			envTask:    testMyTaskName,
			envNS:      defaultNamespace,
			serverCode: http.StatusNoContent,
			wantResult: "Plan updated: phase 1 done (progress: 50%)",
		},
		{
			name:       "successful update with 200",
			args:       `{"summary":"all done","progress_pct":100,"goal_complete":true,"plan_document":"# Complete"}`,
			envTask:    testMyTaskName,
			envNS:      defaultNamespace,
			serverCode: http.StatusOK,
			wantResult: "Plan updated: all done (progress: 100%, goal marked as COMPLETE)",
		},
		{
			name:       "server error 500",
			args:       `{"summary":"test","progress_pct":10,"plan_document":"# Plan"}`,
			envTask:    testMyTaskName,
			envNS:      defaultNamespace,
			serverCode: http.StatusInternalServerError,
			wantErr:    "failed to save plan: HTTP 500",
		},
		{
			name:       "server error 403",
			args:       `{"summary":"test","progress_pct":0,"plan_document":"# Plan"}`,
			envTask:    testMyTaskName,
			envNS:      defaultNamespace,
			serverCode: http.StatusForbidden,
			wantErr:    "failed to save plan: HTTP 403",
		},
		{
			name:       "with SA token from env",
			args:       `{"summary":"with token","progress_pct":25,"plan_document":"# Plan"}`,
			envTask:    "task1",
			envNS:      "ns1",
			envToken:   "my-sa-token",
			serverCode: http.StatusNoContent,
			wantResult: "Plan updated: with token (progress: 25%)",
		},
		{
			name:       "zero progress not complete",
			args:       `{"summary":"starting","progress_pct":0,"goal_complete":false,"plan_document":"# Initial"}`,
			envTask:    "t",
			envNS:      "n",
			serverCode: http.StatusNoContent,
			wantResult: "Plan updated: starting (progress: 0%)",
		},
		{
			name:       "goal complete at partial progress",
			args:       `{"summary":"blocked","progress_pct":60,"goal_complete":true,"plan_document":"# Blocked"}`,
			envTask:    "t",
			envNS:      "n",
			serverCode: http.StatusOK,
			wantResult: "Plan updated: blocked (progress: 60%, goal marked as COMPLETE)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var serverURL string
			if !tc.skipServer && tc.wantErr != invalidArgumentsMessage && tc.wantErr != "summary is required" && tc.wantErr != "plan_document is required" {
				var receivedAuth string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					receivedAuth = r.Header.Get("Authorization")
					if r.Method != http.MethodPost {
						t.Errorf("expected POST, got %s", r.Method)
					}
					expectedPath := fmt.Sprintf("/internal/v1/plans/%s/%s", tc.envNS, tc.envTask)
					if r.URL.Path != expectedPath {
						t.Errorf("path = %q, want %q", r.URL.Path, expectedPath)
					}
					if ct := r.Header.Get("Content-Type"); ct != "application/json" {
						t.Errorf("Content-Type = %q, want application/json", ct)
					}
					w.WriteHeader(tc.serverCode)
				}))
				defer srv.Close()
				serverURL = srv.URL

				// Verify auth header after execution
				if tc.envToken != "" {
					defer func() {
						wantAuth := "Bearer " + tc.envToken
						if receivedAuth != wantAuth {
							t.Errorf("Authorization = %q, want %q", receivedAuth, wantAuth)
						}
					}()
				}
			}

			if serverURL != "" {
				t.Setenv(envOrkaControllerURL, serverURL)
			} else if tc.envURL != "" {
				t.Setenv(envOrkaControllerURL, tc.envURL)
			} else {
				t.Setenv(envOrkaControllerURL, "")
			}
			t.Setenv(envOrkaTaskName, tc.envTask)
			t.Setenv(envOrkaTaskNamespace, tc.envNS)
			t.Setenv("ORKA_SA_TOKEN", tc.envToken)

			result, err := tool.Execute(t.Context(), json.RawMessage(tc.args))

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want containing %q", err.Error(), tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tc.wantResult {
				t.Errorf("result = %q, want %q", result, tc.wantResult)
			}
		})
	}
}

func TestUpdatePlanTool_Execute_ConnectionRefused(t *testing.T) {
	tool := NewUpdatePlanTool()
	t.Setenv(envOrkaControllerURL, "http://127.0.0.1:1")
	t.Setenv(envOrkaTaskName, "task")
	t.Setenv(envOrkaTaskNamespace, "ns")
	t.Setenv("ORKA_SA_TOKEN", "")

	args := json.RawMessage(testPlanJSON)
	_, err := tool.Execute(t.Context(), args)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
	if !strings.Contains(err.Error(), "failed to save plan") {
		t.Errorf("error = %q, want containing 'failed to save plan'", err.Error())
	}
}

func TestUpdatePlanTool_Execute_NoAuthHeaderWhenNoToken(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tool := NewUpdatePlanTool()
	t.Setenv(envOrkaControllerURL, srv.URL)
	t.Setenv(envOrkaTaskName, "task")
	t.Setenv(envOrkaTaskNamespace, "ns")
	t.Setenv("ORKA_SA_TOKEN", "")

	args := json.RawMessage(testPlanJSON)
	_, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedAuth != "" {
		t.Errorf("expected no Authorization header, got %q", receivedAuth)
	}
}

func TestUpdatePlanTool_Execute_RequestBodyValid(t *testing.T) {
	var received updatePlanArgs
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("failed to decode body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tool := NewUpdatePlanTool()
	t.Setenv(envOrkaControllerURL, srv.URL)
	t.Setenv(envOrkaTaskName, "task")
	t.Setenv(envOrkaTaskNamespace, "ns")
	t.Setenv("ORKA_SA_TOKEN", "")

	args := json.RawMessage(`{"summary":"my summary","progress_pct":75,"goal_complete":true,"plan_document":"# My Plan"}`)
	_, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if received.Summary != "my summary" {
		t.Errorf("body summary = %q, want %q", received.Summary, "my summary")
	}
	if received.ProgressPct != 75 {
		t.Errorf("body progress_pct = %d, want 75", received.ProgressPct)
	}
	if !received.GoalComplete {
		t.Error("body goal_complete = false, want true")
	}
	if received.PlanDocument != "# My Plan" {
		t.Errorf("body plan_document = %q, want %q", received.PlanDocument, "# My Plan")
	}
}

func TestUpdatePlanTool_Execute_ResponseHeaderStallIsBounded(t *testing.T) {
	const requestTimeout = 25 * time.Millisecond

	requestStarted := make(chan struct{})
	tool := &UpdatePlanTool{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(requestStarted)
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
		requestTimeout: requestTimeout,
	}
	t.Setenv(envOrkaControllerURL, localhostURL)
	t.Setenv(envOrkaTaskName, "task")
	t.Setenv(envOrkaTaskNamespace, "ns")
	t.Setenv("ORKA_SA_TOKEN", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, json.RawMessage(testPlanJSON))
		done <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("update_plan request did not start")
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
			t.Fatalf("Execute() error = %v, want context deadline exceeded", err)
		}
	case <-time.After(10 * requestTimeout):
		cancel()
		<-done
		t.Fatalf("Execute() did not return within bounded request timeout %s", requestTimeout)
	}
}

func TestUpdatePlanTool_Execute_ResponseDrainHasTimeBound(t *testing.T) {
	for _, statusCode := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			body := newBlockingReadCloser()
			tool := &UpdatePlanTool{
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					go func() {
						<-req.Context().Done()
						_ = body.Close()
					}()
					return &http.Response{
						StatusCode: statusCode,
						Header:     make(http.Header),
						Body:       body,
					}, nil
				})},
				requestTimeout: time.Second,
			}
			t.Setenv(envOrkaControllerURL, localhostURL)
			t.Setenv(envOrkaTaskName, "task")
			t.Setenv(envOrkaTaskNamespace, "ns")
			t.Setenv("ORKA_SA_TOKEN", "")

			done := make(chan error, 1)
			go func() {
				_, err := tool.Execute(t.Context(), json.RawMessage(testPlanJSON))
				done <- err
			}()

			select {
			case <-body.readStarted:
			case <-time.After(time.Second):
				t.Fatal("response drain did not start")
			}

			select {
			case err := <-done:
				if statusCode == http.StatusOK && err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
				if statusCode != http.StatusOK && err == nil {
					t.Fatal("Execute() error = nil, want status error")
				}
			case <-time.After(500 * time.Millisecond):
				_ = body.Close()
				<-done
				t.Fatal("response drain did not return within a bounded interval")
			}
		})
	}
}

func TestUpdatePlanTool_Execute_BoundsIgnoredResponseDrain(t *testing.T) {
	const (
		responseBodyBytes     = 1 << 20
		maxExpectedDrainBytes = 64 << 10
	)

	for _, statusCode := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			body := &trackingReadCloser{reader: strings.NewReader(strings.Repeat("x", responseBodyBytes))}
			tool := &UpdatePlanTool{
				client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: statusCode,
						Header:     make(http.Header),
						Body:       body,
					}, nil
				})},
				requestTimeout: time.Second,
			}
			t.Setenv(envOrkaControllerURL, localhostURL)
			t.Setenv(envOrkaTaskName, "task")
			t.Setenv(envOrkaTaskNamespace, "ns")
			t.Setenv("ORKA_SA_TOKEN", "")

			_, err := tool.Execute(t.Context(), json.RawMessage(testPlanJSON))
			if statusCode == http.StatusOK && err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if statusCode != http.StatusOK && err == nil {
				t.Fatal("Execute() error = nil, want status error")
			}
			if !body.closed {
				t.Fatal("response body was not closed")
			}
			if body.bytesRead == 0 {
				t.Fatal("ignored response body was not drained")
			}
			if body.bytesRead > maxExpectedDrainBytes {
				t.Fatalf("ignored response drain read %d bytes, want at most %d", body.bytesRead, maxExpectedDrainBytes)
			}
		})
	}
}

type trackingReadCloser struct {
	reader    io.Reader
	bytesRead int
	closed    bool
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += n
	return n, err
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type blockingReadCloser struct {
	readStarted chan struct{}
	closed      chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.readStarted) })
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *blockingReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}
