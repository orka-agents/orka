package cliwrapper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const maxStoredResultBytes = 50 << 20

var unwrittenResultFileModTime = time.Unix(1, 0)

type GenericAdapter struct {
	config GenericAdapterConfig
}

func NewGenericAdapter(config GenericAdapterConfig) *GenericAdapter {
	if strings.TrimSpace(config.PromptMode) == "" {
		config.PromptMode = PromptModeStdin
	}
	if strings.TrimSpace(config.PromptEnv) == "" {
		config.PromptEnv = DefaultPromptEnv
	}
	if strings.TrimSpace(config.ResultMode) == "" {
		config.ResultMode = ResultModeStdout
	}
	return &GenericAdapter{config: config}
}

func (a *GenericAdapter) Name() string { return RuntimeGeneric }

func (a *GenericAdapter) Validate() error {
	if a == nil {
		return fmt.Errorf("generic adapter is required")
	}
	if strings.TrimSpace(a.config.Command) == "" {
		return fmt.Errorf("generic runtime requires %s or --command", EnvCommand)
	}
	switch strings.ToLower(strings.TrimSpace(a.config.PromptMode)) {
	case PromptModeStdin, PromptModeEnv, PromptModeFile:
	default:
		return fmt.Errorf("unsupported generic prompt mode %q", a.config.PromptMode)
	}
	switch strings.ToLower(strings.TrimSpace(a.config.ResultMode)) {
	case ResultModeStdout, ResultModeFile:
	default:
		return fmt.Errorf("unsupported generic result mode %q", a.config.ResultMode)
	}
	if strings.ToLower(strings.TrimSpace(a.config.ResultMode)) == ResultModeFile &&
		strings.TrimSpace(a.config.ResultFile) == "" {
		return fmt.Errorf("generic result mode file requires %s or --result-file", EnvResultFile)
	}
	return nil
}

func (a *GenericAdapter) BuildCommand(_ context.Context, turn TurnContext) (*CommandSpec, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	cfg := a.config
	dir := firstNonEmpty(turn.WorkDir, cfg.WorkDir)
	spec := &CommandSpec{
		Path: cfg.Command,
		Args: append([]string(nil), cfg.Args...),
		Env:  append(append([]string(nil), turn.Env...), cfg.Env...),
		Dir:  dir,
	}

	switch strings.ToLower(strings.TrimSpace(cfg.PromptMode)) {
	case PromptModeStdin:
		spec.Stdin = []byte(turn.Prompt)
	case PromptModeEnv:
		spec.Env = setEnv(spec.Env, firstNonEmpty(cfg.PromptEnv, DefaultPromptEnv), turn.Prompt)
	case PromptModeFile:
		path := strings.TrimSpace(cfg.PromptFile)
		generatedPromptFile := path == ""
		if generatedPromptFile {
			f, err := os.CreateTemp("", "orka-turn-prompt-*.txt")
			if err != nil {
				return nil, fmt.Errorf("create prompt temp file: %w", err)
			}
			path = f.Name()
			if err := f.Close(); err != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("close prompt temp file: %w", err)
			}
			spec.TempFiles = append(spec.TempFiles, path)
		} else if !filepath.IsAbs(path) && dir != "" {
			path = filepath.Join(dir, path)
		}
		if !generatedPromptFile {
			if err := validateControlFilePath(dir, path); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(path, []byte(turn.Prompt), 0o600); err != nil {
			return nil, fmt.Errorf("write prompt file: %w", err)
		}
		if err := prepareControlFileForChild(path, 0o640); err != nil {
			return nil, fmt.Errorf("chown prompt file: %w", err)
		}
		spec.TempFiles = appendUniqueString(spec.TempFiles, path)
		spec.Env = setEnv(spec.Env, firstNonEmpty(cfg.PromptEnv, DefaultPromptEnv), path)
	}

	if strings.ToLower(strings.TrimSpace(cfg.ResultMode)) == ResultModeFile {
		resultFile := cfg.ResultFile
		if !filepath.IsAbs(resultFile) && dir != "" {
			resultFile = filepath.Join(dir, resultFile)
		}
		if err := validateControlFilePathNoCreate(dir, resultFile); err != nil {
			return nil, err
		}
		f, err := createResultFileNoFollow(dir, resultFile)
		if err != nil {
			return nil, fmt.Errorf("create result file: %w", err)
		}
		if _, err := validateOpenResultFile(f, resultFile); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("validate result file: %w", err)
		}
		if err := markOpenResultFileUnwritten(f, resultFile); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("mark result file unwritten: %w", err)
		}
		if err := prepareOpenControlFileForChild(f, 0o660); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("chown result file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close result file: %w", err)
		}
		spec.ResultFile = resultFile
	}
	return spec, nil
}

