package cliwrapper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	defaultOpencodePath              = "opencode"
	opencodeProviderName             = "engine"
	opencodeDefaultAgent             = "build"
	opencodeModelContextLimit        = 128000
	opencodeModelOutputLimit         = 8192
	opencodeStartupInstructionsLimit = 96 << 10 // Conservative byte budget below the 128k-token model context.
	opencodeWorkspaceArtifactsDir    = ".orka-artifacts"
	opencodeDisableProjectConfig     = "OPENCODE_DISABLE_PROJECT_CONFIG"
	opencodeDisableAutoUpdate        = "OPENCODE_DISABLE_AUTOUPDATE"
	opencodeDisableExternalSkills    = "OPENCODE_DISABLE_EXTERNAL_SKILLS"
	opencodeConfigPathEnv            = "OPENCODE_CONFIG"
	opencodeEnvPrefix                = "OPENCODE_"
	opencodePermissionAllow          = "allow"
	opencodePermissionDeny           = "deny"
	opencodePermissionWebSearch      = "websearch"
	opencodeEnvTrue                  = "true"
)

// Caller-supplied OPENCODE_* controls are stripped, so instruction discovery mirrors OpenCode's default order.
var opencodeProjectInstructionFiles = []string{"AGENTS.md", "CLAUDE.md", "CONTEXT.md"}

type OpencodeAdapter struct {
	config OpencodeAdapterConfig
}

type opencodeConfig struct {
	Schema       string                      `json:"$schema"`
	Provider     map[string]opencodeProvider `json:"provider"`
	Agent        map[string]opencodeAgent    `json:"agent"`
	Permission   map[string]string           `json:"permission"`
	Instructions []string                    `json:"instructions,omitempty"`
	Share        string                      `json:"share"`
	AutoUpdate   bool                        `json:"autoupdate"`
}

type opencodeAgent struct {
	Steps int `json:"steps"`
}

type opencodeProvider struct {
	NPM     string                   `json:"npm"`
	Name    string                   `json:"name"`
	Options opencodeProviderOptions  `json:"options"`
	Models  map[string]opencodeModel `json:"models"`
}

type opencodeProviderOptions struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
}

type opencodeModel struct {
	Limit opencodeModelLimit `json:"limit"`
}

type opencodeModelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type opencodeOutputEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

func NewOpencodeAdapter(config OpencodeAdapterConfig) *OpencodeAdapter {
	return &OpencodeAdapter{config: config}
}

func (a *OpencodeAdapter) Name() string { return RuntimeOpencode }

func (a *OpencodeAdapter) BuildCommand(ctx context.Context, turn TurnContext) (*CommandSpec, error) {
	agentCfg := agentConfigFromTurn(turn)
	model := strings.TrimSpace(agentCfg.Model)
	if model == "" {
		return nil, fmt.Errorf("%s is required for opencode runtime", workerenv.Model)
	}
	if err := validateOpencodeConfigLiteral(workerenv.Model, model); err != nil {
		return nil, err
	}
	dir := firstNonEmpty(turn.WorkDir, a.config.WorkDir, DefaultWrapperWorkDir)
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat opencode workspace directory: %w", err)
		}
		if wd, wdErr := os.Getwd(); wdErr == nil {
			dir = wd
		}
	}
	dir, err := resolveOpencodeDirectory(dir, "workspace directory")
	if err != nil {
		return nil, err
	}

	baseURL := opencodeBaseURL(envEntryValue(turn.Env, workerenv.OpenAIBaseURL))
	if baseURL == "" {
		return nil, fmt.Errorf("%s is required for opencode runtime", workerenv.OpenAIBaseURL)
	}
	if err := validateOpencodeConfigLiteral(workerenv.OpenAIBaseURL, baseURL); err != nil {
		return nil, err
	}
	apiKey := envEntryValue(turn.Env, workerenv.OpenAIAPIKey)
	if err := validateOpencodeConfigLiteral(workerenv.OpenAIAPIKey, apiKey); err != nil {
		return nil, err
	}
	trustedArtifactDir := ""
	if strings.TrimSpace(turn.ArtifactsDir) != "" {
		trustedArtifactDir = filepath.Join(dir, opencodeWorkspaceArtifactsDir)
	}
	configPath, scratchDir, err := writeOpencodeConfig(
		ctx,
		agentCfg,
		baseURL,
		apiKey,
		dir,
		firstNonEmpty(turn.RootDir, dir),
		trustedArtifactDir,
	)
	if err != nil {
		return nil, err
	}

	xdgConfigHome := filepath.Join(scratchDir, "config")
	xdgDataHome := filepath.Join(scratchDir, "data")
	xdgCacheHome := filepath.Join(scratchDir, "cache")
	xdgStateHome := filepath.Join(scratchDir, "state")
	env, unsetEnv := buildOpencodeEnv(
		turn.Env,
		scratchDir,
		xdgConfigHome,
		xdgDataHome,
		xdgCacheHome,
		xdgStateHome,
		configPath,
	)
	if trustedArtifactDir != "" {
		env = setEnv(env, "ORKA_ARTIFACTS_DIR", trustedArtifactDir)
	} else {
		unsetEnv = append(unsetEnv, "ORKA_ARTIFACTS_DIR")
	}

	args := []string{"run", "--dir", dir, "--format", "json", "--model", opencodeProviderName + "/" + model}
	return &CommandSpec{
		Path:      firstNonEmpty(a.config.Path, defaultOpencodePath),
		Args:      args,
		Env:       env,
		UnsetEnv:  unsetEnv,
		Dir:       dir,
		Stdin:     []byte(turn.Prompt),
		TempFiles: []string{configPath, scratchDir},
	}, nil
}

