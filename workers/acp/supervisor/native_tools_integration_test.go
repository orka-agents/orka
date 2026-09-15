//go:build unix

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const nativeToolMarker = "orka-native-tool-observed-592"

// TestOpenCodeNativeTools runs a caller-supplied executable matching the pinned
// version with the production session projection and provider proxy. The test
// checks --version, not artifact authenticity. All inference comes from a local
// deterministic Chat Completions fixture. Set ORKA_TEST_OPENCODE_BIN to opt in;
// ORKA_TEST_OPENCODE_SEARCH=1 also exercises the public Exa search service,
// without forwarding any local account credentials.
// This host-level test proves native calls, results, denial, and cancellation.
// Linux UID isolation and Kubernetes egress enforcement need separate validation.
func TestOpenCodeNativeTools(t *testing.T) {
	binary := nativeToolBinary(t)
	t.Run("full_native_webfetch", func(t *testing.T) {
		var hits atomic.Int32
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, nativeToolMarker)
		}))
		t.Cleanup(page.Close)
		fixture := newNativeToolFixture(t, []nativeToolStep{{
			name: "webfetch", args: map[string]any{"url": page.URL, "format": "text"}, want: nativeToolMarker,
		}})
		runner := newNativeToolRunner(t, binary, harnessv2.NativeToolPolicyFull, fixture)
		runner.prompt(t)
		fixture.assertComplete(t)
		if hits.Load() != 1 {
			t.Fatalf("native webfetch HTTP requests = %d, want 1", hits.Load())
		}
		runner.assertToolEvents(t)
	})
	t.Run("restricted_read_and_denied_network_tools", func(t *testing.T) {
		var hits atomic.Int32
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			_, _ = io.WriteString(w, nativeToolMarker)
		}))
		t.Cleanup(page.Close)
		fixture := newNativeToolFixture(t, []nativeToolStep{
			{name: "read", args: map[string]any{"filePath": "native-read-canary.txt"}, want: nativeToolMarker},
			{name: "webfetch", args: map[string]any{"url": page.URL, "format": "text"}, denied: true},
			{name: "bash", args: map[string]any{
				"command": "curl --max-time 3 --silent " + page.URL, "description": "Attempt the denied HTTP fetch through a command",
			}, denied: true},
		})
		runner := newNativeToolRunner(t, binary, harnessv2.NativeToolPolicyRestricted, fixture)
		runner.prompt(t)
		fixture.assertComplete(t)
		if hits.Load() != 0 {
			t.Fatalf("denied fetch or command reached the HTTP server %d times", hits.Load())
		}
		runner.assertToolEvents(t)
	})
	t.Run("full_native_websearch", func(t *testing.T) {
		if os.Getenv("ORKA_TEST_OPENCODE_SEARCH") != "1" {
			t.Skip("set ORKA_TEST_OPENCODE_SEARCH=1 to verify the native tool against public Exa search")
		}
		fixture := newNativeToolFixture(t, []nativeToolStep{{
			name: "websearch", args: map[string]any{
				"query": "OpenCode ACP documentation", "numResults": 2, "type": "fast", "contextMaxCharacters": 3000,
			}, want: "https://",
		}})
		runner := newNativeToolRunner(t, binary, harnessv2.NativeToolPolicyFull, fixture)
		runner.prompt(t)
		fixture.assertComplete(t)
		runner.assertToolEvents(t)
	})
	t.Run("cancel_native_webfetch", func(t *testing.T) {
		testNativeToolCancellation(t, binary)
	})
}

func nativeToolBinary(t *testing.T) string {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv("ORKA_TEST_OPENCODE_BIN"))
	if binary == "" {
		t.Skip("set ORKA_TEST_OPENCODE_BIN to the pinned OpenCode " + acp.OpenCodeVersion + " executable")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--version")
	command.Env = []string{"HOME=" + t.TempDir(), "PATH=" + acp.DefaultChildPath, "OPENCODE_DISABLE_AUTOUPDATE=1"}
	version, err := command.Output()
	if err != nil {
		t.Fatalf("read pinned OpenCode version: %v", err)
	}
	if strings.TrimSpace(string(version)) != acp.OpenCodeVersion {
		t.Fatalf("OpenCode version = %q, want pinned %s", strings.TrimSpace(string(version)), acp.OpenCodeVersion)
	}
	return binary
}

