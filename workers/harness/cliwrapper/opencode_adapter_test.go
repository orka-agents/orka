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

const (
	opencodeAllowedToolsMetadata = "allowedTools"
	opencodeToolLSP              = "LSP"
	opencodeToolWebSearch        = "WebSearch"
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
	wantXDGConfigHome := filepath.Join(spec.TempFiles[1], "config")
	if xdgConfigHome != wantXDGConfigHome {
		t.Fatalf("XDG_CONFIG_HOME = %q, want writable per-turn config %q", xdgConfigHome, wantXDGConfigHome)
	}
	if !strings.HasPrefix(configPath, spec.TempFiles[1]+string(filepath.Separator)) {
		t.Fatalf("config path = %q, want isolated under scratch directory", configPath)
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
	if provider.Options.APIKey != "{env:"+opencodeEscapedValueEnv+"}" {
		t.Fatalf("apiKey = %q, want environment reference", provider.Options.APIKey)
	}
	limit := provider.Models["kimi-k2"].Limit
	if limit.Context != opencodeModelContextLimit || limit.Output != opencodeModelOutputLimit {
		t.Fatalf("limit = %#v, want context=%d output=%d", limit, opencodeModelContextLimit, opencodeModelOutputLimit)
	}
	if cfg.Permission[opencodePermissionEdit] != opencodePermissionAllow ||
		cfg.Permission[opencodePermissionBash] != opencodePermissionAllow {
		t.Fatalf("permission = %#v, want edit and bash allowed", cfg.Permission)
	}
	if cfg.Permission[opencodePermissionSkill] != opencodePermissionDeny {
		t.Fatalf("permission.skill = %q, want deny", cfg.Permission[opencodePermissionSkill])
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
	if strings.Contains(string(data), "follow repository conventions") {
		t.Fatal("opencode config contains system prompt content")
	}
	if cfg.Share != "disabled" || cfg.AutoUpdate {
		t.Fatalf("share = %q, autoupdate = %v, want disabled and false", cfg.Share, cfg.AutoUpdate)
	}
}

func TestOpencodeAdapterConfigKeepsAPIKeyOutOfFileAndDisablesSnapshots(t *testing.T) {
	value := "value with \"quotes\", \\slashes, {file:/tmp/example}, and\nnewlines"
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir: t.TempDir(),
		Metadata: map[string]string{
			"model": "kimi-k2",
		},
		Env: []string{
			workerenv.OpenAIBaseURL + "=http://models.example/v1",
			workerenv.OpenAIAPIKey + "=" + value,
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
	if strings.Contains(string(data), value) {
		t.Fatal("opencode config contains raw API key")
	}
	if !strings.Contains(string(data), `"snapshot": false`) {
		t.Fatal("opencode config does not disable snapshots")
	}

	escapedValue := envEntryValue(spec.Env, opencodeEscapedValueEnv)
	resolvedConfig := strings.ReplaceAll(string(data), "{env:"+opencodeEscapedValueEnv+"}", escapedValue)
	var resolved opencodeConfig
	if err := json.Unmarshal([]byte(resolvedConfig), &resolved); err != nil {
		t.Fatalf("unmarshal config after environment substitution: %v", err)
	}
	if got := resolved.Provider[opencodeProviderName].Options.APIKey; got != value {
		t.Fatalf("resolved apiKey = %q, want %q", got, value)
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
			opencodeEnableExa + "=true",
			opencodeExperimentalLSPTool + "=true",
			"OPENCODE_EXPERIMENTAL=true",
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

	for _, name := range []string{
		"OPENCODE_AUTO_SHARE",
		"OPENCODE_PERMISSION",
		"OPENCODE_CONFIG_CONTENT",
		opencodeEnableExa,
		opencodeExperimentalLSPTool,
		"OPENCODE_EXPERIMENTAL",
	} {
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

func TestOpencodeAdapterUsesWritablePerTurnConfigHome(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  t.TempDir(),
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)

	scratchDir := spec.TempFiles[1]
	want := filepath.Join(scratchDir, "config")
	got := envEntryValue(spec.Env, "XDG_CONFIG_HOME")
	if got != want {
		t.Fatalf("XDG_CONFIG_HOME = %q, want writable per-turn path %q", got, want)
	}
	if err := os.WriteFile(filepath.Join(got, "opencode", ".gitignore"), []byte("node_modules\n"), 0o600); err != nil {
		t.Fatalf("write OpenCode config metadata: %v", err)
	}
}

func TestOpencodeAdapterEnablesSafeFeatureFlagsForAllowedTools(t *testing.T) {
	t.Setenv(workerenv.AllowedTools, "")
	t.Setenv(workerenv.DisallowedTools, "")
	t.Setenv(opencodeEnableExa, "false")
	t.Setenv(opencodeExperimentalLSPTool, "false")
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt:  "review the repository",
		WorkDir: t.TempDir(),
		Metadata: map[string]string{
			"model":                      "kimi-k2",
			opencodeAllowedToolsMetadata: opencodeToolWebSearch + "," + opencodeToolLSP,
		},
		Env: []string{
			workerenv.OpenAIBaseURL + "=http://models.example/v1",
			"OPENCODE_EXPERIMENTAL=true",
		},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)

	for _, name := range []string{opencodeEnableExa, opencodeExperimentalLSPTool} {
		if got := envEntryValue(spec.Env, name); got != opencodeEnvTrue {
			t.Fatalf("%s = %q, want true for explicitly allowed tool", name, got)
		}
		if slices.Contains(spec.UnsetEnv, name) {
			t.Fatalf("UnsetEnv = %#v, want synthesized %s preserved", spec.UnsetEnv, name)
		}
	}
	if got := envEntryValue(spec.Env, "OPENCODE_EXPERIMENTAL"); got != "" {
		t.Fatalf("OPENCODE_EXPERIMENTAL = %q, want broad feature flag stripped", got)
	}
	if !slices.Contains(spec.UnsetEnv, "OPENCODE_EXPERIMENTAL") {
		t.Fatalf("UnsetEnv = %#v, want broad feature flag removed", spec.UnsetEnv)
	}
}

func TestOpencodeFeatureEnvRespectsDisallowedTools(t *testing.T) {
	featureEnv := opencodeFeatureEnv(&agentEnvConfig{
		AllowedToolsSet: true,
		AllowedTools:    []string{opencodeToolWebSearch, opencodeToolLSP},
		DisallowedTools: []string{opencodeToolWebSearch, opencodeToolLSP},
	})
	if len(featureEnv) != 0 {
		t.Fatalf("opencodeFeatureEnv() = %#v, want no flags for disallowed tools", featureEnv)
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
	if cfg.Permission[opencodePermissionBash] != opencodePermissionDeny {
		t.Fatalf("permission.bash = %q, want deny", cfg.Permission[opencodePermissionBash])
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

	read, ok := permissions["read"].(opencodeReadPermission)
	if !ok {
		t.Fatalf("permission.read = %#v, want patterned read permission", permissions["read"])
	}
	if read.All != opencodePermissionAllow || read.Env != opencodePermissionDeny ||
		read.EnvFiles != opencodePermissionDeny || read.EnvExample != opencodePermissionAllow {
		t.Fatalf("permission.read = %#v, want dotenv files denied except examples", read)
	}
	for _, permission := range []string{opencodePermissionGlob, opencodePermissionGrep} {
		if permissions[permission] != opencodePermissionAllow {
			t.Fatalf("permission %q = %q, want allow", permission, permissions[permission])
		}
	}
	for _, permission := range []string{
		opencodePermissionEdit,
		opencodePermissionBash,
		opencodePermissionWebFetch,
		opencodePermissionWebSearch,
		opencodePermissionTask,
		opencodePermissionExternalDir,
	} {
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
		read, ok := permissions["read"].(opencodeReadPermission)
		if !ok || read.All != opencodePermissionAllow || read.Env != opencodePermissionDeny {
			t.Fatalf("permission.read = %#v, want protected read access for %s", permissions["read"], tool)
		}
		if _, ok := permissions["list"]; ok {
			t.Fatalf("permissions = %#v, want no unsupported list permission", permissions)
		}
	}
}

func TestOpencodePermissionsRespectDisallowedTools(t *testing.T) {
	permissions, err := opencodePermissions(&agentEnvConfig{
		DisallowedTools: []string{"Patch", opencodeToolWebSearch},
		AllowBash:       true,
	})
	if err != nil {
		t.Fatalf("opencodePermissions() error = %v", err)
	}

	if permissions[opencodePermissionEdit] != opencodePermissionDeny {
		t.Fatalf("permission.edit = %q, want deny for Patch", permissions[opencodePermissionEdit])
	}
	if permissions[opencodePermissionWebSearch] != opencodePermissionDeny {
		t.Fatalf("permission.websearch = %q, want deny", permissions[opencodePermissionWebSearch])
	}
	if permissions[opencodePermissionBash] != opencodePermissionAllow {
		t.Fatalf("permission.bash = %q, want allow", permissions[opencodePermissionBash])
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

	if permissions[opencodePermissionEdit] != opencodePermissionDeny {
		t.Fatalf("permission.edit = %q, want deny", permissions[opencodePermissionEdit])
	}
	if permissions[opencodePermissionBash] != opencodePermissionDeny {
		t.Fatalf("permission.bash = %q, want deny", permissions[opencodePermissionBash])
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
	if permissions[opencodePermissionTodoWrite] != opencodePermissionAllow {
		t.Fatalf("permission.todowrite = %q, want allow for TodoRead", permissions[opencodePermissionTodoWrite])
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
		`{"type":"text","part":{"type":"text","text":"final assistant message"}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}` + "\n"
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	if result.Result != "final assistant message" {
		t.Fatalf("Result = %q, want final assistant message", result.Result)
	}
	if result.Metadata[opencodeMetadataAdapter] != RuntimeOpencode {
		t.Fatalf("Metadata = %#v, want opencode adapter", result.Metadata)
	}
}

func TestOpencodeAdapterParseResultUsesOnlyFinalStepText(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	stdout := "" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"intermediate step text"}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"` + opencodeFinishReasonToolCalls + `"}}` + "\n" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"final step text"}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}` + "\n"
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	if result.Result != "final step text" {
		t.Fatalf("Result = %q, want only final step text", result.Result)
	}
}

func TestOpencodeAdapterParseResultRejectsToolCallsWithoutFinalStep(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	stdout := "" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"intermediate step text"}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"` + opencodeFinishReasonToolCalls + `"}}` + "\n"
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err == nil || !strings.Contains(err.Error(), "terminal step_finish event") {
		t.Fatalf("ParseResult() error = %v, want trailing tool-calls rejection", err)
	}
	if result.Result != stdout {
		t.Fatalf("Result = %q, want exact incomplete stdout", result.Result)
	}
}

func TestOpencodeAdapterParseResultRejectsTextlessToolCallsWithoutFinalStep(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	stdout := "" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"` + opencodeFinishReasonToolCalls + `"}}` + "\n"
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err == nil || !strings.Contains(err.Error(), "terminal step_finish event") {
		t.Fatalf("ParseResult() error = %v, want textless trailing tool-calls rejection", err)
	}
	if result.Result != stdout {
		t.Fatalf("Result = %q, want exact incomplete stdout", result.Result)
	}
}

func TestOpencodeAdapterParseResultAggregatesFinalStepText(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	stdout := "" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"first text part"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"second text part"}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}` + "\n"
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err != nil {
		t.Fatalf("ParseResult() error = %v", err)
	}
	if result.Result != "first text part\nsecond text part" {
		t.Fatalf("Result = %q, want all final-step text parts", result.Result)
	}
}

func TestOpencodeAdapterParseResultRejectsBlankFinishReason(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	for _, reason := range []string{"", "   "} {
		stdout := "" +
			`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
			`{"type":"text","part":{"type":"text","text":"partial assistant message"}}` + "\n" +
			`{"type":"step_finish","part":{"type":"step-finish","reason":"` + reason + `"}}` + "\n"
		result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
		if err == nil || !strings.Contains(err.Error(), "terminal step_finish event") {
			t.Fatalf("ParseResult() error = %v, want blank finish reason rejection", err)
		}
		if result.Result != stdout {
			t.Fatalf("Result = %q, want exact failed stdout", result.Result)
		}
	}
}

