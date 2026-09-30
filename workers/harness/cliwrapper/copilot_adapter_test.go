package cliwrapper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/embeddedcli"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestCopilotAdapterBuildCommandUsesHelperAndCLIEnv(t *testing.T) {
	dir := t.TempDir()
	adapter := NewCopilotAdapter(CopilotAdapterConfig{
		HelperPath: "/tmp/fake-copilot-helper",
		Path:       "/tmp/fake-copilot-cli",
		WorkDir:    dir,
	})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt: "hello",
		Env:    []string{"GITHUB_TOKEN=token"},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	if spec.Path != "/tmp/fake-copilot-helper" {
		t.Fatalf("Path = %q, want helper", spec.Path)
	}
	if !slices.Equal(spec.Args, []string{"copilot-turn"}) {
		t.Fatalf("Args = %#v, want copilot-turn", spec.Args)
	}
	if spec.Dir != dir {
		t.Fatalf("Dir = %q, want %q", spec.Dir, dir)
	}
	if string(spec.Stdin) != "hello" {
		t.Fatalf("Stdin = %q, want prompt", string(spec.Stdin))
	}
	if !containsEnv(spec.Env, workerenv.CopilotCLIPath+"=/tmp/fake-copilot-cli") {
		t.Fatalf("Env = %#v, want copilot CLI path", spec.Env)
	}
}

func TestCopilotAdapterRunsFakeHelperThroughWrapper(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "copilot-helper.sh")
	helperScript := `#!/bin/sh
prompt=$(cat)
if [ -z "$prompt" ]; then prompt=$ORKA_PROMPT; fi
printf 'copilot:%s' "$prompt"
`
	if err := os.WriteFile(helper, []byte(helperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = RuntimeCopilot
	cfg.Copilot.HelperPath = helper
	cfg.Copilot.WorkDir = dir
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, NewCopilotAdapter(cfg.Copilot))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if request.Metadata == nil {
		request.Metadata = map[string]string{}
	}
	request.Metadata["runtime"] = RuntimeCopilot
	request.Input.Prompt = "hello copilot"
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if got := strings.TrimSpace(last.Completed.Result); got != "copilot:hello copilot" {
		t.Fatalf("result = %q, want fake copilot output", got)
	}
}

func TestBuildCopilotSessionConfig(t *testing.T) {
	cfg := &common.AgentConfig{
		Model:           "gpt-5.4",
		SystemPrompt:    "system",
		AllowedTools:    []string{" bash ", "create_file"},
		AllowedToolsSet: true,
		DisallowedTools: []string{" web_search "},
	}
	sessionCfg := buildCopilotSessionConfig(cfg, "/workspace/repo")
	if sessionCfg.Model != "gpt-5.4" || sessionCfg.WorkingDirectory != "/workspace/repo" {
		t.Fatalf("session config = %#v", sessionCfg)
	}
	if sessionCfg.SystemMessage == nil ||
		sessionCfg.SystemMessage.Content != "system" ||
		sessionCfg.SystemMessage.Mode != "append" {
		t.Fatalf("SystemMessage = %#v, want append system", sessionCfg.SystemMessage)
	}
	if !slices.Equal(sessionCfg.AvailableTools, []string{"bash", "create_file"}) {
		t.Fatalf("AvailableTools = %#v", sessionCfg.AvailableTools)
	}
	if !slices.Equal(sessionCfg.ExcludedTools, []string{"web_search"}) {
		t.Fatalf("ExcludedTools = %#v", sessionCfg.ExcludedTools)
	}
	if sessionCfg.OnPermissionRequest == nil {
		t.Fatal("OnPermissionRequest = nil, want auto-approve handler")
	}
	permission, err := sessionCfg.OnPermissionRequest(
		copilot.PermissionRequestRead{Path: "/workspace/repo/file.txt"}, copilot.PermissionInvocation{},
	)
	if err != nil || permission == nil || permission.Kind() != rpc.PermissionDecisionKindApproved {
		t.Fatalf("permission = %#v, %v, want approved", permission, err)
	}
}

