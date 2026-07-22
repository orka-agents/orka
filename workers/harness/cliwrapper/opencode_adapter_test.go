package cliwrapper

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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
	resolvedWorkDir := resolveOpencodeTestDir(t, workDir)
	wantArgs := []string{
		"run", "--dir", resolvedWorkDir, "--format", "json",
		"--model", "engine/kimi-k2",
	}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Fatalf("Args = %#v, want %#v", spec.Args, wantArgs)
	}
	if spec.Dir != resolvedWorkDir {
		t.Fatalf("Dir = %q, want %q", spec.Dir, resolvedWorkDir)
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
	if cfg.Permission["grep"] != opencodePermissionDeny {
		t.Fatalf("permission.grep = %q, want deny", cfg.Permission["grep"])
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

func TestOpencodeAdapterUsesModelLimitOverrides(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir: t.TempDir(),
		Metadata: map[string]string{
			"model":         "kimi-k2",
			"contextWindow": "64000",
			"maxTokens":     "32000",
		},
		Env: []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
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
	limit := cfg.Provider[opencodeProviderName].Models["kimi-k2"].Limit
	if limit.Context != 64000 || limit.Output != 32000 {
		t.Fatalf("limit = %#v, want context=64000 output=32000", limit)
	}
}

func TestOpencodeAdapterRejectsUnusableModelLimitCombinations(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata map[string]string
		want     string
	}{
		{
			name:     "output exceeds opencode cap",
			metadata: map[string]string{"maxTokens": "32001"},
			want:     "must not exceed 32000",
		},
		{
			name:     "context equals default output",
			metadata: map[string]string{"contextWindow": "8192"},
			want:     "contextWindow must be greater than maxTokens after defaults",
		},
		{
			name:     "explicit context equals output",
			metadata: map[string]string{"contextWindow": "10000", "maxTokens": "10000"},
			want:     "contextWindow must be greater than maxTokens after defaults",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata := map[string]string{"model": "kimi-k2"}
			maps.Copy(metadata, tt.metadata)
			_, err := NewOpencodeAdapter(OpencodeAdapterConfig{}).BuildCommand(context.Background(), TurnContext{
				WorkDir:  t.TempDir(),
				Metadata: metadata,
				Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("BuildCommand() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestOpencodeAdapterRejectsInvalidModelLimitMetadata(t *testing.T) {
	for _, tt := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero context window", key: "contextWindow", value: "0"},
		{name: "negative context window", key: "contextWindow", value: "-1"},
		{name: "fractional context window", key: "contextWindow", value: "1.5"},
		{name: "zero max tokens", key: "maxTokens", value: "0"},
		{name: "negative max tokens", key: "maxTokens", value: "-1"},
		{name: "non-numeric max tokens", key: "maxTokens", value: "many"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
			_, err := adapter.BuildCommand(context.Background(), TurnContext{
				WorkDir: t.TempDir(),
				Metadata: map[string]string{
					"model": "kimi-k2",
					tt.key:  tt.value,
				},
				Env: []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
			})
			if err == nil || !strings.Contains(err.Error(), tt.key) || !strings.Contains(err.Error(), "positive integer") {
				t.Fatalf("BuildCommand() error = %v, want invalid %s rejection", err, tt.key)
			}
		})
	}
}

func TestOpencodeAdapterPreservesRepositoryInstructions(t *testing.T) {
	rootDir := t.TempDir()
	workDir := filepath.Join(rootDir, "subdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	for path, contents := range map[string]string{
		filepath.Join(rootDir, "AGENTS.md"): "root repository rules {env:OPENAI_API_KEY}\n",
		filepath.Join(workDir, "AGENTS.md"): "nested repository rules\n",
		filepath.Join(workDir, "CLAUDE.md"): "ignored claude rules\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write repository instructions: %v", err)
		}
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt:  "review the repository",
		WorkDir: workDir,
		RootDir: rootDir,
		Metadata: map[string]string{
			"model":        "kimi-k2",
			"systemPrompt": "agent system rules",
		},
		Env: []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)

	if got := envEntryValue(spec.Env, opencodeDisableProjectConfig); got != opencodeEnvTrue {
		t.Fatalf("%s = %q, want true", opencodeDisableProjectConfig, got)
	}
	data, err := os.ReadFile(spec.TempFiles[0])
	if err != nil {
		t.Fatalf("read opencode config: %v", err)
	}
	var cfg opencodeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal opencode config: %v", err)
	}
	if len(cfg.Instructions) != 3 {
		t.Fatalf("instructions = %#v, want nested, root, and agent instruction files", cfg.Instructions)
	}
	resolvedRootDir := resolveOpencodeTestDir(t, rootDir)
	resolvedWorkDir := resolveOpencodeTestDir(t, workDir)
	want := []string{
		"Instructions from: " + filepath.Join(resolvedWorkDir, "AGENTS.md") + "\nnested repository rules\n",
		"Instructions from: " + filepath.Join(resolvedRootDir, "AGENTS.md") +
			"\nroot repository rules {env:OPENAI_API_KEY}\n",
		"agent system rules\n",
	}
	for i, path := range cfg.Instructions {
		if !strings.HasPrefix(path, spec.TempFiles[1]+string(filepath.Separator)) {
			t.Fatalf("instruction path %q is outside scratch directory %q", path, spec.TempFiles[1])
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read instruction %d: %v", i, err)
		}
		if got := string(contents); got != want[i] {
			t.Fatalf("instruction %d = %q, want %q", i, got, want[i])
		}
	}
	if strings.Contains(string(data), "root repository rules") {
		t.Fatal("opencode config contains substitutable repository instruction content")
	}
}

func TestOpencodeProjectInstructionsFallsBackToClaude(t *testing.T) {
	rootDir := t.TempDir()
	workDir := filepath.Join(rootDir, "subdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	claudePath := filepath.Join(rootDir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte("claude rules\n"), 0o600); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	got, err := opencodeProjectInstructions(workDir, rootDir)
	if err != nil {
		t.Fatalf("opencodeProjectInstructions() error = %v", err)
	}
	want := []string{filepath.Join(resolveOpencodeTestDir(t, rootDir), "CLAUDE.md")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opencodeProjectInstructions() = %#v, want %#v", got, want)
	}
}

func TestOpencodeAdapterRejectsRepositoryInstructionSymlinkEscapes(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "secret")
	if err := os.WriteFile(outsidePath, []byte("must not be copied\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(rootDir, "AGENTS.md")); err != nil {
		t.Skipf("create instruction symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil {
		t.Fatal("BuildCommand() error = nil, want out-of-root symlink rejection")
	}
}

func TestOpencodeAdapterRejectsNestedRepositoryInstructionSymlinkEscapes(t *testing.T) {
	rootDir := t.TempDir()
	nestedDir := filepath.Join(rootDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested directory: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outsidePath, []byte("must not be loaded\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(nestedDir, "AGENTS.md")); err != nil {
		t.Skipf("create nested instruction symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil {
		t.Fatal("BuildCommand() error = nil, want nested out-of-root symlink rejection")
	}
}

func TestOpencodeAdapterRejectsOrdinaryFileSymlinkEscapes(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(rootDir, "AGENTS.md"),
		[]byte("read the token file\n"),
		0o600,
	); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(outsidePath, []byte("must not be read\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(rootDir, "token")); err != nil {
		t.Skipf("create ordinary file symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "file symlink") {
		t.Fatalf("BuildCommand() error = %v, want ordinary file symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsLiveGitWorktreeInstructionSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	nestedDir := filepath.Join(rootDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested directory: %v", err)
	}
	instructionsPath := filepath.Join(nestedDir, "AGENTS.md")
	if err := os.WriteFile(instructionsPath, []byte("tracked rules\n"), 0o600); err != nil {
		t.Fatalf("write tracked AGENTS.md: %v", err)
	}
	runOpencodeGitCommand(t, rootDir, "add", "nested/AGENTS.md")
	if err := os.Remove(instructionsPath); err != nil {
		t.Fatalf("remove tracked AGENTS.md: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outsidePath, []byte("outside rules\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, instructionsPath); err != nil {
		t.Skipf("replace tracked AGENTS.md with symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil {
		t.Fatal("BuildCommand() error = nil, want live worktree instruction symlink rejection")
	}
}

func TestOpencodeAdapterRejectsAssumeUnchangedGitWorktreeSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	trackedPath := filepath.Join(rootDir, "tracked")
	if err := os.WriteFile(trackedPath, []byte("tracked data\n"), 0o600); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	runOpencodeGitCommand(t, rootDir, "add", "tracked")
	runOpencodeGitCommand(t, rootDir, "update-index", "--assume-unchanged", "tracked")
	if err := os.Remove(trackedPath); err != nil {
		t.Fatalf("remove tracked file: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outsidePath, []byte("outside data\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, trackedPath); err != nil {
		t.Skipf("replace assume-unchanged file with symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "file symlink") {
		t.Fatalf("BuildCommand() error = %v, want assume-unchanged symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsAssumeUnchangedGitWorktreeParentSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	trackedDir := filepath.Join(rootDir, "tracked")
	if err := os.MkdirAll(trackedDir, 0o755); err != nil {
		t.Fatalf("mkdir tracked directory: %v", err)
	}
	trackedPath := filepath.Join(trackedDir, "file")
	if err := os.WriteFile(trackedPath, []byte("tracked data\n"), 0o600); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	runOpencodeGitCommand(t, rootDir, "add", "tracked/file")
	runOpencodeGitCommand(t, rootDir, "update-index", "--assume-unchanged", "tracked/file")
	if err := os.Remove(trackedPath); err != nil {
		t.Fatalf("remove tracked file: %v", err)
	}
	if err := os.Remove(trackedDir); err != nil {
		t.Fatalf("remove tracked directory: %v", err)
	}
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "file"), []byte("outside data\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsideDir, trackedDir); err != nil {
		t.Skipf("replace tracked parent directory with symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "directory symlink") {
		t.Fatalf("BuildCommand() error = %v, want assume-unchanged parent symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsIgnoredGitWorktreeInstructionSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(rootDir, ".gitignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	runOpencodeGitCommand(t, rootDir, "add", ".gitignore")
	ignoredDir := filepath.Join(rootDir, "ignored")
	if err := os.MkdirAll(ignoredDir, 0o755); err != nil {
		t.Fatalf("mkdir ignored directory: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outsidePath, []byte("outside rules\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(ignoredDir, "AGENTS.md")); err != nil {
		t.Skipf("create ignored instruction symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil {
		t.Fatal("BuildCommand() error = nil, want ignored worktree instruction symlink rejection")
	}
}

func TestOpencodeAdapterRejectsGitSubmoduleSymlinkEscapes(t *testing.T) {
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outsidePath, []byte("outside data\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	submoduleDir := t.TempDir()
	runOpencodeGitCommand(t, submoduleDir, "init", "--quiet")
	if err := os.Symlink(outsidePath, filepath.Join(submoduleDir, "leak")); err != nil {
		t.Skipf("create submodule symlink: %v", err)
	}
	runOpencodeGitCommand(t, submoduleDir, "add", "leak")
	runOpencodeGitCommand(
		t,
		submoduleDir,
		"-c", "user.name=Orka Test",
		"-c", "user.email=orka@example.invalid",
		"commit", "--quiet", "-m", "add symlink",
	)

	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	runOpencodeGitCommand(
		t,
		rootDir,
		"-c", "protocol.file.allow=always",
		"submodule", "add", "--quiet", submoduleDir, "deps/child",
	)
	runOpencodeGitCommand(
		t,
		rootDir,
		"-c", "user.name=Orka Test",
		"-c", "user.email=orka@example.invalid",
		"commit", "--quiet", "-m", "add submodule",
	)

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "file symlink") {
		t.Fatalf("BuildCommand() error = %v, want submodule file symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsEmbeddedGitRepositorySymlinkEscapes(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	embeddedDir := filepath.Join(rootDir, "vendor", "embedded")
	if err := os.MkdirAll(embeddedDir, 0o755); err != nil {
		t.Fatalf("mkdir embedded repository: %v", err)
	}
	runOpencodeGitCommand(t, embeddedDir, "init", "--quiet")
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outsidePath, []byte("outside data\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(embeddedDir, "leak")); err != nil {
		t.Skipf("create embedded repository symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "file symlink") {
		t.Fatalf("BuildCommand() error = %v, want embedded repository symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsEmbeddedGitfileOutsideWorkspace(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	embeddedDir := filepath.Join(rootDir, "vendor", "embedded")
	if err := os.MkdirAll(embeddedDir, 0o755); err != nil {
		t.Fatalf("mkdir embedded repository: %v", err)
	}
	outsideRepo := t.TempDir()
	runOpencodeGitCommand(t, outsideRepo, "init", "--quiet")
	if err := os.WriteFile(
		filepath.Join(embeddedDir, ".git"),
		[]byte("gitdir: "+filepath.Join(outsideRepo, ".git")+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("write embedded gitfile: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "git metadata directory") ||
		!strings.Contains(err.Error(), "outside workspace root") {
		t.Fatalf("BuildCommand() error = %v, want external gitdir rejection", err)
	}
}

func TestOpencodeAdapterDisablesEmbeddedRepositoryExternalDiff(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	embeddedDir := filepath.Join(rootDir, "vendor", "embedded")
	if err := os.MkdirAll(embeddedDir, 0o755); err != nil {
		t.Fatalf("mkdir embedded repository: %v", err)
	}
	runOpencodeGitCommand(t, embeddedDir, "init", "--quiet")
	trackedPath := filepath.Join(embeddedDir, "tracked")
	if err := os.WriteFile(trackedPath, []byte("before\n"), 0o600); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	runOpencodeGitCommand(t, embeddedDir, "add", "tracked")
	runOpencodeGitCommand(
		t,
		embeddedDir,
		"-c", "user.name=Orka Test",
		"-c", "user.email=orka@example.invalid",
		"commit", "--quiet", "-m", "add tracked file",
	)
	marker := filepath.Join(t.TempDir(), "external-diff-ran")
	script := filepath.Join(t.TempDir(), "external-diff.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ran > '"+marker+"'\n"), 0o700); err != nil {
		t.Fatalf("write external diff script: %v", err)
	}
	runOpencodeGitCommand(t, embeddedDir, "config", "diff.external", script)
	if err := os.WriteFile(trackedPath, []byte("after\n"), 0o600); err != nil {
		t.Fatalf("modify tracked file: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("external diff command ran, marker lstat error = %v", err)
	}
}

func TestOpencodeAdapterDisablesRepositoryFSMonitor(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	trackedPath := filepath.Join(rootDir, "tracked")
	if err := os.WriteFile(trackedPath, []byte("tracked\n"), 0o600); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	runOpencodeGitCommand(t, rootDir, "add", "tracked")

	hookPath := filepath.Join(rootDir, ".git", "fsmonitor.sh")
	markerPath := hookPath + ".ran"
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\n: > \"$0.ran\"\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatalf("write fsmonitor hook: %v", err)
	}
	runOpencodeGitCommand(t, rootDir, "config", "core.fsmonitor", hookPath)

	control := workspaceGitCommand(
		context.Background(),
		"-c", "core.fsmonitor="+hookPath,
		"-C", rootDir,
		"--work-tree="+rootDir,
		"ls-files", "--stage", "-z",
	)
	if output, err := control.CombinedOutput(); err != nil {
		t.Fatalf("run fsmonitor control command: %v: %s", err, output)
	}
	if _, err := os.Lstat(markerPath); err != nil {
		t.Fatalf("fsmonitor control command did not run hook: %v", err)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatalf("remove fsmonitor marker: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	if _, err := os.Lstat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("repository fsmonitor hook ran during BuildCommand, marker lstat error = %v", err)
	}
}

func TestOpencodeAdapterPinsGitWorktreeDuringSymlinkScan(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	configuredWorktree := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "config", "core.worktree", configuredWorktree)
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outsidePath, []byte("outside data\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(rootDir, "leak")); err != nil {
		t.Skipf("create worktree symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "file symlink") {
		t.Fatalf("BuildCommand() error = %v, want actual worktree symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsGitMetadataSymlinkEscapes(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	outsidePath := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outsidePath, []byte("outside data\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(rootDir, ".git", "leak")); err != nil {
		t.Skipf("create git metadata symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "file symlink") {
		t.Fatalf("BuildCommand() error = %v, want git metadata symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsRepositoryDirectorySymlinkEscapes(t *testing.T) {
	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "AGENTS.md"), []byte("outside rules\n"), 0o600); err != nil {
		t.Fatalf("write outside AGENTS.md: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(rootDir, "escape")); err != nil {
		t.Skipf("create directory symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "directory symlink") {
		t.Fatalf("BuildCommand() error = %v, want directory symlink escape rejection", err)
	}
}

func TestOpencodeAdapterRejectsRepositoryDirectorySymlinkCycles(t *testing.T) {
	rootDir := t.TempDir()
	nestedDir := filepath.Join(rootDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "AGENTS.md"), []byte("nested rules\n"), 0o600); err != nil {
		t.Fatalf("write nested AGENTS.md: %v", err)
	}
	if err := os.Symlink(".", filepath.Join(nestedDir, "loop")); err != nil {
		t.Skipf("create directory symlink cycle: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "directory symlink") {
		t.Fatalf("BuildCommand() error = %v, want directory symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsSymlinkedWorkDirEscapes(t *testing.T) {
	rootDir := t.TempDir()
	workDir := filepath.Join(rootDir, "workspace")
	if err := os.Symlink(t.TempDir(), workDir); err != nil {
		t.Skipf("create workdir symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  workDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "outside root") {
		t.Fatalf("BuildCommand() error = %v, want symlinked workdir rejection", err)
	}
}

func TestOpencodeAdapterRejectsSymlinkedWorkDirWithinRoot(t *testing.T) {
	rootDir := t.TempDir()
	packageDir := filepath.Join(rootDir, "packages")
	appDir := filepath.Join(packageDir, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app directory: %v", err)
	}
	instructionsPath := filepath.Join(packageDir, "AGENTS.md")
	if err := os.WriteFile(instructionsPath, []byte("package rules\n"), 0o600); err != nil {
		t.Fatalf("write package AGENTS.md: %v", err)
	}
	workDir := filepath.Join(rootDir, "current")
	if err := os.Symlink(filepath.Join("packages", "app"), workDir); err != nil {
		t.Skipf("create workdir symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  workDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "directory symlink") {
		t.Fatalf("BuildCommand() error = %v, want symlinked workdir rejection", err)
	}
}

func TestOpencodeAdapterValidatesInstructionNamesUsingFilesystemCaseRules(t *testing.T) {
	rootDir := t.TempDir()
	nestedDir := filepath.Join(rootDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested directory: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outsidePath, []byte("must not be loaded\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	lowercasePath := filepath.Join(nestedDir, "agents.md")
	if err := os.Symlink(outsidePath, lowercasePath); err != nil {
		t.Skipf("create lowercase instruction symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nestedDir, "AGENTS.md")); err != nil {
		t.Skip("filesystem is case-sensitive")
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil {
		t.Fatal("BuildCommand() error = nil, want case-equivalent instruction symlink rejection")
	}
}

func TestOpencodeAdapterAllowsTrustedArtifactsDirectory(t *testing.T) {
	rootDir := t.TempDir()
	artifactDir := createOpencodeArtifactsDir(t, rootDir)
	if err := os.WriteFile(
		filepath.Join(artifactDir, "security-review-context-test.json"),
		[]byte("{}\n"),
		0o600,
	); err != nil {
		t.Fatalf("write trusted artifact: %v", err)
	}
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:      rootDir,
		RootDir:      rootDir,
		ArtifactsDir: artifactDir,
		Metadata:     map[string]string{"model": "kimi-k2"},
		Env: []string{
			workerenv.OpenAIBaseURL + "=http://models.example/v1",
			"ORKA_ARTIFACTS_DIR=/tmp/caller-controlled",
		},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	wantArtifactDir := filepath.Join(resolveOpencodeTestDir(t, rootDir), opencodeWorkspaceArtifactsDir)
	if got := envEntryValue(spec.Env, "ORKA_ARTIFACTS_DIR"); got != wantArtifactDir {
		t.Fatalf("ORKA_ARTIFACTS_DIR = %q, want trusted %q", got, wantArtifactDir)
	}
}

func TestOpencodeAdapterIgnoresCallerArtifactDirectoryPath(t *testing.T) {
	rootDir := t.TempDir()
	artifactDir := t.TempDir()
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:      rootDir,
		RootDir:      rootDir,
		ArtifactsDir: artifactDir,
		Metadata:     map[string]string{"model": "kimi-k2"},
		Env:          []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "resolve opencode artifact directory") ||
		strings.Contains(err.Error(), artifactDir) {
		t.Fatalf("BuildCommand() error = %v, want missing canonical artifact directory rejection", err)
	}
}

func TestOpencodeAdapterRejectsArtifactDirectoryInstructions(t *testing.T) {
	rootDir := t.TempDir()
	artifactDir := createOpencodeArtifactsDir(t, rootDir)
	if err := os.WriteFile(filepath.Join(artifactDir, "AGENTS.md"), []byte("artifact rules\n"), 0o600); err != nil {
		t.Fatalf("write artifact AGENTS.md: %v", err)
	}
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:      rootDir,
		RootDir:      rootDir,
		ArtifactsDir: artifactDir,
		Metadata:     map[string]string{"model": "kimi-k2"},
		Env:          []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "artifact directory must not contain") {
		t.Fatalf("BuildCommand() error = %v, want artifact instruction rejection", err)
	}
}

func TestOpencodeAdapterRejectsNestedArtifactDirectoryInstructions(t *testing.T) {
	rootDir := t.TempDir()
	artifactDir := createOpencodeArtifactsDir(t, rootDir)
	nestedDir := filepath.Join(artifactDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested artifact directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "AGENTS.md"), []byte("artifact rules\n"), 0o600); err != nil {
		t.Fatalf("write nested artifact AGENTS.md: %v", err)
	}
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:      rootDir,
		RootDir:      rootDir,
		ArtifactsDir: artifactDir,
		Metadata:     map[string]string{"model": "kimi-k2"},
		Env:          []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "artifact directory must not contain") {
		t.Fatalf("BuildCommand() error = %v, want nested artifact instruction rejection", err)
	}
}

func TestOpencodeAdapterRejectsArtifactFileSymlinkEscapes(t *testing.T) {
	rootDir := t.TempDir()
	artifactDir := createOpencodeArtifactsDir(t, rootDir)
	outsidePath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outsidePath, []byte("must not be read\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(artifactDir, "result.txt")); err != nil {
		t.Skipf("create artifact file symlink: %v", err)
	}
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:      rootDir,
		RootDir:      rootDir,
		ArtifactsDir: artifactDir,
		Metadata:     map[string]string{"model": "kimi-k2"},
		Env:          []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "artifact symlink") {
		t.Fatalf("BuildCommand() error = %v, want artifact symlink escape rejection", err)
	}
}

func TestOpencodeAdapterRejectsDanglingArtifactSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	artifactDir := createOpencodeArtifactsDir(t, rootDir)
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(artifactDir, "result.txt")); err != nil {
		t.Skipf("create dangling artifact symlink: %v", err)
	}
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:      rootDir,
		RootDir:      rootDir,
		ArtifactsDir: artifactDir,
		Metadata:     map[string]string{"model": "kimi-k2"},
		Env:          []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "artifact symlink") {
		t.Fatalf("BuildCommand() error = %v, want dangling artifact symlink rejection", err)
	}
}

func TestOpencodeAdapterAllowsRepositoryInstructionSymlinksWithinRoot(t *testing.T) {
	rootDir := t.TempDir()
	claudePath := filepath.Join(rootDir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte("shared repository rules\n"), 0o600); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	agentsPath := filepath.Join(rootDir, "AGENTS.md")
	if err := os.Symlink("CLAUDE.md", agentsPath); err != nil {
		t.Skipf("create instruction symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
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
	if len(cfg.Instructions) != 1 {
		t.Fatalf("instructions = %#v, want one repository instruction file", cfg.Instructions)
	}
	contents, err := os.ReadFile(cfg.Instructions[0])
	if err != nil {
		t.Fatalf("read copied instructions: %v", err)
	}
	want := "Instructions from: " + filepath.Join(resolveOpencodeTestDir(t, rootDir), "AGENTS.md") +
		"\nshared repository rules\n"
	if got := string(contents); got != want {
		t.Fatalf("copied instructions = %q, want %q", got, want)
	}
}

func TestOpencodeAdapterAllowsAbsoluteRepositoryInstructionSymlinksWithinRoot(t *testing.T) {
	rootDir := t.TempDir()
	claudePath := filepath.Join(rootDir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte("shared repository rules\n"), 0o600); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	if err := os.Symlink(claudePath, filepath.Join(rootDir, "AGENTS.md")); err != nil {
		t.Skipf("create absolute instruction symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
}

func TestOpencodeAdapterRejectsRepositoryInstructionSymlinksToOtherInRootFiles(t *testing.T) {
	rootDir := t.TempDir()
	secretPath := filepath.Join(rootDir, ".env")
	if err := os.WriteFile(secretPath, []byte("TOKEN=must-not-be-copied\n"), 0o600); err != nil {
		t.Fatalf("write in-root secret: %v", err)
	}
	if err := os.Symlink(secretPath, filepath.Join(rootDir, "AGENTS.md")); err != nil {
		t.Skipf("create instruction symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "must target") {
		t.Fatalf("BuildCommand() error = %v, want non-instruction target rejection", err)
	}
}

func TestOpencodeAdapterRejectsCaseFoldedInstructionSymlinkTargets(t *testing.T) {
	rootDir := t.TempDir()
	lowercasePath := filepath.Join(rootDir, "agents.md")
	if err := os.WriteFile(lowercasePath, []byte("not an instruction file\n"), 0o600); err != nil {
		t.Fatalf("write lowercase target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "AGENTS.md")); err == nil {
		t.Skip("filesystem is case-insensitive")
	}
	if err := os.Symlink("agents.md", filepath.Join(rootDir, "AGENTS.md")); err != nil {
		t.Skipf("create instruction symlink: %v", err)
	}
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir: rootDir, RootDir: rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "must target") {
		t.Fatalf("BuildCommand() error = %v, want case-folded target rejection", err)
	}
}

func TestOpencodeAdapterStopsInstructionDiscoveryAtNestedGitWorktree(t *testing.T) {
	rootDir := t.TempDir()
	runOpencodeGitCommand(t, rootDir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(rootDir, "AGENTS.md"), []byte("outer rules\n"), 0o600); err != nil {
		t.Fatalf("write outer instructions: %v", err)
	}
	innerDir := filepath.Join(rootDir, "vendor", "inner")
	if err := os.MkdirAll(innerDir, 0o755); err != nil {
		t.Fatalf("mkdir inner repo: %v", err)
	}
	runOpencodeGitCommand(t, innerDir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(innerDir, "CLAUDE.md"), []byte("inner rules\n"), 0o600); err != nil {
		t.Fatalf("write inner instructions: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir: innerDir, RootDir: rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	data, err := os.ReadFile(spec.TempFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	var cfg opencodeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Instructions) != 1 {
		t.Fatalf("instructions = %#v", cfg.Instructions)
	}
	contents, err := os.ReadFile(cfg.Instructions[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "inner rules") || strings.Contains(string(contents), "outer rules") {
		t.Fatalf("instructions = %q, want inner rules only", contents)
	}
}

func TestOpencodeAdapterRejectsDanglingShadowedInstructionFallbacks(t *testing.T) {
	rootDir := t.TempDir()
	nestedDir := filepath.Join(rootDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "AGENTS.md"), []byte("winning rules\n"), 0o600); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(nestedDir, "CLAUDE.md")); err != nil {
		t.Skipf("create shadowed CLAUDE.md symlink: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "resolve opencode workspace symlink") {
		t.Fatalf("BuildCommand() error = %v, want dangling symlink rejection", err)
	}
}

func TestOpencodeAdapterRejectsShadowedNestedInstructionFallbackSymlinks(t *testing.T) {
	for _, fallbackName := range []string{"CLAUDE.md", "CONTEXT.md"} {
		t.Run(fallbackName, func(t *testing.T) {
			rootDir := t.TempDir()
			nestedDir := filepath.Join(rootDir, "nested")
			workDir := filepath.Join(nestedDir, "work")
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				t.Fatalf("mkdir nested workdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(nestedDir, "AGENTS.md"), []byte("winning rules\n"), 0o600); err != nil {
				t.Fatalf("write AGENTS.md: %v", err)
			}
			if fallbackName == "CONTEXT.md" {
				claudePath := filepath.Join(nestedDir, "CLAUDE.md")
				if err := os.WriteFile(claudePath, []byte("earlier fallback rules\n"), 0o600); err != nil {
					t.Fatalf("write CLAUDE.md: %v", err)
				}
			}
			targetPath := filepath.Join(nestedDir, "notes.txt")
			if err := os.WriteFile(targetPath, []byte("not repository instructions\n"), 0o600); err != nil {
				t.Fatalf("write fallback target: %v", err)
			}
			if err := os.Symlink("notes.txt", filepath.Join(nestedDir, fallbackName)); err != nil {
				t.Skipf("create shadowed %s symlink: %v", fallbackName, err)
			}

			adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
			_, err := adapter.BuildCommand(context.Background(), TurnContext{
				WorkDir:  workDir,
				RootDir:  rootDir,
				Metadata: map[string]string{"model": "kimi-k2"},
				Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
			})
			if err == nil || !strings.Contains(err.Error(), "must target") {
				t.Fatalf("BuildCommand() error = %v, want shadowed %s target rejection", err, fallbackName)
			}
		})
	}
}

func TestOpencodeAdapterRejectsSubstitutableModelNames(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	for _, model := range []string{
		"leak-{env:OPENAI_API_KEY}",
		"leak-{file:/var/run/secrets/token}",
		"leak-{env:",
		"leak-{file:",
	} {
		t.Run(model, func(t *testing.T) {
			_, err := adapter.BuildCommand(context.Background(), TurnContext{
				WorkDir:  t.TempDir(),
				Metadata: map[string]string{"model": model},
				Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
			})
			if err == nil || !strings.Contains(err.Error(), "must not contain opencode config substitutions") {
				t.Fatalf("BuildCommand() error = %v, want config substitution rejection", err)
			}
		})
	}
}

func TestOpencodeAdapterRejectsSubstitutableProviderConfig(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "base URL",
			env:  []string{workerenv.OpenAIBaseURL + "=https://models.example/{env:GITHUB_TOKEN}"},
			want: workerenv.OpenAIBaseURL,
		},
		{
			name: "API key",
			env: []string{
				workerenv.OpenAIBaseURL + "=https://models.example/v1",
				workerenv.OpenAIAPIKey + "={file:/var/run/secrets/token}",
			},
			want: workerenv.OpenAIAPIKey,
		},
		{
			name: "unterminated base URL opener",
			env:  []string{workerenv.OpenAIBaseURL + "=https://models.example/{file:"},
			want: workerenv.OpenAIBaseURL,
		},
		{
			name: "unterminated API key opener",
			env: []string{
				workerenv.OpenAIBaseURL + "=https://models.example/v1",
				workerenv.OpenAIAPIKey + "=key-{env:",
			},
			want: workerenv.OpenAIAPIKey,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adapter.BuildCommand(context.Background(), TurnContext{
				WorkDir:  t.TempDir(),
				Metadata: map[string]string{"model": "kimi-k2"},
				Env:      tt.env,
			})
			if err == nil ||
				!strings.Contains(err.Error(), tt.want) ||
				!strings.Contains(err.Error(), "must not contain opencode config substitutions") {
				t.Fatalf("BuildCommand() error = %v, want %s config substitution rejection", err, tt.want)
			}
		})
	}
}

func TestOpencodeAdapterAllowsHarmlessBracesInAPIKey(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  t.TempDir(),
		Metadata: map[string]string{"model": "kimi-k2"},
		Env: []string{
			workerenv.OpenAIBaseURL + "=http://models.example/v1",
			workerenv.OpenAIAPIKey + "=key-{tenant}-abc",
		},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
}

func TestOpencodeAdapterRejectsOversizedRepositoryInstructions(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(rootDir, "AGENTS.md"),
		[]byte(strings.Repeat("x", opencodeStartupInstructionsLimit+1)),
		0o600,
	); err != nil {
		t.Fatalf("write oversized AGENTS.md: %v", err)
	}

	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir:  rootDir,
		RootDir:  rootDir,
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err == nil || !strings.Contains(err.Error(), "repository instructions exceed") {
		t.Fatalf("BuildCommand() error = %v, want repository instruction size rejection", err)
	}
}

func TestOpencodeAdapterInstructionScanHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	_, err := adapter.BuildCommand(ctx, TurnContext{
		WorkDir:  t.TempDir(),
		Metadata: map[string]string{"model": "kimi-k2"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildCommand() error = %v, want context cancellation", err)
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
			"OPENCODE_DISABLE_CLAUDE_CODE_PROMPT=true",
			"ORKA_ARTIFACTS_DIR=/tmp/caller-controlled",
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
		"OPENCODE_DISABLE_CLAUDE_CODE_PROMPT",
		"ORKA_ARTIFACTS_DIR",
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

func TestOpencodeAdapterGeneratedConfigPreservesSensitiveEnvReadDenies(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		WorkDir: t.TempDir(),
		Metadata: map[string]string{
			"model":        "kimi-k2",
			"allowedTools": "Read",
		},
		Env: []string{workerenv.OpenAIBaseURL + "=http://models.example/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	defer removeTempFiles(spec.TempFiles)

	data, err := os.ReadFile(spec.TempFiles[0])
	if err != nil {
		t.Fatalf("read opencode config: %v", err)
	}
	var cfg struct {
		Permission map[string]json.RawMessage `json:"permission"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal opencode config: %v", err)
	}
	assertOpencodeReadPermission(t, cfg.Permission["read"])
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

	assertOpencodeReadPermission(t, permissions["read"])
	if permissions["glob"] != opencodePermissionAllow {
		t.Fatalf("permission glob = %q, want allow", permissions["glob"])
	}
	if permissions["grep"] != opencodePermissionDeny {
		t.Fatalf("permission grep = %q, want deny even when requested", permissions["grep"])
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
		assertOpencodeReadPermission(t, permissions["read"])
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

func TestOpencodeAdapterParseResultRejectsJSONEventsWithoutCompletedAssistantText(t *testing.T) {
	adapter := NewOpencodeAdapter(OpencodeAdapterConfig{})
	stdout := "" +
		`{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		`{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"completed"}}}` + "\n"
	_, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{FullStdout: stdout})
	if err == nil || !strings.Contains(err.Error(), "completed assistant text") {
		t.Fatalf("ParseResult() error = %v, want missing completed assistant text rejection", err)
	}
}

func assertOpencodeReadPermission(t *testing.T, permission any) {
	t.Helper()
	data, err := json.Marshal(permission)
	if err != nil {
		t.Fatalf("marshal read permission: %v", err)
	}
	var rules map[string]string
	if err := json.Unmarshal(data, &rules); err != nil {
		t.Fatalf("unmarshal read permission %s: %v", data, err)
	}
	want := map[string]string{
		"*":             opencodePermissionAllow,
		"*.env":         opencodePermissionDeny,
		"*.env.*":       opencodePermissionDeny,
		"*.env.example": opencodePermissionAllow,
	}
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("permission.read = %#v, want %#v", rules, want)
	}
	previous := -1
	for _, pattern := range []string{"*", "*.env", "*.env.*", "*.env.example"} {
		index := strings.Index(string(data), `"`+pattern+`"`)
		if index <= previous {
			t.Fatalf("permission.read JSON = %s, want precedence order through %q", data, pattern)
		}
		previous = index
	}
}

func resolveOpencodeTestDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve test directory %q: %v", dir, err)
	}
	return resolved
}

func runOpencodeGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	if output, err := workspaceGitCommand(context.Background(), cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, output)
	}
}

func createOpencodeArtifactsDir(t *testing.T, workDir string) string {
	t.Helper()
	dir := filepath.Join(workDir, opencodeWorkspaceArtifactsDir)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		t.Fatalf("create opencode artifacts directory: %v", err)
	}
	return dir
}
