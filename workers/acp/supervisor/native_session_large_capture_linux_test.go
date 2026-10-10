//go:build linux

package supervisor

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/stretchr/testify/require"
)

func TestSupervisorLargeNativeCaptureConfiguredLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limit   int
		success bool
		legacy  bool
	}{
		{name: "multi-megabyte", limit: 6 << 20, success: true},
		{name: "configured-rejection", limit: harnessv2.LegacyMaxNativeSessionBytes},
		{name: "legacy-caller-rejection", limit: 6 << 20, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "native-capture")
			server.cfg.Capabilities.Limits.MaxNativeSessionBytes = tc.limit
			create := testCreateSessionRequest(t, cfg, profile)
			response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			require.Equal(t, http.StatusCreated, response.Code)
			prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			response = performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
			require.Equal(t, http.StatusOK, response.Code)
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			state.descriptor.State = harnessv2.RuntimeSessionStateIdle
			server.mu.Unlock()
			home := filepath.Join(state.paths.Home, ".codex")
			writeTestNativeRollout(t, home)
			rollout := filepath.Join(home, "sessions", "2026", "10", "05", "rollout-2026-10-05T12-00-00-"+testNativeThreadID+".jsonl")
			file, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			record := map[string]any{"ordinal": 2, "timestamp": "2026-10-05T12:00:02Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": string(bytes.Repeat([]byte("x"), 3<<20))}}}}
			require.NoError(t, json.NewEncoder(file).Encode(record))
			require.NoError(t, file.Close())
			original, err := os.ReadFile(rollout)
			require.NoError(t, err)
			transport := httptest.NewServer(server.Handler())
			defer transport.Close()
			client, err := harnessv2.NewClient(transport.URL, harnessv2.WithControllerBearerToken(cfg.ControllerBearerToken), harnessv2.WithOperationCapabilitySecret(cfg.CapabilitySecret))
			require.NoError(t, err)
			capabilities, err := client.Capabilities(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.limit, capabilities.Limits.MaxNativeSessionBytes)
			request := nativeCaptureRequest(t, create.Metadata.Fence)
			var captured *harnessv2.CaptureNativeSessionResponse
			if tc.legacy {
				call := mutationHTTPRequest(t, http.MethodPost, transport.URL+"/v2/runtime-sessions/session-1/native-session", request, cfg)
				call.RequestURI = ""
				response, callErr := http.DefaultClient.Do(call)
				require.NoError(t, callErr)
				body, readErr := io.ReadAll(response.Body)
				require.NoError(t, readErr)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode)
				var failure harnessv2.ErrorResponse
				require.NoError(t, json.Unmarshal(body, &failure))
				require.Equal(t, harnessv2.ErrorCodeNativeCaptureUnsupported, failure.Code)
				err = &harnessv2.ClientError{Code: failure.Code}
			} else {
				captured, err = client.CaptureNativeSession(t.Context(), create.RuntimeSessionID, request)
			}
			if tc.success {
				require.NoError(t, err)
				require.Greater(t, len(captured.Snapshot.Data), harnessv2.MaxCanonicalJSONBytes)
				require.LessOrEqual(t, len(captured.Snapshot.Data), tc.limit)
				_, err = codexstate.Inspect(t.Context(), captured.Snapshot.Data, tc.limit)
				require.NoError(t, err)
				persisted, err := os.ReadFile(filepath.Join(state.paths.Root, ".native-session-snapshot.json"))
				require.NoError(t, err)
				var snapshot harnessv2.NativeSessionSnapshot
				require.NoError(t, json.Unmarshal(persisted, &snapshot))
				require.Equal(t, captured.Snapshot.Data, snapshot.Data)
				retry, err := client.CaptureNativeSession(t.Context(), create.RuntimeSessionID, request)
				require.NoError(t, err)
				require.Equal(t, captured.Snapshot.Data, retry.Snapshot.Data)
			} else {
				var failure *harnessv2.ClientError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, harnessv2.ErrorCodeNativeCaptureUnsupported, failure.Code)
				_, err = os.Stat(filepath.Join(state.paths.Root, ".native-session-snapshot.json"))
				require.True(t, os.IsNotExist(err))
			}
			select {
			case <-state.runtime.Process().Done():
			default:
				t.Fatal("capture returned before provider exit")
			}
			unchanged, err := os.ReadFile(rollout)
			require.NoError(t, err)
			require.Equal(t, original, unchanged)
			deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "delete-large-captured", false), Reason: "verification complete"}
			sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
			_, err = client.DeleteRuntimeSession(t.Context(), create.RuntimeSessionID, deletion)
			require.NoError(t, err)
			_, err = os.Stat(state.paths.Root)
			require.True(t, os.IsNotExist(err))
		})
	}
}