func TestBuildCopilotSessionConfigExplicitEmptyAllowedToolsDeniesTools(t *testing.T) {
	t.Setenv(workerenv.Prompt, "pure prompt")
	t.Setenv(workerenv.MaxTurns, "")
	t.Setenv(workerenv.AllowedTools, "")
	t.Setenv(workerenv.DisallowedTools, "")
	t.Setenv(workerenv.TimeoutSeconds, "")

	cfg, err := common.LoadConfig(defaultCopilotMaxTurns)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AllowedToolsSet || len(cfg.AllowedTools) != 0 {
		t.Fatalf("AllowedTools = %#v (set=%t), want explicit empty allowlist", cfg.AllowedTools, cfg.AllowedToolsSet)
	}

	sessionCfg := buildCopilotSessionConfig(cfg, "/workspace/repo")
	if sessionCfg.AvailableTools == nil || len(sessionCfg.AvailableTools) != 0 {
		t.Fatalf("AvailableTools = %#v, want explicit empty allowlist", sessionCfg.AvailableTools)
	}
	permission, err := sessionCfg.OnPermissionRequest(
		copilot.PermissionRequestRead{Path: "/workspace/repo/file.txt"}, copilot.PermissionInvocation{},
	)
	if err != nil || permission == nil ||
		permission.Kind() != rpc.PermissionDecisionKindDeniedNoApprovalRuleAndCouldNotRequestFromUser {
		t.Fatalf("permission = %#v, %v, want deny-all result", permission, err)
	}
	if sessionCfg.Hooks == nil || sessionCfg.Hooks.OnPreToolUse == nil {
		t.Fatal("OnPreToolUse = nil, want deny-all hook")
	}
	decision, err := sessionCfg.Hooks.OnPreToolUse(copilot.PreToolUseHookInput{}, copilot.HookInvocation{})
	if err != nil || decision == nil || decision.PermissionDecision != "deny" {
		t.Fatalf("pre-tool decision = %#v, %v, want deny", decision, err)
	}
}

func TestBuildCopilotSessionConfigOmittedAllowedToolsRetainsDefaultBehavior(t *testing.T) {
	sessionCfg := buildCopilotSessionConfig(&common.AgentConfig{}, "/workspace/repo")
	if sessionCfg.AvailableTools != nil {
		t.Fatalf("AvailableTools = %#v, want omitted", sessionCfg.AvailableTools)
	}
	permission, err := sessionCfg.OnPermissionRequest(
		copilot.PermissionRequestRead{Path: "/workspace/repo/file.txt"}, copilot.PermissionInvocation{},
	)
	if err != nil || permission == nil || permission.Kind() != rpc.PermissionDecisionKindApproved {
		t.Fatalf("permission = %#v, %v, want approved", permission, err)
	}
	if sessionCfg.Hooks != nil {
		t.Fatalf("Hooks = %#v, want no deny-all hook", sessionCfg.Hooks)
	}
}

func TestBuildCopilotClientOptions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		path    string
		token   string
		wantCLI string
	}{
		{
			name:    "external CLI",
			path:    " /opt/copilot/bin/copilot ",
			token:   " test-token ",
			wantCLI: "/opt/copilot/bin/copilot",
		},
		{name: "PATH fallback", wantCLI: "copilot"},
		{name: "blank override", path: " \t", token: " ", wantCLI: "copilot"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(workerenv.CopilotCLIPath, tt.path)
			t.Setenv(workerenv.GitHubToken, tt.token)
			t.Setenv("HOME", "/home/worker")
			t.Setenv("ORKA_HARNESS_WRAPPER_TEST", "must-not-reach-child")
			// Explicit stdio must not inherit the SDK's new transport default.
			t.Setenv("COPILOT_SDK_DEFAULT_CONNECTION", "inprocess")

			opts := buildCopilotClientOptions("/workspace/repo")
			connection, ok := opts.Connection.(copilot.StdioConnection)
			if !ok || connection.Path != tt.wantCLI {
				t.Fatalf("connection = %#v, want stdio with path %q", opts.Connection, tt.wantCLI)
			}
			if connection.Env != nil {
				t.Fatal("connection Env must remain unset when client Env is set")
			}
			if opts.WorkingDirectory != "/workspace/repo" {
				t.Fatalf("WorkingDirectory = %q", opts.WorkingDirectory)
			}
			if opts.GitHubToken != strings.TrimSpace(tt.token) {
				t.Fatal("GitHubToken did not preserve the trimmed explicit token")
			}
			if !slices.Equal(opts.Env, sanitizedProcessEnv(os.Environ())) {
				t.Fatal("client environment differs from the sanitized process environment")
			}
			if !containsEnv(opts.Env, "HOME=/home/worker") {
				t.Fatal("client environment lost HOME")
			}
			for _, entry := range opts.Env {
				if strings.HasPrefix(entry, "ORKA_HARNESS_WRAPPER_") {
					t.Fatal("client environment contains wrapper configuration")
				}
			}
			// Exercise SDK option validation without starting a CLI or using auth.
			_ = copilot.NewClient(opts)
		})
	}
}

