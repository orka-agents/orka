package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRouterServerTimeoutDefaults(t *testing.T) {
	server := newHTTPServer(":8080", http.NotFoundHandler(), defaultReadTimeout)
	require.Equal(t, 30*time.Second, server.ReadTimeout)
	require.Equal(t, 10*time.Second, server.ReadHeaderTimeout)
	require.Zero(t, server.WriteTimeout, "uploads must be bounded without imposing a chat response deadline")
	require.Equal(t, 30*time.Minute, defaultShutdownTimeout)
	for _, values := range [][2]time.Duration{
		{0, time.Minute}, {-time.Second, time.Minute},
		{time.Second, 0}, {time.Second, -time.Second},
	} {
		require.ErrorContains(t, run("", "", values[0], values[1]), "must be positive")
	}
}

func TestRouterServerBoundsChunkedRequestBody(t *testing.T) {
	readErr := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		readErr <- err
		w.WriteHeader(http.StatusRequestTimeout)
	})
	entry := httptest.NewUnstartedServer(handler)
	entry.Config = newHTTPServer("", handler, 200*time.Millisecond)
	entry.Start()
	t.Cleanup(entry.Close)
	conn, err := net.DialTimeout("tcp", entry.Listener.Addr().String(), time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn,
		"POST /test HTTP/1.1\r\nHost: router.test\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n")
	require.NoError(t, err)
	select {
	case err := <-readErr:
		var timeout net.Error
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.Timeout())
	case <-time.After(3 * time.Second):
		t.Fatal("incomplete chunked request retained the connection past the read deadline")
	}
}

func TestRouterServerReadTimeoutDoesNotTruncateResponses(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: first\n\n")
					w.(http.Flusher).Flush()
				}
				select {
				case <-time.After(500 * time.Millisecond):
					_, _ = io.WriteString(w, "complete")
				case <-r.Context().Done():
				}
			})
			entry := httptest.NewUnstartedServer(handler)
			entry.Config = newHTTPServer("", handler, 200*time.Millisecond)
			entry.Start()
			t.Cleanup(entry.Close)
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Post(entry.URL, "application/json", strings.NewReader(`{}`))
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, string(body), "complete")
			if stream {
				require.Contains(t, string(body), "data: first")
			}
		})
	}
}

func TestRouterServerDrainsActiveResponses(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: first\n\n")
					w.(http.Flusher).Flush()
				}
				close(started)
				select {
				case <-release:
					_, _ = io.WriteString(w, "complete")
				case <-r.Context().Done():
				}
			})
			server := newHTTPServer("", handler, time.Second)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			t.Cleanup(func() { _ = server.Close() })
			done := make(chan error, 1)
			go func() { done <- serveHTTP(ctx, server, listener, 2*time.Second) }()
			body := make(chan string, 1)
			clientErr := make(chan error, 1)
			go func() {
				resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + listener.Addr().String())
				if err != nil {
					clientErr <- err
					return
				}
				defer func() { _ = resp.Body.Close() }()
				value, err := io.ReadAll(resp.Body)
				if err != nil {
					clientErr <- err
					return
				}
				body <- string(value)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case err := <-done:
				t.Fatalf("shutdown returned before the active response finished: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			select {
			case value := <-body:
				require.Contains(t, value, "complete")
				if stream {
					require.Contains(t, value, "data: first")
				}
			case err := <-clientErr:
				t.Fatal(err)
			case <-time.After(3 * time.Second):
				t.Fatal("active response did not drain")
			}
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("server did not finish graceful shutdown")
			}
		})
	}
}

func TestRouterServerEnforcesShutdownLimit(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	})
	server := newHTTPServer("", handler, time.Second)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = server.Close() })
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, server, listener, 100*time.Millisecond) }()
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	<-started
	cancel()
	select {
	case err := <-done:
		require.True(t, errors.Is(err, context.DeadlineExceeded))
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not enforce its configured limit")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("expired shutdown did not cancel the active request")
	}
}

func TestRouterDeploymentAllowsDefaultShutdown(t *testing.T) {
	data, err := os.ReadFile("../../config/compat-router/router.yaml")
	require.NoError(t, err)
	decoder := kubeyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
	for {
		var object struct {
			Kind string                `json:"kind"`
			Spec appsv1.DeploymentSpec `json:"spec"`
		}
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			t.Fatal("router Deployment was not found")
		}
		require.NoError(t, err)
		if object.Kind != "Deployment" {
			continue
		}
		require.NotNil(t, object.Spec.Template.Spec.TerminationGracePeriodSeconds)
		grace := *object.Spec.Template.Spec.TerminationGracePeriodSeconds
		require.GreaterOrEqual(t, grace, int64(defaultShutdownTimeout/time.Second)+10)
		require.NotNil(t, object.Spec.ProgressDeadlineSeconds)
		require.Greater(t, int64(*object.Spec.ProgressDeadlineSeconds), grace)
		return
	}
}