func (a *OpencodeAdapter) ParseResult(_ context.Context, _ TurnContext, run CommandResult) (TurnResult, error) {
	stdout := run.ExactStdout()
	if message := opencodeFinalMessage(stdout); message != "" {
		return TurnResult{Result: message, Metadata: map[string]string{"adapter": RuntimeOpencode}}, nil
	}
	return TurnResult{Result: stdout, Metadata: map[string]string{"adapter": RuntimeOpencode}}, nil
}

func buildOpencodeEnv(
	turnEnv []string,
	home string,
	xdgConfigHome string,
	xdgDataHome string,
	xdgCacheHome string,
	xdgStateHome string,
	configPath string,
) ([]string, []string) {
	reserved := map[string]struct{}{}
	for _, entry := range append(os.Environ(), turnEnv...) {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(name, opencodeEnvPrefix) {
			continue
		}
		switch name {
		case opencodeConfigPathEnv, opencodeDisableProjectConfig, opencodeDisableAutoUpdate, opencodeDisableExternalSkills:
			continue
		default:
			reserved[name] = struct{}{}
		}
	}
	unsetEnv := make([]string, 0, len(reserved))
	for name := range reserved {
		unsetEnv = append(unsetEnv, name)
	}
	sort.Strings(unsetEnv)

	env := make([]string, 0, len(turnEnv)+6)
	for _, entry := range turnEnv {
		name, _, ok := strings.Cut(entry, "=")
		if ok && (strings.HasPrefix(name, opencodeEnvPrefix) || name == "ORKA_ARTIFACTS_DIR") {
			continue
		}
		env = append(env, entry)
	}
	env = setEnv(env, "HOME", home)
	env = setEnv(env, "XDG_CONFIG_HOME", xdgConfigHome)
	env = setEnv(env, "XDG_DATA_HOME", xdgDataHome)
	env = setEnv(env, "XDG_CACHE_HOME", xdgCacheHome)
	env = setEnv(env, "XDG_STATE_HOME", xdgStateHome)
	env = setEnv(env, opencodeConfigPathEnv, configPath)
	env = setEnv(env, opencodeDisableProjectConfig, opencodeEnvTrue)
	env = setEnv(env, opencodeDisableAutoUpdate, opencodeEnvTrue)
	env = setEnv(env, opencodeDisableExternalSkills, opencodeEnvTrue)
	return env, unsetEnv
}

func validateOpencodeConfigLiteral(name, value string) error {
	// OpenCode expands {env:...} and {file:...} anywhere in config text before parsing it.
	if containsOpencodeConfigSubstitution(value) {
		return fmt.Errorf("%s must not contain opencode config substitutions", name)
	}
	return nil
}

func containsOpencodeConfigSubstitution(value string) bool {
	for _, prefix := range []string{"{env:", "{file:"} {
		if strings.Contains(value, prefix) {
			return true
		}
	}
	return false
}