func TestBuildCopilotClientOptionsEmbeddedCLI(t *testing.T) {
	// SDK bundle configuration and lookup are process-global and one-shot.
	const helperEnv = "ORKA_TEST_COPILOT_EMBEDDED"
	if os.Getenv(helperEnv) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestBuildCopilotClientOptionsEmbeddedCLI$")
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("embedded CLI subprocess: %v\n%s", err, output)
		}
		return
	}

	t.Setenv(workerenv.CopilotCLIPath, "")
	t.Setenv(workerenv.GitHubToken, "")
	fixture := []byte("#!/bin/sh\nexit 0\n")
	digest := sha256.Sum256(fixture)
	embeddedcli.Setup(embeddedcli.Config{
		Cli:     bytes.NewReader(fixture),
		CliHash: digest[:],
		Dir:     t.TempDir(),
	})
	opts := buildCopilotClientOptions("/workspace/repo")
	connection, ok := opts.Connection.(copilot.StdioConnection)
	if !ok || connection.Path == "" || connection.Path == "copilot" || connection.Path != embeddedcli.Path() {
		t.Fatalf("connection = %#v, want installed embedded CLI", opts.Connection)
	}
	installed, err := os.ReadFile(connection.Path)
	if err != nil || !bytes.Equal(installed, fixture) {
		t.Fatalf("embedded CLI fixture was not installed correctly: %v", err)
	}
	t.Setenv(workerenv.CopilotCLIPath, "/opt/copilot/bin/copilot")
	opts = buildCopilotClientOptions("/workspace/repo")
	connection = opts.Connection.(copilot.StdioConnection)
	if connection.Path != "/opt/copilot/bin/copilot" {
		t.Fatalf("explicit CLI did not take precedence over the bundle: %q", connection.Path)
	}
}

