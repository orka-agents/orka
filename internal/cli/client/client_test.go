/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import (
	"bytes"
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

const (
	testAgentName       = "agent1"
	testAgentOne        = "agent-1"
	testModelName       = "gpt-4"
	testSuccessName     = "success"
	testErrorName       = "error"
	testChatStreamName  = "chat SSE"
	testEventStreamName = "event SSE"
	testContinueKey     = "continue"
	testCursorA         = "cursor-a"
	testItemsKey        = "items"
	testMetadataKey     = "metadata"
	testNameKey         = "name"
	testNextContinue    = "next"
	testTaskOne         = "task-1"
)

// helper to create a test server that records requests and returns a fixed response.

func TestNew(t *testing.T) {
	c := New("http://localhost:8080", "tok123")
	if c.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, "http://localhost:8080")
	}
	if c.Token != "tok123" {
		t.Errorf("Token = %q, want %q", c.Token, "tok123")
	}
	if c.HTTPClient == nil {
		t.Error("HTTPClient should not be nil")
	}
}

func TestNew_DoesNotAssumeDefaultTransportType(t *testing.T) {
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	c := New("http://localhost:8080", "")
	if c.HTTPClient == nil || c.HTTPClient.Transport == nil {
		t.Fatal("New() did not configure an HTTP transport")
	}
}

func TestNew_ConfiguresTransportSafetyWithoutOverallTimeout(t *testing.T) {
	c := New("http://localhost:8080", "")
	if c.HTTPClient.Timeout != 0 {
		t.Fatalf("HTTPClient.Timeout = %s, want no overall timeout", c.HTTPClient.Timeout)
	}

	transport, ok := c.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("HTTPClient.Transport = %T, want *http.Transport", c.HTTPClient.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("transport DialContext is nil")
	}
	if transport.TLSHandshakeTimeout <= 0 {
		t.Fatalf("TLSHandshakeTimeout = %s, want positive timeout", transport.TLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != defaultResponseHeaderTimeout {
		t.Fatalf(
			"ResponseHeaderTimeout = %s, want %s",
			transport.ResponseHeaderTimeout,
			defaultResponseHeaderTimeout,
		)
	}
	if transport.IdleConnTimeout <= 0 {
		t.Fatalf("IdleConnTimeout = %s, want positive timeout", transport.IdleConnTimeout)
	}
}

func TestNewWithNamespace(t *testing.T) {
	c := NewWithNamespace("http://localhost:8080", "tok", "ns1")
	if c.Namespace != "ns1" {
		t.Errorf("Namespace = %q, want %q", c.Namespace, "ns1")
	}
}

func TestClient_NonStreamingResponseHeaderStallIsBounded(t *testing.T) {
	const responseHeaderTimeout = 25 * time.Millisecond

	requestStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newClient(srv.URL, "", "", responseHeaderTimeout)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.HealthCheck(ctx)
		done <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("health request did not start")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("HealthCheck() error = nil, want response-header timeout")
		}
		var timeoutErr interface{ Timeout() bool }
		if !errors.As(err, &timeoutErr) || !timeoutErr.Timeout() {
			t.Fatalf("HealthCheck() error = %v, want timeout error", err)
		}
	case <-time.After(10 * responseHeaderTimeout):
		cancel()
		<-done
		t.Fatalf("HealthCheck() did not return within response-header timeout %s", responseHeaderTimeout)
	}
}

func TestClient_BoundsIgnoredResponseDrain(t *testing.T) {
	const responseBodyBytes = 1 << 20

	tests := []struct {
		name                 string
		statusCode           int
		maxExpectedReadBytes int
		wantErr              bool
	}{
		{
			name:                 testSuccessName,
			statusCode:           http.StatusNoContent,
			maxExpectedReadBytes: 64 << 10,
		},
		{
			name:                 testErrorName,
			statusCode:           http.StatusInternalServerError,
			maxExpectedReadBytes: 128 << 10,
			wantErr:              true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &trackingReadCloser{reader: strings.NewReader(strings.Repeat("x", responseBodyBytes))}
			c := New("http://orka.test", "")
			c.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tt.statusCode,
					Header:     make(http.Header),
					Body:       body,
				}, nil
			})}

			err := c.DeleteTask(t.Context(), "task", GetOptions{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("DeleteTask() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !body.closed {
				t.Fatal("response body was not closed")
			}
			if body.bytesRead == 0 {
				t.Fatal("response body was not drained")
			}
			if body.bytesRead > tt.maxExpectedReadBytes {
				t.Fatalf("response handling read %d bytes, want at most %d", body.bytesRead, tt.maxExpectedReadBytes)
			}
		})
	}
}

func TestClient_ResponseDrainHasTimeBound(t *testing.T) {
	for _, statusCode := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			body := newBlockingReadCloser()
			c := New("http://orka.test", "")
			c.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				go func() {
					<-req.Context().Done()
					_ = body.Close()
				}()
				return &http.Response{
					StatusCode: statusCode,
					Header:     make(http.Header),
					Body:       body,
				}, nil
			})}

			done := make(chan error, 1)
			go func() {
				done <- c.DeleteTask(t.Context(), "task", GetOptions{})
			}()

			select {
			case <-body.readStarted:
			case <-time.After(time.Second):
				t.Fatal("response drain did not start")
			}

			select {
			case err := <-done:
				if statusCode < 400 && err != nil {
					t.Fatalf("DeleteTask() error = %v", err)
				}
				if statusCode >= 400 && err == nil {
					t.Fatal("DeleteTask() error = nil, want status error")
				}
			case <-time.After(500 * time.Millisecond):
				_ = body.Close()
				<-done
				t.Fatal("response drain did not return within a bounded interval")
			}
		})
	}
}