func writeOpencodeConfig(
	ctx context.Context,
	cfg *agentEnvConfig,
	baseURL, apiKey, workDir, rootDir, artifactDir string,
) (string, string, error) {
	scratchDir, err := os.MkdirTemp("", "orka-opencode-*")
	if err != nil {
		return "", "", fmt.Errorf("create opencode scratch directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(scratchDir) }

	configDir := filepath.Join(scratchDir, "config", "opencode")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		cleanup()
		return "", "", fmt.Errorf("create opencode config directory: %w", err)
	}
	for _, dir := range []string{"data", "cache", "state"} {
		if err := os.MkdirAll(filepath.Join(scratchDir, dir), 0o700); err != nil {
			cleanup()
			return "", "", fmt.Errorf("create opencode %s directory: %w", dir, err)
		}
	}

	models := map[string]opencodeModel{}
	if cfg != nil {
		if model := strings.TrimSpace(cfg.Model); model != "" {
			models[model] = opencodeModel{Limit: opencodeModelLimit{
				Context: opencodeModelContextLimit,
				Output:  opencodeModelOutputLimit,
			}}
		}
	}
	instructions, err := writeOpencodeInstructions(ctx, scratchDir, cfg, workDir, rootDir, artifactDir)
	if err != nil {
		cleanup()
		return "", "", err
	}
	maxTurns := 50
	if cfg != nil {
		if cfg.MaxTurns > 0 {
			maxTurns = cfg.MaxTurns
		}
	}
	permissions, err := opencodePermissions(cfg)
	if err != nil {
		cleanup()
		return "", "", err
	}
	contents, err := json.MarshalIndent(opencodeConfig{
		Schema: "https://opencode.ai/config.json",
		Provider: map[string]opencodeProvider{
			opencodeProviderName: {
				NPM:  "@ai-sdk/openai-compatible",
				Name: opencodeProviderName,
				Options: opencodeProviderOptions{
					BaseURL: baseURL,
					APIKey:  apiKey,
				},
				Models: models,
			},
		},
		Agent: map[string]opencodeAgent{
			opencodeDefaultAgent: {Steps: maxTurns},
		},
		Permission:   permissions,
		Instructions: instructions,
		Share:        "disabled",
		AutoUpdate:   false,
	}, "", "  ")
	if err != nil {
		cleanup()
		return "", "", fmt.Errorf("marshal opencode config: %w", err)
	}

	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, append(contents, '\n'), 0o600); err != nil {
		cleanup()
		return "", "", fmt.Errorf("write opencode config: %w", err)
	}
	if err := chownTreeForChild(scratchDir); err != nil {
		cleanup()
		return "", "", fmt.Errorf("chown opencode scratch directory: %w", err)
	}
	return configPath, scratchDir, nil
}

func writeOpencodeInstructions(
	ctx context.Context,
	scratchDir string,
	cfg *agentEnvConfig,
	workDir, rootDir, artifactDir string,
) ([]string, error) {
	if err := validateOpencodeWorkspaceSymlinks(ctx, workDir, rootDir, artifactDir); err != nil {
		return nil, err
	}
	projectFiles, err := opencodeProjectInstructions(workDir, rootDir)
	if err != nil {
		return nil, err
	}
	instructions := make([]string, 0, len(projectFiles)+1)
	remaining := opencodeStartupInstructionsLimit
	for i, source := range projectFiles {
		header := []byte("Instructions from: " + source + "\n")
		if len(header) > remaining {
			return nil, fmt.Errorf("opencode repository instructions exceed %d bytes", opencodeStartupInstructionsLimit)
		}
		contents, err := readOpencodeRepositoryInstructions(rootDir, source, remaining-len(header))
		if err != nil {
			return nil, fmt.Errorf("read opencode repository instructions: %w", err)
		}
		// Keep repository-controlled paths and contents out of config text while preserving native source provenance.
		contents = append(header, contents...)
		remaining -= len(contents)
		copyPath := filepath.Join(scratchDir, fmt.Sprintf("repository-instructions-%d.md", i))
		if err := os.WriteFile(copyPath, contents, 0o600); err != nil {
			return nil, fmt.Errorf("write opencode repository instructions: %w", err)
		}
		instructions = append(instructions, copyPath)
	}
	if cfg != nil {
		if systemPrompt := strings.TrimSpace(cfg.SystemPrompt); systemPrompt != "" {
			instructionsPath := filepath.Join(scratchDir, "agent-instructions.md")
			if err := os.WriteFile(instructionsPath, []byte(systemPrompt+"\n"), 0o600); err != nil {
				return nil, fmt.Errorf("write opencode agent instructions: %w", err)
			}
			instructions = append(instructions, instructionsPath)
		}
	}
	return instructions, nil
}

