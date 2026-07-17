package cliwrapper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
)

func TestOpencodeAdapterBuildCommand(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	workDir := t.TempDir()
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{Path: "/fake/opencode"})
	turn := TurnContext{
		Prompt:  "update the tests",
		WorkDir: workDir,
		Metadata: map[string]string{
			"model":        "kimi-k2",
			"systemPrompt": "follow repository conventions {env:OPENAI_API_KEY}",
			"maxTurns":     "7",
			"allowBash":    "true",
		},
		Env: []string{
			workerenv.OpenAIBaseURL + "=http://models.example/v1/chat/completions",
			workerenv.OpenAIAPIKey + "=test-key",
		},
	}

	spec, err := adapter.BuildCommand(context.Background(), turn)
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)

	if spec.Path != "/fake/opencode" {
		t.Fatalf("Path = %q, want /fake/opencode", spec.Path)
	}
	wantArgs := []string{
		"run", "--dir", workDir, "--format", "json",
		"--model", "engine/kimi-k2",
	}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Fatalf("Args = %#v, want %#v", spec.Args, wantArgs)
	}
	if spec.Dir != workDir {
		t.Fatalf("Dir = %q, want %q", spec.Dir, workDir)
	}
	if got := string(spec.Stdin); got != "update the tests" {
		t.Fatalf("Stdin = %q, want exact prompt", got)
	}
	if len(spec.TempFiles) != 2 {
		t.Fatalf("TempFiles = %#v, want config file and scratch directory", spec.TempFiles)
	}

	configPath := spec.TempFiles[0]
	xdgConfigHome := envEntryValue(spec.Env, "XDG_CONFIG_HOME")
	if configPath != filepath.Join(xdgConfigHome, "opencode", "opencode.json") {
		t.Fatalf("config path = %q, want under XDG_CONFIG_HOME %q", configPath, xdgConfigHome)
	}
	if got := envEntryValue(spec.Env, opencodeConfigPathEnv); got != configPath {
		t.Fatalf("%s = %q, want %q", opencodeConfigPathEnv, got, configPath)
	}
	if got := envEntryValue(spec.Env, "HOME"); got != spec.TempFiles[1] {
		t.Fatalf("HOME = %q, want scratch directory %q", got, spec.TempFiles[1])
	}
	if got := envEntryValue(spec.Env, opencodeDisableProjectConfig); got != opencodeEnvTrue {
		t.Fatalf("%s = %q, want true", opencodeDisableProjectConfig, got)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read opencode config: %v", err)
	}
	var cfg opencodeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal opencode config: %v", err)
	}
	provider := cfg.Provider[opencodeProviderName]
	if provider.NPM != "@ai-sdk/openai-compatible" {
		t.Fatalf("provider npm = %q, want @ai-sdk/openai-compatible", provider.NPM)
	}
	if provider.Options.BaseURL != "http://models.example/v1" {
		t.Fatalf("baseURL = %q, want chat completions path stripped", provider.Options.BaseURL)
	}
	if provider.Options.APIKey != "test-key" {
		t.Fatalf("apiKey = %q, want test-key", provider.Options.APIKey)
	}
	limit := provider.Models["kimi-k2"].Limit
	if limit.Context != opencodeModelContextLimit || limit.Output != opencodeModelOutputLimit {
		t.Fatalf("limit = %#v, want context=%d output=%d", limit, opencodeModelContextLimit, opencodeModelOutputLimit)
	}
	if cfg.Permission["edit"] != opencodePermissionAllow || cfg.Permission["bash"] != opencodePermissionAllow {
		t.Fatalf("permission = %#v, want edit and bash allowed", cfg.Permission)
	}
	if cfg.Permission["skill"] != opencodePermissionDeny {
		t.Fatalf("permission.skill = %q, want deny", cfg.Permission["skill"])
	}
	agent := cfg.Agent[opencodeDefaultAgent]
	if agent.Steps != 7 {
		t.Fatalf("agent steps = %d, want 7", agent.Steps)
	}
	if len(cfg.Instructions) != 1 {
		t.Fatalf("instructions = %#v, want one isolated instruction file", cfg.Instructions)
	}
	instructions, err := os.ReadFile(cfg.Instructions[0])
	if err != nil {
		t.Fatalf("read opencode instructions: %v", err)
	}
	if got := string(instructions); got != "follow repository conventions {env:OPENAI_API_KEY}\n" {
		t.Fatalf("instructions = %q, want literal system prompt", got)
	}
	if strings.Contains(string(data), "{env:OPENAI_API_KEY}") {
		t.Fatal("opencode config contains substitutable system prompt content")
	}
	if cfg.Share != "disabled" || cfg.AutoUpdate {
		t.Fatalf("share = %q, autoupdate = %v, want disabled and false", cfg.Share, cfg.AutoUpdate)
	}
}