type nativeToolStep struct {
	name   string
	args   map[string]any
	want   string
	denied bool
}

type nativeToolFixture struct {
	server *httptest.Server
	steps  []nativeToolStep
	mu     sync.Mutex
	issued int
	done   int
	err    string
}

func newNativeToolFixture(t *testing.T, steps []nativeToolStep) *nativeToolFixture {
	t.Helper()
	fixture := &nativeToolFixture{steps: steps}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(fixture.server.Close)
	return fixture
}

type nativeToolChatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role       string          `json:"role"`
		ToolCallID string          `json:"tool_call_id"`
		Content    json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func (f *nativeToolFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var request nativeToolChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issued > f.done {
		if err := f.acceptResult(request); err != nil {
			f.err = err.Error()
			http.Error(w, f.err, http.StatusUnprocessableEntity)
			return
		}
	}
	message := map[string]any{"role": "assistant", "content": "fixture complete"}
	finishReason := "stop"
	if f.done < len(f.steps) {
		step := f.steps[f.done]
		if !step.denied && !request.advertises(step.name) {
			f.err = "runner did not advertise native tool " + step.name
			http.Error(w, "required native tool was not advertised", http.StatusUnprocessableEntity)
			return
		}
		arguments, err := json.Marshal(step.args)
		if err != nil {
			f.err = "invalid fixture tool arguments"
			http.Error(w, f.err, http.StatusInternalServerError)
			return
		}
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{
			"index": 0, "id": nativeToolCallID(f.done), "type": "function",
			"function": map[string]any{"name": step.name, "arguments": string(arguments)},
		}}
		finishReason = "tool_calls"
		f.issued++
	}
	writeNativeToolChatResponse(w, request, message, finishReason)
}

func (f *nativeToolFixture) acceptResult(request nativeToolChatRequest) error {
	step := f.steps[f.done]
	for _, message := range request.Messages {
		if message.Role != "tool" || message.ToolCallID != nativeToolCallID(f.done) {
			continue
		}
		var output string
		if err := json.Unmarshal(message.Content, &output); err != nil {
			return fmt.Errorf("native %s returned a non-text tool result", step.name)
		}
		if step.denied {
			if strings.Contains(output, nativeToolMarker) {
				return fmt.Errorf("native %s returned the controlled HTTP marker despite restricted policy", step.name)
			}
			if request.advertises(step.name) || !nativeToolDeniedOutput(output) {
				return fmt.Errorf("native %s did not return an explicit denied or unavailable tool result: bytes=%d, advertised=%t", step.name, len(output), request.advertises(step.name))
			}
		} else if !strings.Contains(output, step.want) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(output)), "error:") {
			return fmt.Errorf("native %s returned no successful expected tool output", step.name)
		}
		f.done++
		return nil
	}
	return fmt.Errorf("runner omitted the result for native %s call %s", step.name, nativeToolCallID(f.done))
}

func nativeToolDeniedOutput(output string) bool {
	value := strings.ToLower(output)
	for _, marker := range []string{"permission", "denied", "disabled", "unavailable", "not available", "not enabled", "not allowed"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (r nativeToolChatRequest) advertises(name string) bool {
	for _, tool := range r.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func nativeToolCallID(index int) string { return fmt.Sprintf("call_native_fixture_%d", index) }

func writeNativeToolChatResponse(w http.ResponseWriter, request nativeToolChatRequest, message map[string]any, finishReason string) {
	base := map[string]any{"id": "chatcmpl_native_fixture", "created": 1, "model": request.Model}
	if !request.Stream {
		base["object"] = "chat.completion"
		base["choices"] = []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}}
		writeJSON(w, http.StatusOK, base)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	base["object"] = "chat.completion.chunk"
	base["choices"] = []any{map[string]any{"index": 0, "delta": message, "finish_reason": nil}}
	encoded, _ := json.Marshal(base)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
	base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}
	encoded, _ = json.Marshal(base)
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded)
}