func opencodeProjectInstructions(workDir, rootDir string) ([]string, error) {
	workDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode workspace directory: %w", err)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode workspace directory: %w", err)
	}
	rootDir, err = filepath.EvalSymlinks(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode workspace root: %w", err)
	}
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode workspace root: %w", err)
	}
	if !opencodePathWithin(rootDir, workDir) {
		return nil, fmt.Errorf("opencode workspace directory %q is outside root %q", workDir, rootDir)
	}
	instructionRoot, err := opencodeInstructionRoot(workDir, rootDir)
	if err != nil {
		return nil, err
	}
	for _, name := range opencodeProjectInstructionFiles {
		var matches []string
		for current := workDir; ; current = filepath.Dir(current) {
			candidate := filepath.Join(current, name)
			resolvedCandidate, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil && !os.IsNotExist(resolveErr) {
				return nil, fmt.Errorf("resolve opencode repository instructions %q: %w", candidate, resolveErr)
			}
			if resolveErr == nil {
				resolvedCandidate, resolveErr = filepath.Abs(resolvedCandidate)
				if resolveErr != nil {
					return nil, fmt.Errorf("resolve opencode repository instructions %q: %w", candidate, resolveErr)
				}
				if !opencodePathWithin(rootDir, resolvedCandidate) {
					return nil, fmt.Errorf("opencode repository instructions %q escape workspace root", candidate)
				}
				if err := validateOpencodeInstructionTarget(candidate, resolvedCandidate); err != nil {
					return nil, err
				}
				info, statErr := os.Stat(resolvedCandidate)
				switch {
				case statErr == nil && info.Mode().IsRegular():
					matches = append(matches, candidate)
				case statErr != nil && !os.IsNotExist(statErr):
					return nil, fmt.Errorf("stat opencode repository instructions: %w", statErr)
				}
			}
			if current == instructionRoot {
				break
			}
			parent := filepath.Dir(current)
			if parent == current {
				return nil, fmt.Errorf("opencode workspace root %q is not an ancestor of %q", rootDir, workDir)
			}
		}
		if len(matches) > 0 {
			return matches, nil
		}
	}
	return nil, nil
}

func opencodeInstructionRoot(workDir, rootDir string) (string, error) {
	for current := workDir; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect opencode git boundary %q: %w", current, err)
		}
		if current == rootDir {
			return rootDir, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("opencode workspace root %q is not an ancestor of %q", rootDir, workDir)
		}
	}
}

func readOpencodeRepositoryInstructions(rootDir, source string, maxBytes int) ([]byte, error) {
	rootDir, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode workspace root: %w", err)
	}
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode workspace root: %w", err)
	}
	rootHandle, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("open opencode workspace root: %w", err)
	}
	defer rootHandle.Close() //nolint:errcheck
	file, err := openOpencodeRepositoryInstructions(rootHandle, rootDir, source, maxBytes)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck
	contents, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > maxBytes {
		return nil, fmt.Errorf("opencode repository instructions exceed %d bytes", opencodeStartupInstructionsLimit)
	}
	return contents, nil
}

func openOpencodeRepositoryInstructions(
	rootHandle *os.Root,
	rootDir, source string,
	maxBytes int,
) (*os.File, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("invalid opencode repository instruction limit")
	}
	resolvedSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	resolvedSource, err = filepath.Abs(resolvedSource)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode repository instructions: %w", err)
	}
	if !opencodePathWithin(rootDir, resolvedSource) {
		return nil, fmt.Errorf("opencode repository instructions %q escape workspace root", source)
	}
	if err := validateOpencodeInstructionTarget(source, resolvedSource); err != nil {
		return nil, err
	}
	localPath, err := filepath.Rel(rootDir, resolvedSource)
	if err != nil || !filepath.IsLocal(localPath) {
		return nil, fmt.Errorf("opencode repository instructions %q escape workspace root", source)
	}
	info, err := rootHandle.Stat(localPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("opencode repository instructions %q are not a regular file", source)
	}
	file, err := rootHandle.Open(localPath)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("opencode repository instructions %q changed while being opened", source)
	}
	if openedInfo.Size() > int64(maxBytes) {
		_ = file.Close()
		return nil, fmt.Errorf("opencode repository instructions exceed %d bytes", opencodeStartupInstructionsLimit)
	}
	return file, nil
}

