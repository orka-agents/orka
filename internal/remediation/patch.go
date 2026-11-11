package remediation

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/pmezard/go-difflib/difflib"
)

// EditMatchError identifies an ambiguous or missing replacement without
// returning source or replacement text.
type EditMatchError struct {
	Index    int
	Matches  int
	Expected int
}

func (e *EditMatchError) Error() string {
	if e.Expected > 1 {
		return fmt.Sprintf("candidate edit %d old text matches %d times; exactly %d matches are required", e.Index, e.Matches, e.Expected)
	}
	return fmt.Sprintf("candidate edit %d old text matches %d times; exactly one match is required", e.Index, e.Matches)
}

// CompilePatch applies exact, count-bound text replacements to pinned source bytes
// and computes the diff locally. The model never supplies hunk line counts.
func CompilePatch(proposal PatchProposal, original map[string][]byte) (string, error) {
	if proposal.Patch != "" {
		if len(proposal.Edits) != 0 {
			return "", errors.New("candidate must use a diff or exact edits, not both")
		}
		return proposal.Patch, nil
	}
	if len(proposal.Edits) == 0 || len(proposal.Edits) > 64 {
		return "", errors.New("candidate has no bounded source edits")
	}
	changed := make(map[string]string)
	for index, edit := range proposal.Edits {
		expected := 1
		if edit.Occurrences != nil {
			expected = *edit.Occurrences
		}
		if expected < 1 || expected > MaxEditOccurrences {
			return "", errors.New("candidate edit occurrence count is outside its bounds")
		}
		raw, found := original[edit.Path]
		if !found || len(raw) > maxProposalBytes || !utf8.Valid(raw) || strings.ContainsRune(string(raw), '\x00') ||
			!strings.HasSuffix(string(raw), "\n") || strings.ContainsAny(edit.Path, "\x00\r\n\t\"\\ ") {
			return "", errors.New("exact edit source must be a pinned bounded text file with a safe path")
		}
		current, found := changed[edit.Path]
		if !found {
			current = string(raw)
		}
		if matches := strings.Count(current, edit.Old); edit.Old == "" || matches != expected {
			return "", &EditMatchError{Index: index, Matches: matches, Expected: expected}
		}
		next := strings.Replace(current, edit.Old, edit.New, expected)
		if next == current || len(next) > maxProposalBytes || !strings.HasSuffix(next, "\n") {
			return "", errors.New("candidate edit is empty, oversized, or removes the final newline")
		}
		changed[edit.Path] = next
	}
	files := make([]string, 0, len(changed))
	for name := range changed {
		files = append(files, name)
	}
	sort.Strings(files)
	var patch strings.Builder
	for _, name := range files {
		before := strings.SplitAfter(string(original[name]), "\n")
		after := strings.SplitAfter(changed[name], "\n")
		diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: before[:len(before)-1], B: after[:len(after)-1],
			FromFile: "a/" + name, ToFile: "b/" + name, Context: 3,
		})
		if err != nil {
			return "", errors.New("candidate diff could not be computed")
		}
		fmt.Fprintf(&patch, "diff --git a/%s b/%s\n%s", name, name, diff)
		if patch.Len() > maxProposalBytes {
			return "", errors.New("candidate diff exceeds its size limit")
		}
	}
	return patch.String(), nil
}
