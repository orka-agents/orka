/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSubstrateExecUsesDetachedPolling(t *testing.T) {
	var sawDetached bool
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "actor-1.actors.test" {
			t.Errorf("Host = %q, want actor-1.actors.test", r.Host)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/exec":
			var req substrateExecRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode exec request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			sawDetached = req.Detach
			_ = json.NewEncoder(w).Encode(substrateExecResponse{ExecID: "exec-1", Running: true})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/exec/exec-1":
			polls++
			_ = json.NewEncoder(w).Encode(substrateExecResponse{
				ExecID:   "exec-1",
				Stdout:   "ok",
				Stderr:   "warn",
				ExitCode: 0,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := &SubstrateWorkspaceExecutor{
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   "token",
	}

	got, err := executor.Exec(t.Context(), ExecRequest{
		Ref:     WorkspaceRef{ID: "actor-1"},
		Command: []string{"echo", "ok"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if !sawDetached {
		t.Fatal("Exec() did not request detached execution")
	}
	if polls == 0 {
		t.Fatal("Exec() did not poll detached execution")
	}
	if got.Stdout != "ok" || got.Stderr != "warn" || got.ExitCode != 0 {
		t.Fatalf("Exec() result = stdout %q stderr %q exit %d, want ok/warn/0", got.Stdout, got.Stderr, got.ExitCode)
	}
}