func validateOpencodeWorkspaceSymlinks(
	ctx context.Context,
	workDir, rootDir, artifactDir string,
) error {
	rootDir, err := resolveOpencodeDirectory(rootDir, "workspace root")
	if err != nil {
		return err
	}
	workDir, err = resolveOpencodeDirectory(workDir, "workspace directory")
	if err != nil {
		return err
	}
	if !opencodePathWithin(rootDir, workDir) {
		return fmt.Errorf("opencode workspace directory %q is outside root %q", workDir, rootDir)
	}
	artifactDir = strings.TrimSpace(artifactDir)
	if artifactDir != "" {
		artifactDir, err = resolveOpencodeDirectory(artifactDir, "artifact directory")
		if err != nil {
			return err
		}
		expectedArtifactDir := filepath.Join(workDir, opencodeWorkspaceArtifactsDir)
		if artifactDir != expectedArtifactDir || !opencodePathWithin(rootDir, artifactDir) {
			return fmt.Errorf(
				"opencode artifact directory must be %q inside workspace root %q",
				expectedArtifactDir,
				rootDir,
			)
		}
	}
	if err := validateOpencodeArtifactInstructions(ctx, artifactDir); err != nil {
		return err
	}

	symlinks := newOpencodeSymlinkCollector()
	indexed, err := gitOpencodeSymlinks(ctx, rootDir, rootDir, symlinks)
	if err != nil {
		return err
	}
	if !indexed {
		if err := walkOpencodeSymlinks(ctx, rootDir, symlinks); err != nil {
			return err
		}
	}
	symlinks.append(filepath.Join(rootDir, opencodeWorkspaceArtifactsDir))
	if workDir != rootDir {
		symlinks.append(filepath.Join(workDir, opencodeWorkspaceArtifactsDir))
	}
	for _, path := range symlinks.paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateOpencodeWorkspaceSymlink(path, rootDir); err != nil {
			return err
		}
	}
	return nil
}

func resolveOpencodeDirectory(path, label string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve opencode %s: %w", label, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve opencode %s: %w", label, err)
	}
	return resolved, nil
}

func validateOpencodeArtifactInstructions(ctx context.Context, artifactDir string) error {
	if artifactDir == "" {
		return nil
	}
	return filepath.WalkDir(artifactDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("inspect opencode artifact directory: %w", walkErr)
		}
		if entry.IsDir() {
			for _, name := range opencodeProjectInstructionFiles {
				candidate := filepath.Join(path, name)
				if _, err := os.Lstat(candidate); err == nil {
					return fmt.Errorf("opencode artifact directory must not contain %s", candidate)
				} else if !os.IsNotExist(err) {
					return fmt.Errorf("inspect opencode artifact directory: %w", err)
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		return fmt.Errorf("opencode artifact symlink %q is not allowed", path)
	})
}

func gitOpencodeSymlinks(
	ctx context.Context,
	rootDir string,
	workspaceRoot string,
	symlinks *opencodeSymlinkCollector,
) (bool, error) {
	repository, gitDir, err := validateOpencodeGitRepository(rootDir, workspaceRoot)
	if err != nil || !repository {
		return false, err
	}
	if gitDir != "" && symlinks.markGitMetadataScanned(gitDir) {
		if err := validateOpencodeGitMetadataSymlinks(ctx, gitDir, workspaceRoot); err != nil {
			return false, err
		}
	}
	var repositories []string
	seenRepositories := make(map[string]struct{})
	var repositoryCandidates []string
	seenRepositoryCandidates := make(map[string]struct{})
	checkedRepositoryAncestors := make(map[string]struct{})
	appendRepository := func(path string) {
		if _, seen := seenRepositories[path]; seen {
			return
		}
		seenRepositories[path] = struct{}{}
		repositories = append(repositories, path)
	}
	ok, err := scanGitOpencodeRecords(ctx, rootDir, func(record string) {
		if tab := strings.IndexByte(record, '\t'); tab >= 0 {
			fields := strings.Fields(record[:tab])
			if len(fields) == 0 {
				return
			}
			path := filepath.Join(rootDir, filepath.FromSlash(record[tab+1:]))
			mode, exists := symlinks.appendPath(rootDir, path)
			appendOpencodeRepositoryAncestors(
				path,
				rootDir,
				&repositoryCandidates,
				seenRepositoryCandidates,
				checkedRepositoryAncestors,
				symlinks,
			)
			if fields[0] == "160000" && exists && mode.IsDir() {
				appendRepository(path)
			}
		}
	}, "ls-files", "--stage", "-z")
	if err != nil || !ok {
		return ok, err
	}
	worktreeCommands := [][]string{
		{"ls-files", "--others", "--exclude-standard", "-z"},
		{"ls-files", "--others", "--ignored", "--exclude-standard", "-z"},
	}
	for _, args := range worktreeCommands {
		ok, err = scanGitOpencodeRecords(ctx, rootDir, func(record string) {
			path := filepath.Join(rootDir, filepath.FromSlash(record))
			if mode, exists := symlinks.appendPath(rootDir, path); exists && mode.IsDir() {
				appendOpencodeRepositoryCandidate(path, &repositoryCandidates, seenRepositoryCandidates)
			}
		}, args...)
		if err != nil || !ok {
			return ok, err
		}
	}
	if err := appendOpencodeEmbeddedGitRepositories(repositoryCandidates, symlinks, appendRepository); err != nil {
		return false, err
	}
	for _, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		info, err := os.Lstat(repository)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("inspect opencode git repository %q: %w", repository, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			symlinks.append(repository)
			continue
		}
		if !info.IsDir() {
			continue
		}
		indexed, err := gitOpencodeSymlinks(ctx, repository, workspaceRoot, symlinks)
		if err != nil {
			return false, err
		}
		if !indexed {
			if err := walkOpencodeSymlinks(ctx, repository, symlinks); err != nil {
				return false, err
			}
		}
	}
	return symlinks.indexedResult()
}

