package main

import (
	"bytes"
	"errors"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var errRemediationPatch = errors.New("candidate response is not a valid bounded Git or unified patch")

// The worker's changed-line reader uses this unified-hunk grammar. Downloads
// additionally account for every body line: extracting a range is not enough to
// distinguish an actual patch from a partial diff or an error response.
var remediationHunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?:[ \t].*)?$`)
var remediationIndexHeader = regexp.MustCompile(`^([0-9a-f]{4,64})\.\.([0-9a-f]{4,64})(?: ([0-7]{6}))?$`)

const (
	remediationNullPath        = "/dev/null"
	remediationNoNewline       = `\ No newline at end of file`
	remediationBinaryBudget    = 2 << 20
	remediationMaxDiffPathSize = 4096
)

type remediationPatchParser struct {
	lines        []string
	position     int
	binaryBudget int
}

type remediationPatchMetadata struct {
	oldMode, newMode, addedMode, deletedMode string
	renameFrom, renameTo, copyFrom, copyTo   string
	index                                    []string
	similarity, dissimilarity                string
}

func validateRemediationPatch(content []byte) error {
	if len(content) == 0 || len(content) > remediationMaxPatchBytes || !utf8.Valid(content) ||
		bytes.IndexByte(content, 0) >= 0 || content[len(content)-1] != '\n' {
		return errRemediationPatch
	}
	parser := remediationPatchParser{
		lines: strings.Split(string(content), "\n"), binaryBudget: remediationBinaryBudget,
	}
	files := 0
	for parser.skipBlank(); parser.position < len(parser.lines); parser.skipBlank() {
		if !parser.file() {
			return errRemediationPatch
		}
		files++
	}
	if files == 0 {
		return errRemediationPatch
	}
	return nil
}

func (p *remediationPatchParser) line() string {
	if p.position >= len(p.lines) {
		return ""
	}
	return strings.TrimSuffix(p.lines[p.position], "\r")
}

func (p *remediationPatchParser) skipBlank() {
	for p.position < len(p.lines) && p.line() == "" {
		p.position++
	}
}

func (p *remediationPatchParser) file() bool {
	gitHeader, git := strings.CutPrefix(p.line(), "diff --git ")
	var metadata remediationPatchMetadata
	if git {
		p.position++
		for {
			handled, valid := metadata.read(p.line())
			if !valid {
				return false
			}
			if !handled {
				break
			}
			p.position++
		}
		if !metadata.complete() {
			return false
		}
		if p.line() == "GIT binary patch" {
			return metadata.matchesGitHeader(gitHeader) && len(metadata.index) != 0 &&
				(len(metadata.index[1]) == 40 || len(metadata.index[1]) == 64) &&
				len(metadata.index[1]) == len(metadata.index[2]) && p.binary()
		}
		if !strings.HasPrefix(p.line(), "--- ") {
			return metadata.meaningful() && metadata.matchesGitHeader(gitHeader)
		}
	}
	oldPath, ok := remediationFileHeader(p.line(), "--- ")
	if !ok {
		return false
	}
	p.position++
	newPath, ok := remediationFileHeader(p.line(), "+++ ")
	if !ok || (oldPath == remediationNullPath && newPath == remediationNullPath) ||
		!metadata.matchesTextPaths(oldPath, newPath) {
		return false
	}
	p.position++
	if git && !remediationTextGitHeader(gitHeader, oldPath, newPath) {
		return false
	}
	return p.hunks(oldPath == remediationNullPath, newPath == remediationNullPath)
}

func (m *remediationPatchMetadata) read(line string) (bool, bool) {
	for _, field := range []struct {
		prefix string
		value  *string
		mode   bool
	}{
		{"old mode ", &m.oldMode, true}, {"new mode ", &m.newMode, true},
		{"new file mode ", &m.addedMode, true}, {"deleted file mode ", &m.deletedMode, true},
		{"rename from ", &m.renameFrom, false}, {"rename to ", &m.renameTo, false},
		{"copy from ", &m.copyFrom, false}, {"copy to ", &m.copyTo, false},
	} {
		if value, found := strings.CutPrefix(line, field.prefix); found {
			if *field.value != "" {
				return true, false
			}
			if field.mode {
				if !remediationGitMode(value) {
					return true, false
				}
			} else {
				var ok bool
				value, ok = remediationWholeDiffPath(value)
				if !ok {
					return true, false
				}
			}
			*field.value = value
			return true, true
		}
	}
	if value, found := strings.CutPrefix(line, "index "); found {
		if len(m.index) != 0 {
			return true, false
		}
		m.index = remediationIndexHeader.FindStringSubmatch(value)
		return true, len(m.index) != 0 && (m.index[3] == "" || remediationGitMode(m.index[3]))
	}
	for _, field := range []struct {
		prefix string
		value  *string
	}{
		{"similarity index ", &m.similarity},
		{"dissimilarity index ", &m.dissimilarity},
	} {
		if value, found := strings.CutPrefix(line, field.prefix); found {
			raw, percent := strings.CutSuffix(value, "%")
			number, err := strconv.ParseUint(raw, 10, 8)
			if *field.value != "" || !percent || err != nil || number > 100 {
				return true, false
			}
			*field.value = value
			return true, true
		}
	}
	return false, true
}

func (m *remediationPatchMetadata) complete() bool {
	if (m.oldMode == "") != (m.newMode == "") ||
		(m.renameFrom == "") != (m.renameTo == "") || (m.copyFrom == "") != (m.copyTo == "") {
		return false
	}
	actions := 0
	for _, value := range []string{m.addedMode, m.deletedMode, m.renameFrom, m.copyFrom} {
		if value != "" {
			actions++
		}
	}
	return actions <= 1 && (m.oldMode == "" || (m.addedMode == "" && m.deletedMode == "")) &&
		(m.similarity == "" || m.dissimilarity == "") &&
		(m.similarity == "" || m.renameFrom != "" || m.copyFrom != "")
}

func (m *remediationPatchMetadata) meaningful() bool {
	if m.renameFrom != "" || m.copyFrom != "" {
		return m.similarity == "100%"
	}
	return m.addedMode != "" || m.deletedMode != "" || (m.oldMode != "" && m.oldMode != m.newMode)
}

func (m *remediationPatchMetadata) matchesGitHeader(header string) bool {
	from, to := m.renameFrom, m.renameTo
	if m.copyFrom != "" {
		from, to = m.copyFrom, m.copyTo
	}
	if from != "" {
		return from != to && (remediationGitHeaderMatches(header, "a/"+from, "b/"+to) ||
			remediationGitHeaderMatches(header, from, to))
	}
	var oldRaw, newRaw string
	if strings.HasPrefix(header, `"`) {
		oldPath, rest, ok := remediationQuotedDiffPath(header)
		if !ok || !strings.HasPrefix(rest, " ") {
			return false
		}
		newPath, ok := remediationWholeDiffPath(rest[1:])
		return ok && remediationSameGitPath(oldPath, newPath)
	}
	// For a non-renaming canonical Git header the two unquoted names differ
	// only in their equal-length a/ and b/ prefixes (or have no prefixes).
	middle := len(header) / 2
	if len(header)%2 != 1 || header[middle] != ' ' {
		return false
	}
	oldRaw, newRaw = header[:middle], header[middle+1:]
	return remediationDiffPath(oldRaw) && remediationDiffPath(newRaw) && remediationSameGitPath(oldRaw, newRaw)
}