func (a *GenericAdapter) ParseResult(_ context.Context, turn TurnContext, run CommandResult) (TurnResult, error) {
	result := run.ExactStdout()
	if strings.ToLower(strings.TrimSpace(a.config.ResultMode)) == ResultModeFile {
		path := strings.TrimSpace(run.ResultFile)
		if path == "" {
			path = strings.TrimSpace(a.config.ResultFile)
			if path != "" && !filepath.IsAbs(path) && strings.TrimSpace(a.config.WorkDir) != "" {
				path = filepath.Join(a.config.WorkDir, path)
			}
		}
		if path == "" {
			return TurnResult{}, fmt.Errorf("result file path is required")
		}
		data, err := readBoundedResultFile(path, firstNonEmpty(turn.WorkDir, a.config.WorkDir))
		if err != nil {
			return TurnResult{Result: result}, err
		}
		if resultFileUnwritten(data.info) {
			return TurnResult{Result: result}, fmt.Errorf("result file was not written")
		}
		result = data.contents
	}
	return TurnResult{Result: result}, nil
}

type boundedResultFile struct {
	contents string
	info     os.FileInfo
}

func resultFileUnwritten(info os.FileInfo) bool {
	return info.Size() == 0 && info.ModTime().Unix() == unwrittenResultFileModTime.Unix()
}

func readBoundedResultFile(path string, workDirs ...string) (boundedResultFile, error) {
	workDir := firstNonEmpty(workDirs...)
	if err := validateResultFileRegularBeforeOpen(path, workDir); err != nil {
		return boundedResultFile{}, err
	}
	var file *os.File
	var err error
	if workDir != "" {
		file, err = openResultFileInWorkspaceNoFollow(workDir, path)
	} else {
		file, err = openResultFileNoFollow(path)
	}
	if err != nil {
		return boundedResultFile{}, fmt.Errorf("read result file: %w", err)
	}
	defer file.Close() //nolint:errcheck
	info, err := validateOpenResultFile(file, path)
	if err != nil {
		return boundedResultFile{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxStoredResultBytes)+1))
	if err != nil {
		return boundedResultFile{}, fmt.Errorf("read result file: %w", err)
	}
	if len(data) > maxStoredResultBytes {
		return boundedResultFile{}, fmt.Errorf("result file exceeds harness storage limit")
	}
	return boundedResultFile{contents: string(data), info: info}, nil
}

func validateResultFileRegularBeforeOpen(path, workDir string) error {
	target := strings.TrimSpace(path)
	if strings.TrimSpace(workDir) != "" {
		root, rel, err := workspaceRelativePath(workDir, path, "result file")
		if err != nil {
			return err
		}
		target = filepath.Join(root, filepath.Clean(rel))
	}
	info, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("stat result file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("result file %q must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("result file %q must be a regular file", path)
	}
	return nil
}

func validateOpenResultFile(file *os.File, path string) (os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat result file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("result file %q must be a regular file", path)
	}
	if links, ok := resultFileLinkCount(info); ok && links != 1 {
		return nil, fmt.Errorf("result file %q must not have multiple hard links", path)
	}
	return info, nil
}

