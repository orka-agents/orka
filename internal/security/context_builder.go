package security

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sozercan/orka/internal/store"
)

const (
	defaultMaxReviewContextFiles = 24
	defaultMaxReviewContextBytes = 96 * 1024
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
	intro.WriteString("\nValid evidence paths for this review:\n")

	candidates := reviewContextCandidates(slice)
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
		usedBytes := intro.Len() + pathList.Len() + len(citeDirective) + excerpts.Len() + len(finalDirective)
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
			Truncated: truncated || totalBytes > len(data),
			Readable:  true,
		})
		usedFiles++
	}

	var prompt strings.Builder
	prompt.WriteString(intro.String())
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
