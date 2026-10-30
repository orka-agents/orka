package slices

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const (
	maxInvestigationOwnedFiles = 12
	maxBaselineEntrypoints     = 8
)

// addInvestigationSlices turns threat-model questions into slices whose files
// are starting points; the reviewer traces the question across the repository.
func addInvestigationSlices(builder *sliceBuilder, files []repoFile, investigations []security.SecurityInvestigation) {
	for _, investigation := range security.SanitizeInvestigations(investigations) {
		owned := investigationFiles(files, investigation.Paths)
		if len(owned) == 0 {
			owned = repositoryEntrypoints(files)
		}
		if len(owned) == 0 {
			continue
		}
		sum := sha256.Sum256([]byte(builder.repositoryScan + "|investigation|" + strings.ToLower(investigation.Question)))
		builder.add(store.ReviewSlice{
			ID:          "slice_inv_" + hex.EncodeToString(sum[:])[:16],
			Source:      "threat-model-investigation",
			Title:       "Investigation: " + investigation.Title,
			Summary:     investigation.Question,
			Kind:        security.SliceKindInvestigation,
			Entrypoints: []store.ReviewSliceFile{owned[0]},
			OwnedFiles:  owned,
			Tags:        []string{"cross-component"},
			Confidence:  riskLevelMedium,
		})
	}
}

// addBaselineAuditSlice adds one review that is not anchored on per-component
// slicing. Its files are the repository's entrypoints.
func addBaselineAuditSlice(builder *sliceBuilder, files []repoFile) {
	owned := repositoryEntrypoints(files)
	if len(owned) == 0 {
		return
	}
	builder.add(store.ReviewSlice{
		Source:      "deterministic-baseline-audit",
		Title:       "Independent repository audit",
		Summary:     "Repository-wide review of trust boundaries and cross-component attack paths, independent of per-component slices.",
		Kind:        security.SliceKindBaseline,
		Entrypoints: owned[:1],
		OwnedFiles:  owned,
		Tags:        []string{"cross-component"},
		Confidence:  riskLevelMedium,
	})
}

func investigationFiles(files []repoFile, paths []string) []store.ReviewSliceFile {
	out := []store.ReviewSliceFile{}
	seen := map[string]struct{}{}
	for _, want := range paths {
		want = strings.Trim(want, "/")
		for _, file := range files {
			if file.Path != want && !strings.HasPrefix(file.Path, want+"/") {
				continue
			}
			if isTestFile(file) {
				continue
			}
			if _, ok := seen[file.Path]; ok {
				continue
			}
			seen[file.Path] = struct{}{}
			out = append(out, fileRef(file, "investigation starting point"))
			if len(out) == maxInvestigationOwnedFiles {
				return out
			}
		}
	}
	return out
}

// repositoryEntrypoints prefers program entrypoints, shallowest first.
func repositoryEntrypoints(files []repoFile) []store.ReviewSliceFile {
	var candidates []repoFile
	for _, file := range files {
		if isTestFile(file) {
			continue
		}
		switch file.Name {
		case "main.go", "main.py", "__main__.py", "index.ts", "index.js", "server.ts", "server.js", "app.py":
			candidates = append(candidates, file)
		}
	}
	if len(candidates) == 0 {
		for _, file := range files {
			if isSourceCodePath(file.Path) && !isTestFile(file) {
				candidates = append(candidates, file)
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		di, dj := strings.Count(candidates[i].Path, "/"), strings.Count(candidates[j].Path, "/")
		if di != dj {
			return di < dj
		}
		return candidates[i].Path < candidates[j].Path
	})
	out := make([]store.ReviewSliceFile, 0, min(len(candidates), maxBaselineEntrypoints))
	for _, file := range candidates[:min(len(candidates), maxBaselineEntrypoints)] {
		out = append(out, fileRef(file, "repository entrypoint"))
	}
	return out
}

func isTestFile(file repoFile) bool {
	name := strings.ToLower(file.Name)
	if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "test_") || strings.Contains(name, ".test.") || strings.Contains(name, ".spec.") {
		return true
	}
	for segment := range strings.SplitSeq(path.Dir(file.Path), "/") {
		switch segment {
		case "test", "tests", "testdata", "__tests__", "e2e":
			return true
		}
	}
	return false
}
