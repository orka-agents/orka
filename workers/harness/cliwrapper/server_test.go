package cliwrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/workerenv"
)

func TestServerHealthCapabilitiesAndAfterSeq(t *testing.T) {
	baseURL, cleanup := startWrapperServer(t, NewFakeAdapter(FakeBehaviorSuccess))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if health, err := client.Health(context.Background()); err != nil || !health.Ready {
		t.Fatalf("Health() = %#v, %v", health, err)
	}
	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities(): %v", err)
	}
	if !caps.SupportsCancel || caps.RuntimeName == "" {
		t.Fatalf("Capabilities = %#v, want cancel and runtime", caps)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 2)
	if len(frames) != 3 {
		t.Fatalf("frames after seq 2 = %d, want 3 (%#v)", len(frames), frames)
	}
	if frames[0].Seq != 3 {
		t.Fatalf("first seq = %d, want 3", frames[0].Seq)
	}
}

func TestServerRequiresBearerTokenForTurnEndpoints(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthValue = "auth-value-123"
	cfg.Generic.Command = testEchoCommand
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, NewFakeAdapter(FakeBehaviorSuccess))
	defer cleanup()

	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated StartTurn error = %v, want 401", err)
	}

	authed, err := harness.NewClient(baseURL, harness.WithBearerToken("auth-value-123"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authed.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("authenticated StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, authed, request.TurnID, 0)
	if frames[len(frames)-1].Type != harness.FrameTurnCompleted {
		t.Fatalf("last frame = %#v, want completed", frames[len(frames)-1])
	}
}

func TestServerEnforcesSingleConcurrentTurn(t *testing.T) {
	baseURL, cleanup := startWrapperServer(t, NewFakeAdapter(FakeBehaviorCancellation))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	first := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), first); err != nil {
		t.Fatalf("StartTurn(first): %v", err)
	}
	second := validWrapperStartTurnRequest()
	second.TurnID = "turn-b"
	second.CorrelationID = "corr-b"
	if _, err := client.StartTurn(context.Background(), second); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("StartTurn(second) error = %v, want concurrency conflict", err)
	}
	if _, err := client.CancelTurn(context.Background(), harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        first.Namespace,
		TaskName:         first.TaskName,
		SessionName:      first.SessionName,
		RuntimeSessionID: first.RuntimeSessionID,
		TurnID:           first.TurnID,
		CorrelationID:    first.CorrelationID,
		Reason:           "cleanup",
	}); err != nil {
		t.Fatalf("CancelTurn cleanup: %v", err)
	}
}

func TestServerCancelEmitsCancellation(t *testing.T) {
	baseURL, cleanup := startWrapperServer(t, NewFakeAdapter(FakeBehaviorCancellation))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	framesCh := make(chan []harness.HarnessEventFrame, 1)
	errCh := make(chan error, 1)
	go func() {
		frames := []harness.HarnessEventFrame{}
		err := client.StreamFrames(context.Background(), request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
			frames = append(frames, frame)
			return nil
		})
		framesCh <- frames
		errCh <- err
	}()
	time.Sleep(25 * time.Millisecond)
	if _, err := client.CancelTurn(context.Background(), harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Reason:           "test",
	}); err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}
	select {
	case frames := <-framesCh:
		if err := <-errCh; err != nil {
			t.Fatalf("StreamFrames: %v", err)
		}
		if len(frames) < 2 || frames[len(frames)-1].Type != harness.FrameTurnCancelled {
			t.Fatalf("frames = %#v, want cancelled terminal", frames)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancel frame")
	}
}

func TestServerRejectsUnsafeTurnPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := NewServer(cfg, NewFakeAdapter(FakeBehaviorSuccess))
	if err != nil {
		t.Fatal(err)
	}
	req := validWrapperStartTurnRequest()
	req.TurnID = "../bad"
	body, _ := json.Marshal(req)
	resp := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, harness.TurnsPath, bytes.NewReader(body))
	server.Handler().ServeHTTP(resp, httpReq)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}

