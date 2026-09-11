//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentSessionCleanupArchivesBeforeTaskAbsence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		tasks  []string
		want   []string
	}{
		{
			name: "single Task", status: http.StatusNoContent, tasks: []string{"first-task"},
			want: []string{"delete:first-task", "archive:original-session", "get:first-task"},
		},
		{
			name: "continued Session", status: http.StatusNoContent, tasks: []string{"first-task", "second-task"},
			want: []string{
				"delete:first-task", "delete:second-task", "archive:original-session", "get:first-task", "get:second-task",
			},
		},
		{
			name: "absent Session", status: http.StatusNotFound, tasks: []string{"first-task", "second-task"},
			want: []string{
				"delete:first-task", "delete:second-task", "archive:original-session", "get:first-task", "get:second-task",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var events []string
			record := func(event string) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, event)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/sessions/original-session" ||
					r.URL.Query().Get("namespace") != namespace || r.Header.Get("Authorization") != "Bearer synthetic-cleanup-token" {
					t.Error("cleanup did not use the exact authenticated Session endpoint")
				}
				record("archive:original-session")
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			run := func(cmd *exec.Cmd) (string, error) {
				checkAgentSessionCleanupCommand(t, cmd)
				record(cmd.Args[1] + ":" + cmd.Args[3])
				return "", nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := cleanupAgentSessionTasks(ctx, server.URL, "synthetic-cleanup-token", "original-session",
				tc.tasks, run)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("cleanup order = %v, want %v", events, tc.want)
			}
		})
	}
}

func TestAgentSessionCleanupRetriesOnlyPendingArchive(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	deletions := 0
	observations := 0
	run := func(cmd *exec.Cmd) (string, error) {
		checkAgentSessionCleanupCommand(t, cmd)
		if cmd.Args[1] == "delete" {
			deletions++
		} else {
			observations++
			if requests.Load() != 2 {
				t.Error("Task absence was observed before Session archival completed")
			}
		}
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cleanupAgentSessionTasks(ctx, server.URL, "synthetic-cleanup-token", "original-session",
		[]string{"first-task", "second-task"}, run); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || deletions != 2 || observations != 2 {
		t.Fatalf("archive requests = %d, cancellations = %d, absence checks = %d", requests.Load(), deletions, observations)
	}
}

func TestAgentSessionCleanupPreservesUnprovedOwners(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		retainedTask bool
	}{
		{name: "archive forbidden", status: http.StatusForbidden},
		{name: "archive failure", status: http.StatusInternalServerError},
		{name: "archive still unsettled", status: http.StatusConflict},
		{name: "second Task finalizer retained", status: http.StatusNoContent, retainedTask: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.status != http.StatusNoContent {
					_, _ = w.Write([]byte("response body must not enter the cleanup error"))
				}
			}))
			defer server.Close()
			var observed []string
			run := func(cmd *exec.Cmd) (string, error) {
				checkAgentSessionCleanupCommand(t, cmd)
				if cmd.Args[1] == "get" {
					observed = append(observed, cmd.Args[3])
					if cmd.Args[3] == "second-task" {
						return "task.core.orka.ai/second-task\n", nil
					}
				}
				return "", nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := cleanupAgentSessionTasks(ctx, server.URL, "synthetic-cleanup-token", "original-session",
				[]string{"first-task", "second-task"}, run)
			if err == nil {
				t.Fatal("unproved original cleanup was reported as complete")
			}
			if strings.Contains(err.Error(), "response body") || strings.Contains(err.Error(), "synthetic-cleanup-token") {
				t.Fatal("cleanup error exposed response or authorization contents")
			}
			if tc.status == http.StatusConflict || tc.retainedTask {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cleanup error = %v, want bounded deadline", err)
				}
			}
			if tc.retainedTask {
				if !slices.Contains(observed, "first-task") || !slices.Contains(observed, "second-task") {
					t.Fatalf("did not observe both original Tasks: %v", observed)
				}
			} else if len(observed) != 0 {
				t.Fatalf("observed Task deletion before proven archival: %v", observed)
			}
		})
	}
}

func TestAgentSessionCleanupStopsOnCancellationFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	wantErr := errors.New("Task cancellation not acknowledged")
	commands := 0
	run := func(cmd *exec.Cmd) (string, error) {
		checkAgentSessionCleanupCommand(t, cmd)
		commands++
		return "", wantErr
	}
	err := cleanupAgentSessionTasks(context.Background(), server.URL, "synthetic-cleanup-token", "original-session",
		[]string{"first-task", "second-task"}, run)
	if !errors.Is(err, wantErr) || commands != 1 || requests.Load() != 0 {
		t.Fatalf("error = %v, commands = %d, archive requests = %d", err, commands, requests.Load())
	}
}

func TestAgentSessionCleanupBoundsUnresponsiveAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	run := func(cmd *exec.Cmd) (string, error) {
		checkAgentSessionCleanupCommand(t, cmd)
		if cmd.Args[1] != "delete" {
			t.Error("Task finalization was observed without an archival acknowledgement")
		}
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := cleanupAgentSessionTasks(ctx, server.URL, "synthetic-cleanup-token", "original-session",
		[]string{"first-task", "second-task"}, run)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup error = %v, want the original deadline", err)
	}
}

func checkAgentSessionCleanupCommand(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if len(cmd.Args) < 4 || cmd.Args[0] != "kubectl" || cmd.Args[2] != "task" ||
		!slices.Contains([]string{"first-task", "second-task"}, cmd.Args[3]) {
		t.Fatalf("unexpected cleanup target: %v", cmd.Args)
	}
	var want []string
	switch cmd.Args[1] {
	case "delete":
		want = []string{"kubectl", "delete", "task", cmd.Args[3], "-n", namespace,
			"--ignore-not-found", "--wait=false", "--request-timeout=10s"}
	case "get":
		want = []string{"kubectl", "get", "task", cmd.Args[3], "-n", namespace,
			"--ignore-not-found", "-o", "name", "--request-timeout=10s"}
	default:
		t.Fatalf("unexpected cleanup operation: %v", cmd.Args)
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("cleanup command = %v, want %v", cmd.Args, want)
	}
}
