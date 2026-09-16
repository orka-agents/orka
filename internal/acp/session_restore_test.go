package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRuntimeSessionRestoreSelectsMethodAndIsolatesHistory(t *testing.T) {
	for _, test := range []struct{ mode, method string }{
		{"resume", MethodSessionResume},
		{"load", MethodSessionLoad},
		{"load-null-resume", MethodSessionLoad},
		{"load-boolean-resume", MethodSessionLoad},
		{"load-string-resume", MethodSessionLoad},
		{"load-permissions", MethodSessionLoad},
		{"resume-permissions", MethodSessionResume},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cfg := testRestoreConfig(t, test.mode)
			session := startTestRestoreSession(t, cfg)
			if session.ID() != "replacement-session" || session.Generation() != 2 || session.ProviderSessionID() != "provider-saved-session" || session.RestoreMethod() != test.method {
				t.Fatalf("restored identity = %q/%d/%q/%q", session.ID(), session.Generation(), session.ProviderSessionID(), session.RestoreMethod())
			}
			if methods := readRestoreMethods(t, cfg); !reflect.DeepEqual(methods, []string{MethodInitialize, MethodAuthenticate, test.method}) {
				t.Fatalf("restoration issued unexpected requests: %v", methods)
			}
			assertRestoredPromptOnlyContainsNewOutput(t, session)
			if methods := readRestoreMethods(t, cfg); !reflect.DeepEqual(methods, []string{MethodInitialize, MethodAuthenticate, test.method, MethodSessionPrompt}) {
				t.Fatalf("prompt requests = %v", methods)
			}
		})
	}
}

func TestRuntimeSessionRestoreReappliesCurrentSettings(t *testing.T) {
	for _, mode := range []string{"resume-settings", "load-settings"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testRestoreConfig(t, mode)
			cfg.Restore.ModelID = "orka/current-model"
			cfg.Restore.ModeID = "build"
			session := startTestRestoreSession(t, cfg)
			want := []string{MethodInitialize, MethodAuthenticate, session.RestoreMethod(), MethodSessionSetConfigOption, MethodSessionSetConfigOption}
			if methods := readRestoreMethods(t, cfg); !reflect.DeepEqual(methods, want) {
				t.Fatalf("configuration requests = %v, want %v", methods, want)
			}
			assertRestoredPromptOnlyContainsNewOutput(t, session)
		})
	}
}

func TestRuntimeSessionRestoreAllowsNewPromptPermissions(t *testing.T) {
	session := startTestRestoreSession(t, testRestoreConfig(t, "load-new-permission"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := session.StartPrompt(ctx, "new-prompt", "sha256:new-prompt", []ContentBlock{Text("new request")})
	if err != nil {
		t.Fatal(err)
	}
	var permissions int
	for event := range run.Events {
		run.Release(event)
		if event.Type != PromptEventPermissionRequested {
			continue
		}
		permissions++
		if event.Permission == nil || event.Permission.RequestID != "60" {
			t.Fatalf("unexpected current permission: %#v", event.Permission)
		}
		if err := session.ResolvePermission("new-prompt", event.Permission.RequestID, SelectedPermissionOutcome("current-allow")); err != nil {
			t.Fatal(err)
		}
	}
	if result := <-run.Result; permissions != 1 || result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("new prompt permission was denied: permissions=%d result=%#v", permissions, result)
	}
}

func TestRuntimeSessionRestoreRejectsUnavailableOrUnappliedSettings(t *testing.T) {
	for _, mode := range []string{"load-settings-unavailable", "load-settings-mismatch", "load-settings-reverted", "load-settings-timeout", "load-settings-error"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testRestoreConfig(t, mode)
			cfg.Restore.ModelID = "orka/current-model"
			cfg.Restore.ModeID = "build"
			if strings.HasSuffix(mode, "timeout") {
				cfg.RestoreTimeout = 50 * time.Millisecond
			}
			_, err := NewRuntimeSession(context.Background(), cfg)
			_ = assertRestoreFailureCleanup(t, err)
			for _, method := range readRestoreMethods(t, cfg) {
				if method == MethodSessionNew || method == MethodSessionPrompt || method == MethodSessionResume {
					t.Fatalf("failed restoration attempted %s", method)
				}
			}
		})
	}
}

