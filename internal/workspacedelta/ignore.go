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

	// A baseline whose ignore files exceed these bounds gets no ignore rules,
	// so the capture keeps today's full walk instead of spending unbounded
	// time matching every path against them.
	maxIgnoreSourceBytes = 1 << 20
	maxIgnorePatterns    = 10_000
)

// ignorePattern is one parsed gitignore(5) line. segments are git wildmatch
// patterns for single path components (see globMatch), with "**" standing
// for any number of whole components.
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
	var sourceBytes, patternCount int
	for relative, current := range entries {
		if current.kind != EntryFile || !isIgnoreSource(relative, current.protected) {
			continue
		}
		content := current.content
		if !retained {
			current.content = nil
			entries[relative] = current
		}
		// Past either bound, keep looping only to release retained content.
		sourceBytes += len(content)
		if sourceBytes > maxIgnoreSourceBytes || patternCount > maxIgnorePatterns {
			continue
		}
		patterns := parseIgnorePatterns(content)
		patternCount += len(patterns)
		if len(patterns) == 0 {
			continue
		}
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
	if patternCount == 0 || sourceBytes > maxIgnoreSourceBytes || patternCount > maxIgnorePatterns {
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
	if line == "" || line == "**/" {
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
		if segment == "" || !validGlob(segment) {
			return ignorePattern{}, false
		}
		pattern.segments = append(pattern.segments, segment)
	}
	return pattern, true
}

// trimUnescapedTrailingSpaces drops trailing spaces unless a backslash quotes
// them, reading left to right as git does: `foo\\ ` (an escaped backslash
// and a space) still loses its space.
func trimUnescapedTrailingSpaces(line string) string {
	keep := 0
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' {
			continue
		}
		if line[i] == '\\' {
			i++
		}
		keep = min(i+1, len(line))
	}
	return line[:keep]
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
	next := make([]bool, len(parts)+1)
	reach[0] = true
	for i, segment := range pattern {
		clear(next)
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
					next[j] = globMatch(segment, parts[j-1])
				}
			}
		}
		reach, next = next, reach
	}
	return reach[len(parts)]
}

// globMatch matches one path component against one pattern component byte by
// byte, as git's wildmatch does: '*' matches any run of bytes, '?' one byte,
// '[...]' one byte from a set (ranges, '!' or '^' negation, POSIX classes),
// and '\' makes the next byte literal. Patterns are checked by validGlob when
// parsed.
func globMatch(pattern, name string) bool {
	p, n := 0, 0
	starP, starN := -1, 0
	for n < len(name) {
		if p < len(pattern) {
			switch c := pattern[p]; c {
			case '*':
				for p < len(pattern) && pattern[p] == '*' {
					p++
				}
				starP, starN = p, n
				continue
			case '?':
				p++
				n++
				continue
			case '[':
				if matched, width, ok := matchBracket(pattern[p:], name[n]); ok && matched {
					p += width
					n++
					continue
				}
			case '\\':
				if p+1 < len(pattern) && pattern[p+1] == name[n] {
					p += 2
					n++
					continue
				}
			default:
				if c == name[n] {
					p++
					n++
					continue
				}
			}
		}
		if starP < 0 {
			return false
		}
		starN++
		p, n = starP, starN
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// validGlob reports whether every bracket expression in pattern is closed and
// uses known classes, and no backslash is left dangling. Git matches nothing
// for such patterns, so they are dropped.
func validGlob(pattern string) bool {
	for p := 0; p < len(pattern); p++ {
		switch pattern[p] {
		case '\\':
			if p+1 == len(pattern) {
				return false
			}
			p++
		case '[':
			_, width, ok := matchBracket(pattern[p:], 0)
			if !ok {
				return false
			}
			p += width - 1
		}
	}
	return true
}

// matchBracket matches b against the bracket expression at the start of
// pattern and returns the expression's width; ok is false when it is malformed.
func matchBracket(pattern string, b byte) (matched bool, width int, ok bool) {
	i := 1
	negate := false
	if i < len(pattern) && (pattern[i] == '!' || pattern[i] == '^') {
		negate = true
		i++
	}
	for first := true; ; first = false {
		if i >= len(pattern) {
			return false, 0, false
		}
		c := pattern[i]
		if c == ']' && !first {
			return matched != negate, i + 1, true
		}
		if c == '[' && i+1 < len(pattern) && pattern[i+1] == ':' {
			end := strings.Index(pattern[i+2:], ":]")
			if end < 0 {
				return false, 0, false
			}
			in, known := posixClass(pattern[i+2:i+2+end], b)
			if !known {
				return false, 0, false
			}
			matched = matched || in
			i += end + 4
			continue
		}
		lo, next, ok := bracketByte(pattern, i)
		if !ok {
			return false, 0, false
		}
		hi := lo
		if next+1 < len(pattern) && pattern[next] == '-' && pattern[next+1] != ']' {
			if hi, next, ok = bracketByte(pattern, next+1); !ok {
				return false, 0, false
			}
		}
		matched = matched || (lo <= b && b <= hi)
		i = next
	}
}

func bracketByte(pattern string, i int) (value byte, next int, ok bool) {
	if pattern[i] != '\\' {
		return pattern[i], i + 1, true
	}
	if i+1 >= len(pattern) {
		return 0, 0, false
	}
	return pattern[i+1], i + 2, true
}

func isUpperByte(b byte) bool { return 'A' <= b && b <= 'Z' }
func isLowerByte(b byte) bool { return 'a' <= b && b <= 'z' }
func isDigitByte(b byte) bool { return '0' <= b && b <= '9' }
func isGraphByte(b byte) bool { return 0x21 <= b && b <= 0x7e }
func isAlphaByte(b byte) bool { return isUpperByte(b) || isLowerByte(b) }
func isAlnumByte(b byte) bool { return isAlphaByte(b) || isDigitByte(b) }

// posixClasses are the bracket classes git's wildmatch supports, over ASCII.
var posixClasses = map[string]func(byte) bool{
	"alnum":  isAlnumByte,
	"alpha":  isAlphaByte,
	"blank":  func(b byte) bool { return b == ' ' || b == '\t' },
	"cntrl":  func(b byte) bool { return b < 0x20 || b == 0x7f },
	"digit":  isDigitByte,
	"graph":  isGraphByte,
	"lower":  isLowerByte,
	"print":  func(b byte) bool { return isGraphByte(b) || b == ' ' },
	"punct":  func(b byte) bool { return isGraphByte(b) && !isAlnumByte(b) },
	"space":  func(b byte) bool { return b == ' ' || ('\t' <= b && b <= '\r') },
	"upper":  isUpperByte,
	"xdigit": func(b byte) bool { return isDigitByte(b) || ('a' <= b && b <= 'f') || ('A' <= b && b <= 'F') },
}

func posixClass(name string, b byte) (in, known bool) {
	class, known := posixClasses[name]
	if !known {
		return false, false
	}
	return class(b), true
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
	_, tracked := f.baseline[relative]
	if tracked && !isDir {
		return false
	}
	ignored := f.ignoredDirs[parent] || f.rules.matches(relative, isDir)
	if tracked {
		if isDir && ignored {
			f.ignoredDirs[relative] = true
		}
		return false
	}
	return ignored
}