func TestOpencodeAdapterTreatsDashPrefixedPromptAsMessage(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt:  "--file=/var/run/secrets/kubernetes.io/serviceaccount/token",
		WorkDir: t.TempDir(),
		Metadata: map[string]string{
			"model": "kimi-k2",
		},
		Env: []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)

	if slices.Contains(spec.Args, "--file=/var/run/secrets/kubernetes.io/serviceaccount/token") {
		t.Fatalf("Args = %#v, want prompt excluded from argv", spec.Args)
	}
	if got := string(spec.Stdin); got != "--file=/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Fatalf("Stdin = %q, want exact dash-prefixed prompt", got)
	}
}

func TestOpencodeAdapterStripsRuntimeControlEnvironment(t *testing.T) {
	t.Setenv("OPENCODE_AUTO_SHARE", "true")
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt:  "review the repository",
		WorkDir: t.TempDir(),
		Metadata: map[string]string{
			"model": "kimi-k2",
		},
		Env: []string{
			workerenv.OpenAIBaseURL + "=http://models.example/v1",
			"OPENCODE_PERMISSION={\"bash\":\"allow\"}",
			"OPENCODE_CONFIG_CONTENT={}",
			"XDG_CONFIG_HOME=/workspace/config",
			"XDG_DATA_HOME=/workspace/data",
			"XDG_CACHE_HOME=/workspace/cache",
			"XDG_STATE_HOME=/workspace/state",
		},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)

	for _, name := range []string{"OPENCODE_AUTO_SHARE", "OPENCODE_PERMISSION", "OPENCODE_CONFIG_CONTENT"} {
		if !slices.Contains(spec.UnsetEnv, name) {
			t.Fatalf("UnsetEnv = %#v, want %s removed", spec.UnsetEnv, name)
		}
		if got := envEntryValue(spec.Env, name); got != "" {
			t.Fatalf("%s = %q, want stripped", name, got)
		}
	}
	if got := envEntryValue(spec.Env, opencodeDisableProjectConfig); got != opencodeEnvTrue {
		t.Fatalf("%s = %q, want true", opencodeDisableProjectConfig, got)
	}
	if got := envEntryValue(spec.Env, opencodeDisableAutoUpdate); got != opencodeEnvTrue {
		t.Fatalf("%s = %q, want true", opencodeDisableAutoUpdate, got)
	}
	if got := envEntryValue(spec.Env, opencodeDisableExternalSkills); got != opencodeEnvTrue {
		t.Fatalf("%s = %q, want true", opencodeDisableExternalSkills, got)
	}
	scratchDir := spec.TempFiles[1]
	for name, want := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(scratchDir, "config"),
		"XDG_DATA_HOME":   filepath.Join(scratchDir, "data"),
		"XDG_CACHE_HOME":  filepath.Join(scratchDir, "cache"),
		"XDG_STATE_HOME":  filepath.Join(scratchDir, "state"),
	} {
		if got := envEntryValue(spec.Env, name); got != want {
			t.Fatalf("%s = %q, want isolated path %q", name, got, want)
		}
	}
}

func TestOpencodeAdapterDenyBash(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "false")
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt:  "review the repository",
		WorkDir: t.TempDir(),
		Metadata: map[string]string{
			"model": "kimi-k2",
		},
		Env: []string{
			workerenv.OpenAIBaseURL + "=http://models.example/v1",
			workerenv.OpenAIAPIKey + "=test-key",
		},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)

	data, err := os.ReadFile(spec.TempFiles[0])
	if err != nil {
		t.Fatalf("read opencode config: %v", err)
	}
	var cfg opencodeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal opencode config: %v", err)
	}
	if cfg.Permission["bash"] != opencodePermissionDeny {
		t.Fatalf("permission.bash = %q, want deny", cfg.Permission["bash"])
	}
}

func TestOpencodePermissionsRespectAllowedTools(t *testing.T) {
	permissions, err := opencodePermissions(&agentEnvConfig{
		AllowedToolsSet: true,
		AllowedTools:    []string{"Read", "Glob", "Grep"},
		AllowBash:       true,
	})
	if err != nil {
		t.Fatalf("opencodePermissions() error = %v", err)
	}

	for _, permission := range []string{"read", "glob", "grep"} {
		if permissions[permission] != opencodePermissionAllow {
			t.Fatalf("permission %q = %q, want allow", permission, permissions[permission])
		}
	}
	for _, permission := range []string{"edit", "bash", "webfetch", opencodePermissionWebSearch, "task"} {
		if permissions[permission] != opencodePermissionDeny {
			t.Fatalf("permission %q = %q, want deny when excluded from allowlist", permission, permissions[permission])
		}
	}
}