func TestClient_BoundsNonStreamingErrorResponseBody(t *testing.T) {
	const responseBodyBytes = 1 << 20

	body := &trackingReadCloser{reader: strings.NewReader(strings.Repeat("x", responseBodyBytes))}
	c := New("http://orka.test", "")
	c.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})}

	if _, err := c.HealthCheck(t.Context()); err == nil {
		t.Fatal("HealthCheck() error = nil, want status error")
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
	maxExpectedReadBytes := int(maxErrorResponseBodyBytes + maxResponseBodyDrainBytes)
	if body.bytesRead == 0 || body.bytesRead > maxExpectedReadBytes {
		t.Fatalf("response handling read %d bytes, want 1..%d", body.bytesRead, maxExpectedReadBytes)
	}
}

func TestHealthCheck(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    any
		wantOK  bool
		wantErr bool
	}{
		{
			name:   "healthy",
			status: http.StatusOK,
			body:   map[string]any{"status": "ok"},
			wantOK: true,
		},
		{
			name:   "not healthy",
			status: http.StatusOK,
			body:   map[string]any{"status": "degraded"},
			wantOK: false,
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			body:    "error",
			wantErr: true,
		},
		{
			name:   "invalid json returns false",
			status: http.StatusOK,
			body:   nil,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				if tt.body != nil {
					json.NewEncoder(w).Encode(tt.body) //nolint:errcheck
				}
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			ok, err := c.HealthCheck(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestReadyCheck(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    any
		wantOK  bool
		wantErr bool
	}{
		{
			name:   "ready",
			status: http.StatusOK,
			body:   map[string]any{"status": "ok"},
			wantOK: true,
		},
		{
			name:   "not ready",
			status: http.StatusOK,
			body:   map[string]any{"status": "not_ready"},
			wantOK: false,
		},
		{
			name:    "server error",
			status:  http.StatusServiceUnavailable,
			body:    "error",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				if tt.body != nil {
					json.NewEncoder(w).Encode(tt.body) //nolint:errcheck
				}
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			ok, err := c.ReadyCheck(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestCreateTask(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		resp    any
		wantErr bool
	}{
		{
			name:   "success",
			status: http.StatusCreated,
			resp: TaskDetail{
				"metadata": map[string]any{"name": "task1"},
				"spec":     map[string]any{"type": "ai"},
			},
		},
		{
			name:    "server error",
			status:  http.StatusBadRequest,
			resp:    map[string]string{"error": "bad request"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedMethod string
			var capturedPath string
			var capturedAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedMethod = r.Method
				capturedPath = r.URL.Path
				capturedAuth = r.Header.Get("Authorization")
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.resp) //nolint:errcheck
			}))
			defer srv.Close()

			c := New(srv.URL, "mytoken")
			result, err := c.CreateTask(context.Background(), CreateTaskRequest{
				Name:      "task1",
				Namespace: "default",
				Type:      "ai",
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if result == nil {
					t.Fatal("expected non-nil result")
				}
				if capturedMethod != http.MethodPost {
					t.Errorf("method = %q, want POST", capturedMethod)
				}
				if capturedPath != "/api/v1/tasks" {
					t.Errorf("path = %q, want /api/v1/tasks", capturedPath)
				}
				if capturedAuth != "Bearer mytoken" {
					t.Errorf("auth = %q, want Bearer mytoken", capturedAuth)
				}
			}
		})
	}
}

func TestCreateTask_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json")) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.CreateTask(context.Background(), CreateTaskRequest{Name: "t"})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error should mention decode: %v", err)
	}
}

func TestListTasks(t *testing.T) {
	tests := []struct {
		name    string
		opts    ListTasksOptions
		status  int
		resp    any
		wantLen int
		wantErr bool
	}{
		{
			name:   "success with items",
			opts:   ListTasksOptions{Namespace: "ns1", Limit: 10, Continue: "abc"},
			status: http.StatusOK,
			resp: taskListResponse{
				Items: []TaskDetail{
					{
						"metadata": map[string]any{"name": "t1", "namespace": "ns1", "creationTimestamp": "2024-01-01T00:00:00Z"},
						"spec": map[string]any{
							"type":        "ai",
							"transaction": map[string]any{"id": "txn-123"},
						},
						"status": map[string]any{"phase": "Running", "iteration": float64(2)},
					},
				},
			},
			wantLen: 1,
		},
		{
			name:    "empty list",
			status:  http.StatusOK,
			resp:    taskListResponse{Items: []TaskDetail{}},
			wantLen: 0,
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			resp:    "error",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedQuery = r.URL.RawQuery
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.resp) //nolint:errcheck
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			tasks, err := c.ListTasks(context.Background(), tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if len(tasks) != tt.wantLen {
					t.Errorf("len(tasks) = %d, want %d", len(tasks), tt.wantLen)
				}
				if tt.name == "success with items" && tasks[0].TransactionID != "txn-123" {
					t.Errorf("TransactionID = %q, want txn-123", tasks[0].TransactionID)
				}
				if tt.opts.Namespace != "" && !strings.Contains(capturedQuery, "namespace=ns1") {
					t.Errorf("query %q missing namespace param", capturedQuery)
				}
				if tt.opts.Limit > 0 && !strings.Contains(capturedQuery, "limit=10") {
					t.Errorf("query %q missing limit param", capturedQuery)
				}
				if tt.opts.Continue != "" && !strings.Contains(capturedQuery, "continue=abc") {
					t.Errorf("query %q missing continue param", capturedQuery)
				}
			}
		})
	}
}

