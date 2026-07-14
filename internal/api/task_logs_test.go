/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestHandlers_GetTaskLogsFollowStreamsLargeLines(t *testing.T) {
	line := strings.Repeat("x", 128*1024)
	h, app, _ := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, line+"\n")
	})
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	resp, err := app.Test(
		httptest.NewRequest(http.MethodGet, "/tasks/log-task/logs?follow="+queryTrue, nil),
		fiber.TestConfig{Timeout: 2 * time.Second},
	)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "data: "+line+"\n\n", string(body))
}

func TestHandlers_GetTaskLogsFollowAcceptsExactMaximumLine(t *testing.T) {
	line := strings.Repeat("x", taskLogStreamMaxLineBytes)
	h, app, _ := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, line+"\n")
	})
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	resp, err := app.Test(
		httptest.NewRequest(http.MethodGet, "/tasks/log-task/logs?follow="+queryTrue, nil),
		fiber.TestConfig{Timeout: 2 * time.Second},
	)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "data: "+line+"\n\n", string(body))
}

func TestHandlers_GetTaskLogsFollowBoundsOversizeLines(t *testing.T) {
	tests := []struct {
		name      string
		lineBytes int
	}{
		{name: "one byte over", lineBytes: taskLogStreamMaxLineBytes + 1},
		{name: "well over", lineBytes: taskLogStreamMaxLineBytes + 128*1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := strings.Repeat("x", tt.lineBytes)
			h, app, _ := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, line+"\n")
			})
			app.Get("/tasks/:id/logs", h.GetTaskLogs)

			resp, err := app.Test(
				httptest.NewRequest(http.MethodGet, "/tasks/log-task/logs?follow="+queryTrue, nil),
				fiber.TestConfig{Timeout: 2 * time.Second},
			)
			require.NoError(t, err)
			defer resp.Body.Close() //nolint:errcheck
			require.Equal(t, http.StatusOK, resp.StatusCode)

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NotContains(t, string(body), "data: "+strings.Repeat("x", 64))
			require.Contains(t, string(body), "event: error\n")
			require.Contains(t, string(body), "task log line exceeds 1048576 bytes")
		})
	}
}