func TestRuntimeSessionRestoreFailuresDoNotFallbackInTheSameProcess(t *testing.T) {
	for _, test := range []struct {
		mode string
		edit func(*RuntimeSessionConfig)
	}{
		{mode: "unsupported"},
		{mode: "resume-error"},
		{mode: "load-wrong-session"},
		{mode: "load-malformed"},
		{mode: "load-null-response"},
		{mode: "load-count", edit: func(cfg *RuntimeSessionConfig) { cfg.MaxRestoreHistoryMessages = 3 }},
		{mode: "load-bytes", edit: func(cfg *RuntimeSessionConfig) { cfg.MaxRestoreHistoryBytes = 1500 }},
		{mode: "load-oversized", edit: func(cfg *RuntimeSessionConfig) { cfg.Process.ClientOptions.MaxMessageBytes = 1024 }},
		{mode: "load-timeout", edit: func(cfg *RuntimeSessionConfig) { cfg.RestoreTimeout = 50 * time.Millisecond }},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cfg := testRestoreConfig(t, test.mode)
			if test.edit != nil {
				test.edit(&cfg)
			}
			_, err := NewRuntimeSession(context.Background(), cfg)
			failure := assertRestoreFailureCleanup(t, err)
			if test.mode == "load-timeout" && !errors.Is(failure, context.DeadlineExceeded) {
				t.Fatalf("timeout cause = %v", failure.Cause)
			}
			var attempted int
			for _, method := range readRestoreMethods(t, cfg) {
				switch method {
				case MethodSessionNew, MethodSessionPrompt:
					t.Fatalf("failed restoration attempted %s", method)
				case MethodSessionResume, MethodSessionLoad:
					attempted++
				}
			}
			if attempted > 1 {
				t.Fatalf("restoration attempted %d methods in the same process", attempted)
			}
		})
	}
}

func TestRuntimeSessionRestoreDeniesHistoryPermissionDispatchedAfterCompletion(t *testing.T) {
	finished := time.Now()
	session := &RuntimeSession{
		providerSessionID: "provider-saved-session",
		restore:           &sessionRestorePhase{finishedAt: finished},
		config:            RuntimeSessionConfig{PermissionTimeout: time.Second},
		active: &activePrompt{
			id:          "new-prompt",
			accepted:    true,
			permissions: make(map[string]*pendingPermission),
		},
	}
	response, rpcErr := session.handleRequest(context.Background(), IncomingRequest{
		ID:         json.RawMessage(`50`),
		Method:     MethodRequestPermission,
		ReceivedAt: finished.Add(-time.Millisecond),
		Params:     json.RawMessage(`{"sessionId":"provider-saved-session","toolCall":{"toolCallId":"old"},"options":[{"optionId":"allow","kind":"allow_once","name":"Allow"}]}`),
	})
	permission, ok := response.(RequestPermissionResponse)
	if rpcErr != nil || !ok || permission.Outcome.Outcome != permissionOutcomeCancelled || len(session.active.permissions) != 0 {
		t.Fatalf("history permission escaped its phase: response=%#v error=%v", response, rpcErr)
	}
}

