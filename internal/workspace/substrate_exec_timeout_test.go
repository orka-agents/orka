package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSubstrateExecCollectsCommandTimeout(t *testing.T) {
	var finishedAt atomic.Int64
	var starts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == substrateTestExecPath:
			var req substrateExecRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode exec request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if req.TimeoutSeconds != 1 || !req.Detach {
				t.Errorf("command timeout=%d detach=%t, want 1/true", req.TimeoutSeconds, req.Detach)
			}
			starts.Add(1)
			// A detached daemon can only publish its timeout result after its
			// own command timer expires and command cleanup completes.
			finishedAt.Store(time.Now().Add(time.Duration(req.TimeoutSeconds)*time.Second + 25*time.Millisecond).UnixNano())
			_ = json.NewEncoder(w).Encode(substrateExecResponse{ExecID: "timeout-1", Running: true})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/exec/timeout-1":
			response := substrateExecResponse{ExecID: "timeout-1", Running: time.Now().UnixNano() < finishedAt.Load()}
			if !response.Running {
				response.ExitCode = 124
				response.Stderr = "command timed out"
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	executor := &SubstrateWorkspaceExecutor{
		httpClient: server.Client(), routerURL: server.URL, actorDNSSuffix: "actors.test", handoffToken: substrateTestToken,
	}
	result, err := executor.Exec(t.Context(), ExecRequest{
		Ref: WorkspaceRef{ID: "actor-1"}, Command: []string{"sleep", "60"}, Timeout: time.Second,
	})
	require.NotNil(t, result, "daemon timeout result must survive the command deadline: %v", err)
	require.Equal(t, 124, result.ExitCode)
	require.Equal(t, "command timed out", result.Stderr)
	require.True(t, IsKind(err, ErrorKindCommandFailed), "error = %v", err)
	require.EqualValues(t, 1, starts.Load(), "result collection must never replay execution")
}

func TestSubstrateExecResultCollectionHonorsCallerContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name, kind := "canceled", ErrorKindCanceled
		if deadline {
			name, kind = "deadline", ErrorKindTimeout
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
			}
			defer cancel()
			var starts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					starts.Add(1)
				}
				_ = json.NewEncoder(w).Encode(substrateExecResponse{ExecID: "running-1", Running: true})
				if r.Method == http.MethodGet && !deadline {
					cancel()
				}
			}))
			defer server.Close()
			executor := &SubstrateWorkspaceExecutor{
				httpClient: server.Client(), routerURL: server.URL, actorDNSSuffix: "actors.test", handoffToken: substrateTestToken,
			}
			result, err := executor.Exec(ctx, ExecRequest{
				Ref: WorkspaceRef{ID: "actor-1"}, Command: []string{"sleep", "60"}, Timeout: time.Second,
			})
			require.Nil(t, result)
			require.Error(t, ctx.Err())
			require.True(t, IsKind(err, kind), "error = %v", err)
			require.EqualValues(t, 1, starts.Load(), "caller cancellation must never replay execution")
		})
	}
}
