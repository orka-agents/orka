/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// waitPhaseServer returns a server whose /api/v1/tasks/example-task
// endpoint answers with the given phase.
func waitPhaseServer(phase string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"metadata": map[string]any{"name": "example-task"},
			"status":   map[string]any{"phase": phase},
		})
	}))
}

func runTaskWait(t *testing.T, srv *httptest.Server, extraArgs ...string) error {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	root := newRootCmd()
	root.SetOut(io.Discard)
	args := append([]string{"task", "wait", "example-task"}, extraArgs...)
	if srv != nil {
		args = append(args, "--server", srv.URL)
	}
	root.SetArgs(args)
	return root.Execute()
}

func TestTaskWaitDeadlineDuringRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	shutdown := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requestStarted) })
		select {
		case <-r.Context().Done():
			close(requestCanceled)
		case <-shutdown:
		}
	}))
	defer func() {
		close(shutdown)
		srv.Close()
	}()

	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetArgs([]string{"task", "wait", "example-task", "--server", srv.URL, "--timeout", "2s"})

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- root.Execute()
	}()

	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("status request never reached the server")
	}

	select {
	case err := <-resultCh:
		if err == nil || !strings.Contains(err.Error(), "timed out waiting for task example-task") {
			t.Fatalf("wait error = %v, want timed out waiting for task example-task", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait command did not return after the deadline")
	}

	select {
	case <-requestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight status request was not canceled by the deadline")
	}
}

func TestWaitForTaskPhaseRejectsLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var out strings.Builder

		err := waitForTaskPhase(ctx, "example-task", time.Millisecond, func(context.Context) (string, error) {
			// A response can finish decoding after its request deadline expires.
			time.Sleep(2 * time.Second)
			return "Succeeded", nil
		}, &out)
		if err == nil || !strings.Contains(err.Error(), "timed out waiting for task example-task") {
			t.Fatalf("wait error = %v, want timed out waiting for task example-task", err)
		}
		if out.Len() != 0 {
			t.Fatalf("wait output = %q, want no success message", out.String())
		}
	})
}

func TestTaskWaitDeadlineBetweenPolls(t *testing.T) {
	srv := waitPhaseServer("Running")
	defer srv.Close()

	err := runTaskWait(t, srv, "--timeout", "100ms")
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for task example-task") {
		t.Fatalf("wait error = %v, want timed out waiting for task example-task", err)
	}
}

func TestTaskWaitDeadlineSuccessBeforeDeadline(t *testing.T) {
	srv := waitPhaseServer("Succeeded")
	defer srv.Close()

	if err := runTaskWait(t, srv, "--timeout", "1s"); err != nil {
		t.Fatalf("wait error = %v, want nil", err)
	}
}

func TestTaskWaitDeadlineFailedPhase(t *testing.T) {
	srv := waitPhaseServer("Failed")
	defer srv.Close()

	err := runTaskWait(t, srv, "--timeout", "1s")
	if err == nil || !strings.Contains(err.Error(), "finished with phase Failed") {
		t.Fatalf("wait error = %v, want finished with phase Failed", err)
	}
}

func TestTaskWaitInvalidTimeout(t *testing.T) {
	err := runTaskWait(t, nil, "--timeout", "abc")
	if err == nil || !strings.Contains(err.Error(), "invalid timeout") {
		t.Fatalf("wait error = %v, want invalid timeout", err)
	}
}