func TestRestoreHistoryValidatesPinnedUpdateStructures(t *testing.T) {
	for _, raw := range []string{
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"history"}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"old","title":"Read","status":"completed","content":[{"type":"content","content":{"type":"text","text":"private result"}}]}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"old","rawOutput":{"result":"history"}}`,
		`{"sessionUpdate":"plan","entries":[{"content":"done","priority":"medium","status":"completed"}]}`,
		`{"sessionUpdate":"available_commands_update","availableCommands":[]}`,
		`{"sessionUpdate":"config_option_update","configOptions":[]}`,
		`{"sessionUpdate":"usage_update","used":100,"size":200}`,
	} {
		if err := validateRestoreHistoryUpdate(json.RawMessage(raw)); err != nil {
			t.Fatalf("valid update was rejected: %v", err)
		}
	}
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"sessionUpdate":42}`,
		`{"sessionUpdate":"unknown","content":{"type":"text","text":"history"}}`,
		`{"sessionUpdate":"agent_message_chunk"}`,
		`{"sessionUpdate":"agent_message_chunk","content":null}`,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":42}}`,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"resource","resource":{}}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"old"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":42}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"old","content":[{}]}`,
		`{"sessionUpdate":"plan","entries":[{}]}`,
		`{"sessionUpdate":"config_option_update","configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"x","options":[{}]}]}`,
		`{"sessionUpdate":"usage_update","used":-1,"size":200}`,
		`{"sessionUpdate":"usage_update","used":1.5,"size":200}`,
	} {
		if err := validateRestoreHistoryUpdate(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid history update was accepted: %s", raw)
		}
	}
}

func TestSessionRestoreErrorOmitsPrivateAdapterText(t *testing.T) {
	private := &RPCError{Code: -32603, Message: "private-history-marker"}
	failure := &SessionRestoreError{Cause: private, Cleanup: CleanupStatus{Proven: true}}
	if strings.Contains(failure.Error(), private.Message) || !errors.Is(failure, private) {
		t.Fatalf("restore error did not keep private cause separate: %v", failure)
	}
}

