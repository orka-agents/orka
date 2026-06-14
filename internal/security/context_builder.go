package security

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sozercan/orka/internal/store"
)

const (
	defaultMaxReviewContextFiles      = 24
	defaultMaxReviewContextBytes      = 96 * 1024
	maxReviewContextChangedFiles      = 32
	maxReviewContextChangedLineRanges = 64
	maxReviewContextChangedBlockBytes = 4096
)

type ReviewContextOptions struct {
	MaxFiles int
	MaxBytes int
}

// BuildReviewContext builds a bounded deterministic prompt excerpt and manifest for a review slice.
func BuildReviewContext(root string, slice store.ReviewSlice, opts ReviewContextOptions) (string, ReviewContextManifest, error) {
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = defaultMaxReviewContextFiles
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxReviewContextBytes
	}

	manifest := ReviewContextManifest{
		SchemaVersion: SchemaVersionReviewContext,
		SliceID:       slice.ID,
	}
	var intro strings.Builder
	fmt.Fprintf(&intro, "Review slice: %s\n", slice.Title)
	fmt.Fprintf(&intro, "Slice ID: %s\n", slice.ID)
	fmt.Fprintf(&intro, "Kind: %s\n", slice.Kind)
	if strings.TrimSpace(slice.Summary) != "" {
		fmt.Fprintf(&intro, "Summary: %s\n", slice.Summary)
	}

	candidates := reviewContextCandidates(slice)
	candidatePaths := candidatePathSet(candidates)
	preliminaryChangedFiles := changedFilesForIncludedPaths(slice.ChangedFiles, candidatePaths)
	preliminaryChangedLineRanges := changedLineRangesForIncludedPaths(slice.ChangedLineRanges, candidatePaths)
	changedBlockBudget := reviewContextChangedBlockBudget(opts.MaxBytes)
	reservedChangedBlockBytes := len(reviewContextChangedRiskBlock(preliminaryChangedFiles, preliminaryChangedLineRanges, changedBlockBudget))
	const validPathsDirective = "\nValid evidence paths for this review:\n"
	const citeDirective = "\nCite findings only from included file ranges below.\n"
	const finalDirective = "\nReturn security-findings.v2.json only. Invalid evidence will be dropped.\n"

	var pathList strings.Builder
	var excerpts strings.Builder
	usedFiles := 0
	for _, candidate := range candidates {
		if usedFiles >= opts.MaxFiles {
			manifest.OmittedFiles = append(manifest.OmittedFiles, ReviewContextOmittedFile{
				Path:   candidate.Path,
				Role:   candidate.Role,
				Reason: "maxFiles",
			})
			continue
		}
		if !SafeRepoPath(candidate.Path) {
			manifest.OmittedFiles = append(manifest.OmittedFiles, ReviewContextOmittedFile{
				Path:   candidate.Path,
				Role:   candidate.Role,
				Reason: "unsafePath",
			})
			continue
		}

		pathLine := fmt.Sprintf("- %s (%s)\n", candidate.Path, candidate.Role)
		header := fmt.Sprintf("\n--- %s (%s) ---\n", candidate.Path, candidate.Role)
		usedBytes := intro.Len() + reservedChangedBlockBytes + len(validPathsDirective) + pathList.Len() + len(citeDirective) + excerpts.Len() + len(finalDirective)
		remaining := opts.MaxBytes - usedBytes - len(pathLine) - len(header)
		if remaining <= 0 {
			manifest.OmittedFiles = append(manifest.OmittedFiles, ReviewContextOmittedFile{
				Path:   candidate.Path,
				Role:   candidate.Role,
				Reason: "maxBytes",
			})
			continue
		}

		data, totalBytes, err := readRepoFilePrefix(root, candidate.Path, remaining)
		if err != nil {
			reason := "unreadable"
			manifest.IncludedFiles = append(manifest.IncludedFiles, ReviewContextIncludedFile{
				Path:          candidate.Path,
				Role:          candidate.Role,
				Readable:      false,
				SkippedReason: &reason,
			})
			continue
		}

		rendered, endLine, truncated := numberedExcerpt(string(data), remaining)
		if rendered == "" {
			manifest.OmittedFiles = append(manifest.OmittedFiles, ReviewContextOmittedFile{
				Path:   candidate.Path,
				Role:   candidate.Role,
				Reason: "maxBytes",
			})
			continue
		}
		pathList.WriteString(pathLine)
		excerpts.WriteString(header)
		excerpts.WriteString(rendered)
		if !strings.HasSuffix(rendered, "\n") {
			excerpts.WriteString("\n")
		}

		includedBytes := len([]byte(rendered))
		manifest.IncludedFiles = append(manifest.IncludedFiles, ReviewContextIncludedFile{
			Path:          candidate.Path,
			Role:          candidate.Role,
			Bytes:         totalBytes,
			IncludedBytes: includedBytes,
			IncludedLineRanges: []ReviewContextLineRange{{
				StartLine: 1,
				EndLine:   endLine,
			}},
			Excerpt:   linesInRange(string(data), 1, endLine),
			Truncated: truncated || totalBytes > len(data),
			Readable:  true,
		})
		usedFiles++
	}

	includedPaths := includedFilePathSet(manifest.IncludedFiles)
	includedRanges := includedFileLineRangeMap(manifest.IncludedFiles)
	manifest.ChangedFiles = changedFilesForIncludedPaths(slice.ChangedFiles, includedPaths)
	manifest.ChangedLineRanges = changedLineRangesForIncludedRanges(slice.ChangedLineRanges, includedRanges)

	var prompt strings.Builder
	prompt.WriteString(intro.String())
	if changeBlock := reviewContextChangedRiskBlock(manifest.ChangedFiles, manifest.ChangedLineRanges, changedBlockBudget); changeBlock != "" {
		prompt.WriteString(changeBlock)
	}
	prompt.WriteString(validPathsDirective)
	prompt.WriteString(pathList.String())
	prompt.WriteString(citeDirective)
	prompt.WriteString(excerpts.String())
	prompt.WriteString(finalDirective)
	manifest.PromptBytes = prompt.Len()
	manifest.ApproximateTokens = (manifest.PromptBytes + 3) / 4
	return prompt.String(), manifest, nil
}

