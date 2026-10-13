package supervisor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestLoadConfigFromEnvNativeSessionLimit(t *testing.T) {
	for _, test := range []struct {
		name    string
		raw     string
		want    int
		invalid bool
	}{
		{name: "default", want: harnessv2.DefaultMaxNativeSessionBytes},
		{name: "zero", raw: "0", want: harnessv2.DefaultMaxNativeSessionBytes},
		{name: "custom", raw: " 1048576 ", want: 1 << 20},
		{name: "maximum", raw: strconv.Itoa(harnessv2.MaxNativeSessionBytes), want: harnessv2.MaxNativeSessionBytes},
		{name: "negative", raw: "-1", invalid: true},
		{name: "oversize", raw: strconv.Itoa(harnessv2.MaxNativeSessionBytes + 1), invalid: true},
		{name: "noninteger", raw: "8MiB", invalid: true},
		{name: "overflow", raw: strings.Repeat("9", 30), invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			setArtifactClientSupervisorEnv(t)
			t.Setenv(EnvNativeSessionMaxBytes, test.raw)
			cfg, err := LoadConfigFromEnv()
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), EnvNativeSessionMaxBytes) {
					t.Fatalf("invalid configuration error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Capabilities.Limits.MaxNativeSessionBytes != test.want {
				t.Fatalf("advertised limit = %d, want %d", cfg.Capabilities.Limits.MaxNativeSessionBytes, test.want)
			}
			if cfg.Capabilities.Limits.MaxRequestBytes != 2<<20 {
				t.Fatal("native configuration changed ordinary request cap")
			}
		})
	}
	if defaultProtocolLimits().MaxNativeSessionBytes != harnessv2.DefaultMaxNativeSessionBytes {
		t.Fatal("default protocol limits omit the native session default")
	}
}

func TestSupervisorNativeSessionConfigValidation(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	for _, limit := range []int{-1, harnessv2.MaxNativeSessionBytes + 1} {
		cfg.Capabilities.Limits.MaxNativeSessionBytes = limit
		if err := cfg.Validate(); err == nil {
			t.Fatalf("Validate accepted native session limit %d", limit)
		}
		called := false
		_, err := newServer(cfg, func(string, *acp.UIDAllocator) (io.Closer, error) {
			called = true
			return nil, errors.New("unexpected identity preparation")
		})
		if err == nil || called {
			t.Fatal("invalid size reached identity preparation")
		}
	}
	server, _, _ := newTestServer(t, "immediate")
	if server.cfg.Capabilities.Limits.MaxNativeSessionBytes != harnessv2.DefaultMaxNativeSessionBytes {
		t.Fatal("zero supervisor configuration did not normalize to 8 MiB")
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v2/capabilities", nil)
	request.Header.Set(harnessv2.NativeSessionLimitsHeader, "1")
	server.Handler().ServeHTTP(response, request)
	var capabilities harnessv2.CapabilitiesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &capabilities); err != nil || capabilities.Limits.MaxNativeSessionBytes != harnessv2.DefaultMaxNativeSessionBytes {
		t.Fatalf("capabilities did not advertise normalized limit: %v", err)
	}
}

