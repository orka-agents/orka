package cliwrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/workers/common"
)

const wrapperSafeCommandPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

const turnMetadataSkillsFiles = "skillsFiles"

var wrapperGitBinary = resolveSafeExecutable("git")

func resolveSafeExecutable(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsRune(name, os.PathSeparator) {
		return name
	}
	for _, dir := range filepath.SplitList(wrapperSafeCommandPath) {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return name
}

// FinalizeTurnResult reuses the existing worker structured-result and workspace
// diff finalization behavior. A non-git or empty workDir falls back to the raw
// agent output, matching workers/common.FinalizeResult semantics.
func FinalizeTurnResult(workDir, output string) ([]byte, error) {
	return common.FinalizeResult(workDir, output)
}

// UploadTurnArtifacts reuses the existing worker artifact uploader. It is a
// no-op when the selected turn artifact directory is absent.
func ClearTurnArtifacts(artifactDirs ...string) {
	artifactDir := firstNonEmpty(artifactDirs...)
	if artifactDir == "" {
		artifactDir = wrapperArtifactsDir()
	}
	_ = removeAllForChild(artifactDir)
	_ = os.RemoveAll(artifactDir)
}

func wrapperArtifactsDir() string {
	if dir := strings.TrimSpace(os.Getenv("ORKA_ARTIFACTS_DIR")); dir != "" {
		return filepath.Clean(dir)
	}
	return "/tmp/artifacts"
}

func UploadTurnArtifacts(turn TurnContext, artifactDir string) error {
	restoreTurnEnv := setTemporaryEnvEntries(turn.Env)
	defer restoreTurnEnv()
	restoreArtifactDir := setTemporaryEnv("ORKA_ARTIFACTS_DIR", artifactDir)
	defer restoreArtifactDir()
	restoreTaskName := setTemporaryEnv(workerenv.TaskName, turn.TaskName)
	defer restoreTaskName()
	restoreTaskNamespace := setTemporaryEnv(workerenv.TaskNamespace, turn.Namespace)
	defer restoreTaskNamespace()
	err := common.UploadArtifacts()
	return err
}

func PrepareTurnContext(
	ctx context.Context,
	turn *TurnContext,
	workspaceRoot string,
	artifactDir string,
) (*common.AgentConfig, error) {
	if turn == nil {
		return nil, nil
	}
	cfg := agentConfigForTurn(*turn)
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		root = strings.TrimSpace(turn.WorkDir)
	}
	restoreEnv := setTemporaryEnvEntries(turn.Env)
	defer restoreEnv()
	restoreArtifactDir := setTemporaryEnv("ORKA_ARTIFACTS_DIR", artifactDir)
	defer restoreArtifactDir()
	if root != "" {
		if err := common.EnsureWorkspaceArtifactsLink(root); err != nil {
			return cfg, err
		}
		if err := materializeTurnSkillFiles(root, turn.Metadata[turnMetadataSkillsFiles]); err != nil {
			return cfg, err
		}
		if err := common.PrepareWorkspace(root); err != nil {
			return cfg, err
		}
		if err := common.PreparePullRequestReviewContext(root, cfg); err != nil {
			return cfg, err
		}
		if err := common.PrepareSecurityReviewContext(root, cfg); err != nil {
			return cfg, err
		}
	}
	turn.Prompt = cfg.Prompt
	turn.Env = setEnv(turn.Env, workerenv.Prompt, cfg.Prompt)
	if artifactDir != "" {
		turn.Env = setEnv(turn.Env, "ORKA_ARTIFACTS_DIR", artifactDir)
	}
	if root != "" && strings.TrimSpace(turn.Metadata[turnMetadataSkillsFiles]) != "" {
		turn.Env = setEnv(turn.Env, workerenv.SkillsDir, filepath.Join(root, ".skills"))
	}
	return cfg, nil
}

func materializeTurnSkillFiles(root, raw string) error {
	root = strings.TrimSpace(root)
	raw = strings.TrimSpace(raw)
	if root == "" || raw == "" {
		return nil
	}
	var files map[string]string
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return fmt.Errorf("parse turn skill files: %w", err)
	}
	skillsRoot := filepath.Join(root, ".skills")
	if err := removeAllForChild(skillsRoot); err != nil {
		return fmt.Errorf("clear turn skills directory: %w", err)
	}
	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		return fmt.Errorf("create turn skills directory: %w", err)
	}
	for rel, content := range files {
		clean := filepath.Clean(strings.TrimSpace(rel))
		if clean == "." || clean == "" || filepath.IsAbs(clean) ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
			return fmt.Errorf("invalid turn skill file path %q", rel)
		}
		target := filepath.Join(skillsRoot, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create turn skill file directory: %w", err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write turn skill file %q: %w", rel, err)
		}
	}
	return nil
}

func EnsureTurnRequiredSecurityArtifacts(
	ctx context.Context,
	cfg *common.AgentConfig,
	result string,
	followUp common.SecurityArtifactFollowUp,
	artifactDir string,
) (string, error) {
	restoreArtifactDir := setTemporaryEnv("ORKA_ARTIFACTS_DIR", artifactDir)
	defer restoreArtifactDir()
	return common.EnsureRequiredSecurityArtifacts(ctx, cfg, result, followUp)
}