func (m *remediationPatchMetadata) matchesTextPaths(oldPath, newPath string) bool {
	if (m.addedMode != "" && oldPath != remediationNullPath) ||
		(m.deletedMode != "" && newPath != remediationNullPath) {
		return false
	}
	if m.renameFrom != "" {
		return m.renameFrom != m.renameTo &&
			((oldPath == "a/"+m.renameFrom && newPath == "b/"+m.renameTo) ||
				(oldPath == m.renameFrom && newPath == m.renameTo))
	}
	if m.copyFrom != "" {
		return m.copyFrom != m.copyTo &&
			((oldPath == "a/"+m.copyFrom && newPath == "b/"+m.copyTo) ||
				(oldPath == m.copyFrom && newPath == m.copyTo))
	}
	return true
}

func remediationGitMode(mode string) bool {
	switch mode {
	case "100644", "100755", "120000", "160000":
		return true
	default:
		return false
	}
}

func remediationSameGitPath(oldPath, newPath string) bool {
	return oldPath == newPath ||
		(strings.HasPrefix(oldPath, "a/") && strings.HasPrefix(newPath, "b/") && oldPath[2:] == newPath[2:])
}

func remediationTextGitHeader(header, oldPath, newPath string) bool {
	if oldPath == remediationNullPath {
		return remediationGitHeaderMatches(header, newPath, newPath) ||
			(strings.HasPrefix(newPath, "b/") && remediationGitHeaderMatches(header, "a/"+newPath[2:], newPath))
	}
	if newPath == remediationNullPath {
		return remediationGitHeaderMatches(header, oldPath, oldPath) ||
			(strings.HasPrefix(oldPath, "a/") && remediationGitHeaderMatches(header, oldPath, "b/"+oldPath[2:]))
	}
	return remediationGitHeaderMatches(header, oldPath, newPath)
}