func validateControlFilePath(workDir, controlFile string) error {
	if err := validateControlFilePathNoCreate(workDir, controlFile); err != nil {
		return err
	}
	workDir = strings.TrimSpace(workDir)
	controlFile = strings.TrimSpace(controlFile)
	if workDir == "" || controlFile == "" {
		return nil
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("resolve control file workspace: %w", err)
	}
	absControlFile, err := filepath.Abs(controlFile)
	if err != nil {
		return fmt.Errorf("resolve control file path: %w", err)
	}
	parent := filepath.Dir(absControlFile)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create control file directory: %w", err)
	}
	if err := rejectExistingSymlinkComponents(absWorkDir, parent, "control file directory", controlFile); err != nil {
		return err
	}
	return nil
}

func validateControlFilePathNoCreate(workDir, controlFile string) error {
	workDir = strings.TrimSpace(workDir)
	controlFile = strings.TrimSpace(controlFile)
	if workDir == "" || controlFile == "" {
		return nil
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("resolve control file workspace: %w", err)
	}
	resolvedWorkDir, err := filepath.EvalSymlinks(absWorkDir)
	if err != nil {
		return fmt.Errorf("resolve control file workspace: %w", err)
	}
	absControlFile, err := filepath.Abs(controlFile)
	if err != nil {
		return fmt.Errorf("resolve control file path: %w", err)
	}
	parent := filepath.Dir(absControlFile)
	if err := requireContainedPath(absWorkDir, parent, "control file directory", controlFile); err != nil {
		return err
	}
	if err := rejectExistingSymlinkComponents(absWorkDir, parent, "control file directory", controlFile); err != nil {
		return err
	}
	existing := parent
	for {
		if info, statErr := os.Lstat(existing); statErr == nil {
			if !info.IsDir() {
				return fmt.Errorf("control file directory %q is not a directory", existing)
			}
			break
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect control file directory %q: %w", existing, statErr)
		}
		next := filepath.Dir(existing)
		if next == existing {
			return fmt.Errorf("control file %q has no existing parent", controlFile)
		}
		existing = next
	}
	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return fmt.Errorf("resolve control file directory: %w", err)
	}
	if err := requireContainedPath(resolvedWorkDir, resolvedExisting, "control file directory", controlFile); err != nil {
		return err
	}
	if info, err := os.Lstat(absControlFile); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("control file %q must not be a symlink", controlFile)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect control file %q: %w", controlFile, err)
	}
	return nil
}

func rejectExistingSymlinkComponents(root, candidate, label, controlFile string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%s for %q escapes workspace %q", label, controlFile, root)
	}
	if rel == "." {
		return nil
	}
	current := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%s for %q has invalid path component %q", label, controlFile, part)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect %s %q: %w", label, current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s for %q must not contain symlink %q", label, controlFile, current)
		}
	}
	return nil
}

func requireContainedPath(root, candidate, label, controlFile string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%s for %q escapes workspace %q", label, controlFile, root)
	}
	return nil
}

func workspaceRelativePath(workDir, target, label string) (string, string, error) {
	workDir = strings.TrimSpace(workDir)
	target = strings.TrimSpace(target)
	if workDir == "" || target == "" {
		return "", "", fmt.Errorf("%s path is required", label)
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s workspace: %w", label, err)
	}
	resolvedWorkDir, err := filepath.EvalSymlinks(absWorkDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s workspace: %w", label, err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s path: %w", label, err)
	}
	if err := requireContainedPath(absWorkDir, absTarget, label, target); err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(absWorkDir, absTarget)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s relative path: %w", label, err)
	}
	return resolvedWorkDir, rel, nil
}

func appendUniqueString(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func setEnv(env []string, key, value string) []string {
	key = strings.TrimSpace(key)
	if key == "" {
		return env
	}
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
