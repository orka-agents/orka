package workspacedelta

import (
	"bytes"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// Repository ignore rules let the post-prompt capture skip build output such
// as node_modules/ or target/ that the repository itself ignores. The rules
// are read only from the trusted baseline, so an agent's edit to .gitignore
// cannot widen what is skipped, and they only ever apply to paths absent from
// the baseline: like git, ignore rules never hide a tracked path.

const (
	gitignoreName   = ".gitignore"
	gitInfoExclude  = ".git/info/exclude"
	utf8ByteOrderMk = "\xef\xbb\xbf"
)

// ignorePattern is one parsed gitignore(5) line. segments are path.Match
// patterns, with "**" standing for any number of whole path components.
type ignorePattern struct {
	segments []string
	negate   bool
	dirOnly  bool
}

// ignoreRules holds the baseline's patterns: .git/info/exclude first, then
// each .gitignore keyed by the directory that contains it ("" is the root).
type ignoreRules struct {
	exclude []ignorePattern
	byDir   map[string][]ignorePattern
}

// retainForCapture keeps the content of a baseline's ignore sources so their
// rules are parsed from the exact bytes the walk hashed.
func retainForCapture(retainContent bool, baseline *Snapshot, relative string, protected bool) bool {
	return retainContent || (baseline == nil && isIgnoreSource(relative, protected))
}

func isIgnoreSource(relative string, protected bool) bool {
	if relative == gitInfoExclude {
		return true
	}
	return !protected && path.Base(relative) == gitignoreName
}

// collectIgnoreRules parses the ignore sources a baseline walk (one without a
// baseline of its own) retained and drops their content from the snapshot
// entries. It returns nil for a post-prompt walk or when the baseline has no
// usable pattern.
func collectIgnoreRules(baseline *Snapshot, entries map[string]entry, retained bool) *ignoreRules {
	if baseline != nil {
		return nil
	}
	rules := &ignoreRules{byDir: make(map[string][]ignorePattern)}
	found := false
	for relative, current := range entries {
		if current.kind != EntryFile || !isIgnoreSource(relative, current.protected) {
			continue
		}
		patterns := parseIgnorePatterns(current.content)
		if !retained {
			current.content = nil
			entries[relative] = current
		}
		if len(patterns) == 0 {
			continue
		}
		found = true
		if relative == gitInfoExclude {
			rules.exclude = patterns
			continue
		}
		dir := path.Dir(relative)
		if dir == "." {
			dir = ""
		}
		rules.byDir[dir] = patterns
	}
	if !found {
		return nil
	}
	return rules
}

func parseIgnorePatterns(content []byte) []ignorePattern {
	content = bytes.TrimPrefix(content, []byte(utf8ByteOrderMk))
	var patterns []ignorePattern
	for line := range strings.SplitSeq(string(content), "\n") {
		if pattern, ok := parseIgnoreLine(line); ok {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

func parseIgnoreLine(line string) (ignorePattern, bool) {
	line = strings.TrimSuffix(line, "\r")
	if line == "" || line[0] == '#' {
		return ignorePattern{}, false
	}
	var pattern ignorePattern
	switch {
	case line[0] == '!':
		pattern.negate = true
		line = line[1:]
	case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
		line = line[1:]
	}
	line = trimUnescapedTrailingSpaces(line)
	if strings.HasSuffix(line, "/") {
		pattern.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	// A pattern without a leading or middle slash matches a name at any depth,
	// which "**/" expresses for the segment matcher.
	if !strings.Contains(line, "/") {
		line = "**/" + line
	}
	line = strings.TrimPrefix(line, "/")
	if line == "" || line == "**/" || strings.Contains(line, "[:") {
		return ignorePattern{}, false
	}
	for segment := range strings.SplitSeq(line, "/") {
		if segment == "**" {
			if n := len(pattern.segments); n > 0 && pattern.segments[n-1] == "**" {
				continue
			}
			pattern.segments = append(pattern.segments, segment)
			continue
		}
		segment = strings.ReplaceAll(segment, "[!", "[^")
		if segment == "" {
			return ignorePattern{}, false
		}
		if _, err := path.Match(segment, ""); err != nil {
			return ignorePattern{}, false
		}
		pattern.segments = append(pattern.segments, segment)
	}
	return pattern, true
}

func trimUnescapedTrailingSpaces(line string) string {
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}
	return line
}

// matches reports whether relative, a path inside the workspace, is ignored by
// the rules for itself. The last matching pattern wins, read in increasing
// precedence: .git/info/exclude, then each .gitignore from the root down to
// the path's parent directory.
func (r *ignoreRules) matches(relative string, isDir bool) bool {
	parts := strings.Split(relative, "/")
	ignored := false
	apply := func(patterns []ignorePattern, parts []string) {
		for _, pattern := range patterns {
			if (!pattern.dirOnly || isDir) && matchIgnoreSegments(pattern.segments, parts) {
				ignored = !pattern.negate
			}
		}
	}
	apply(r.exclude, parts)
	for depth := range parts {
		dir := strings.Join(parts[:depth], "/")
		if patterns, ok := r.byDir[dir]; ok {
			apply(patterns, parts[depth:])
		}
	}
	return ignored
}

// matchIgnoreSegments matches path components against pattern segments, where
// "**" matches zero or more components, except at the end, where it matches
// at least one ("dir/**" matches what is inside dir, not dir itself). It runs
// in O(len(pattern) * len(parts)).
func matchIgnoreSegments(pattern, parts []string) bool {
	// reach[j] reports whether the pattern prefix consumed so far can end
	// right before parts[j].
	reach := make([]bool, len(parts)+1)
	reach[0] = true
	for i, segment := range pattern {
		next := make([]bool, len(parts)+1)
		if segment == "**" {
			trailing := i == len(pattern)-1
			seen := false
			for j := range reach {
				if trailing {
					next[j] = seen
				} else {
					next[j] = seen || reach[j]
				}
				seen = seen || reach[j]
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				if reach[j-1] {
					ok, err := path.Match(segment, parts[j-1])
					next[j] = ok && err == nil
				}
			}
		}
		reach = next
	}
	return reach[len(parts)]
}

// ignoreFilter decides, during the post-prompt walk, which paths to skip.
// WalkDir visits a directory before its contents, so ignoredDirs is filled
// before any child asks for its parent.
type ignoreFilter struct {
	rules       *ignoreRules
	baseline    map[string]entry
	ignoredDirs map[string]bool
}

func newIgnoreFilter(baseline *Snapshot) *ignoreFilter {
	if baseline == nil || baseline.ignore == nil {
		return nil
	}
	return &ignoreFilter{rules: baseline.ignore, baseline: baseline.entries, ignoredDirs: make(map[string]bool)}
}

// skipEntry reports whether the walk leaves dirEntry out, and the value the
// WalkDir callback returns for it. Protected paths are never skipped.
func (f *ignoreFilter) skipEntry(relative string, protected bool, dirEntry fs.DirEntry) (bool, error) {
	if f == nil || protected || !f.skip(relative, dirEntry.IsDir()) {
		return false, nil
	}
	if dirEntry.IsDir() {
		return true, filepath.SkipDir
	}
	return true, nil
}

// skip reports whether relative is new since the baseline and ignored, either
// itself or through an ignored ancestor directory. Tracked paths are never
// skipped, but a tracked ignored directory still marks its new contents as
// ignored, as in git.
func (f *ignoreFilter) skip(relative string, isDir bool) bool {
	parent := path.Dir(relative)
	if parent == "." {
		parent = ""
	}
	ignored := f.ignoredDirs[parent] || f.rules.matches(relative, isDir)
	if _, tracked := f.baseline[relative]; tracked {
		if isDir && ignored {
			f.ignoredDirs[relative] = true
		}
		return false
	}
	return ignored
}
