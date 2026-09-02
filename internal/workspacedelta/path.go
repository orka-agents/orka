package workspacedelta

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func canonicalRelativePath(root, filePath string, maxBytes int) (string, error) {
	rel, err := filepath.Rel(root, filePath)
	if err != nil {
		return "", fmt.Errorf("make workspace path relative: %w", err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", nil
	}
	if err := validateArchivePath(rel, maxBytes); err != nil {
		return "", err
	}
	return rel, nil
}

func validateArchivePath(value string, maxBytes int) error {
	if value == "" || value == "." || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") ||
		strings.IndexByte(value, 0) >= 0 || !utf8.ValidString(value) {
		return pathError("validate", value, ErrPathTraversal)
	}
	if len(value) > maxBytes {
		return pathError("validate", value, fmt.Errorf("%w: path exceeds %d bytes", ErrLimitExceeded, maxBytes))
	}
	if hasControl(value) {
		return pathError("validate", value, ErrPathTraversal)
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return pathError("validate", value, ErrPathTraversal)
	}
	for component := range strings.SplitSeq(value, "/") {
		if component == "" || component == "." || component == ".." {
			return pathError("validate", value, ErrPathTraversal)
		}
	}
	return nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateSymlinkTarget(linkPath, target string, options normalizedOptions) (string, error) {
	if target == "" || len(target) > options.limits.MaxSymlinkBytes || !utf8.ValidString(target) ||
		strings.IndexByte(target, 0) >= 0 || strings.Contains(target, "\\") || hasControl(target) {
		return "", pathError("validate symlink", linkPath, ErrUnsafeSymlink)
	}
	if path.IsAbs(target) || filepath.IsAbs(target) {
		return "", pathError("validate symlink", linkPath, ErrUnsafeSymlink)
	}
	resolved := path.Clean(path.Join(path.Dir(linkPath), target))
	if resolved == "." || strings.HasPrefix(linkPath, resolved+"/") {
		return "", pathError("validate symlink", linkPath, ErrUnsafeSymlink)
	}
	if resolved == ".." || strings.HasPrefix(resolved, "../") || strings.HasPrefix(resolved, "/") {
		return "", pathError("validate symlink", linkPath, ErrUnsafeSymlink)
	}
	if resolved != "." {
		if err := validateArchivePath(resolved, options.limits.MaxPathBytes); err != nil {
			return "", pathError("validate symlink", linkPath, fmt.Errorf("%w: %v", ErrUnsafeSymlink, err))
		}
		protected, err := options.classifyPath(resolved)
		if err != nil {
			return "", pathError("validate symlink", linkPath, fmt.Errorf("%w: %v", ErrUnsafeSymlink, err))
		}
		if protected && !isReadOnlySkillsAlias(linkPath, target, resolved) {
			return "", pathError("validate symlink", linkPath, ErrUnsafeSymlink)
		}
	}
	return resolved, nil
}

func validateSymlinkGraph(ctx context.Context, entries map[string]entry, options normalizedOptions) error {
	for start, current := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if current.kind != EntrySymlink {
			continue
		}
		resolved, err := validateSymlinkTarget(start, current.target, options)
		if err != nil {
			return err
		}
		if isReadOnlySkillsAlias(start, current.target, resolved) {
			target, found := entries[resolved]
			if !found || target.kind != EntryDirectory || !target.protected {
				return pathError("validate symlink", start, ErrUnsafeSymlink)
			}
		}
		if err := resolveSymlinkPath(ctx, start, resolved, entries, options, map[string]struct{}{start: {}}); err != nil {
			return err
		}
	}
	return nil
}

func resolveSymlinkPath(ctx context.Context, start, value string, entries map[string]entry, options normalizedOptions, visiting map[string]struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prefix, remainder, found := firstSymlinkPrefix(value, entries)
	if !found {
		return nil
	}
	if _, cycle := visiting[prefix]; cycle {
		return pathError("validate symlink", start, ErrUnsafeSymlink)
	}
	visiting[prefix] = struct{}{}
	defer delete(visiting, prefix)

	linked := entries[prefix]
	base, err := validateSymlinkTarget(prefix, linked.target, options)
	if err != nil {
		return err
	}
	resolved := base
	if remainder != "" {
		resolved = path.Clean(path.Join(base, remainder))
	}
	if err := validateResolvedSymlinkPath(start, resolved, options); err != nil {
		return err
	}
	return resolveSymlinkPath(ctx, start, resolved, entries, options, visiting)
}

func validateResolvedSymlinkPath(start, value string, options normalizedOptions) error {
	if value == "." {
		return nil
	}
	if value == ".." || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "/") {
		return pathError("validate symlink", start, ErrUnsafeSymlink)
	}
	if err := validateArchivePath(value, options.limits.MaxPathBytes); err != nil {
		return pathError("validate symlink", start, fmt.Errorf("%w: %v", ErrUnsafeSymlink, err))
	}
	protected, err := options.classifyPath(value)
	if err != nil || protected {
		return pathError("validate symlink", start, ErrUnsafeSymlink)
	}
	return nil
}

func firstSymlinkPrefix(value string, entries map[string]entry) (prefix, remainder string, found bool) {
	if value == "." || value == "" {
		return "", "", false
	}
	components := strings.Split(value, "/")
	for i := range components {
		candidate := strings.Join(components[:i+1], "/")
		if current, exists := entries[candidate]; exists && current.kind == EntrySymlink {
			return candidate, strings.Join(components[i+1:], "/"), true
		}
	}
	return "", "", false
}
