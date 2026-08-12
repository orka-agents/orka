// Package safesymlink validates repository-relative symbolic-link graphs
// without following links on the host filesystem.
package safesymlink

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Resolve validates one link and returns its canonical repository-relative
// lexical target. The target may be dangling, but must not escape the root,
// point to itself or an ancestor, or contain non-canonical bytes.
func Resolve(linkPath, target string, maxPathBytes, maxTargetBytes int) (string, error) {
	if maxPathBytes < 1 || maxTargetBytes < 1 {
		return "", fmt.Errorf("safe symlink limits must be positive")
	}
	if err := validatePath(linkPath, maxPathBytes); err != nil {
		return "", err
	}
	if target == "" || len(target) > maxTargetBytes || !utf8.ValidString(target) ||
		strings.IndexByte(target, 0) >= 0 || strings.Contains(target, "\\") || hasControl(target) ||
		path.IsAbs(target) || filepath.IsAbs(target) {
		return "", fmt.Errorf("symlink %q has an unsafe target", linkPath)
	}
	resolved := path.Clean(path.Join(path.Dir(linkPath), target))
	if resolved == "." || resolved == linkPath || strings.HasPrefix(linkPath, resolved+"/") ||
		resolved == ".." || strings.HasPrefix(resolved, "../") || strings.HasPrefix(resolved, "/") {
		return "", fmt.Errorf("symlink %q escapes or aliases an unsafe path", linkPath)
	}
	if err := validatePath(resolved, maxPathBytes); err != nil {
		return "", fmt.Errorf("symlink %q target is unsafe: %w", linkPath, err)
	}
	return resolved, nil
}

// ValidateGraph validates all links against the complete archive/repository
// path set. It rejects cycles and any non-directory archive entry nested below
// a symlink path, preventing extraction through a link.
func ValidateGraph(paths map[string]struct{}, links map[string]string, maxPathBytes, maxTargetBytes int) error {
	resolutions := make(map[string]string, len(links))
	for linkPath, target := range links {
		if _, ok := paths[linkPath]; !ok {
			return fmt.Errorf("symlink %q is absent from the path inventory", linkPath)
		}
		resolved, err := Resolve(linkPath, target, maxPathBytes, maxTargetBytes)
		if err != nil {
			return err
		}
		resolutions[linkPath] = resolved
	}
	if len(links) > 0 {
		// An entry is nested below a link exactly when the bytes before one
		// of its "/" separators equal a link path, so walking each entry's
		// ancestors costs O(paths × depth) instead of O(links × paths).
		for entryPath := range paths {
			for index := range len(entryPath) {
				if entryPath[index] != '/' {
					continue
				}
				if _, ok := links[entryPath[:index]]; ok {
					return fmt.Errorf("archive entry %q is nested below symlink %q", entryPath, entryPath[:index])
				}
			}
		}
	}
	for linkPath, resolved := range resolutions {
		if err := resolveGraph(linkPath, resolved, resolutions, maxPathBytes, map[string]struct{}{linkPath: {}}); err != nil {
			return err
		}
	}
	return nil
}

func resolveGraph(start, value string, resolutions map[string]string, maxPathBytes int, visiting map[string]struct{}) error {
	prefix, remainder, found := firstLinkPrefix(value, resolutions)
	if !found {
		return nil
	}
	if _, cycle := visiting[prefix]; cycle {
		return fmt.Errorf("symlink %q participates in a cycle", start)
	}
	visiting[prefix] = struct{}{}
	defer delete(visiting, prefix)

	resolved := resolutions[prefix]
	if remainder != "" {
		resolved = path.Clean(path.Join(resolved, remainder))
		if err := validatePath(resolved, maxPathBytes); err != nil {
			return fmt.Errorf("symlink %q chain is unsafe: %w", start, err)
		}
	}
	return resolveGraph(start, resolved, resolutions, maxPathBytes, visiting)
}

func firstLinkPrefix(value string, links map[string]string) (string, string, bool) {
	components := strings.Split(value, "/")
	for index := range components {
		prefix := strings.Join(components[:index+1], "/")
		if _, ok := links[prefix]; ok {
			return prefix, strings.Join(components[index+1:], "/"), true
		}
	}
	return "", "", false
}

func validatePath(value string, maxBytes int) error {
	if value == "" || value == "." || len(value) > maxBytes || !utf8.ValidString(value) ||
		strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.IndexByte(value, 0) >= 0 || hasControl(value) {
		return fmt.Errorf("path %q is not canonical and repository-relative", value)
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("path %q escapes the repository", value)
	}
	for component := range strings.SplitSeq(cleaned, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("path %q contains an unsafe component", value)
		}
	}
	return nil
}

func hasControl(value string) bool {
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return true
		}
	}
	return false
}
