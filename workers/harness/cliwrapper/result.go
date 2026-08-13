package cliwrapper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const wrapperSafeCommandPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

const turnMetadataSkillsFiles = "skillsFiles"

// wrapperGitPostTurnTimeout bounds each post-turn git finalization phase
// (ShouldFinalizeWorkDir, FinalizeTurnResult, CleanFinalizedWorkDir) so a hung
// git process cannot wedge the turn. It is applied per phase, never shared
// across phases, and must stay generous enough for `git add -A` plus a cached
// binary diff on multi-gigabyte working trees.
const wrapperGitPostTurnTimeout = 5 * time.Minute

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

// FinalizeTurnResult builds the bounded structured workspace result inside the
// dedicated child identity boundary. Harness v1 workspaces never carry
// publication authority, so finalization records a diff without committing or
// pushing it. A non-git or empty workDir falls back to the raw agent output.
func FinalizeTurnResult(ctx context.Context, workDir, output string) ([]byte, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return []byte(output), nil
	}
	if strings.TrimSpace(os.Getenv(workerenv.PushBranch)) != "" {
		return nil, errors.New("harness v1 wrapper finalization does not permit branch publication")
	}
	ctx, cancel := context.WithTimeout(ctx, wrapperGitPostTurnTimeout)
	defer cancel()
	baseSHA, err := wrapperGitOutput(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return []byte(output), nil
	}
	baseSHA = strings.TrimSpace(baseSHA)
	if err := neutralizeWrapperRepoExecutableConfig(ctx, workDir); err != nil {
		return nil, fmt.Errorf("neutralize child-owned repository executable Git configuration: %w", err)
	}
	if staged, err := wrapperGitOutput(ctx, workDir, "add", "-A"); err != nil {
		return nil, fmt.Errorf("stage harness v1 workspace result: %w: %s", err, strings.TrimSpace(staged))
	}
	_, _ = wrapperGitOutput(ctx, workDir, "reset", "-q", "--", ".orka-artifacts", ":(glob)**/.orka-artifacts")
	diff, err := wrapperGitOutput(
		ctx, workDir, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv",
	)
	if err != nil {
		return nil, fmt.Errorf("render harness v1 workspace diff: %w", err)
	}
	files := make([]string, 0)
	for _, args := range [][]string{
		{"diff", "--cached", "--name-status", "-z", "--no-ext-diff", "--no-textconv"},
		{"diff", "--name-status", "-z", "--no-ext-diff", "--no-textconv"},
	} {
		names, namesErr := wrapperGitOutput(ctx, workDir, args...)
		if namesErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			continue
		}
		files = append(files, parseWrapperDiffNameStatusPaths(names)...)
	}
	files = uniqueWrapperPaths(files)
	result := &common.StructuredResult{
		Summary: common.TruncateStructuredSummary(output),
		BaseSHA: baseSHA,
		HeadSHA: baseSHA,
	}
	if diff != "" {
		result.Diff = diff
		result.Files = files
	}
	return common.FormatStructuredResult(result)
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
	resolvedArtifactDir := firstNonEmpty(artifactDir, wrapperArtifactsDir())
	if err := prepareArtifactsForWrapper(resolvedArtifactDir); err != nil {
		return fmt.Errorf("prepare artifacts for wrapper upload: %w", err)
	}
	restoreTurnEnv := setTemporaryEnvEntries(turn.Env)
	defer restoreTurnEnv()
	restoreArtifactDir := setTemporaryEnv("ORKA_ARTIFACTS_DIR", resolvedArtifactDir)
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
	skillsRoot := ""
	if root != "" && strings.TrimSpace(turn.Metadata[turnMetadataSkillsFiles]) != "" {
		skillsRoot = turnSkillsRoot(turn, artifactDir)
	}
	if root != "" {
		if strings.TrimSpace(cfg.PRBaseRepo) != "" {
			if err := validateWorkspaceRepoURL(cfg.PRBaseRepo); err != nil {
				return cfg, fmt.Errorf("validate PR base repo: %w", err)
			}
		}
		if err := common.EnsureWorkspaceArtifactsLink(root); err != nil {
			return cfg, err
		}
		if err := materializeTurnSkillFiles(skillsRoot, turn.Metadata[turnMetadataSkillsFiles]); err != nil {
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
	if skillsRoot != "" {
		turn.Env = setEnv(turn.Env, workerenv.SkillsDir, skillsRoot)
	}
	return cfg, nil
}

func turnSkillsRoot(turn *TurnContext, _ string) string {
	if turn != nil {
		if root := strings.TrimSpace(turn.SkillsRoot); root != "" && filepath.IsAbs(filepath.Clean(root)) {
			return filepath.Clean(root)
		}
	}
	identity := "unknown"
	if turn != nil {
		identity = strings.Join([]string{turn.RuntimeSessionID, turn.TurnID, turn.CorrelationID}, "|")
	}
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(os.TempDir(), "orka-harness-skills", hex.EncodeToString(sum[:8]))
}

func materializeTurnSkillFiles(skillsRoot, raw string) error {
	skillsRoot = strings.TrimSpace(skillsRoot)
	raw = strings.TrimSpace(raw)
	if skillsRoot == "" || raw == "" {
		return nil
	}
	var files map[string]string
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return fmt.Errorf("parse turn skill files: %w", err)
	}
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
	if err := prepareArtifactsForWrapper(artifactDir); err != nil {
		return result, fmt.Errorf("prepare artifacts for wrapper security validation: %w", err)
	}
	restoreArtifactDir := setTemporaryEnv("ORKA_ARTIFACTS_DIR", artifactDir)
	defer restoreArtifactDir()
	wrappedFollowUp := followUp
	if followUp != nil {
		wrappedFollowUp = func(ctx context.Context, prompt string) (string, error) {
			if err := prepareArtifactsForChild(artifactDir); err != nil {
				return "", fmt.Errorf("prepare artifacts for child follow-up: %w", err)
			}
			followUpResult, followUpErr := followUp(ctx, prompt)
			if err := prepareArtifactsForWrapper(artifactDir); err != nil && followUpErr == nil {
				followUpErr = fmt.Errorf("prepare artifacts for wrapper after follow-up: %w", err)
			}
			return followUpResult, followUpErr
		}
	}
	return common.EnsureRequiredSecurityArtifacts(ctx, cfg, result, wrappedFollowUp)
}