func (f *nativeToolFixture) assertComplete(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != "" {
		t.Fatal(f.err)
	}
	if f.done != len(f.steps) {
		t.Fatalf("verified native tool results = %d, want %d", f.done, len(f.steps))
	}
}

type nativeToolRunner struct {
	client     *acp.Client
	sessionID  string
	toolEvents atomic.Int32
	stop       func()
}

func newNativeToolRunner(t *testing.T, binary string, mode harnessv2.NativeToolPolicyMode, fixture *nativeToolFixture) *nativeToolRunner {
	t.Helper()
	paths, err := acp.PrepareSessionPaths(filepath.Join(t.TempDir(), "sessions"), "native-tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Workspace, "native-read-canary.txt"), []byte(nativeToolMarker), 0600); err != nil {
		t.Fatal(err)
	}
	request := nativeToolSessionRequest(t, mode)
	profile, err := providerProfile(providerKindOpencode, request.Profile.Model, request.Profile.WorkspaceIntent, testOpenCodeModelLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, proxySession, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: providerUpstreamBaseURL(providerKindOpencode, fixture.server.URL), UpstreamBearerToken: "native-fixture-upstream",
		ProviderKind: providerKindOpencode, Model: request.Profile.Model, ModelOutputLimit: 4096,
	})
	t.Cleanup(func() { proxySession.close() })
	if _, err := profile.ProjectSession(request, paths, binding); err != nil {
		t.Fatalf("production native tool policy projection: %v", err)
	}
	if err := profile.PrepareSession(paths); err != nil {
		t.Fatal(err)
	}
	values, err := profile.EnvironmentForSession(request, paths, binding)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := acp.BuildChildEnvironment(paths, acp.EnvironmentConfig{Values: values})
	if err != nil {
		t.Fatal(err)
	}
	return startNativeToolRunner(t, binary, profile.Args, paths, environment)
}

func nativeToolSessionRequest(t *testing.T, mode harnessv2.NativeToolPolicyMode) harnessv2.CreateRuntimeSessionRequest {
	t.Helper()
	var allowed []string
	if mode == harnessv2.NativeToolPolicyRestricted {
		allowed = []string{"read"}
	}
	request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", "", "", allowed, nil, mode == harnessv2.NativeToolPolicyFull)
	request.Profile.ModelLimits = testOpenCodeModelLimits()
	if mode == harnessv2.NativeToolPolicyFull {
		request.Profile.WorkspaceIntent = harnessv2.WorkspaceIntentWrite
	}
	request.Workspace.Intent = request.Profile.WorkspaceIntent
	policy := &request.MCPConfiguration.ToolPolicy
	policy.NativeToolPolicy = mode
	digest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(policy.AllowedToolNames, policy.DisallowedToolNames, policy.AllowBash, mode)
	if err != nil {
		t.Fatal(err)
	}
	request.Profile.ToolPolicyDigest = digest
	request.MCPConfiguration.ToolPolicyDigest = digest
	return request
}