func remediationGitHeaderMatches(header, oldPath, newPath string) bool {
	var rest string
	if strings.HasPrefix(header, `"`) {
		first, remaining, ok := remediationQuotedDiffPath(header)
		if !ok || first != oldPath || !strings.HasPrefix(remaining, " ") {
			return false
		}
		rest = remaining[1:]
	} else {
		var found bool
		rest, found = strings.CutPrefix(header, oldPath+" ")
		if !found {
			return false
		}
	}
	last, ok := remediationWholeDiffPath(rest)
	return ok && last == newPath
}

func remediationFileHeader(line, prefix string) (string, bool) {
	value, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return "", false
	}
	var name string
	if strings.HasPrefix(value, `"`) {
		var rest string
		name, rest, ok = remediationQuotedDiffPath(value)
		if !ok || (rest != "" && !strings.HasPrefix(rest, "\t")) {
			return "", false
		}
	} else {
		name, _, _ = strings.Cut(value, "\t")
	}
	return name, name == remediationNullPath || remediationDiffPath(name)
}

func remediationWholeDiffPath(raw string) (string, bool) {
	if strings.HasPrefix(raw, `"`) {
		name, rest, ok := remediationQuotedDiffPath(raw)
		return name, ok && rest == "" && remediationDiffPath(name)
	}
	return raw, !strings.ContainsRune(raw, '"') && remediationDiffPath(raw)
}

func remediationQuotedDiffPath(raw string) (string, string, bool) {
	if !strings.HasPrefix(raw, `"`) {
		return "", "", false
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++
			if i >= len(raw) || !strings.ContainsRune(`\"abfnrtv01234567`, rune(raw[i])) {
				return "", "", false
			}
			continue
		}
		if raw[i] == '"' {
			name, err := strconv.Unquote(raw[:i+1])
			return name, raw[i+1:], err == nil && remediationDiffPath(name)
		}
	}
	return "", "", false
}

func remediationDiffPath(name string) bool {
	if name == "" || len(name) > remediationMaxDiffPathSize || !utf8.ValidString(name) ||
		path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\x00\r\n\t\\:") {
		return false
	}
	for component := range strings.SplitSeq(name, "/") {
		if component == "." || component == ".." || component == ".git" {
			return false
		}
	}
	return true
}

func (p *remediationPatchParser) hunks(added, deleted bool) bool {
	hunks := 0
	ended := false
	var oldEnd, newEnd uint64
	for strings.HasPrefix(p.line(), "@@ ") {
		matches := remediationHunkHeader.FindStringSubmatch(p.line())
		if matches == nil {
			return false
		}
		oldStart, oldCount, oldOK := remediationHunkRange(matches[1], matches[2])
		newStart, newCount, newOK := remediationHunkRange(matches[3], matches[4])
		if ended || !oldOK || !newOK || (added && (oldStart != 0 || oldCount != 0)) ||
			(deleted && (newStart != 0 || newCount != 0)) ||
			(hunks != 0 && (oldStart < oldEnd || newStart < newEnd)) {
			return false
		}
		p.position++
		if !p.hunkBody(oldCount, newCount, &ended) {
			return false
		}
		oldEnd, newEnd = oldStart+oldCount, newStart+newCount
		hunks++
	}
	return hunks != 0
}

func remediationHunkRange(startRaw, countRaw string) (uint64, uint64, bool) {
	start, err := strconv.ParseUint(startRaw, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	count := uint64(1)
	if countRaw != "" {
		count, err = strconv.ParseUint(countRaw, 10, 32)
	}
	return start, count, err == nil && count <= remediationMaxPatchBytes && (count == 0 || start != 0)
}

func (p *remediationPatchParser) hunkBody(oldRemaining, newRemaining uint64, ended *bool) bool {
	changed, marked := false, false
	var previous byte
	for {
		line := p.line()
		if line == remediationNoNewline {
			if previous == 0 || marked || (previous != '+' && oldRemaining != 0) || (previous != '-' && newRemaining != 0) {
				return false
			}
			p.position++
			marked = true
			*ended = true
			continue
		}
		if oldRemaining == 0 && newRemaining == 0 {
			return changed
		}
		if line == "" || p.position >= len(p.lines) {
			return false
		}
		switch line[0] {
		case ' ':
			if oldRemaining == 0 || newRemaining == 0 {
				return false
			}
			oldRemaining--
			newRemaining--
		case '-':
			if oldRemaining == 0 {
				return false
			}
			oldRemaining--
			changed = true
		case '+':
			if newRemaining == 0 {
				return false
			}
			newRemaining--
			changed = true
		default:
			return false
		}
		previous, marked = line[0], false
		p.position++
	}
}