type reviewContextCandidate struct {
	Path string
	Role string
}

func reviewContextCandidates(slice store.ReviewSlice) []reviewContextCandidate {
	out := []reviewContextCandidate{}
	seen := map[string]struct{}{}
	add := func(path, role string) {
		path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
		if path == "" {
			return
		}
		key := role + "|" + path
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, reviewContextCandidate{Path: path, Role: role})
	}
	for _, file := range slice.OwnedFiles {
		add(file.Path, "owned")
	}
	for _, file := range slice.ContextFiles {
		add(file.Path, "context")
	}
	for _, test := range slice.Tests {
		add(test.Path, "test")
	}
	return out
}

func candidatePathSet(candidates []reviewContextCandidate) map[string]struct{} {
	out := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		path := strings.TrimSpace(strings.ReplaceAll(candidate.Path, "\\", "/"))
		if SafeRepoPath(path) {
			out[path] = struct{}{}
		}
	}
	return out
}

func includedFilePathSet(files []ReviewContextIncludedFile) map[string]struct{} {
	out := make(map[string]struct{}, len(files))
	for _, file := range files {
		if file.Readable && SafeRepoPath(file.Path) {
			out[file.Path] = struct{}{}
		}
	}
	return out
}

func changedFilesForIncludedPaths(files []string, included map[string]struct{}) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, min(len(files), maxReviewContextChangedFiles))
	for _, file := range files {
		file = strings.TrimSpace(strings.ReplaceAll(file, "\\", "/"))
		if !SafeRepoPath(file) {
			continue
		}
		if _, ok := included[file]; !ok {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
		if len(out) == maxReviewContextChangedFiles {
			break
		}
	}
	sort.Strings(out)
	return out
}

func includedFileLineRangeMap(files []ReviewContextIncludedFile) map[string][]ReviewContextLineRange {
	out := make(map[string][]ReviewContextLineRange, len(files))
	for _, file := range files {
		if file.Readable && SafeRepoPath(file.Path) {
			out[file.Path] = append([]ReviewContextLineRange(nil), file.IncludedLineRanges...)
		}
	}
	return out
}

func changedLineRangesForIncludedPaths(ranges []store.ChangedLineRange, included map[string]struct{}) []store.ChangedLineRange {
	rangeMap := make(map[string][]ReviewContextLineRange, len(included))
	for path := range included {
		rangeMap[path] = []ReviewContextLineRange{{StartLine: 1, EndLine: int(^uint(0) >> 1)}}
	}
	return changedLineRangesForIncludedRanges(ranges, rangeMap)
}

