package cliwrapper

import (
	"context"
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
// no-op when /tmp/artifacts is absent.
func ClearTurnArtifacts() {
	if err := removeAllForChild("/tmp/artifacts"); err != nil {
		_ = os.RemoveAll("/tmp/artifacts")
	}
}

func UploadTurnArtifacts(turn TurnContext) error {
	restoreTurnEnv := setTemporaryEnvEntries(turn.Env)
	defer restoreTurnEnv()
	restoreTaskName := setTemporaryEnv(workerenv.TaskName, turn.TaskName)
	defer restoreTaskName()
	restoreTaskNamespace := setTemporaryEnv(workerenv.TaskNamespace, turn.Namespace)
	defer restoreTaskNamespace()
	err := common.UploadArtifacts()
	return err
}

func PrepareTurnContext(ctx context.Context, turn *TurnContext, workspaceRoot string) (*common.AgentConfig, error) {
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
	if root != "" {
		if err := common.EnsureWorkspaceArtifactsLink(root); err != nil {
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
	return cfg, nil
}

func EnsureTurnRequiredSecurityArtifacts(ctx context.Context, cfg *common.AgentConfig, result string) (string, error) {
	return common.EnsureRequiredSecurityArtifacts(ctx, cfg, result, nil)
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
		if key == "PATH" {
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
