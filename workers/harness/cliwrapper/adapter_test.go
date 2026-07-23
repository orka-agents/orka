package cliwrapper

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	testEchoCommand = "echo"
	testTurnPrompt  = "hello"
)

func TestNewRuntimeAdapterSelection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Generic.Command = testEchoCommand
	adapter, err := NewRuntimeAdapter(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeAdapter(generic): %v", err)
	}
	if adapter.Name() != RuntimeGeneric {
		t.Fatalf("adapter.Name() = %s, want generic", adapter.Name())
	}

	cfg.Runtime = RuntimeCodex
	adapter, err = NewRuntimeAdapter(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeAdapter(codex): %v", err)
	}
	if adapter.Name() != RuntimeCodex {
		t.Fatalf("adapter.Name() = %s, want codex", adapter.Name())
	}

	cfg.Runtime = RuntimeOpencode
	adapter, err = NewRuntimeAdapter(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeAdapter(opencode): %v", err)
	}
	if adapter.Name() != RuntimeOpencode {
		t.Fatalf("adapter.Name() = %s, want opencode", adapter.Name())
	}

	cfg.Runtime = "bogus"
	if _, err := NewRuntimeAdapter(cfg); err == nil || !strings.Contains(err.Error(), "unsupported runtime adapter") {
		t.Fatalf("NewRuntimeAdapter(bogus) error = %v, want unsupported", err)
	}
}

func TestGenericAdapterBuildCommandPromptModes(t *testing.T) {
	turn := TurnContext{Prompt: testTurnPrompt, WorkDir: t.TempDir()}
	adapter := NewGenericAdapter(GenericAdapterConfig{Command: "cat", PromptMode: PromptModeStdin})
	spec, err := adapter.BuildCommand(context.Background(), turn)
	if err != nil {
		t.Fatalf("BuildCommand(stdin): %v", err)
	}
	if string(spec.Stdin) != testTurnPrompt {
		t.Fatalf("stdin = %q, want prompt", string(spec.Stdin))
	}

	adapter = NewGenericAdapter(GenericAdapterConfig{
		Command:    "printenv",
		PromptMode: PromptModeEnv,
		PromptEnv:  "PROMPT_VALUE",
	})
	spec, err = adapter.BuildCommand(context.Background(), turn)
	if err != nil {
		t.Fatalf("BuildCommand(env): %v", err)
	}
	if !containsEnv(spec.Env, "PROMPT_VALUE="+testTurnPrompt) {
		t.Fatalf("env = %#v, want prompt env", spec.Env)
	}

	adapter = NewGenericAdapter(GenericAdapterConfig{
		Command:    "cat",
		PromptMode: PromptModeFile,
		PromptEnv:  "PROMPT_FILE",
	})
	spec, err = adapter.BuildCommand(context.Background(), turn)
	if err != nil {
		t.Fatalf("BuildCommand(file): %v", err)
	}
	if len(spec.TempFiles) != 1 {
		t.Fatalf("TempFiles = %#v, want one prompt temp file", spec.TempFiles)
	}
	data, err := os.ReadFile(spec.TempFiles[0])
	if err != nil {
		t.Fatalf("read prompt temp file: %v", err)
	}
	if string(data) != testTurnPrompt {
		t.Fatalf("prompt file = %q, want prompt", string(data))
	}
	removeTempFiles(spec.TempFiles)
}

func TestGenericAdapterRejectsResultFileEscapingWorkspaceSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	outsideResult := filepath.Join(outside, "result.txt")
	if err := os.WriteFile(outsideResult, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    testEchoCommand,
		ResultMode: ResultModeFile,
		ResultFile: filepath.Join("escape", "result.txt"),
		WorkDir:    dir,
	})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{WorkDir: dir})
	if err == nil || (!strings.Contains(err.Error(), "escapes workspace") &&
		!strings.Contains(err.Error(), "must not contain symlink")) {
		t.Fatalf("BuildCommand error = %v, want workspace escape or symlink rejection", err)
	}
	if _, statErr := os.Stat(outsideResult); statErr != nil {
		t.Fatalf("outside result was modified before validation: %v", statErr)
	}
}