func TestNativeCreateDecoderSeparatesBodyLimits(t *testing.T) {
	const ordinary = 2 << 20
	const native = 4 << 20
	server := &Server{cfg: Config{
		ControllerBearerToken: strings.Repeat("t", 32),
		Capabilities:          harnessv2.CapabilitiesResponse{Limits: harnessv2.ProtocolLimits{MaxRequestBytes: ordinary, MaxNativeSessionBytes: native}},
	}}
	data := bytes.Repeat([]byte{'x'}, 3<<20)
	encoded := base64.StdEncoding.EncodeToString(data)
	nativeBody := `{"nativeRestore":{"snapshot":{"data":"` + encoded + `"}}}`
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{name: "ordinary exact", body: `{}` + strings.Repeat(" ", ordinary-2), want: true},
		{name: "ordinary one over", body: `{}` + strings.Repeat(" ", ordinary-1)},
		{name: "large ordinary field", body: `{"runtimeSessionID":"` + strings.Repeat("x", ordinary) + `"}`},
		{name: "native", body: nativeBody, want: true},
		{name: "native with unrelated padding", body: nativeBody + strings.Repeat(" ", ordinary)},
		{name: "native with large unrelated field", body: `{"runtimeSessionID":"` + strings.Repeat("x", ordinary) + `","nativeRestore":{"snapshot":{"data":"` + encoded + `"}}}`},
		{name: "native escaped padding", body: `{"nativeRestore":{"snapshot":{"data":"` + strings.ReplaceAll(encoded, "e", `\u0065`) + `"}}}`},
		{name: "native null does not exempt body", body: `{"nativeRestore":null}` + strings.Repeat(" ", ordinary)},
		{name: "configured size exceeded", body: `{"nativeRestore":{"snapshot":{"data":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, native+1)) + `"}}}`},
		{name: "unknown field", body: `{"irrelevant":true,"nativeRestore":{"snapshot":{"data":"eA=="}}}`},
		{name: "trailing JSON", body: nativeBody + `{}`},
		{name: "native transport cap", body: nativeBody + strings.Repeat(" ", harnessv2.NativeSessionJSONLimit(native))},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/v2/runtime-sessions/test", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+server.cfg.ControllerBearerToken)
			response := httptest.NewRecorder()
			var decoded harnessv2.CreateRuntimeSessionRequest
			got := server.decodeAuthenticatedCreateJSON(response, request, &decoded)
			if got != test.want {
				t.Fatalf("decode = %v, want %v; status %d", got, test.want, response.Code)
			}
			if !got && response.Code != http.StatusBadRequest {
				t.Fatalf("rejection status = %d", response.Code)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPut, "/v2/runtime-sessions/test/prompts/test", strings.NewReader(nativeBody))
	request.Header.Set("Authorization", "Bearer "+server.cfg.ControllerBearerToken)
	response := httptest.NewRecorder()
	var decoded harnessv2.CreateRuntimeSessionRequest
	if server.decodeAuthenticatedJSON(response, request, &decoded) {
		t.Fatal("ordinary decoder accepted large native body")
	}
	unauthenticated := httptest.NewRequest(http.MethodPut, "/v2/runtime-sessions/test", &unreadableBody{t: t})
	if server.decodeAuthenticatedCreateJSON(httptest.NewRecorder(), unauthenticated, &decoded) {
		t.Fatal("native decoder accepted unauthenticated peer")
	}
}

func TestNativeRestoreConfiguredLimitBeforeMutation(t *testing.T) {
	cfg, profile := newSessionIdentityTestConfig(t)
	cfg.Capabilities.Limits.MaxNativeSessionBytes = 1024
	server := &Server{cfg: cfg}
	request := testCreateSessionRequest(t, cfg, profile)
	request.NativeRestore = &harnessv2.NativeSessionRestore{Snapshot: harnessv2.NativeSessionSnapshot{Data: make([]byte, 1025)}}
	if err := server.validateNativeRestoreSize(request); err == nil {
		t.Fatal("configured restore limit was ignored")
	}
	_, _, _, _, _, _, _, err := server.createSession(t.Context(), request, time.Now(), os.Getuid(), os.Getgid())
	if err == nil {
		t.Fatal("oversize restore reached runtime creation")
	}
	if _, err := os.Stat(cfg.SessionBaseDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversize restore created a private home")
	}
	request.NativeRestore.Snapshot.Data = request.NativeRestore.Snapshot.Data[:1024]
	if err := server.validateNativeRestoreSize(request); err != nil {
		t.Fatalf("exact configured cap: %v", err)
	}
}

// The fixture is an actual synthetic paginated rollout. Only the ACP process
// is faked; no model invocation or fabricated provider result is involved.
func TestSupervisorNativeRestoreAboveOrdinaryLimit(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestNativeRollout(t, source)
	rolloutPath := filepath.Join(source, "sessions", "2026", "10", "05", "rollout-2026-10-05T12-00-00-"+testNativeThreadID+".jsonl")
	rollout, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	rollout = bytes.Replace(rollout, []byte("private history"), bytes.Repeat([]byte{'x'}, 3<<20), 1)
	if err := os.WriteFile(rolloutPath, rollout, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := codexstate.Capture(t.Context(), source, testNativeThreadID)
	if err != nil {
		t.Fatal(err)
	}
	server, cfg, profile := newTestServer(t, "native-resume")
	server.cfg.Provider.PrepareSession = prepareCodexHome
	parent, err := filepath.EvalSymlinks(filepath.Dir(cfg.SessionBaseDir))
	if err != nil {
		t.Fatal(err)
	}
	server.cfg.SessionBaseDir = filepath.Join(parent, "sessions")
	cfg.Capabilities.Limits.MaxRequestBytes = 2 << 20
	server.cfg.Capabilities.Limits.MaxRequestBytes = cfg.Capabilities.Limits.MaxRequestBytes
	server.cfg.Capabilities.Limits.MaxNativeSessionBytes = len(data)
	request := testCreateSessionRequest(t, cfg, profile)
	request.NativeRestore = &harnessv2.NativeSessionRestore{Snapshot: harnessv2.NativeSessionSnapshot{
		Data: data, DataDigest: codexstate.DataDigest(data), ProviderSessionID: testNativeThreadID,
		ProviderKind: providerKindCodex, ProviderVersion: acp.CodexCLIVersion,
		RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest, WorkingDirectory: "/source/work",
	}}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	if len(data) <= cfg.Capabilities.Limits.MaxRequestBytes {
		t.Fatal("fixture does not exceed ordinary HTTP cap")
	}
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, cfg)
	if response.Code != http.StatusCreated {
		t.Fatalf("large native restore: %d %s", response.Code, response.Body.String())
	}
	state := server.sessions[request.RuntimeSessionID]
	// Check the real child ACP client's outgoing cap through its public method.
	// Raising the native HTTP cap must not let this message onto provider stdin.
	if err := state.runtime.Process().Client().Notify(t.Context(), "test/oversize", map[string]string{"text": strings.Repeat("x", 2<<20)}); err == nil || !strings.Contains(err.Error(), "ACP message exceeds") {
		t.Fatalf("child ACP cap changed: %v", err)
	}
	installed, err := filepath.Glob(filepath.Join(state.paths.Home, ".codex", "sessions", "2026", "10", "05", "rollout-*.jsonl"))
	if err != nil || len(installed) != 1 {
		t.Fatal("native restore did not install the rollout")
	}
	body, err := os.ReadFile(installed[0])
	if err != nil || !bytes.Contains(body, bytes.Repeat([]byte{'x'}, 3<<20)) {
		t.Fatal("large native rollout content was not preserved")
	}
}