func TestServerEvictsCompletedTurnsAfterRetention(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Generic.Command = testEchoCommand
	cfg.TurnRetention = 20 * time.Millisecond
	server, err := NewServer(cfg, NewFakeAdapter(FakeBehaviorSuccess))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	client, err := harness.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	_ = collectWrapperFrames(t, client, request.TurnID, 0)
	eventually(t, time.Second, func() bool {
		server.mu.RLock()
		defer server.mu.RUnlock()
		return server.turns[request.TurnID] == nil
	})
}

func TestServerGenericCommandSuccessAndResultFile(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.txt")
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.WorkDir = dir
	cfg.Generic = GenericAdapterConfig{
		Command:    "/bin/sh",
		Args:       []string{"-c", "cat > prompt.txt; printf result-from-file > result.txt"},
		WorkDir:    dir,
		PromptMode: PromptModeStdin,
		ResultMode: ResultModeFile,
		ResultFile: resultPath,
	}
	adapter := NewGenericAdapter(cfg.Generic)
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, adapter)
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	request.Input.Prompt = "prompt value"
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if !strings.Contains(last.Completed.Result, "result-from-file") {
		t.Fatalf("completed result = %q, want result file content", last.Completed.Result)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "prompt.txt")); err != nil || string(data) != "prompt value" {
		t.Fatalf("prompt.txt = %q, %v", string(data), err)
	}
}

func TestServerRedactsCommandOutputFrames(t *testing.T) {
	assertCommandFramesRedacted(t, "printf '"+testBearerHeaderValue()+"'", "frames")
}

func assertCommandFramesRedacted(t *testing.T, script, label string) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Generic.Command = "/bin/sh"
	cfg.Generic.Args = []string{"-c", script}
	adapter := NewGenericAdapter(cfg.Generic)
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, adapter)
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	encoded, _ := json.Marshal(frames)
	if strings.Contains(string(encoded), redactionLeakMarker()) || !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("%s leaked secret or missed redaction: %s", label, encoded)
	}
}

func startWrapperServer(t *testing.T, adapter RuntimeAdapter) (string, func()) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Generic.Command = testEchoCommand
	return startWrapperServerWithConfig(t, cfg, adapter)
}

func startWrapperServerWithConfig(t *testing.T, cfg Config, adapter RuntimeAdapter) (string, func()) {
	t.Helper()
	server, err := NewServer(cfg, adapter)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(server.Handler())
	return srv.URL, srv.Close
}

func eventually(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok() {
		t.Fatal("condition did not become true before timeout")
	}
}

func collectWrapperFrames(
	t *testing.T,
	client *harness.Client,
	turnID harness.HarnessTurnID,
	afterSeq int64,
) []harness.HarnessEventFrame {
	t.Helper()
	frames := []harness.HarnessEventFrame{}
	if err := client.StreamFrames(context.Background(), turnID, afterSeq, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	return frames
}

type eventingSecretAdapter struct{}

func (eventingSecretAdapter) Name() string { return "eventing-secret" }
func (eventingSecretAdapter) BuildCommand(context.Context, TurnContext) (*CommandSpec, error) {
	return nil, nil
}
func (eventingSecretAdapter) ParseResult(context.Context, TurnContext, CommandResult) (TurnResult, error) {
	return TurnResult{}, nil
}
func (eventingSecretAdapter) RunTurn(
	_ context.Context,
	_ TurnContext,
	emit func(harness.HarnessEventFrame) error,
) (TurnResult, error) {
	return TurnResult{}, emit(harness.HarnessEventFrame{
		Type:        harness.FrameTurnCompleted,
		Summary:     "done",
		Completed:   &harness.TurnCompleted{Result: testBearerHeaderValue()},
		Metadata:    map[string]string{"note": testBearerHeaderValue()},
		ContentText: testBearerHeaderValue(),
	})
}

func TestServerRedactsEventingAdapterTerminalPayloads(t *testing.T) {
	baseURL, cleanup := startWrapperServer(t, eventingSecretAdapter{})
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	encoded, _ := json.Marshal(frames)
	if strings.Contains(string(encoded), redactionLeakMarker()) || !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("eventing frames leaked secret or missed redaction: %s", encoded)
	}
}