func changedLineRangesForIncludedRanges(ranges []store.ChangedLineRange, included map[string][]ReviewContextLineRange) []store.ChangedLineRange {
	out := make([]store.ChangedLineRange, 0, min(len(ranges), maxReviewContextChangedLineRanges))
	for _, lineRange := range ranges {
		lineRange.Path = strings.TrimSpace(strings.ReplaceAll(lineRange.Path, "\\", "/"))
		if !validChangedLineRange(lineRange) {
			continue
		}
		for _, includedRange := range included[lineRange.Path] {
			startLine := max(lineRange.StartLine, includedRange.StartLine)
			endLine := min(lineRange.EndLine, includedRange.EndLine)
			if startLine > endLine {
				continue
			}
			out = append(out, store.ChangedLineRange{Path: lineRange.Path, StartLine: startLine, EndLine: endLine})
			if len(out) == maxReviewContextChangedLineRanges {
				break
			}
		}
		if len(out) == maxReviewContextChangedLineRanges {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].EndLine < out[j].EndLine
	})
	return out
}

func reviewContextChangedBlockBudget(maxPromptBytes int) int {
	if maxPromptBytes <= 0 {
		return 0
	}
	budget := min(maxPromptBytes/5, maxReviewContextChangedBlockBytes)
	return budget
}

func reviewContextChangedRiskBlock(changedFiles []string, changedLineRanges []store.ChangedLineRange, maxBytes int) string {
	if maxBytes <= 0 || (len(changedFiles) == 0 && len(changedLineRanges) == 0) {
		return ""
	}
	var out strings.Builder
	appendLine := func(line string) bool {
		if out.Len()+len(line) > maxBytes {
			return false
		}
		out.WriteString(line)
		return true
	}
	if !appendLine("\nChanged-code focus for incremental/manual review:\n") {
		return ""
	}
	if !appendLine("\n- Focus on newly introduced, newly exposed, or materially worsened security risk.\n") {
		return out.String()
	}
	if !appendLine("- Primary evidence should intersect changed lines when possible. Existing unchanged lines may be cited only as supporting context.\n") {
		return out.String()
	}
	if !appendLine("- Do not report old repository-wide issues unless changed lines introduce, expose, or materially worsen the risk.\n") {
		return out.String()
	}
	if len(changedFiles) > 0 && appendLine("- Changed files included in this slice:\n") {
		for _, file := range changedFiles {
			if !appendLine(fmt.Sprintf("  - %s\n", file)) {
				appendLine("  - ...additional changed files omitted from prompt budget...\n")
				return out.String()
			}
		}
	}
	if len(changedLineRanges) > 0 && appendLine("- Changed line ranges included in this slice:\n") {
		for _, lineRange := range changedLineRanges {
			if !appendLine(fmt.Sprintf("  - %s:%d-%d\n", lineRange.Path, lineRange.StartLine, lineRange.EndLine)) {
				appendLine("  - ...additional changed line ranges omitted from prompt budget...\n")
				return out.String()
			}
		}
	}
	return out.String()
}

func readRepoFilePrefix(root, repoPath string, maxBytes int) ([]byte, int, error) {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, 0, err
	}
	fullPath := filepath.Join(cleanRoot, filepath.FromSlash(repoPath))
	cleanPath, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, 0, err
	}
	if cleanPath != cleanRoot && !strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator)) {
		return nil, 0, fmt.Errorf("path escapes root")
	}
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return nil, 0, err
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("not a regular file")
	}
	file, err := os.Open(cleanPath)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close() //nolint:errcheck
	if maxBytes <= 0 {
		return nil, int(info.Size()), nil
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)))
	if err != nil {
		return nil, 0, err
	}
	return data, int(info.Size()), nil
}

func numberedExcerpt(content string, maxBytes int) (string, int, bool) {
	var out strings.Builder
	lines := strings.Split(content, "\n")
	endLine := 0
	for i, line := range lines {
		rendered := fmt.Sprintf("%6d  %s\n", i+1, line)
		if out.Len()+len(rendered) > maxBytes {
			return out.String(), endLine, true
		}
		out.WriteString(rendered)
		endLine = i + 1
	}
	return out.String(), endLine, false
}