func TestCopilotPermissionWireKinds(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  *common.AgentConfig
		want string
	}{
		{name: "omitted", cfg: &common.AgentConfig{}, want: `{"kind":"approved"}`},
		{name: "nil config", want: `{"kind":"approved"}`},
		{
			name: "implicit nonempty",
			cfg:  &common.AgentConfig{AllowedTools: []string{"read_file"}},
			want: `{"kind":"approved"}`,
		},
		{
			name: "explicit empty",
			cfg:  &common.AgentConfig{AllowedToolsSet: true},
			want: `{"kind":"denied-no-approval-rule-and-could-not-request-from-user"}`,
		},
		{
			name: "trimmed empty",
			cfg:  &common.AgentConfig{AllowedToolsSet: true, AllowedTools: []string{" ", "\t"}},
			want: `{"kind":"denied-no-approval-rule-and-could-not-request-from-user"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildCopilotSessionConfig(tt.cfg, "/workspace/repo")
			for _, request := range []copilot.PermissionRequest{
				copilot.PermissionRequestRead{Path: "/workspace/repo/file.txt"},
				copilot.PermissionRequestWrite{FileName: "/workspace/repo/file.txt"},
				copilot.PermissionRequestShell{},
			} {
				decision, err := cfg.OnPermissionRequest(request, copilot.PermissionInvocation{})
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(decision)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != tt.want {
					t.Fatalf("%s permission = %s, want %s", request.Kind(), data, tt.want)
				}
			}
		})
	}
}

func TestExtractCopilotResultTypedEvents(t *testing.T) {
	content := "  assistant content\n"
	summary := "  summary content\n"
	detailed := "  detailed result\n"
	blank := " \t\n"
	for _, tt := range []struct {
		name  string
		event *copilot.SessionEvent
		want  string
	}{
		{name: "nil event"},
		{name: "nil data", event: &copilot.SessionEvent{}},
		{name: "nil assistant", event: &copilot.SessionEvent{Data: (*copilot.AssistantMessageData)(nil)}},
		{
			name:  "assistant",
			event: &copilot.SessionEvent{Data: &copilot.AssistantMessageData{Content: content}},
			want:  content,
		},
		{name: "blank assistant", event: &copilot.SessionEvent{Data: &copilot.AssistantMessageData{Content: blank}}},
		{name: "nil compaction", event: &copilot.SessionEvent{Data: (*copilot.SessionCompactionCompleteData)(nil)}},
		{
			name:  "summary",
			event: &copilot.SessionEvent{Data: &copilot.SessionCompactionCompleteData{SummaryContent: &summary}},
			want:  summary,
		},
		{name: "absent summary", event: &copilot.SessionEvent{Data: &copilot.SessionCompactionCompleteData{}}},
		{
			name:  "blank summary",
			event: &copilot.SessionEvent{Data: &copilot.SessionCompactionCompleteData{SummaryContent: &blank}},
		},
		{name: "nil tool data", event: &copilot.SessionEvent{Data: (*copilot.ToolExecutionCompleteData)(nil)}},
		{name: "absent result", event: &copilot.SessionEvent{Data: &copilot.ToolExecutionCompleteData{}}},
		{
			name: "detailed preferred",
			event: &copilot.SessionEvent{Data: &copilot.ToolExecutionCompleteData{
				Result: &copilot.ToolExecutionCompleteResult{DetailedContent: &detailed, Content: content},
			}},
			want: detailed,
		},
		{
			name: "blank detailed falls back",
			event: &copilot.SessionEvent{Data: &copilot.ToolExecutionCompleteData{
				Result: &copilot.ToolExecutionCompleteResult{DetailedContent: &blank, Content: content},
			}},
			want: content,
		},
		{
			name: "content fallback",
			event: &copilot.SessionEvent{Data: &copilot.ToolExecutionCompleteData{
				Result: &copilot.ToolExecutionCompleteResult{Content: content},
			}},
			want: content,
		},
		{
			name: "blank result",
			event: &copilot.SessionEvent{Data: &copilot.ToolExecutionCompleteData{
				Result: &copilot.ToolExecutionCompleteResult{Content: blank},
			}},
		},
		{name: "unhandled event", event: &copilot.SessionEvent{Data: &copilot.SessionIdleData{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractCopilotResult(tt.event); got != tt.want {
				t.Fatalf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractCopilotResultFromWireEvents(t *testing.T) {
	for _, tt := range []struct {
		name string
		json string
		want string
	}{
		{name: "assistant", json: `{"type":"assistant.message","data":{"content":"  answer\n"}}`, want: "  answer\n"},
		{
			name: "compaction",
			json: `{"type":"session.compaction_complete","data":{"summaryContent":"summary"}}`,
			want: "summary",
		},
		{
			name: "tool completion",
			json: `{"type":"tool.execution_complete","data":{"result":{"detailedContent":"detail","content":"short"}}}`,
			want: "detail",
		},
		{name: "unknown event", json: `{"type":"future.event","data":{"content":"not an assistant response"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var event copilot.SessionEvent
			if err := json.Unmarshal([]byte(tt.json), &event); err != nil {
				t.Fatal(err)
			}
			if got := extractCopilotResult(&event); got != tt.want {
				t.Fatalf("result = %q, want %q", got, tt.want)
			}
		})
	}
}