func appendOpencodeRepositoryCandidate(path string, candidates *[]string, seen map[string]struct{}) {
	if _, ok := seen[path]; ok {
		return
	}
	seen[path] = struct{}{}
	*candidates = append(*candidates, path)
}

func appendOpencodeRepositoryAncestors(
	path, rootDir string,
	candidates *[]string,
	seen map[string]struct{},
	checked map[string]struct{},
	symlinks *opencodeSymlinkCollector,
) {
	for current := filepath.Dir(path); current != rootDir; current = filepath.Dir(current) {
		if _, ok := checked[current]; ok {
			break
		}
		checked[current] = struct{}{}
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			appendOpencodeRepositoryCandidate(current, candidates, seen)
		} else if !os.IsNotExist(err) && symlinks.inspectErr == nil {
			symlinks.inspectErr = fmt.Errorf("inspect embedded opencode git repository %q: %w", current, err)
		}
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
}

func validateOpencodeGitRepository(repositoryDir, workspaceRoot string) (bool, string, error) {
	gitPath := filepath.Join(repositoryDir, ".git")
	info, err := os.Lstat(gitPath)
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("inspect opencode git metadata %q: %w", gitPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, "", fmt.Errorf("opencode git metadata %q must not be a symlink", gitPath)
	}
	gitDir := gitPath
	if info.Mode().IsRegular() {
		file, err := os.Open(gitPath)
		if err != nil {
			return false, "", fmt.Errorf("open opencode git metadata %q: %w", gitPath, err)
		}
		contents, readErr := io.ReadAll(io.LimitReader(file, 4097))
		closeErr := file.Close()
		if readErr != nil {
			return false, "", fmt.Errorf("read opencode git metadata %q: %w", gitPath, readErr)
		}
		if closeErr != nil {
			return false, "", fmt.Errorf("close opencode git metadata %q: %w", gitPath, closeErr)
		}
		if len(contents) > 4096 {
			return false, "", fmt.Errorf("opencode git metadata %q exceeds 4096 bytes", gitPath)
		}
		line := strings.TrimSpace(string(contents))
		if strings.ContainsAny(line, "\r\n") {
			return false, "", fmt.Errorf("opencode git metadata %q is malformed", gitPath)
		}
		target, ok := strings.CutPrefix(line, "gitdir:")
		if !ok || strings.TrimSpace(target) == "" {
			return false, "", fmt.Errorf("opencode git metadata %q is malformed", gitPath)
		}
		gitDir = strings.TrimSpace(target)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(repositoryDir, gitDir)
		}
	} else if !info.IsDir() {
		return false, "", fmt.Errorf("opencode git metadata %q is not a file or directory", gitPath)
	}
	gitDir, err = resolveOpencodeDirectory(gitDir, "git metadata directory")
	if err != nil {
		return false, "", err
	}
	if !opencodePathWithin(workspaceRoot, gitDir) {
		if filepath.Clean(repositoryDir) == filepath.Clean(workspaceRoot) {
			return true, "", nil
		}
		return false, "", fmt.Errorf("opencode git metadata directory %q is outside workspace root %q", gitDir, workspaceRoot)
	}
	return true, gitDir, nil
}

func validateOpencodeGitMetadataSymlinks(ctx context.Context, gitDir, workspaceRoot string) error {
	return filepath.WalkDir(gitDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("scan opencode git metadata symlinks: %w", walkErr)
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		return validateOpencodeWorkspaceSymlink(path, workspaceRoot)
	})
}

func appendOpencodeEmbeddedGitRepositories(
	candidates []string,
	symlinks *opencodeSymlinkCollector,
	appendRepository func(string),
) error {
	for _, candidate := range candidates {
		gitPath := filepath.Join(candidate, ".git")
		gitInfo, err := os.Lstat(gitPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect embedded opencode git repository %q: %w", candidate, err)
		}
		if gitInfo.Mode()&os.ModeSymlink != 0 {
			symlinks.append(gitPath)
		}
		if gitInfo.IsDir() || gitInfo.Mode().IsRegular() || gitInfo.Mode()&os.ModeSymlink != 0 {
			appendRepository(candidate)
		}
	}
	return nil
}