func TestOpencodeAdapterParseResultRejectsFailedFinish(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	for _, reason := range []string{opencodeFinishReasonUnknown, opencodeFinishReasonError} {
		t.Run(reason, func(t *testing.T) {
			stdout := "" +
				`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
				`{"type":"text","part":{"type":"text","text":"partial assistant message"}}` + "\n" +
				`{"type":"step_finish","part":{"type":"step-finish","reason":"` + reason + `"}}` + "\n"
			result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
			if err == nil || !strings.Contains(err.Error(), reason+" finish reason") {
				t.Fatalf("ParseResult() error = %v, want %s finish rejection", err, reason)
			}
			if result.Result != stdout {
				t.Fatalf("Result = %q, want exact failed stdout", result.Result)
			}
		})
	}
}

func TestOpencodeAdapterParseResultRejectsFinalStepTextWithoutStepFinish(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	stdout := "" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"completed earlier step"}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}` + "\n" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"partial final step"}}` + "\n"
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err == nil || !strings.Contains(err.Error(), "terminal step_finish event") {
		t.Fatalf("ParseResult() error = %v, want missing terminal step_finish rejection", err)
	}
	if result.Result != stdout {
		t.Fatalf("Result = %q, want exact truncated stdout", result.Result)
	}
}

func TestOpencodeAdapterParseResultRejectsStepWithoutFinish(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	stdout := `{"type":"step_start","part":{"type":"step-start"}}` + "\n"
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err == nil || !strings.Contains(err.Error(), "terminal step_finish event") {
		t.Fatalf("ParseResult() error = %v, want missing terminal step_finish rejection", err)
	}
	if result.Result != stdout {
		t.Fatalf("Result = %q, want exact incomplete stdout", result.Result)
	}
}
