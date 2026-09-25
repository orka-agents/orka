package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkspaceLifetimeCommandStartsAndStopsWithItsProcess(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is needed to execute the Codex fixture command")
	}
	for _, tool := range []string{"exec_command", "shell_command"} {
		t.Run(tool, func(t *testing.T) {
			resetWorkspaceCanary(t, workspaceLifetimeMarker)
			key := markerKey(workspaceLifetimeToolMarker)
			markerCounts.Delete(key)
			markerDisconnects.Delete(key)
			markerDisconnectTimes.Delete(key)
			t.Cleanup(func() {
				markerCounts.Delete(key)
				markerDisconnects.Delete(key)
				markerDisconnectTimes.Delete(key)
			})
			server := httptest.NewServer(http.HandlerFunc(handleResponses))
			t.Cleanup(server.Close)
			call := workspaceCanaryResponseItem(t, requestWorkspaceCanary(t, []any{
				map[string]any{"role": "user", "content": "Reply exactly: " + workspaceLifetimeMarker},
			}, tool, true))
			var args map[string]any
			if err := json.Unmarshal([]byte(call["arguments"].(string)), &args); err != nil {
				t.Fatal(err)
			}
			command, _ := args["cmd"].(string)
			if tool == "shell_command" {
				command, _ = args["command"].(string)
				if args["timeout_ms"] != float64(300000) {
					t.Fatal("the shell timeout must outlast workspace expiry")
				}
			}
			command = strings.ReplaceAll(command, "http://vekil.vekil-system.svc:1337", server.URL)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			finished := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(finished)
			}()
			t.Cleanup(func() { cancel(); <-finished })
			waitLifetimeFixtureCount(t, &markerCounts, key)
			select {
			case <-finished:
				t.Fatal("the command finished before cancellation")
			default:
			}
			cancelledAt := time.Now().UnixMilli()
			cancel()
			waitLifetimeFixtureCount(t, &markerDisconnects, key)
			response := httptest.NewRecorder()
			handleMarkerObservations(response, httptest.NewRequest(http.MethodGet, "/fixture/marker-observations", nil))
			var observations map[string]struct {
				DisconnectedAt int64 `json:"disconnectedAtUnixMilli"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &observations); err != nil {
				t.Fatal(err)
			}
			if observations[key].DisconnectedAt < cancelledAt {
				t.Fatal("the disconnect timestamp must show when the process was stopped")
			}
			assertWorkspaceCanaryVerified(t, workspaceLifetimeMarker, false)
		})
	}
}

func TestWorkspaceLifetimeRejectsAnUnissuedToolResult(t *testing.T) {
	resetWorkspaceCanary(t, workspaceLifetimeMarker)
	response := requestWorkspaceCanary(t, []any{
		map[string]any{"role": "user", "content": "Reply exactly: " + workspaceLifetimeMarker},
		map[string]any{"type": functionCallOutputType, "call_id": "not-issued", "output": "still running"},
	}, "exec_command", false)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unissued shell result status = %d, want 422", response.Code)
	}
}

func waitLifetimeFixtureCount(t *testing.T, counters *sync.Map, key string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if value, ok := counters.Load(key); ok {
			if count := value.(*atomic.Uint64).Load(); count == 1 {
				return
			} else if count > 1 {
				t.Fatal("the shell command made more than one request")
			}
		}
		select {
		case <-deadline:
			t.Fatal("the shell command did not reach the expected request state")
		case <-tick.C:
		}
	}
}