func TestHandlers_GetTaskLogsNonFollowRemainsJSON(t *testing.T) {
	h, app, _ := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("tailLines"); got != "100" {
			t.Errorf("tailLines = %q, want 100", got)
		}
		if got := r.URL.Query().Get("follow"); got != "" {
			t.Errorf("follow = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "tail-one\ntail-two\n")
	})
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	resp, err := app.Test(
		httptest.NewRequest(http.MethodGet, "/tasks/log-task/logs", nil),
		fiber.TestConfig{Timeout: 2 * time.Second},
	)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json; charset=utf-8", resp.Header.Get("Content-Type"))

	var payload struct {
		Logs string `json:"logs"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	require.Equal(t, "tail-one\ntail-two\n", payload.Logs)
}

func TestHandlers_GetTaskLogsFollowSurfacesScannerErrors(t *testing.T) {
	const partial = "before-error\n"
	h, app, _ := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(partial)+32))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, partial)
	})
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	resp, err := app.Test(
		httptest.NewRequest(http.MethodGet, "/tasks/log-task/logs?follow="+queryTrue, nil),
		fiber.TestConfig{Timeout: 2 * time.Second},
	)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "data: before-error\n\n")
	require.Contains(t, string(body), "event: error\n")
	require.Contains(t, string(body), "unexpected EOF")
}

func TestHandlers_GetTaskLogsFollowHeartbeatsWhileUpstreamIsQuiet(t *testing.T) {
	releaseUpstream := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseUpstream)
		}
	}()
	h, app, _ := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-releaseUpstream
	})
	h.eventStreamHeartbeatEvery = 10 * time.Millisecond
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	client := newTaskLogHTTPClient()
	resp, err := client.Get(startTaskLogFiberServer(t, app) + "/tasks/log-task/logs?follow=" + queryTrue)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)

	heartbeat := make([]byte, len(": heartbeat\n\n"))
	_, err = io.ReadFull(resp.Body, heartbeat)
	require.NoError(t, err)
	require.Equal(t, ": heartbeat\n\n", string(heartbeat))

	close(releaseUpstream)
	released = true
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
}

func TestHandlers_GetTaskLogsFollowDisconnectCancelsQuietUpstream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	h, app, kubeServer := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	})
	h.eventStreamHeartbeatEvery = 10 * time.Millisecond
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	client := newTaskLogHTTPClient()
	resp, err := client.Get(startTaskLogFiberServer(t, app) + "/tasks/log-task/logs?follow=" + queryTrue)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	heartbeat := make([]byte, len(": heartbeat\n\n"))
	_, err = io.ReadFull(resp.Body, heartbeat)
	require.NoError(t, err)
	require.Equal(t, ": heartbeat\n\n", string(heartbeat))
	require.NoError(t, resp.Body.Close())

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		kubeServer.CloseClientConnections()
		t.Fatal("client disconnect did not cancel the quiet Kubernetes log stream")
	}
}

func TestHandlers_GetTaskLogsFollowCancelsUpstreamDuringSetup(t *testing.T) {
	logStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	h, app, kubeServer := newTaskLogStreamTestApp(t, func(_ http.ResponseWriter, r *http.Request) {
		close(logStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	})

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(requestCtx)
		return c.Next()
	})
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	result := make(chan error, 1)
	go func() {
		resp, err := app.Test(
			httptest.NewRequest(http.MethodGet, "/tasks/log-task/logs?follow="+queryTrue, nil),
			fiber.TestConfig{Timeout: 2 * time.Second},
		)
		if resp != nil {
			_ = resp.Body.Close()
		}
		result <- err
	}()

	select {
	case <-logStarted:
	case <-time.After(2 * time.Second):
		kubeServer.CloseClientConnections()
		t.Fatal("Kubernetes log stream setup did not start")
	}
	cancelRequest()

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		kubeServer.CloseClientConnections()
		t.Fatal("request cancellation did not stop Kubernetes log stream setup")
	}
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		kubeServer.CloseClientConnections()
		t.Fatal("log request did not finish after setup cancellation")
	}
}

func TestHandlers_GetTaskLogsFollowReturnsTimeoutWhenSetupDeadlineExpires(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	h, app, kubeServer := newTaskLogStreamTestApp(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(upstreamCanceled)
	})

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelRequest()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(requestCtx)
		return c.Next()
	})
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	resp, err := app.Test(
		httptest.NewRequest(http.MethodGet, "/tasks/log-task/logs?follow="+queryTrue, nil),
		fiber.TestConfig{Timeout: 2 * time.Second},
	)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		kubeServer.CloseClientConnections()
		t.Fatal("request deadline did not cancel Kubernetes log stream setup")
	}
}

func TestHandlers_GetTaskLogsFollowOutlivesFiberRequestContext(t *testing.T) {
	releaseUpstream := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	h, app, kubeServer := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "before-request-context-cancel\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-releaseUpstream:
			_, _ = io.WriteString(w, "after-request-context-cancel\n")
		case <-r.Context().Done():
			close(upstreamCanceled)
		}
	})

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(requestCtx)
		return c.Next()
	})
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	client := newTaskLogHTTPClient()
	resp, err := client.Get(startTaskLogFiberServer(t, app) + "/tasks/log-task/logs?follow=" + queryTrue)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)

	firstFrame := make([]byte, len("data: before-request-context-cancel\n\n"))
	_, err = io.ReadFull(resp.Body, firstFrame)
	require.NoError(t, err)
	require.Equal(t, "data: before-request-context-cancel\n\n", string(firstFrame))

	cancelRequest()
	select {
	case <-upstreamCanceled:
		kubeServer.CloseClientConnections()
		t.Fatal("Fiber request context cancellation stopped the handed-off Kubernetes log stream")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseUpstream)

	remainingBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(remainingBody), "data: after-request-context-cancel\n\n")
}

func TestHandlers_GetTaskLogsFollowHonorsDeadlineAfterHandoff(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	h, app, kubeServer := newTaskLogStreamTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "before-request-deadline\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	})

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancelRequest()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(requestCtx)
		return c.Next()
	})
	app.Get("/tasks/:id/logs", h.GetTaskLogs)

	client := newTaskLogHTTPClient()
	resp, err := client.Get(startTaskLogFiberServer(t, app) + "/tasks/log-task/logs?follow=" + queryTrue)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusOK, resp.StatusCode)

	firstFrame := make([]byte, len("data: before-request-deadline\n\n"))
	_, err = io.ReadFull(resp.Body, firstFrame)
	require.NoError(t, err)
	require.Equal(t, "data: before-request-deadline\n\n", string(firstFrame))

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		kubeServer.CloseClientConnections()
		t.Fatal("request deadline did not cancel the handed-off Kubernetes log stream")
	}
}

func startTaskLogFiberServer(t *testing.T, app *fiber.App) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = app.ShutdownWithContext(shutdownCtx)
		select {
		case <-serveErr:
		case <-time.After(time.Second):
		}
	})
	return "http://" + listener.Addr().String()
}

func newTaskLogHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
}

func newTaskLogStreamTestApp(
	t *testing.T,
	logHandler http.HandlerFunc,
) (*Handlers, *fiber.App, *httptest.Server) {
	t.Helper()

	kubeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/default/pods":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(corev1.PodList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
				Items: []corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{Name: "log-pod", Namespace: "default"},
				}},
			}); err != nil {
				t.Errorf("encode pod list: %v", err)
			}
		case "/api/v1/namespaces/default/pods/log-pod/log":
			logHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kubeServer.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: kubeServer.URL})
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "log-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{JobName: "log-job"},
	}
	controllerClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	h := NewHandlers(HandlersConfig{Client: controllerClient, KubeClient: clientset})

	return h, fiber.New(), kubeServer
}