func TestGenericAdapterRejectsResultFileSymlinkedParentInsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual")
	if err := os.Mkdir(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    testEchoCommand,
		ResultMode: ResultModeFile,
		ResultFile: filepath.Join("link", "result.txt"),
		WorkDir:    dir,
	})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{WorkDir: dir})
	if err == nil || !strings.Contains(err.Error(), "must not contain symlink") {
		t.Fatalf("BuildCommand error = %v, want symlinked parent rejection", err)
	}
}

func TestGenericAdapterResultFileModeRequiresConfiguredPath(t *testing.T) {
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    testEchoCommand,
		ResultMode: ResultModeFile,
	})
	if err := adapter.Validate(); err == nil || !strings.Contains(err.Error(), EnvResultFile) {
		t.Fatalf("Validate() error = %v, want missing result file", err)
	}
	_, err := adapter.BuildCommand(context.Background(), TurnContext{WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), EnvResultFile) {
		t.Fatalf("BuildCommand() error = %v, want missing result file", err)
	}
}

func TestGenericAdapterResultFilePrecreateReplacesHardLink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	outsideResult := filepath.Join(outside, "result.txt")
	if err := os.WriteFile(outsideResult, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(dir, "result.txt")
	if err := os.Link(outsideResult, resultPath); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    testEchoCommand,
		ResultMode: ResultModeFile,
		ResultFile: "result.txt",
		WorkDir:    dir,
	})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{WorkDir: dir})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	data, err := os.ReadFile(outsideResult)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("outside hard-link target = %q, want untouched", string(data))
	}
}

func TestGenericAdapterResultFileRequiresChildWrite(t *testing.T) {
	dir := t.TempDir()
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    testEchoCommand,
		ResultMode: ResultModeFile,
		ResultFile: "result.txt",
		WorkDir:    dir,
	})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{WorkDir: dir})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	_, err = adapter.ParseResult(
		context.Background(),
		TurnContext{},
		CommandResult{Stdout: "stdout", ResultFile: spec.ResultFile},
	)
	if err == nil || !strings.Contains(err.Error(), "result file was not written") {
		t.Fatalf("ParseResult error = %v, want unwritten result file", err)
	}
}

func TestGenericAdapterRejectsResultFileReplacedWithSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	outsideResult := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideResult, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    testEchoCommand,
		ResultMode: ResultModeFile,
		ResultFile: "result.txt",
		WorkDir:    dir,
	})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{WorkDir: dir})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	if err := os.Remove(spec.ResultFile); err != nil {
		t.Fatalf("replace result file: %v", err)
	}
	if err := os.Symlink(outsideResult, spec.ResultFile); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	result, err := adapter.ParseResult(
		context.Background(),
		TurnContext{},
		CommandResult{Stdout: "stdout", ResultFile: spec.ResultFile},
	)
	if !resultPathRejectedError(err) {
		t.Fatalf("ParseResult error = %v, result=%q; want symlink read rejection", err, result.Result)
	}
	if strings.Contains(result.Result, "outside secret") {
		t.Fatalf("ParseResult leaked symlink target content: %q", result.Result)
	}
}