func TestListTasksPageAllUsesLimitZero(t *testing.T) {
	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(taskListResponse{Items: []TaskDetail{}}) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.ListTasksPage(context.Background(), ListTasksOptions{Namespace: "ns1", All: true})
	if err != nil {
		t.Fatalf("ListTasksPage() error = %v", err)
	}
	if !strings.Contains(capturedQuery, "namespace=ns1") {
		t.Fatalf("query %q missing namespace", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "limit=0") {
		t.Fatalf("query %q missing limit=0", capturedQuery)
	}
}

func TestListTasksPageReturnsPaginationMetadata(t *testing.T) {
	remaining := int64(42)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Errorf("limit query = %q, want 10", got)
		}
		if got := r.URL.Query().Get("continue"); got != "abc" {
			t.Errorf("continue query = %q, want abc", got)
		}
		json.NewEncoder(w).Encode(taskListResponse{ //nolint:errcheck
			Items: []TaskDetail{
				{
					"metadata": map[string]any{"name": "t1", "namespace": "ns1"},
					"spec":     map[string]any{"type": "ai"},
					"status":   map[string]any{"phase": "Running"},
				},
			},
			Metadata: struct {
				Continue           string `json:"continue,omitempty"`
				RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
			}{
				Continue:           "next",
				RemainingItemCount: &remaining,
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	page, err := c.ListTasksPage(context.Background(), ListTasksOptions{Limit: 10, Continue: "abc"})
	if err != nil {
		t.Fatalf("ListTasksPage() error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(page.Items))
	}
	if page.Continue != "next" {
		t.Fatalf("Continue = %q, want next", page.Continue)
	}
	if page.RemainingItemCount == nil || *page.RemainingItemCount != remaining {
		t.Fatalf("RemainingItemCount = %v, want %d", page.RemainingItemCount, remaining)
	}
}

func TestListAllTasksReturnsAllPagesAndForwardsOpaqueContinuation(t *testing.T) {
	const continuation = "task-cursor+/=? segment"

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Errorf("limit query = %q, want 2", got)
		}
		switch requests {
		case 1:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				testItemsKey: []TaskDetail{
					{testMetadataKey: map[string]any{testNameKey: testTaskOne}},
					{testMetadataKey: map[string]any{testNameKey: "task-2"}},
				},
				testMetadataKey: map[string]any{testContinueKey: continuation},
			})
		case 2:
			if got := r.URL.Query().Get(testContinueKey); got != continuation {
				t.Errorf("continue query = %q, want %q", got, continuation)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				testItemsKey: []TaskDetail{
					{testMetadataKey: map[string]any{testNameKey: "task-3"}},
				},
				testMetadataKey: map[string]any{},
			})
		default:
			t.Errorf("unexpected request %d", requests)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	tasks, err := c.ListAllTasks(context.Background(), ListTasksOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListAllTasks() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(tasks) != 3 || tasks[2].Name != "task-3" {
		t.Fatalf("tasks = %#v, want three tasks through task-3", tasks)
	}
}

func TestListTasksRemainsSinglePage(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			testItemsKey: []TaskDetail{
				{testMetadataKey: map[string]any{testNameKey: testTaskOne}},
			},
			testMetadataKey: map[string]any{testContinueKey: testNextContinue},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	tasks, err := c.ListTasks(context.Background(), ListTasksOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListTasks() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one page", requests)
	}
	if len(tasks) != 1 || tasks[0].Name != testTaskOne {
		t.Fatalf("tasks = %#v, want first page only", tasks)
	}
}

func TestGetTask(t *testing.T) {
	tests := []struct {
		name    string
		task    string
		opts    GetOptions
		status  int
		resp    any
		wantErr bool
	}{
		{
			name:   "success",
			task:   "my-task",
			opts:   GetOptions{Namespace: "ns1"},
			status: http.StatusOK,
			resp:   TaskDetail{"metadata": map[string]any{"name": "my-task"}},
		},
		{
			name:    "not found",
			task:    "missing",
			status:  http.StatusNotFound,
			resp:    "not found",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedPath = r.URL.Path
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.resp) //nolint:errcheck
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			result, err := c.GetTask(context.Background(), tt.task, tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if result == nil {
					t.Fatal("expected non-nil result")
				}
				if capturedPath != "/api/v1/tasks/"+tt.task {
					t.Errorf("path = %q, want /api/v1/tasks/%s", capturedPath, tt.task)
				}
			}
		})
	}
}