func agentConfigForTurn(turn TurnContext) *common.AgentConfig {
	maxTurns := 50
	maxTurnsValue := firstNonEmpty(turn.Metadata["maxTurns"], envEntryValue(turn.Env, workerenv.MaxTurns))
	if parsed, err := strconv.Atoi(maxTurnsValue); err == nil && parsed > 0 {
		maxTurns = parsed
	}
	timeoutSeconds := 0
	timeoutValue := firstNonEmpty(
		turn.Metadata["timeoutSeconds"],
		envEntryValue(turn.Env, workerenv.TimeoutSeconds),
	)
	if parsed, err := strconv.Atoi(timeoutValue); err == nil && parsed > 0 {
		timeoutSeconds = parsed
	}
	return &common.AgentConfig{
		TaskName:       turn.TaskName,
		TaskNamespace:  turn.Namespace,
		Prompt:         turn.Prompt,
		Model:          firstNonEmpty(turn.Metadata["model"], envEntryValue(turn.Env, workerenv.Model)),
		SystemPrompt:   firstNonEmpty(turn.Metadata["systemPrompt"], envEntryValue(turn.Env, workerenv.SystemPrompt)),
		MaxTurns:       maxTurns,
		TimeoutSeconds: timeoutSeconds,
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
	restores := make([]func(), 0, len(entries)+4)
	hasGitToken := false
	gitHubAuth := ""
	gitRepo := ""
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		key = strings.TrimSpace(key)
		if key == workerenv.GitToken && strings.TrimSpace(value) != "" {
			hasGitToken = true
		}
		if key == workerenv.GitHubToken {
			gitHubAuth = strings.TrimSpace(value)
		}
		if key == workerenv.GitRepo {
			gitRepo = strings.TrimSpace(value)
		}
		if temporaryEnvEntryBlocked(key) {
			continue
		}
		restores = append(restores, setTemporaryEnv(key, value))
	}
	if !hasGitToken && gitHubAuth != "" && workspaceRepoUsesGitHubToken(gitRepo) {
		restores = append(restores, setTemporaryEnv(workerenv.GitToken, gitHubAuth))
		hasGitToken = true
	}
	if hasGitToken {
		restores = append(restores, setTemporaryEnv(workerenv.GitAskpass, controllerGitAskpassPath))
		restores = append(restores, setTemporaryEnv("GIT_ASKPASS", controllerGitAskpassPath))
	}
	restores = append(restores, setTemporaryEnv("HOME", "/tmp/orka-empty-git-home"))
	restores = append(restores, setTemporaryEnv("XDG_CONFIG_HOME", "/tmp/orka-empty-git-config"))
	restores = append(restores, setTemporaryEnv("XDG_CONFIG_DIRS", "/tmp/orka-empty-git-config-dirs"))
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
	if upper == "ORKA_ARTIFACTS_DIR" || upper == "HOME" ||
		upper == "XDG_CONFIG_HOME" || upper == "XDG_CONFIG_DIRS" ||
		upper == workerenv.GitAskpass {
		return true
	}
	switch upper {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT":
		return true
	}
	if strings.HasPrefix(upper, "DYLD_") {
		return true
	}
	if strings.HasPrefix(upper, "GIT_") {
		switch key {
		case workerenv.GitToken, workerenv.GitHubToken, workerenv.GitUsername:
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

func wrapperGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := wrapperGitCommand(ctx, dir, args...).CombinedOutput()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return string(out), ctxErr
	}
	return string(out), err
}

func neutralizeWrapperRepoExecutableConfig(ctx context.Context, dir string) error {
	raw, err := wrapperGitOutput(ctx, dir, "config", "--local", "--no-includes", "--name-only", "-z", "--list")
	if err != nil {
		return fmt.Errorf("list local Git configuration: %w", err)
	}
	for key := range strings.SplitSeq(raw, "\x00") {
		key = strings.TrimSpace(key)
		if key == "" || !wrapperExecutableGitConfigKey(key) {
			continue
		}
		if out, err := wrapperGitOutput(
			ctx, dir, "config", "--local", "--no-includes", "--unset-all", key,
		); err != nil {
			return fmt.Errorf("remove executable Git configuration %q: %w: %s", key, err, strings.TrimSpace(out))
		}
	}
	return nil
}

func wrapperExecutableGitConfigKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, prefix := range []string{"alias.", "credential.", "filter.", "include.", "includeif."} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	switch key {
	case "core.editor", "core.fsmonitor", "core.fsmonitorhookversion", "core.hookspath",
		"core.pager", "core.sshcommand", "diff.external", "gpg.program", "sequence.editor":
		return true
	}
	return strings.HasPrefix(key, "diff.") &&
		(strings.HasSuffix(key, ".command") || strings.HasSuffix(key, ".textconv")) ||
		strings.HasPrefix(key, "merge.") && strings.HasSuffix(key, ".driver")
}