func startNativeToolRunner(t *testing.T, binary string, args []string, paths acp.SessionPaths, environment []string) *nativeToolRunner {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir, command.Env = paths.Workspace, environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error {
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		_ = stdout.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	runner := &nativeToolRunner{}
	runner.client = acp.NewClient(stdout, stdin, acp.Options{
		RequestHandler: func(_ context.Context, request acp.IncomingRequest) (any, *acp.RPCError) {
			if request.Method == acp.MethodRequestPermission {
				return acp.RequestPermissionResponse{Outcome: acp.CancelledPermissionOutcome()}, nil
			}
			return nil, &acp.RPCError{Code: -32601, Message: "client operation is not supported by the native tool fixture"}
		},
		NotificationHandler: func(_ context.Context, notification acp.IncomingNotification) {
			if notification.Method != acp.MethodSessionUpdate {
				return
			}
			var envelope acp.SessionNotification
			var update struct {
				Kind string `json:"sessionUpdate"`
			}
			if json.Unmarshal(notification.Params, &envelope) == nil && json.Unmarshal(envelope.Update, &update) == nil &&
				(update.Kind == "tool_call" || update.Kind == "tool_call_update") {
				runner.toolEvents.Add(1)
			}
		},
	})
	var once sync.Once
	runner.stop = func() {
		once.Do(func() {
			cancel()
			_ = runner.client.Close()
			_ = stdin.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("native OpenCode process did not exit after fixture cleanup")
			}
		})
	}
	t.Cleanup(runner.stop)
	setupCtx, setupCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer setupCancel()
	var initialized acp.InitializeResponse
	if err := runner.client.Call(setupCtx, acp.MethodInitialize, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersion,
		ClientInfo:      &acp.Implementation{Name: "orka-native-tool-fixture", Version: "1"},
	}, &initialized); err != nil {
		t.Fatalf("initialize native OpenCode: %v", err)
	}
	var session acp.NewSessionResponse
	if err := runner.client.Call(setupCtx, acp.MethodSessionNew, acp.NewSessionRequest{CWD: paths.Workspace, MCPServers: []acp.MCPServer{}}, &session); err != nil {
		t.Fatalf("create native OpenCode session: %v", err)
	}
	if initialized.ProtocolVersion != acp.ProtocolVersion || session.SessionID == "" {
		t.Fatal("native OpenCode did not complete ACP initialization")
	}
	runner.sessionID = session.SessionID
	return runner
}

func (r *nativeToolRunner) prompt(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	var result acp.PromptResponse
	if err := r.client.Call(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: r.sessionID, Prompt: []acp.ContentBlock{acp.Text("Run the native tool fixture.")},
	}, &result); err != nil {
		t.Fatalf("native OpenCode prompt: %v", err)
	}
	if result.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("native OpenCode stop reason = %s, want end_turn", result.StopReason)
	}
}

func (r *nativeToolRunner) assertToolEvents(t *testing.T) {
	t.Helper()
	if r.toolEvents.Load() == 0 {
		t.Fatal("native tool results arrived without ACP tool events")
	}
}

func testNativeToolCancellation(t *testing.T, binary string) {
	t.Helper()
	started, disconnected, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var startOnce, disconnectOnce sync.Once
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.(http.Flusher).Flush()
		startOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			disconnectOnce.Do(func() { close(disconnected) })
		case <-release:
		}
	}))
	t.Cleanup(page.Close)
	t.Cleanup(func() { close(release) })
	fixture := newNativeToolFixture(t, []nativeToolStep{{
		name: "webfetch", args: map[string]any{"url": page.URL, "format": "text", "timeout": 60}, want: nativeToolMarker,
	}})
	runner := newNativeToolRunner(t, binary, harnessv2.NativeToolPolicyFull, fixture)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		var result acp.PromptResponse
		err := runner.client.Call(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: runner.sessionID, Prompt: []acp.ContentBlock{acp.Text("Run the held native fetch.")},
		}, &result)
		if err == nil && result.StopReason != acp.StopReasonCancelled {
			err = fmt.Errorf("cancelled native tool prompt stop reason = %s", result.StopReason)
		}
		finished <- err
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("native prompt finished before its fetch started: %v", err)
	case <-ctx.Done():
		t.Fatal("native fetch never reached the controlled HTTP server")
	}
	if err := runner.client.Notify(ctx, acp.MethodSessionCancel, acp.CancelNotification{SessionID: runner.sessionID}); err != nil {
		t.Fatalf("cancel native fetch: %v", err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native OpenCode did not settle its cancelled prompt")
	}
	runner.assertToolEvents(t)
	// The pinned runner can settle ACP cancellation before closing a native
	// response body. Orka must retire its owned process to complete cleanup.
	runner.stop()
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled native fetch survived process cleanup")
	}
}