func TestDeleteTask(t *testing.T) {
	tests := []struct {
		name    string
		task    string
		opts    GetOptions
		status  int
		wantErr bool
	}{
		{
			name:   "success",
			task:   "my-task",
			opts:   GetOptions{Namespace: "ns1"},
			status: http.StatusOK,
		},
		{
			name:    "not found",
			task:    "missing",
			status:  http.StatusNotFound,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedMethod = r.Method
				w.WriteHeader(tt.status)
				if tt.status >= 400 {
					w.Write([]byte("error body")) //nolint:errcheck
				}
			}))
			defer srv.Close()

			c := New(srv.URL, "tok")
			err := c.DeleteTask(context.Background(), tt.task, tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if capturedMethod != http.MethodDelete {
				t.Errorf("method = %q, want DELETE", capturedMethod)
			}
		})
	}
}

func TestGetTaskLogs(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		resp    any
		wantErr bool
	}{
		{
			name:   "success",
			status: http.StatusOK,
			resp:   TaskLogsResponse{Logs: "line1\nline2", JobName: "job-1"},
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			resp:    "error",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.resp) //nolint:errcheck
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			result, err := c.GetTaskLogs(context.Background(), "task1", GetOptions{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && result.Logs != "line1\nline2" {
				t.Errorf("logs = %q, want %q", result.Logs, "line1\nline2")
			}
		})
	}
}

func TestGetTaskResult(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		resp    any
		wantErr bool
	}{
		{
			name:   "success",
			status: http.StatusOK,
			resp:   TaskResultResponse{Result: "done!"},
		},
		{
			name:    "server error",
			status:  http.StatusNotFound,
			resp:    "not found",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.resp) //nolint:errcheck
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			result, err := c.GetTaskResult(context.Background(), "t1", GetOptions{Namespace: "ns1"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && result.Result != "done!" {
				t.Errorf("result = %q, want %q", result.Result, "done!")
			}
		})
	}
}

func TestListAgents(t *testing.T) {
	tests := []struct {
		name    string
		opts    ListOptions
		status  int
		resp    any
		wantLen int
		wantErr bool
	}{
		{
			name:   "success",
			opts:   ListOptions{Namespace: "ns1"},
			status: http.StatusOK,
			resp: agentListResponse{
				Items: []AgentDetail{
					{
						"metadata": map[string]any{"name": testAgentName},
						"spec": map[string]any{
							"model":   map[string]any{"name": testModelName},
							"runtime": map[string]any{"type": "container"},
						},
						"status": map[string]any{"activeTasks": float64(3)},
					},
				},
			},
			wantLen: 1,
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			resp:    "error",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.resp) //nolint:errcheck
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			agents, err := c.ListAgents(context.Background(), tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if len(agents) != tt.wantLen {
					t.Fatalf("len(agents) = %d, want %d", len(agents), tt.wantLen)
				}
				if tt.wantLen > 0 {
					if agents[0].Name != testAgentName {
						t.Errorf("name = %q, want agent1", agents[0].Name)
					}
					if agents[0].Model != testModelName {
						t.Errorf("model = %q, want gpt-4", agents[0].Model)
					}
					if agents[0].Runtime != "container" {
						t.Errorf("runtime = %q, want container", agents[0].Runtime)
					}
					if agents[0].Active != 3 {
						t.Errorf("active = %d, want 3", agents[0].Active)
					}
				}
			}
		})
	}
}

func TestListAgentsEmptyResultIsAnEmptySlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			testItemsKey:    []AgentDetail{},
			testMetadataKey: map[string]any{},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	agents, err := c.ListAgents(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("ListAgents() error = %v", err)
	}
	if agents == nil {
		t.Fatal("ListAgents() returned nil, want an empty slice")
	}
	if len(agents) != 0 {
		t.Fatalf("len(agents) = %d, want 0", len(agents))
	}
}