func testRestoreConfig(t *testing.T, mode string) RuntimeSessionConfig {
	t.Helper()
	root, err := os.MkdirTemp("", "orka-acp-restore-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	paths, err := PrepareSessionPaths(filepath.Join(root, "sessions"), "replacement-session")
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 65534, 65534
	}
	if err := FinalizeSessionOwnership(paths.Root, uid, gid); err != nil {
		t.Fatal(err)
	}
	env, err := BuildChildEnvironment(paths, EnvironmentConfig{Values: map[string]string{
		"GO_WANT_ACP_RESTORE_HELPER": "1",
		"ACP_RESTORE_HELPER_MODE":    mode,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return RuntimeSessionConfig{
		ID: "replacement-session", Generation: 2, ProfileDigest: "sha256:current-profile",
		Process: ProcessConfig{
			Command: testAdapterCommand(t), Args: []string{"-test.run=^TestACPRestoreHelperProcess$"},
			Environment: env, Paths: paths, UID: uid, GID: gid, ExecHelperCommand: testExecHelperCommand(t),
		},
		MCPServers:     []MCPServer{{Type: "http", Name: "current-tools", URL: "http://current.invalid/mcp"}},
		NewSessionMeta: Meta{"systemPrompt": "current instructions"},
		AuthMethodID:   "current-auth",
		Restore:        &SessionRestore{SessionID: "provider-saved-session"},
		RestoreTimeout: 3 * time.Second,
		PromptLease:    3 * time.Second,
		CancelGrace:    25 * time.Millisecond,
	}
}

func startTestRestoreSession(t *testing.T, cfg RuntimeSessionConfig) *RuntimeSession {
	t.Helper()
	session, err := NewRuntimeSession(context.Background(), cfg)
	if err != nil {
		if failure, ok := errors.AsType[*SessionRestoreError](err); ok {
			t.Fatalf("restore session: %v; cause=%v", failure, failure.Cause)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = session.Delete(ctx)
	})
	return session
}

func readRestoreMethods(t *testing.T, cfg RuntimeSessionConfig) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cfg.Process.Paths.Home, "restore-methods"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func assertRestoreFailureCleanup(t *testing.T, err error) *SessionRestoreError {
	t.Helper()
	failure, ok := errors.AsType[*SessionRestoreError](err)
	if !ok || failure.Cause == nil {
		t.Fatalf("restore error did not expose cleanup evidence: %v", err)
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		if !failure.Cleanup.Proven || failure.CleanupErr != nil || len(failure.Cleanup.RemainingPIDs) != 0 {
			t.Fatalf("failed child cleanup was not proven: %#v, %v", failure.Cleanup, failure.CleanupErr)
		}
	} else if runtime.GOOS != "linux" && (failure.Cleanup.Proven || failure.CleanupErr == nil) {
		t.Fatalf("unsupported descendant inventory claimed proven cleanup: %#v, %v", failure.Cleanup, failure.CleanupErr)
	}
	return failure
}

func assertRestoredPromptOnlyContainsNewOutput(t *testing.T, session *RuntimeSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := session.StartPrompt(ctx, "new-prompt", "sha256:new-prompt", []ContentBlock{Text("new request")})
	if err != nil {
		t.Fatal(err)
	}
	var accepted, updates int
	for event := range run.Events {
		run.Release(event)
		switch event.Type {
		case PromptEventAccepted:
			accepted++
		case PromptEventUpdate:
			updates++
			var update struct{ Content ContentBlock }
			if event.Update == nil || json.Unmarshal(event.Update.Update, &update) != nil || update.Content.Text != "new-only" {
				t.Fatalf("old history entered new prompt events: %#v", event)
			}
		default:
			t.Fatalf("restoration caused a new prompt event: %#v", event)
		}
	}
	result := <-run.Result
	if accepted != 1 || updates != 1 || result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("new prompt events accepted=%d updates=%d result=%#v", accepted, updates, result)
	}
}

//nolint:gocyclo // Keep protocol order and hostile restoration cases in one controlled adapter fixture.
func TestACPRestoreHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_RESTORE_HELPER") != "1" {
		return
	}
	mode := os.Getenv("ACP_RESTORE_HELPER_MODE")
	log, err := os.OpenFile(filepath.Join(os.Getenv("HOME"), "restore-methods"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), "restore-pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, writer := bufio.NewReader(os.Stdin), bufio.NewWriter(os.Stdout)
	authenticated, restored := false, false
	modelID, modeID := "old-provider/old-model", "old-mode"
	for {
		message, err := readTestMessage(reader)
		if err != nil {
			os.Exit(0)
		}
		if message.Method != "" {
			if _, err := fmt.Fprintln(log, message.Method); err != nil {
				t.Fatal(err)
			}
		}
		respond := func(result any) {
			writeACPHelper(writer, map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
		}
		switch message.Method {
		case MethodInitialize:
			capabilities := AgentCapabilities{LoadSession: mode != "unsupported", MCPCapabilities: map[string]any{"http": true}}
			if strings.HasPrefix(mode, "resume") {
				capabilities.SessionCapabilities = map[string]any{"resume": map[string]any{}}
			} else if strings.HasSuffix(mode, "null-resume") {
				capabilities.SessionCapabilities = map[string]any{"resume": nil}
			} else if strings.HasSuffix(mode, "boolean-resume") {
				capabilities.SessionCapabilities = map[string]any{"resume": true}
			} else if strings.HasSuffix(mode, "string-resume") {
				capabilities.SessionCapabilities = map[string]any{"resume": "supported"}
			}
			respond(InitializeResponse{ProtocolVersion: ProtocolVersion, AgentCapabilities: capabilities, AuthMethods: []AuthMethod{{ID: "current-auth", Name: "Current auth"}}})
		case MethodAuthenticate:
			var request AuthenticateRequest
			if json.Unmarshal(message.Params, &request) != nil || request.MethodID != "current-auth" {
				t.Fatal("restore did not authenticate using current settings")
			}
			authenticated = true
			respond(AuthenticateResponse{})
		case MethodSessionResume, MethodSessionLoad:
			if !authenticated || restored {
				t.Fatal("restore occurred without authentication or was repeated")
			}
			var request ResumeSessionRequest
			if err := json.Unmarshal(message.Params, &request); err != nil || !validTestRestoreRequest(request) {
				t.Fatal("restore did not carry the saved identity and current settings")
			}
			if mode == "resume-error" {
				writeACPHelper(writer, map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": &RPCError{Code: -32603, Message: "private restore error"}})
				continue
			}
			if strings.HasSuffix(mode, "timeout") && !strings.Contains(mode, "settings") {
				time.Sleep(20 * time.Second)
				continue
			}
			if message.Method == MethodSessionLoad {
				emitTestRestoreHistory(writer, mode)
			}
			if strings.Contains(mode, "permissions") {
				assertHelperRestoreRequestsDenied(t, reader, writer)
			}
			restored = true
			if mode == "load-null-response" {
				respond(nil)
			} else if strings.Contains(mode, "settings") {
				respond(map[string]any{"configOptions": testRestoreConfigOptions(modelID, modeID, mode)})
			} else {
				respond(ResumeSessionResponse{})
			}
		case MethodSessionSetConfigOption:
			var request SetSessionConfigOptionRequest
			if !restored || json.Unmarshal(message.Params, &request) != nil || request.SessionID != "provider-saved-session" {
				t.Fatal("settings changed outside the restored session")
			}
			if strings.HasSuffix(mode, "timeout") {
				time.Sleep(20 * time.Second)
				continue
			}
			if strings.HasSuffix(mode, "error") {
				writeACPHelper(writer, map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": &RPCError{Code: -32601, Message: "unsupported setting"}})
				continue
			}
			if request.ConfigID == "model" && request.Value == "orka/current-model" {
				modelID = request.Value
			} else if request.ConfigID == "mode" && request.Value == "build" {
				modeID = request.Value
			} else {
				t.Fatal("unexpected current setting")
			}
			if strings.HasSuffix(mode, "mismatch") {
				modeID = "old-mode"
			}
			if strings.HasSuffix(mode, "reverted") && request.ConfigID == "mode" {
				modelID = "old-provider/old-model"
			}
			assertHelperRestoreRequestsDenied(t, reader, writer)
			respond(map[string]any{"configOptions": testRestoreConfigOptions(modelID, modeID, mode)})
		case MethodSessionPrompt:
			if !restored || (strings.Contains(mode, "settings") && (modelID != "orka/current-model" || modeID != "build")) {
				t.Fatal("prompt ran before restoration applied current settings")
			}
			var request PromptRequest
			if json.Unmarshal(message.Params, &request) != nil || request.SessionID != "provider-saved-session" || len(request.Prompt) != 1 || request.Prompt[0].Text != "new request" {
				t.Fatal("prompt duplicated history or changed the provider session")
			}
			if mode == "load-new-permission" {
				writeACPHelper(writer, map[string]any{"jsonrpc": "2.0", "id": 60, "method": MethodRequestPermission, "params": map[string]any{
					"sessionId": "provider-saved-session", "toolCall": map[string]any{"toolCallId": "current-tool", "title": "Current action"},
					"options": []PermissionOption{{OptionID: "current-allow", Name: "Allow", Kind: "allow_once"}},
				}})
				permission, err := readTestMessage(reader)
				var result RequestPermissionResponse
				if err != nil || permission.Error != nil || json.Unmarshal(permission.Result, &result) != nil || result.Outcome.Outcome != permissionOutcomeSelected || result.Outcome.OptionID != "current-allow" {
					t.Fatal("current prompt permission was not approved")
				}
			}
			writeACPHelper(writer, map[string]any{"jsonrpc": "2.0", "method": MethodSessionUpdate, "params": map[string]any{"sessionId": "provider-saved-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": Text("new-only")}}})
			respond(PromptResponse{StopReason: StopReasonEndTurn})
		case MethodCancelRequest:
		case MethodSessionNew:
			t.Fatal("restore attempted same-process reconstruction")
		default:
			t.Fatal("unexpected method during restoration")
		}
	}
}

func validTestRestoreRequest(request ResumeSessionRequest) bool {
	cwd, err := os.Getwd()
	return err == nil && request.SessionID == "provider-saved-session" && request.CWD == cwd && len(request.MCPServers) == 1 &&
		request.MCPServers[0].Name == "current-tools" && request.MCPServers[0].URL == "http://current.invalid/mcp" &&
		request.Meta["systemPrompt"] == "current instructions" && request.Meta[sessionMetaRuntimeSessionID] == "replacement-session" &&
		request.Meta[sessionMetaGeneration] == float64(2) && request.Meta[sessionMetaRuntimeProfileDigest] == "sha256:current-profile"
}

func emitTestRestoreHistory(writer *bufio.Writer, mode string) {
	sessionID := "provider-saved-session"
	if mode == "load-wrong-session" {
		sessionID = "another-private-session"
	}
	updates := []any{
		map[string]any{"sessionUpdate": "user_message_chunk", "content": Text("old request")},
		map[string]any{"sessionUpdate": "tool_call", "toolCallId": "old-tool", "title": "Read", "status": "pending"},
		map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "old-tool", "status": "completed", "rawOutput": "unique old result"},
		map[string]any{"sessionUpdate": "agent_message_chunk", "content": Text("old answer")},
		map[string]any{"sessionUpdate": "usage_update", "used": 100, "size": 200},
	}
	if mode == "load-malformed" {
		updates = []any{map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": 42}}}
	}
	if mode == "load-bytes" || mode == "load-oversized" {
		updates = []any{map[string]any{"sessionUpdate": "agent_message_chunk", "content": Text(strings.Repeat("x", 2000))}}
	}
	for _, update := range updates {
		writeACPHelper(writer, map[string]any{"jsonrpc": "2.0", "method": MethodSessionUpdate, "params": map[string]any{"sessionId": sessionID, "update": update}})
	}
}

func assertHelperRestoreRequestsDenied(t *testing.T, reader *bufio.Reader, writer *bufio.Writer) {
	t.Helper()
	writeACPHelper(writer, map[string]any{"jsonrpc": "2.0", "id": 50, "method": MethodRequestPermission, "params": map[string]any{
		"sessionId": "provider-saved-session", "toolCall": map[string]any{"toolCallId": "old-tool", "title": "Old action"},
		"options": []PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}},
	}})
	permission, err := readTestMessage(reader)
	if err != nil {
		t.Fatal(err)
	}
	var outcome RequestPermissionResponse
	if permission.Error != nil || json.Unmarshal(permission.Result, &outcome) != nil || outcome.Outcome.Outcome != permissionOutcomeCancelled {
		t.Fatal("old permission was not denied")
	}
	writeACPHelper(writer, map[string]any{"jsonrpc": "2.0", "id": 51, "method": "fs/write_text_file", "params": map[string]any{"sessionId": "provider-saved-session", "path": "/workspace/forbidden", "content": "old action"}})
	action, err := readTestMessage(reader)
	if err != nil || action.Error == nil || action.Error.Code != -32601 {
		t.Fatal("restore allowed a filesystem action")
	}
}

func testRestoreConfigOptions(modelID, modeID, fixtureMode string) []map[string]any {
	modelOptions := []map[string]any{{"value": "old-provider/old-model", "name": "Old"}, {"value": "orka/current-model", "name": "Current"}}
	if strings.HasSuffix(fixtureMode, "unavailable") {
		modelOptions = modelOptions[:1]
	}
	return []map[string]any{
		{"id": "model", "name": "Model", "type": "select", "currentValue": modelID, "options": []map[string]any{{"group": "current-provider", "name": "Provider", "options": modelOptions}}},
		{"id": "mode", "name": "Mode", "type": "select", "currentValue": modeID, "options": []map[string]any{{"value": "old-mode", "name": "Old"}, {"value": "build", "name": "Build"}}},
	}
}