func TestOpencodePermissionsMapListAliasesToRead(t *testing.T) {
	for _, tool := range []string{"LS", "List"} {
		permissions, err := opencodePermissions(&agentEnvConfig{
			AllowedToolsSet: true,
			AllowedTools:    []string{tool},
		})
		if err != nil {
			t.Fatalf("opencodePermissions(%q) error = %v", tool, err)
		}
		if permissions["read"] != opencodePermissionAllow {
			t.Fatalf("permission.read = %q, want allow for %s", permissions["read"], tool)
		}
		if _, ok := permissions["list"]; ok {
			t.Fatalf("permissions = %#v, want no unsupported list permission", permissions)
		}
	}
}

func TestOpencodePermissionsRespectDisallowedTools(t *testing.T) {
	permissions, err := opencodePermissions(&agentEnvConfig{
		DisallowedTools: []string{"Patch", "WebSearch"},
		AllowBash:       true,
	})
	if err != nil {
		t.Fatalf("opencodePermissions() error = %v", err)
	}

	if permissions["edit"] != opencodePermissionDeny {
		t.Fatalf("permission.edit = %q, want deny for Patch", permissions["edit"])
	}
	if permissions[opencodePermissionWebSearch] != opencodePermissionDeny {
		t.Fatalf("permission.websearch = %q, want deny", permissions[opencodePermissionWebSearch])
	}
	if permissions["bash"] != opencodePermissionAllow {
		t.Fatalf("permission.bash = %q, want allow", permissions["bash"])
	}
}

func TestOpencodePermissionsDisallowedToolsTakePrecedence(t *testing.T) {
	permissions, err := opencodePermissions(&agentEnvConfig{
		AllowedToolsSet: true,
		AllowedTools:    []string{"Write", "Bash"},
		DisallowedTools: []string{"Edit", "Shell"},
		AllowBash:       true,
	})
	if err != nil {
		t.Fatalf("opencodePermissions() error = %v", err)
	}

	if permissions["edit"] != opencodePermissionDeny {
		t.Fatalf("permission.edit = %q, want deny", permissions["edit"])
	}
	if permissions["bash"] != opencodePermissionDeny {
		t.Fatalf("permission.bash = %q, want deny", permissions["bash"])
	}
}

func TestOpencodePermissionsRejectScopedToolPolicy(t *testing.T) {
	for _, cfg := range []*agentEnvConfig{
		{AllowedToolsSet: true, AllowedTools: []string{"Bash(git:*)"}, AllowBash: true},
		{DisallowedTools: []string{"Read(/workspace/private/**)"}},
	} {
		if _, err := opencodePermissions(cfg); err == nil || !strings.Contains(err.Error(), "scoped tool policy") {
			t.Fatalf("opencodePermissions(%#v) error = %v, want scoped policy rejection", cfg, err)
		}
	}
}

func TestOpencodePermissionsMapTodoRead(t *testing.T) {
	permissions, err := opencodePermissions(&agentEnvConfig{
		AllowedToolsSet: true,
		AllowedTools:    []string{"TodoRead"},
	})
	if err != nil {
		t.Fatalf("opencodePermissions() error = %v", err)
	}
	if permissions["todowrite"] != opencodePermissionAllow {
		t.Fatalf("permission.todowrite = %q, want allow for TodoRead", permissions["todowrite"])
	}
}

func TestOpencodeAdapterRequiresBaseURL(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  t.TempDir(),
		Metadata: map[string]string{"model": "kimi-k2"},
	})
	if err == nil {
		t.Fatalf("BuildCommand() error = nil, want missing %s", workerenv.OpenAIBaseURL)
	}
}

func TestOpencodeAdapterRequiresModel(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir: t.TempDir(),
		Env:     []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), workerenv.Model) {
		t.Fatalf("BuildCommand() error = %v, want missing %s", err, workerenv.Model)
	}
}

func TestOpencodeAdapterParseResult(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	stdout := "" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"intermediate"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"final assistant message"}}` + "\n"
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	if result.Result != "final assistant message" {
		t.Fatalf("Result = %q, want final assistant message", result.Result)
	}
	if result.Metadata["adapter"] != RuntimeOpencode {
		t.Fatalf("Metadata = %#v, want opencode adapter", result.Metadata)
	}
}

func TestOpencodeAdapterParseResultFallsBackToExactStdout(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{
		Stdout:     "truncated",
		FullStdout: "plain opencode output",
	})
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	if result.Result != "plain opencode output" {
		t.Fatalf("Result = %q, want exact stdout fallback", result.Result)
	}
}