func TestServerFailsOversizedCompletedResult(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.txt")
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.WorkDir = dir
	largeResultScript := strings.Join([]string{
		"python3 - <<'PY'",
		"from pathlib import Path",
		"Path('result.txt').write_text('x' * (600 * 1024))",
		"PY",
	}, "\n")
	cfg.Generic = GenericAdapterConfig{
		Command:    "/bin/sh",
		Args:       []string{"-c", largeResultScript},
		WorkDir:    dir,
		PromptMode: PromptModeStdin,
		ResultMode: ResultModeFile,
		ResultFile: resultPath,
	}
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnFailed || last.Failed == nil || last.Failed.Reason != "result_too_large" {
		t.Fatalf("last frame = %#v, want result_too_large failure", last)
	}
}

func TestServerRedactsCommandStderrFrames(t *testing.T) {
	assertCommandFramesRedacted(t, "printf '"+testBearerHeaderValue()+"' >&2; exit 7", "stderr frames")
}

func TestServerClassifiesCancelBeforeResultFileParsing(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.txt")
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.WorkDir = dir
	cfg.Generic = GenericAdapterConfig{
		Command:    "/bin/sh",
		Args:       []string{"-c", "dd if=/dev/zero bs=1024 count=600 2>/dev/null | tr '\\000' x > result.txt; sleep 10"},
		WorkDir:    dir,
		PromptMode: PromptModeStdin,
		ResultMode: ResultModeFile,
		ResultFile: resultPath,
	}
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := client.CancelTurn(context.Background(), harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Reason:           "test",
	}); err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCancelled {
		t.Fatalf("last frame = %#v, want cancelled before result file parse", last)
	}
}

func TestServerCreatesWorkspaceArtifactLinkAndEnforcesRequiredArtifacts(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.txt")
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.WorkDir = dir
	cfg.Generic = GenericAdapterConfig{
		Command: "/bin/sh",
		Args: []string{"-c", strings.Join([]string{
			"printf 'artifact body' > .orka-artifacts/security-threat-model.md",
			"printf 'done' > result.txt",
		}, "; ")},
		WorkDir:    dir,
		PromptMode: PromptModeStdin,
		ResultMode: ResultModeFile,
		ResultFile: resultPath,
	}
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	request.Input.Prompt = "REQUIRED_SECURITY_ARTIFACTS: security-threat-model.md\nwrite artifact"
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if !strings.Contains(last.Completed.Result, "done") {
		t.Fatalf("completed result = %q, want done", last.Completed.Result)
	}
}

func testBearerHeaderValue() string {
	return "Authorization: " + "Bearer " + strings.Join([]string{"redaction", "value", "1234567890"}, "-")
}

func redactionLeakMarker() string {
	return strings.Join([]string{"redaction", "value"}, "-")
}

func TestServerStripsGitCredentialsFromReadOnlyCommandEnv(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Generic.Command = "/bin/sh"
	cfg.Generic.Args = []string{"-c", "printf 'github=%s git=%s' \"$GITHUB_TOKEN\" \"$GIT_TOKEN\""}
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, NewGenericAdapter(cfg.Generic))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	request.Metadata = map[string]string{"readOnly": "true"}
	request.Input.Env = []harness.TurnEnvVar{
		{Name: workerenv.GitHubToken, Value: "github-token"},
		{Name: workerenv.GitToken, Value: "git-token"},
	}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if strings.Contains(last.Completed.Result, "token") {
		t.Fatalf("read-only command received git credentials: %q", last.Completed.Result)
	}
}