func parseWrapperDiffNameStatusPaths(raw string) []string {
	parts := strings.Split(raw, "\x00")
	paths := make([]string, 0, len(parts))
	for index := 0; index < len(parts); {
		status := strings.TrimSpace(parts[index])
		index++
		if status == "" {
			continue
		}
		pathCount := 1
		if status[0] == 'R' || status[0] == 'C' {
			pathCount = 2
		}
		for range pathCount {
			if index >= len(parts) {
				return uniqueWrapperPaths(paths)
			}
			if parts[index] != "" {
				paths = append(paths, parts[index])
			}
			index++
		}
	}
	return uniqueWrapperPaths(paths)
}

func uniqueWrapperPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	return unique
}

func wrapperGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	safeDir := strings.TrimSpace(dir)
	if abs, err := filepath.Abs(safeDir); err == nil {
		safeDir = abs
	}
	gitArgs := append([]string{
		"-c", "safe.directory=" + safeDir,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"-c", "core.askPass=",
		"-c", "diff.external=",
		"-c", "core.pager=cat",
		"-C", dir,
	}, args...)
	cmd := exec.CommandContext(ctx, wrapperGitBinary, gitArgs...)
	cmd.Env = []string{
		"GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=/tmp/orka-empty-git-home",
		"LC_ALL=C",
		"PATH=" + wrapperSafeCommandPath,
		"SSH_ASKPASS=/bin/false",
		"XDG_CONFIG_HOME=/tmp/orka-empty-git-config",
	}
	cmd.SysProcAttr = commandSysProcAttr()
	return cmd
}

func ShouldFinalizeWorkDir(ctx context.Context, workDir string) (bool, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, wrapperGitPostTurnTimeout)
	defer cancel()
	_, err := wrapperGitOutput(ctx, workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	return true, nil
}

func CleanFinalizedWorkDir(ctx context.Context, workDir string) error {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, wrapperGitPostTurnTimeout)
	defer cancel()
	rootOut, err := wrapperGitOutput(ctx, workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return nil
	}
	repoRoot := strings.TrimSpace(rootOut)
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
		if out, err := wrapperGitOutput(ctx, repoRoot, "reset", "--hard", "HEAD"); err != nil {
			return fmt.Errorf("clean finalized workdir reset: %w: %s", err, strings.TrimSpace(out))
		}
		if out, err := wrapperGitOutput(ctx, repoRoot, "clean", "-fd"); err != nil {
			return fmt.Errorf("clean finalized workdir clean: %w: %s", err, strings.TrimSpace(out))
		}
		return nil
	}
	if out, err := wrapperGitOutput(ctx, repoRoot, "reset", "HEAD", "--", relPath); err != nil {
		return fmt.Errorf("clean finalized workdir unstage: %w: %s", err, strings.TrimSpace(out))
	}
	if out, err := wrapperGitOutput(ctx, repoRoot, "checkout", "--", relPath); err != nil {
		return fmt.Errorf("clean finalized workdir checkout: %w: %s", err, strings.TrimSpace(out))
	}
	if out, err := wrapperGitOutput(ctx, repoRoot, "clean", "-fd", "--", relPath); err != nil {
		return fmt.Errorf("clean finalized workdir clean: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}