func TestGenericAdapterRejectsResultFileParentReplacedWithSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "result.txt"), []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    testEchoCommand,
		ResultMode: ResultModeFile,
		ResultFile: filepath.Join("nested", "result.txt"),
		WorkDir:    dir,
	})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{WorkDir: dir})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	if err := os.Remove(spec.ResultFile); err != nil {
		t.Fatalf("remove result file before replacing parent: %v", err)
	}
	parent := filepath.Dir(spec.ResultFile)
	if err := os.Remove(parent); err != nil {
		t.Fatalf("remove result parent before replacing it: %v", err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	result, err := adapter.ParseResult(
		context.Background(),
		TurnContext{WorkDir: dir},
		CommandResult{Stdout: "stdout", ResultFile: spec.ResultFile},
	)
	if !resultPathRejectedError(err) {
		t.Fatalf("ParseResult error = %v, result=%q; want symlinked parent rejection", err, result.Result)
	}
	if strings.Contains(result.Result, "outside secret") {
		t.Fatalf("ParseResult leaked symlinked parent target content: %q", result.Result)
	}
}

func TestGenericAdapterResultFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(path, []byte("file result"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := NewGenericAdapter(GenericAdapterConfig{
		Command:    "ignored",
		ResultMode: ResultModeFile,
		ResultFile: "result.txt",
		WorkDir:    dir,
	})
	result, err := adapter.ParseResult(context.Background(), TurnContext{}, CommandResult{Stdout: "stdout"})
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if result.Result != "file result" {
		t.Fatalf("Result = %q, want file result", result.Result)
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = "invalid"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid runtime")
	}
	cfg = DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.StdoutLimitBytes = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid stdout limit")
	}
}

func TestTurnContextFromRequestDoesNotDefaultWorkDirToRepo(t *testing.T) {
	request := validWrapperStartTurnRequest()
	turn := turnContextFromRequest(RuntimeGeneric, DefaultConfig(), request)
	if turn.WorkDir != "" {
		t.Fatalf("WorkDir = %q, want empty unless configured", turn.WorkDir)
	}
}

func TestTurnEnvFromRequestCarriesTimeoutMetadata(t *testing.T) {
	req := validWrapperStartTurnRequest()
	req.Metadata = map[string]string{"timeoutSeconds": "2700"}
	env := turnEnvFromRequest(DefaultConfig(), req, req.Metadata)
	if !containsEnv(env, workerenv.TimeoutSeconds+"=2700") {
		t.Fatalf("env = %#v, want timeout seconds", env)
	}
}

func validWrapperStartTurnRequest() harness.StartTurnRequest {
	return harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "user:test"},
		Input:            harness.TurnInput{Prompt: testTurnPrompt},
	}
}

func containsEnv(env []string, want string) bool {
	return slices.Contains(env, want)
}

func TestLoadConfigFromEnvUnvalidatedAllowsFlagOnlyAuth(t *testing.T) {
	t.Setenv(EnvRuntime, RuntimeGeneric)
	t.Setenv(EnvCommand, testEchoCommand)
	cfg, err := LoadConfigFromEnvUnvalidated()
	if err != nil {
		t.Fatalf("LoadConfigFromEnvUnvalidated() error = %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want missing auth before flag override")
	}
	cfg.AllowUnauthenticated = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() after flag override = %v", err)
	}
}

func TestAgentConfigFromTurnNarrowsWorkerToolPolicy(t *testing.T) {
	t.Setenv("ORKA_ALLOWED_TOOLS", "web_search,file_read")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "shell")
	cfg := agentConfigFromTurn(TurnContext{Metadata: map[string]string{
		"allowedTools":    "web_search,code_exec",
		"disallowedTools": "web_search",
	}})
	if strings.Join(cfg.AllowedTools, ",") != "web_search" {
		t.Fatalf("AllowedTools = %#v, want intersection with worker allowlist", cfg.AllowedTools)
	}
	if !slices.Contains(cfg.DisallowedTools, "shell") || !slices.Contains(cfg.DisallowedTools, "web_search") {
		t.Fatalf("DisallowedTools = %#v, want worker+turn union", cfg.DisallowedTools)
	}
}

func TestAgentConfigFromTurnCannotBroadenWorkerAllowBash(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "false")
	cfg := agentConfigFromTurn(TurnContext{Metadata: map[string]string{"allowBash": "true"}})
	if cfg.AllowBash {
		t.Fatal("AllowBash = true, want worker env to remain hard upper bound")
	}
}

func TestAgentConfigFromTurnDisjointAllowlistsRemainDenyAll(t *testing.T) {
	t.Setenv("ORKA_ALLOWED_TOOLS", "file_read")
	cfg := agentConfigFromTurn(TurnContext{Metadata: map[string]string{"allowedTools": "web_search"}})
	if !cfg.AllowedToolsSet {
		t.Fatal("AllowedToolsSet = false, want explicit deny-all state")
	}
	if len(cfg.AllowedTools) != 0 {
		t.Fatalf("AllowedTools = %#v, want empty intersection", cfg.AllowedTools)
	}
}