func scanGitOpencodeRecords(
	ctx context.Context,
	rootDir string,
	visit func(string),
	args ...string,
) (bool, error) {
	cmdArgs := append([]string{"-C", rootDir, "--work-tree=" + rootDir}, args...)
	cmd := workspaceGitCommand(ctx, cmdArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, nil
	}
	if err := cmd.Start(); err != nil {
		return false, nil
	}
	reader := bufio.NewReader(stdout)
	for {
		record, readErr := reader.ReadString(0)
		if record != "" {
			visit(strings.TrimSuffix(record, "\x00"))
		}
		if readErr != nil {
			if readErr != io.EOF {
				_ = cmd.Wait()
				return false, fmt.Errorf("read git workspace paths: %w", readErr)
			}
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	return true, nil
}

func walkOpencodeSymlinks(
	ctx context.Context,
	rootDir string,
	symlinks *opencodeSymlinkCollector,
) error {
	return filepath.WalkDir(rootDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return fmt.Errorf("scan opencode workspace symlinks: %w", walkErr)
		}
		if entry.IsDir() && path == filepath.Join(rootDir, ".git") {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			symlinks.append(path)
		}
		return nil
	})
}

type opencodeSymlinkCollector struct {
	paths              []string
	seen               map[string]struct{}
	checkedDirectories map[string]struct{}
	scannedGitMetadata map[string]struct{}
	inspectErr         error
}

func newOpencodeSymlinkCollector() *opencodeSymlinkCollector {
	return &opencodeSymlinkCollector{
		seen:               make(map[string]struct{}),
		checkedDirectories: make(map[string]struct{}),
		scannedGitMetadata: make(map[string]struct{}),
	}
}

func (c *opencodeSymlinkCollector) markGitMetadataScanned(path string) bool {
	if _, scanned := c.scannedGitMetadata[path]; scanned {
		return false
	}
	c.scannedGitMetadata[path] = struct{}{}
	return true
}

func (c *opencodeSymlinkCollector) indexedResult() (bool, error) {
	if c.inspectErr != nil {
		return false, c.inspectErr
	}
	return true, nil
}

func (c *opencodeSymlinkCollector) append(path string) (fs.FileMode, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) && c.inspectErr == nil {
			c.inspectErr = fmt.Errorf("inspect opencode workspace path %q: %w", path, err)
		}
		return 0, false
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return info.Mode(), true
	}
	if _, ok := c.seen[path]; ok {
		return info.Mode(), true
	}
	c.seen[path] = struct{}{}
	c.paths = append(c.paths, path)
	return info.Mode(), true
}

func (c *opencodeSymlinkCollector) appendPath(rootDir, path string) (fs.FileMode, bool) {
	rel, err := filepath.Rel(rootDir, path)
	if err != nil || !filepath.IsLocal(rel) {
		return 0, false
	}
	if rel == "." {
		return c.append(rootDir)
	}
	components := strings.Split(rel, string(filepath.Separator))
	current := rootDir
	for index, component := range components {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		last := index == len(components)-1
		if !last {
			if _, checked := c.checkedDirectories[current]; checked {
				continue
			}
		}
		mode, exists := c.append(current)
		if !exists || mode&os.ModeSymlink != 0 || last {
			return mode, exists
		}
		if !mode.IsDir() {
			return mode, true
		}
		c.checkedDirectories[current] = struct{}{}
	}
	return 0, false
}

func validateOpencodeWorkspaceSymlink(path, rootDir string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect opencode workspace symlink %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve opencode workspace symlink %q: %w", path, err)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve opencode workspace symlink %q: %w", path, err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat opencode workspace symlink target %q: %w", path, err)
	}
	if targetInfo.IsDir() {
		// OpenCode checks external-directory permission against the lexical request path.
		// Directory symlinks can reinterpret later ".." components after that check.
		return fmt.Errorf("opencode workspace directory symlink %q is not allowed", path)
	}
	if !opencodePathWithin(rootDir, target) {
		return fmt.Errorf("opencode workspace file symlink %q escapes root %q", path, rootDir)
	}
	selected, err := isSelectedOpencodeInstructionSymlink(path)
	if err != nil {
		return err
	}
	if !selected {
		return nil
	}
	if err := validateOpencodeInstructionTarget(path, target); err != nil {
		return err
	}
	if !opencodePathWithin(rootDir, target) {
		return fmt.Errorf("opencode repository instructions %q escape workspace root", path)
	}
	if !targetInfo.Mode().IsRegular() {
		return fmt.Errorf("opencode repository instructions %q are not a regular file", path)
	}
	return nil
}