func agentConfigForTurn(turn TurnContext) *common.AgentConfig {
	maxTurns := 50
	maxTurnsValue := firstNonEmpty(turn.Metadata["maxTurns"], envEntryValue(turn.Env, workerenv.MaxTurns))
	if parsed, err := strconv.Atoi(maxTurnsValue); err == nil && parsed > 0 {
		maxTurns = parsed
	}
	return &common.AgentConfig{
		TaskName:      turn.TaskName,
		TaskNamespace: turn.Namespace,
		Prompt:        turn.Prompt,
		Model:         firstNonEmpty(turn.Metadata["model"], envEntryValue(turn.Env, workerenv.Model)),
		SystemPrompt:  firstNonEmpty(turn.Metadata["systemPrompt"], envEntryValue(turn.Env, workerenv.SystemPrompt)),
		MaxTurns:      maxTurns,
		AllowedTools: splitCSV(firstNonEmpty(
			turn.Metadata["allowedTools"],
			envEntryValue(turn.Env, workerenv.AllowedTools),
		)),
		DisallowedTools: splitCSV(firstNonEmpty(
			turn.Metadata["disallowedTools"],
			envEntryValue(turn.Env, workerenv.DisallowedTools),
		)),
		GitRepo:      firstNonEmpty(turn.Metadata["gitRepo"], envEntryValue(turn.Env, workerenv.GitRepo)),
		GitBranch:    firstNonEmpty(turn.Metadata["gitBranch"], envEntryValue(turn.Env, workerenv.GitBranch)),
		GitRef:       firstNonEmpty(turn.Metadata["gitRef"], envEntryValue(turn.Env, workerenv.GitRef)),
		PRBaseBranch: firstNonEmpty(turn.Metadata["prBaseBranch"], envEntryValue(turn.Env, workerenv.PRBaseBranch)),
		PRBaseRepo:   firstNonEmpty(turn.Metadata["prBaseRepo"], envEntryValue(turn.Env, workerenv.PRBaseRepo)),
		PRBaseSHA:    firstNonEmpty(turn.Metadata["prBaseSHA"], envEntryValue(turn.Env, workerenv.PRBaseSHA)),
		SubPath:      firstNonEmpty(turn.Metadata["workspaceSubPath"], envEntryValue(turn.Env, workerenv.WorkspaceSubpath)),
	}
}

func setTemporaryEnvEntries(entries []string) func() {
	restores := make([]func(), 0, len(entries)+1)
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		key = strings.TrimSpace(key)
		if temporaryEnvEntryBlocked(key) {
			continue
		}
		restores = append(restores, setTemporaryEnv(key, value))
	}
	restores = append(restores, setTemporaryEnv("PATH", wrapperSafeCommandPath))
	return func() {
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
	}
}

func temporaryEnvEntryBlocked(key string) bool {
	key = strings.TrimSpace(key)
	if key == "PATH" {
		return true
	}
	upper := strings.ToUpper(key)
	if upper == "ORKA_ARTIFACTS_DIR" {
		return true
	}
	switch upper {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	}
	if strings.HasPrefix(upper, "GIT_") {
		switch key {
		case workerenv.GitToken, workerenv.GitHubToken, workerenv.GitAskpass, workerenv.GitUsername:
			return false
		default:
			return true
		}
	}
	return false
}

func setTemporaryEnv(key, value string) func() {
	previous, hadPrevious := os.LookupEnv(key)
	if strings.TrimSpace(value) == "" {
		return func() {}
	}
	_ = os.Setenv(key, value)
	return func() {
		if hadPrevious {
			_ = os.Setenv(key, previous)
			return
		}
		_ = os.Unsetenv(key)
	}
}

func wrapperGitCommand(dir string, args ...string) *exec.Cmd {
	safeDir := strings.TrimSpace(dir)
	if abs, err := filepath.Abs(safeDir); err == nil {
		safeDir = abs
	}
	gitArgs := append([]string{"-c", "safe.directory=" + safeDir, "-C", dir}, args...)
	cmd := exec.Command(wrapperGitBinary, gitArgs...)
	cmd.Env = setEnv(os.Environ(), "PATH", wrapperSafeCommandPath)
	cmd.SysProcAttr = commandSysProcAttr()
	return cmd
}

func ShouldFinalizeWorkDir(workDir string) bool {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return false
	}
	return wrapperGitCommand(workDir, "rev-parse", "--show-toplevel").Run() == nil
}

func CleanFinalizedWorkDir(workDir string) error {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil
	}
	rootOut, err := wrapperGitCommand(workDir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	repoRoot := strings.TrimSpace(string(rootOut))
	if repoRoot == "" {
		return nil
	}
	cleanPath, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("clean finalized workdir path: %w", err)
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("clean finalized repo root path: %w", err)
	}
	relPath, err := filepath.Rel(repoRoot, cleanPath)
	if err != nil || strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
		return fmt.Errorf("clean finalized workdir %q is outside repository root %q", cleanPath, repoRoot)
	}
	if relPath == "." {
		if out, err := wrapperGitCommand(repoRoot, "reset", "--hard", "HEAD").CombinedOutput(); err != nil {
			return fmt.Errorf("clean finalized workdir reset: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := wrapperGitCommand(repoRoot, "clean", "-fd").CombinedOutput(); err != nil {
			return fmt.Errorf("clean finalized workdir clean: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if out, err := wrapperGitCommand(repoRoot, "reset", "HEAD", "--", relPath).CombinedOutput(); err != nil {
		return fmt.Errorf("clean finalized workdir unstage: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := wrapperGitCommand(repoRoot, "checkout", "--", relPath).CombinedOutput(); err != nil {
		return fmt.Errorf("clean finalized workdir checkout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := wrapperGitCommand(repoRoot, "clean", "-fd", "--", relPath).CombinedOutput(); err != nil {
		return fmt.Errorf("clean finalized workdir clean: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