func TestPrepareTurnContextRejectsUnsafePRBaseRepo(t *testing.T) {
	root := t.TempDir()
	turn := &TurnContext{
		WorkDir: root,
		Metadata: map[string]string{
			"prBaseRepo": "https://127.0.0.1/private/repo.git",
		},
	}
	_, err := PrepareTurnContext(context.Background(), turn, root, filepath.Join(t.TempDir(), "artifacts"))
	if err == nil {
		t.Fatal("PrepareTurnContext error = nil, want unsafe PR base repo rejection")
	}
	if !strings.Contains(err.Error(), "validate PR base repo") {
		t.Fatalf("PrepareTurnContext error = %v, want PR base validation", err)
	}
}

func TestPrepareTurnContextUsesTrustedOpencodeArtifactsDirectory(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	artifactDir := filepath.Join(workDir, opencodeWorkspaceArtifactsDir)
	turn := &TurnContext{
		RuntimeName: RuntimeOpencode,
		WorkDir:     workDir,
		Env:         []string{"ORKA_ARTIFACTS_DIR=/tmp/caller-controlled"},
	}
	if _, err := PrepareTurnContext(context.Background(), turn, root, artifactDir); err != nil {
		t.Fatalf("PrepareTurnContext() error = %v", err)
	}
	if turn.ArtifactsDir != artifactDir {
		t.Fatalf("ArtifactsDir = %q, want %q", turn.ArtifactsDir, artifactDir)
	}
	if got := envEntryValue(turn.Env, "ORKA_ARTIFACTS_DIR"); got != artifactDir {
		t.Fatalf("ORKA_ARTIFACTS_DIR = %q, want trusted %q", got, artifactDir)
	}
	info, err := os.Lstat(artifactDir)
	if err != nil {
		t.Fatalf("lstat artifacts directory: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("artifacts path mode = %v, want real directory", info.Mode())
	}
}

func TestPrepareTurnArtifactsDirForRuntimeRejectsSymlinkedWorkDirBeforeWrite(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	workDir := filepath.Join(root, "repo")
	if err := os.Symlink(outside, workDir); err != nil {
		t.Skipf("create symlinked workdir: %v", err)
	}
	artifactDir := filepath.Join(workDir, opencodeWorkspaceArtifactsDir)
	err := prepareTurnArtifactsDirForRuntime(TurnContext{
		RuntimeName: RuntimeOpencode,
		RootDir:     root,
		WorkDir:     workDir,
	}, artifactDir)
	if err == nil || !strings.Contains(err.Error(), "outside root") {
		t.Fatalf("prepareTurnArtifactsDirForRuntime() error = %v, want symlinked workdir rejection", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, opencodeWorkspaceArtifactsDir)); !os.IsNotExist(err) {
		t.Fatalf("outside artifact path was created, lstat error = %v", err)
	}
}

func TestTurnArtifactDirUsesOpencodeWorkDir(t *testing.T) {
	workspace := preparedWorkspace{
		workDir: filepath.Join("/tmp", "turn", "repo", "subdir"),
		rootDir: filepath.Join("/tmp", "turn", "repo"),
		baseDir: filepath.Join("/tmp", "turn"),
	}
	want := filepath.Join(workspace.workDir, opencodeWorkspaceArtifactsDir)
	for _, turn := range []TurnContext{
		{RuntimeName: RuntimeOpencode},
		{RuntimeName: RuntimeMulti, Metadata: map[string]string{"runtime": RuntimeOpencode}},
	} {
		if got := turnArtifactDir(workspace, turn); got != want {
			t.Fatalf("turnArtifactDir() = %q, want %q", got, want)
		}
	}
}

func TestPrepareTurnWorkspaceAlwaysCreatesWorkDirWithoutRepo(t *testing.T) {
	workspace, err := prepareTurnWorkspace(context.Background(), TurnContext{})
	if err != nil {
		t.Fatalf("prepareTurnWorkspace() error = %v", err)
	}
	defer workspace.cleanup()
	if workspace.workDir == "" || workspace.rootDir == "" {
		t.Fatalf("prepared workspace = %#v, want non-empty workDir and rootDir", workspace)
	}
	info, err := os.Stat(workspace.workDir)
	if err != nil {
		t.Fatalf("stat workdir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("workdir mode = %v, want directory", info.Mode())
	}
}

func TestPrepareTurnArtifactsDirForRuntimeRejectsPreexistingOpencodePath(t *testing.T) {
	rootDir := t.TempDir()
	workDir := filepath.Join(rootDir, "repo")
	artifactDir := filepath.Join(workDir, opencodeWorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact path: %v", err)
	}
	marker := filepath.Join(artifactDir, "repository-file")
	if err := os.WriteFile(marker, []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write repository marker: %v", err)
	}
	err := prepareTurnArtifactsDirForRuntime(TurnContext{
		RuntimeName: RuntimeOpencode,
		RootDir:     rootDir,
		WorkDir:     workDir,
	}, artifactDir)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("prepareTurnArtifactsDirForRuntime() error = %v, want reserved path rejection", err)
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "keep\n" {
		t.Fatalf("preexisting artifact marker = %q, %v; want preserved", got, readErr)
	}
}

func TestEnsureDirectoryTraversableAddsNonOwnerExecute(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod workdir: %v", err)
	}
	if err := ensureDirectoryTraversable(dir); err != nil {
		t.Fatalf("ensureDirectoryTraversable() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat workdir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o711 {
		t.Fatalf("workdir mode = %#o, want 0711", got)
	}
}

func TestEnsureOpencodeWorkDirAccessibleAddsWrapperGroupAccess(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod workdir: %v", err)
	}
	if err := ensureOpencodeWorkDirAccessible(dir); err != nil {
		t.Fatalf("ensureOpencodeWorkDirAccessible() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat workdir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o770 {
		t.Fatalf("workdir mode = %#o, want 0770", got)
	}
}

func TestValidateWorkspaceRepoURLRejectsLocalInputs(t *testing.T) {
	withWorkspaceHostLookup(t, map[string][]net.IPAddr{
		"github.com": {{IP: net.ParseIP("140.82.112.3")}},
	})
	for _, repo := range []string{
		"/tmp/repo",
		"file:///tmp/repo",
		"https://localhost/repo.git",
		"https://127.0.0.1/repo.git",
		"https://10.0.0.1/repo.git",
		"https://metadata.local/repo.git",
		"https://user@github.com/orka-agents/orka.git",
		"https://github.com/orka-agents/orka.git?credential=value",
	} {
		if err := validateWorkspaceRepoURL(repo); err == nil {
			t.Fatalf("validateWorkspaceRepoURL(%q) error = nil, want rejection", repo)
		}
	}
}

func TestValidateWorkspaceRepoURLAllowsHTTPSRemote(t *testing.T) {
	withWorkspaceHostLookup(t, map[string][]net.IPAddr{
		"gitlab.com": {{IP: net.ParseIP("172.65.251.78")}},
	})
	if err := validateWorkspaceRepoURL("https://gitlab.com/group/repo.git"); err != nil {
		t.Fatalf("validateWorkspaceRepoURL() error = %v", err)
	}
}

func TestValidateWorkspaceRepoURLAllowsConfiguredEnterpriseHost(t *testing.T) {
	t.Setenv(envAllowedGitHosts, "git.example.com")
	withWorkspaceHostLookup(t, map[string][]net.IPAddr{
		"git.example.com": {{IP: net.ParseIP("203.0.113.10")}},
	})
	if err := validateWorkspaceRepoURL("https://git.example.com/group/repo.git"); err != nil {
		t.Fatalf("validateWorkspaceRepoURL() error = %v", err)
	}
}

func TestValidateWorkspaceRepoURLRejectsPrivateResolvedHost(t *testing.T) {
	withWorkspaceHostLookup(t, map[string][]net.IPAddr{
		"git.internal.example.com": {{IP: net.ParseIP("10.0.0.10")}},
	})
	if err := validateWorkspaceRepoURL("https://git.internal.example.com/group/repo.git"); err == nil {
		t.Fatal("validateWorkspaceRepoURL() error = nil, want private resolved host rejection")
	}
}

func withWorkspaceHostLookup(t *testing.T, hosts map[string][]net.IPAddr) {
	t.Helper()
	original := lookupWorkspaceHostIPs
	lookupWorkspaceHostIPs = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if addrs, ok := hosts[host]; ok {
			return addrs, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { lookupWorkspaceHostIPs = original })
}

func resultPathRejectedError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "must not be a symlink") ||
		strings.Contains(message, "not a directory")
}