func TestListAgentsReturnsAllPagesAndForwardsOpaqueContinuation(t *testing.T) {
	const continuation = "agent-cursor+/=? segment"

	firstPage := make([]AgentDetail, 100)
	for i := range firstPage {
		firstPage[i] = AgentDetail{
			testMetadataKey: map[string]any{testNameKey: fmt.Sprintf("agent-%03d", i+1)},
		}
	}

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			if got := r.URL.Query().Get(testContinueKey); got != "" {
				t.Errorf("first continue query = %q, want empty", got)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				testItemsKey: firstPage,
				testMetadataKey: map[string]any{
					testContinueKey: continuation,
				},
			})
		case 2:
			if got := r.URL.Query().Get(testContinueKey); got != continuation {
				t.Errorf("second continue query = %q, want %q", got, continuation)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				testItemsKey: []AgentDetail{
					{testMetadataKey: map[string]any{testNameKey: "agent-101"}},
				},
				testMetadataKey: map[string]any{},
			})
		default:
			t.Errorf("unexpected request %d", requests)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	agents, err := c.ListAgents(context.Background(), ListOptions{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("ListAgents() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(agents) != 101 {
		t.Fatalf("len(agents) = %d, want 101", len(agents))
	}
	if got := agents[100].Name; got != "agent-101" {
		t.Fatalf("agents[100].Name = %q, want agent-101", got)
	}
}

func TestListSkillsReturnsAllPagesAndForwardsOpaqueContinuation(t *testing.T) {
	const (
		continuation = "skill-cursor+/=? segment"
		namespace    = "ns1"
	)

	firstPage := make([]SkillSummary, 100)
	for i := range firstPage {
		firstPage[i] = SkillSummary{Name: fmt.Sprintf("skill-%03d", i+1)}
	}

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("namespace"); got != namespace {
			t.Errorf("namespace query = %q, want %q", got, namespace)
		}
		switch requests {
		case 1:
			if got := r.URL.Query().Get(testContinueKey); got != "" {
				t.Errorf("first continue query = %q, want empty", got)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				testItemsKey: firstPage,
				testMetadataKey: map[string]any{
					testContinueKey: continuation,
				},
			})
		case 2:
			if got := r.URL.Query().Get(testContinueKey); got != continuation {
				t.Errorf("second continue query = %q, want %q", got, continuation)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				testItemsKey:    []SkillSummary{{Name: "skill-101"}},
				testMetadataKey: map[string]any{},
			})
		default:
			t.Errorf("unexpected request %d", requests)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	skills, err := c.ListSkills(context.Background(), ListOptions{Namespace: namespace})
	if err != nil {
		t.Fatalf("ListSkills() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(skills) != 101 {
		t.Fatalf("len(skills) = %d, want 101", len(skills))
	}
	if got := skills[100].Name; got != "skill-101" {
		t.Fatalf("skills[100].Name = %q, want skill-101", got)
	}
}

func TestListAgentsStopsOnContinuationCycleWithPartialResults(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var next string
		switch requests {
		case 1:
			next = testCursorA
		case 2:
			if got := r.URL.Query().Get(testContinueKey); got != testCursorA {
				t.Errorf("second continue query = %q, want cursor-a", got)
			}
			next = "cursor-b"
		case 3:
			if got := r.URL.Query().Get(testContinueKey); got != "cursor-b" {
				t.Errorf("third continue query = %q, want cursor-b", got)
			}
			next = testCursorA
		default:
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			testItemsKey: []AgentDetail{
				{testMetadataKey: map[string]any{testNameKey: fmt.Sprintf("agent-%d", requests)}},
			},
			testMetadataKey: map[string]any{testContinueKey: next},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	agents, err := c.ListAgents(context.Background(), ListOptions{})
	if err == nil {
		t.Fatal("ListAgents() error = nil, want continuation cycle error")
	}
	if !strings.Contains(err.Error(), "continuation cycle") {
		t.Fatalf("ListAgents() error = %q, want continuation cycle", err)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
	if len(agents) != 3 {
		t.Fatalf("len(agents) = %d, want 3 partial results", len(agents))
	}
}

func TestListAgentsStopsWhenContinuationDoesNotAdvance(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 2 && r.URL.Query().Get(testContinueKey) != "stuck" {
			t.Errorf("continue query = %q, want stuck", r.URL.Query().Get(testContinueKey))
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			testItemsKey: []AgentDetail{
				{testMetadataKey: map[string]any{testNameKey: fmt.Sprintf("agent-%d", requests)}},
			},
			testMetadataKey: map[string]any{testContinueKey: "stuck"},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	agents, err := c.ListAgents(context.Background(), ListOptions{})
	if err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("ListAgents() error = %v, want non-progress error", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(agents) != 2 {
		t.Fatalf("len(agents) = %d, want 2 partial results", len(agents))
	}
}

func TestListAgentsErrorsWhenMetadataClaimsUnreachableRemainingItems(t *testing.T) {
	remaining := int64(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(agentListResponse{ //nolint:errcheck
			Items: []AgentDetail{
				{testMetadataKey: map[string]any{testNameKey: testAgentOne}},
			},
			Metadata: struct {
				Continue           string `json:"continue,omitempty"`
				RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
			}{
				RemainingItemCount: &remaining,
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	agents, err := c.ListAgents(context.Background(), ListOptions{})
	if err == nil || !strings.Contains(err.Error(), "remaining items") {
		t.Fatalf("ListAgents() error = %v, want unreachable remaining items error", err)
	}
	if len(agents) != 1 || agents[0].Name != testAgentOne {
		t.Fatalf("agents = %#v, want first-page partial result", agents)
	}
}

func TestListAgentsStopsAtPaginationPageLimitWithPartialResults(t *testing.T) {
	const maxRequests = maxAutoPaginationPages

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests > maxRequests {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			testItemsKey: []AgentDetail{
				{testMetadataKey: map[string]any{testNameKey: fmt.Sprintf("agent-%d", requests)}},
			},
			testMetadataKey: map[string]any{
				testContinueKey: fmt.Sprintf("cursor-%d", requests),
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	agents, err := c.ListAgents(context.Background(), ListOptions{})
	if err == nil {
		t.Fatal("ListAgents() error = nil, want page limit error")
	}
	if !strings.Contains(err.Error(), "page limit") {
		t.Fatalf("ListAgents() error = %q, want page limit", err)
	}
	if requests != maxRequests {
		t.Fatalf("requests = %d, want %d", requests, maxRequests)
	}
	if len(agents) != maxRequests {
		t.Fatalf("len(agents) = %d, want %d partial results", len(agents), maxRequests)
	}
}

func TestListAgentsReturnsPartialResultsWhenLaterPageFails(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				testItemsKey: []AgentDetail{
					{testMetadataKey: map[string]any{testNameKey: testAgentOne}},
				},
				testMetadataKey: map[string]any{testContinueKey: testNextContinue},
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "later page failed") //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	agents, err := c.ListAgents(context.Background(), ListOptions{})
	if err == nil {
		t.Fatal("ListAgents() error = nil, want later-page error")
	}
	if !strings.Contains(err.Error(), "after 1 items") {
		t.Fatalf("ListAgents() error = %q, want partial result count", err)
	}
	if len(agents) != 1 || agents[0].Name != testAgentOne {
		t.Fatalf("agents = %#v, want one partial result", agents)
	}
}

func TestListAgentsHonorsContextCancellationBetweenPages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var requests int
	c := New("http://orka.test", "")
	c.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		body := `{"items":[{"metadata":{"name":"agent-1"}}],"metadata":{"continue":"next"}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: &cancelOnCloseReadCloser{
				ReadCloser: io.NopCloser(strings.NewReader(body)),
				cancel:     cancel,
			},
		}, nil
	})}

	agents, err := c.ListAgents(ctx, ListOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListAgents() error = %v, want context canceled", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if len(agents) != 1 || agents[0].Name != testAgentOne {
		t.Fatalf("agents = %#v, want first-page partial result", agents)
	}
}

func TestGetAgent(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		opts    GetOptions
		status  int
		resp    any
		wantErr bool
	}{
		{
			name:   "success",
			agent:  testAgentName,
			opts:   GetOptions{Namespace: "ns1"},
			status: http.StatusOK,
			resp:   AgentDetail{"metadata": map[string]any{"name": testAgentName}},
		},
		{
			name:    "not found",
			agent:   "missing",
			status:  http.StatusNotFound,
			resp:    "not found",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedPath = r.URL.Path
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.resp) //nolint:errcheck
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			result, err := c.GetAgent(context.Background(), tt.agent, tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if result == nil {
					t.Fatal("expected non-nil result")
				}
				if capturedPath != "/api/v1/agents/"+tt.agent {
					t.Errorf("path = %q, want /api/v1/agents/%s", capturedPath, tt.agent)
				}
			}
		})
	}
}

func TestDeleteAgent(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		opts    GetOptions
		status  int
		wantErr bool
	}{
		{
			name:   "success",
			agent:  testAgentName,
			opts:   GetOptions{Namespace: "ns1"},
			status: http.StatusOK,
		},
		{
			name:    "not found",
			agent:   "missing",
			status:  http.StatusNotFound,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedMethod = r.Method
				w.WriteHeader(tt.status)
				if tt.status >= 400 {
					w.Write([]byte("error")) //nolint:errcheck
				}
			}))
			defer srv.Close()

			c := New(srv.URL, "tok")
			err := c.DeleteAgent(context.Background(), tt.agent, tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if capturedMethod != http.MethodDelete {
				t.Errorf("method = %q, want DELETE", capturedMethod)
			}
		})
	}
}

func TestStreamChat(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    bool
		errContain string
	}{
		{
			name:   "success",
			status: http.StatusOK,
			body:   "event: message\ndata: {\"content\":\"hello\"}\n\n",
		},
		{
			name:       "unauthorized",
			status:     http.StatusUnauthorized,
			body:       "",
			wantErr:    true,
			errContain: "authentication failed",
		},
		{
			name:       "server error",
			status:     http.StatusInternalServerError,
			body:       "internal error",
			wantErr:    true,
			errContain: "server error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedContentType string
			var capturedAccept string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedContentType = r.Header.Get("Content-Type")
				capturedAccept = r.Header.Get("Accept")
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body)) //nolint:errcheck
			}))
			defer srv.Close()

			c := NewWithNamespace(srv.URL, "tok", "default")
			reader, resp, err := c.StreamChat(context.Background(), ChatRequest{Message: "hi"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContain)
				}
				return
			}
			if reader == nil {
				t.Fatal("expected non-nil reader")
			}
			if resp != nil {
				resp.Body.Close() //nolint:errcheck
			}
			if capturedContentType != "application/json" {
				t.Errorf("content-type = %q, want application/json", capturedContentType)
			}
			if capturedAccept != "text/event-stream" {
				t.Errorf("accept = %q, want text/event-stream", capturedAccept)
			}
		})
	}
}

func TestClient_StreamErrorCancelsDerivedContext(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{
			name: testChatStreamName,
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.StreamChat(ctx, ChatRequest{Message: "hi"})
				return err
			},
		},
		{
			name: testEventStreamName,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Stream(ctx, "/api/v1/events", nil)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestDone := make(chan struct{})
			c := New("http://orka.test", "")
			c.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				go func() {
					<-req.Context().Done()
					close(requestDone)
				}()
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("error")),
				}, nil
			})}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := tt.call(ctx, c); err == nil {
				t.Fatal("stream call error = nil, want status error")
			}

			select {
			case <-requestDone:
			case <-time.After(250 * time.Millisecond):
				cancel()
				<-requestDone
				t.Fatal("stream error did not cancel its derived request context")
			}
		})
	}
}

func TestClient_StreamErrorBodyReadHasTimeBound(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{
			name: testChatStreamName,
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.StreamChat(ctx, ChatRequest{Message: "hi"})
				return err
			},
		},
		{
			name: testEventStreamName,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Stream(ctx, "/api/v1/events", nil)
				return err
			},
		},
		{
			name: "task logs",
			call: func(ctx context.Context, c *Client) error {
				return c.StreamTaskLogs(ctx, "task", StreamLogsOptions{})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := newBlockingReadCloser()
			c := New("http://orka.test", "")
			c.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				go func() {
					<-req.Context().Done()
					_ = body.Close()
				}()
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Header:     make(http.Header),
					Body:       body,
				}, nil
			})}

			done := make(chan error, 1)
			go func() { done <- tt.call(t.Context(), c) }()

			select {
			case <-body.readStarted:
			case <-time.After(time.Second):
				t.Fatal("stream error body read did not start")
			}

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("stream call error = nil, want status error")
				}
			case <-time.After(500 * time.Millisecond):
				_ = body.Close()
				<-done
				t.Fatal("stream error body read did not return within a bounded interval")
			}
		})
	}
}

func TestClient_LongStreamsRemainUsablePastResponseHeaderTimeout(t *testing.T) {
	const (
		responseHeaderTimeout = 100 * time.Millisecond
		bodyDelay             = 250 * time.Millisecond
	)

	tests := []struct {
		name         string
		responseBody string
		want         string
		readStream   func(context.Context, *Client) (string, error)
	}{
		{
			name:         "chat SSE",
			responseBody: "event: message\ndata: {\"content\":\"still streaming\"}\n\n",
			want:         `{"content":"still streaming"}`,
			readStream: func(ctx context.Context, c *Client) (string, error) {
				reader, resp, err := c.StreamChat(ctx, ChatRequest{Message: "hi"})
				if err != nil {
					return "", err
				}
				defer resp.Body.Close() //nolint:errcheck
				event, ok := reader.Next()
				if !ok {
					if err := reader.Err(); err != nil {
						return "", fmt.Errorf("read delayed event: %w", err)
					}
					return "", errors.New("stream ended before delayed event")
				}
				return event.Data, nil
			},
		},
		{
			name:         "event SSE",
			responseBody: "data: delayed event\n\n",
			want:         "data: delayed event\n\n",
			readStream: func(ctx context.Context, c *Client) (string, error) {
				body, err := c.Stream(ctx, "/api/v1/events", nil)
				if err != nil {
					return "", err
				}
				defer body.Close() //nolint:errcheck
				data, err := io.ReadAll(body)
				return string(data), err
			},
		},
		{
			name:         "task logs",
			responseBody: "data: delayed log\n",
			want:         "delayed log\n",
			readStream: func(ctx context.Context, c *Client) (string, error) {
				var output bytes.Buffer
				err := c.StreamTaskLogs(ctx, "task", StreamLogsOptions{Writer: &output})
				return output.String(), err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				time.Sleep(bodyDelay)
				fmt.Fprint(w, tt.responseBody) //nolint:errcheck
			}))
			defer srv.Close()

			c := newClient(srv.URL, "", "", responseHeaderTimeout)
			got, err := tt.readStream(t.Context(), c)
			if err != nil {
				t.Fatalf("stream error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("stream output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStreamChat_NamespaceFromClient(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWithNamespace(srv.URL, "", "client-ns")
	reader, resp, err := c.StreamChat(context.Background(), ChatRequest{Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if reader != nil && resp != nil {
		resp.Body.Close() //nolint:errcheck
	}

	var req ChatRequest
	json.Unmarshal(capturedBody, &req) //nolint:errcheck
	if req.Namespace != "client-ns" {
		t.Errorf("namespace = %q, want client-ns", req.Namespace)
	}
}

func TestGetChatConfig(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		resp    any
		wantErr bool
	}{
		{
			name:   "success",
			status: http.StatusOK,
			resp: ChatConfigResponse{
				Enabled:        true,
				Provider:       "openai",
				Model:          testModelName,
				AvailableTools: []string{"code_exec", "web_search"},
			},
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			resp:    "error",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				json.NewEncoder(w).Encode(tt.resp) //nolint:errcheck
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			cfg, err := c.GetChatConfig(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if !cfg.Enabled {
					t.Error("expected enabled=true")
				}
				if cfg.Model != testModelName {
					t.Errorf("model = %q, want gpt-4", cfg.Model)
				}
			}
		})
	}
}

func TestStreamTaskLogs(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantOut string
		wantErr bool
	}{
		{
			name:    "success",
			status:  http.StatusOK,
			body:    "data: line1\ndata: line2\n",
			wantOut: "line1\nline2\n",
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			body:    "error",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body)) //nolint:errcheck
			}))
			defer srv.Close()

			var buf bytes.Buffer
			c := New(srv.URL, "tok")
			err := c.StreamTaskLogs(context.Background(), "t1", StreamLogsOptions{
				Namespace: "ns1",
				Writer:    &buf,
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && buf.String() != tt.wantOut {
				t.Errorf("output = %q, want %q", buf.String(), tt.wantOut)
			}
		})
	}
}

func TestStreamTaskLogs_NilWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: line\n")) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	err := c.StreamTaskLogs(context.Background(), "t1", StreamLogsOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoGet_AuthHeader(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}")) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "secret-token")
	_, err := c.doGet(context.Background(), srv.URL+"/test")
	if err != nil {
		t.Fatal(err)
	}
	if capturedAuth != "Bearer secret-token" {
		t.Errorf("auth = %q, want Bearer secret-token", capturedAuth)
	}
}

func TestDoGet_NoAuthWhenEmpty(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}")) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.doGet(context.Background(), srv.URL+"/test")
	if err != nil {
		t.Fatal(err)
	}
	if capturedAuth != "" {
		t.Errorf("auth = %q, want empty", capturedAuth)
	}
}

func TestStringField(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]any
		keys []string
		want string
	}{
		{
			name: "nested value",
			m:    map[string]any{"metadata": map[string]any{"name": "test"}},
			keys: []string{"metadata", "name"},
			want: "test",
		},
		{
			name: "top-level value",
			m:    map[string]any{"name": "test"},
			keys: []string{"name"},
			want: "test",
		},
		{
			name: "missing key",
			m:    map[string]any{"other": "val"},
			keys: []string{"metadata", "name"},
			want: "",
		},
		{
			name: "non-string value",
			m:    map[string]any{"count": 42},
			keys: []string{"count"},
			want: "",
		},
		{
			name: "empty keys",
			m:    map[string]any{"a": "b"},
			keys: []string{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StringField(tt.m, tt.keys...)
			if got != tt.want {
				t.Errorf("StringField() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractTaskSummary(t *testing.T) {
	item := TaskDetail{
		"metadata": map[string]any{
			"name":              "task1",
			"namespace":         "ns1",
			"creationTimestamp": "2024-01-01T00:00:00Z",
		},
		"spec":   map[string]any{"type": "ai"},
		"status": map[string]any{"phase": "Succeeded", "iteration": float64(5)},
	}

	s := extractTaskSummary(item)
	if s.Name != "task1" {
		t.Errorf("name = %q, want task1", s.Name)
	}
	if s.Namespace != "ns1" {
		t.Errorf("namespace = %q, want ns1", s.Namespace)
	}
	if s.Type != "ai" {
		t.Errorf("type = %q, want ai", s.Type)
	}
	if s.Phase != "Succeeded" {
		t.Errorf("phase = %q, want Succeeded", s.Phase)
	}
	if s.Iteration != 5 {
		t.Errorf("iteration = %d, want 5", s.Iteration)
	}
}

func TestExtractAgentSummary(t *testing.T) {
	item := AgentDetail{
		"metadata": map[string]any{"name": testAgentName},
		"spec": map[string]any{
			"model":   map[string]any{"name": testModelName},
			"runtime": map[string]any{"type": "container"},
		},
		"status": map[string]any{"activeTasks": float64(2)},
	}

	s := extractAgentSummary(item)
	if s.Name != testAgentName {
		t.Errorf("name = %q, want agent1", s.Name)
	}
	if s.Model != testModelName {
		t.Errorf("model = %q, want gpt-4", s.Model)
	}
	if s.Runtime != "container" {
		t.Errorf("runtime = %q, want container", s.Runtime)
	}
	if s.Active != 2 {
		t.Errorf("active = %d, want 2", s.Active)
	}
}

func TestExtractAgentSummary_MissingFields(t *testing.T) {
	item := AgentDetail{"metadata": map[string]any{"name": testAgentName}}
	s := extractAgentSummary(item)
	if s.Name != testAgentName {
		t.Errorf("name = %q, want agent1", s.Name)
	}
	if s.Model != "" {
		t.Errorf("model = %q, want empty", s.Model)
	}
	if s.Active != 0 {
		t.Errorf("active = %d, want 0", s.Active)
	}
}

func TestListTasks_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json")) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.ListTasks(context.Background(), ListTasksOptions{})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestGetTask_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json")) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.GetTask(context.Background(), "t1", GetOptions{})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestListAgents_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json")) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.ListAgents(context.Background(), ListOptions{})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestGetAgent_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json")) //nolint:errcheck
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.GetAgent(context.Background(), "a1", GetOptions{})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestDoJSONAndTxnToken(t *testing.T) {
	var gotTxn string
	var gotNamespace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTxn = r.Header.Get("Txn-Token")
		gotNamespace = r.URL.Query().Get("namespace")
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewWithNamespace(srv.URL, "bearer", "team-a")
	c.TxnToken = "txn-secret"
	result, err := c.DoJSON(context.Background(), http.MethodPost, "/api/v1/example", nil, []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("DoJSON error: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok || m["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
	if gotTxn != "txn-secret" {
		t.Fatalf("Txn-Token = %q, want txn-secret", gotTxn)
	}
	if gotNamespace != "team-a" {
		t.Fatalf("namespace query = %q, want team-a", gotNamespace)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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
