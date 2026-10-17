package acp

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstructionProjection is supervisor-authored input, never repository content.
// RelativePath is beneath the private session home, outside the task workspace.
type InstructionProjection struct {
	RelativePath string
	Content      string
}

// ProjectInstructions runs after child ownership finalization and before spawn.
func ProjectInstructions(paths SessionPaths, projection InstructionProjection, gid int) error {
	path, err := protectInstructionAncestors(paths, projection, gid)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("create instruction projection: %w", err)
	}
	defer file.Close() //nolint:errcheck // Sync reports write errors before provider startup.
	if err := file.Chown(0, gid); err != nil {
		return err
	}
	if err := file.Chmod(0o440); err != nil {
		return err
	}
	if _, err := io.WriteString(file, projection.Content); err != nil {
		return fmt.Errorf("write instruction projection: %w", err)
	}
	return file.Sync()
}

// RestoreInstructions reestablishes protected ownership after whole-tree
// validation. The caller must keep the child frozen until this succeeds. It
// verifies existing bytes rather than replacing a damaged or missing projection.
func RestoreInstructions(paths SessionPaths, projection InstructionProjection, gid int) error {
	path, err := protectInstructionAncestors(paths, projection, gid)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("instruction projection is missing or not a regular file")
	}
	// The supervisor deliberately lacks DAC_OVERRIDE/DAC_READ_SEARCH. Reclaim
	// the child-owned 0440 file with CHOWN before opening it; the child is frozen.
	if err := os.Lchown(path, 0, gid); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open instruction projection: %w", err)
	}
	defer file.Close() //nolint:errcheck // The file is read-only.
	content, err := io.ReadAll(io.LimitReader(file, int64(len(projection.Content))+1))
	if err != nil || string(content) != projection.Content {
		return fmt.Errorf("instruction projection no longer matches frozen configuration")
	}
	if err := file.Chown(0, gid); err != nil {
		return err
	}
	return file.Chmod(0o440)
}

func protectInstructionAncestors(paths SessionPaths, projection InstructionProjection, gid int) (string, error) {
	if projection.Content == "" {
		return "", fmt.Errorf("instruction projection must not be empty")
	}
	if os.Geteuid() != 0 || gid <= 0 {
		return "", fmt.Errorf("protected instructions require a root supervisor and a private child group")
	}
	root := filepath.Clean(paths.Root)
	if !filepath.IsAbs(root) || root == string(filepath.Separator) || paths.Home != filepath.Join(root, "home") {
		return "", fmt.Errorf("invalid private instruction root")
	}
	relative := filepath.Clean(projection.RelativePath)
	if filepath.IsAbs(relative) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("instruction projection must remain beneath the private home")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("instruction projection requires one private directory and a filename")
	}
	protect := func(path string, mode os.FileMode, create bool) error {
		if create {
			if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
				return err
			}
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("instruction ancestor is not a real directory")
		}
		if err := os.Lchown(path, 0, gid); err != nil {
			return err
		}
		return os.Chmod(path, mode)
	}
	if err := protect(root, 0o710, false); err != nil {
		return "", fmt.Errorf("protect session instruction root: %w", err)
	}
	if err := protect(paths.Home, 0o770|os.ModeSticky, false); err != nil {
		return "", fmt.Errorf("protect instruction home: %w", err)
	}
	dir := filepath.Join(paths.Home, parts[0])
	if err := protect(dir, 0o770|os.ModeSticky, true); err != nil {
		return "", fmt.Errorf("protect instruction directory: %w", err)
	}
	return filepath.Join(dir, parts[1]), nil
}

// ValidateCopilotInstructions conservatively rejects native file-import markers.
func ValidateCopilotInstructions(text string) error {
	if strings.ContainsRune(text, '@') {
		return fmt.Errorf("copilot Agent instructions must not contain @ references; inline the referenced text")
	}
	return nil
}