func validateOpencodeInstructionTarget(source, target string) error {
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect opencode repository instructions %q: %w", source, err)
	}
	if sourceInfo.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat opencode repository instruction target %q: %w", target, err)
	}
	for _, name := range opencodeProjectInstructionFiles {
		candidate := filepath.Join(filepath.Dir(target), name)
		if filepath.Clean(candidate) == filepath.Clean(source) {
			continue
		}
		candidateInfo, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat opencode repository instruction target %q: %w", candidate, err)
		}
		if candidateInfo.Mode()&os.ModeSymlink != 0 || !candidateInfo.Mode().IsRegular() {
			continue
		}
		if os.SameFile(targetInfo, candidateInfo) {
			return nil
		}
	}
	return fmt.Errorf(
		"opencode repository instructions %q must target %s, %s, or %s, not %q",
		source,
		opencodeProjectInstructionFiles[0],
		opencodeProjectInstructionFiles[1],
		opencodeProjectInstructionFiles[2],
		target,
	)
}

func isSelectedOpencodeInstructionSymlink(path string) (bool, error) {
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(path)
	for _, name := range opencodeProjectInstructionFiles {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false, fmt.Errorf("inspect opencode repository instructions %q: %w", candidate, err)
		}
		candidateInfo, err := os.Lstat(candidate)
		if err != nil {
			return false, fmt.Errorf("inspect opencode repository instructions %q: %w", candidate, err)
		}
		return os.SameFile(linkInfo, candidateInfo), nil
	}
	return false, nil
}

func opencodePathWithin(rootDir, path string) bool {
	rel, err := filepath.Rel(rootDir, path)
	return err == nil && filepath.IsLocal(rel)
}

func opencodePermissions(cfg *agentEnvConfig) (map[string]string, error) {
	permissions := map[string]string{
		"edit":  opencodePermissionAllow,
		"bash":  opencodePermissionDeny,
		"skill": opencodePermissionDeny,
	}
	if cfg == nil {
		return permissions, nil
	}
	for _, tools := range [][]string{cfg.AllowedTools, cfg.DisallowedTools} {
		for _, tool := range tools {
			if _, _, scoped := strings.Cut(strings.TrimSpace(tool), "("); scoped {
				return nil, fmt.Errorf("opencode runtime does not support scoped tool policy entry %q", tool)
			}
		}
	}

	if cfg.AllowedToolsSet {
		for _, permission := range opencodeManagedPermissions() {
			permissions[permission] = opencodePermissionDeny
		}
		for _, tool := range cfg.AllowedTools {
			if permission := opencodePermissionForTool(tool); permission != "" {
				permissions[permission] = opencodePermissionAllow
			}
		}
	} else if cfg.AllowBash {
		permissions["bash"] = opencodePermissionAllow
	}
	for _, tool := range cfg.DisallowedTools {
		if permission := opencodePermissionForTool(tool); permission != "" {
			permissions[permission] = opencodePermissionDeny
		}
	}
	if !cfg.AllowBash {
		permissions["bash"] = opencodePermissionDeny
	}
	permissions["skill"] = opencodePermissionDeny
	return permissions, nil
}

func opencodeManagedPermissions() []string {
	return []string{
		"read",
		"glob",
		"grep",
		"edit",
		"bash",
		"webfetch",
		opencodePermissionWebSearch,
		"task",
		"todowrite",
		"question",
		"lsp",
	}
}

func opencodePermissionForTool(tool string) string {
	name := normalizeToolName(tool)
	switch name {
	case "read", "fileread", "ls", "list":
		return "read"
	case "glob":
		return "glob"
	case "grep":
		return "grep"
	case "write", "edit", "multiedit", "notebookedit", "patch", "filewrite":
		return "edit"
	case "bash", "shell", "codeexec":
		return "bash"
	case "webfetch":
		return "webfetch"
	case "websearch":
		return opencodePermissionWebSearch
	case "task":
		return "task"
	case "todoread", "todowrite":
		return "todowrite"
	case "question":
		return "question"
	case "lsp":
		return "lsp"
	default:
		return ""
	}
}

func opencodeBaseURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	return strings.TrimSuffix(value, "/chat/completions")
}

func opencodeFinalMessage(stdout string) string {
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), maxStoredResultBytes)
	last := ""
	for scanner.Scan() {
		var event opencodeOutputEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type != "text" || event.Part.Type != "text" || strings.TrimSpace(event.Part.Text) == "" {
			continue
		}
		last = event.Part.Text
	}
	return last
}
