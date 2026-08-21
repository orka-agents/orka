package slices

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

func TestMapRepositoryCoversLanguagesAndOperationalSurfaces(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "internal/api/handler.go", "package api\n")
	writeFile(t, root, "internal/api/handler_test.go", "package api\n")
	writeFile(t, root, "ui/package.json", `{"scripts":{"test":"vitest","lint":"eslint .","build":"vite build"}}`)
	writeFile(t, root, "ui/src/app.tsx", "export function App() { return null }\n")
	writeFile(t, root, "service/pyproject.toml", "[project]\nname='svc'\n")
	writeFile(t, root, "service/app/main.py", "from fastapi import FastAPI\n")
	writeFile(t, root, ".github/workflows/ci.yml", "name: ci\n")
	writeFile(t, root, "scripts/release.sh", "#!/bin/sh\n")
	writeFile(t, root, "Dockerfile", "FROM scratch\n")
	writeFile(t, root, "misc/tool.rb", "puts 'fallback'\n")

	got, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	sources := map[string]bool{}
	for _, slice := range got.Slices {
		sources[slice.Source] = true
		if slice.RepositoryScan != "repo" {
			t.Fatalf("slice.RepositoryScan = %q, want repo", slice.RepositoryScan)
		}
	}
	for _, want := range []string{
		"deterministic-go-package",
		"deterministic-node-package",
		"deterministic-python-package",
		"deterministic-workflows",
		"deterministic-scripts",
		"deterministic-config",
	} {
		if !sources[want] {
			t.Fatalf("sources = %#v, want %s", sources, want)
		}
	}
}

func TestMapRepositoryStableAcrossRepeatedRuns(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cmd/app/main.go", "package main\n")
	writeFile(t, root, "cmd/app/main_test.go", "package main\n")

	first, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository(first) error = %v", err)
	}
	second, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository(second) error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("MapRepository() not stable:\nfirst=%#v\nsecond=%#v", first, second)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("json.Marshal(first) error = %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("json.Marshal(second) error = %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("serialized mapper result is not deterministic:\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
	}
}

func TestMapRepositoryBoundsInventoryWithTruthfulTruncationSummary(t *testing.T) {
	oldLimit := mapperInventoryEntryLimit
	mapperInventoryEntryLimit = 3
	t.Cleanup(func() { mapperInventoryEntryLimit = oldLimit })

	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		writeFile(t, root, name, "package app\n")
	}

	got, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	wantSummary := security.MapperInventorySummary{
		EntryLimit:       3,
		TotalEntries:     5,
		RetainedEntries:  3,
		TruncatedEntries: 2,
		Truncated:        true,
		Reason:           security.MapperCoverageReasonInventoryEntryLimit,
	}
	if !reflect.DeepEqual(got.InventorySummary, wantSummary) {
		t.Fatalf("InventorySummary = %#v, want %#v", got.InventorySummary, wantSummary)
	}
	if len(got.DiscoveredFiles) != 3 || len(got.ReviewableFiles) != 3 {
		t.Fatalf(
			"retained inventory lengths = discovered %d/reviewable %d, want 3/3",
			len(got.DiscoveredFiles),
			len(got.ReviewableFiles),
		)
	}
	for _, excess := range []string{"d.go", "e.go"} {
		if hasInventoryPath(got.DiscoveredFiles, excess) || hasInventoryPath(got.ReviewableFiles, excess) ||
			hasInventoryPath(got.OmittedFiles, excess) {
			t.Fatalf("excess path %q was materialized instead of summarized: %#v", excess, got)
		}
	}
}

func TestMapRepositoryBoundsDerivedOmissionsWithTruthfulTruncationSummary(t *testing.T) {
	oldLimit := mapperOmittedInventoryEntryLimit
	mapperOmittedInventoryEntryLimit = 3
	t.Cleanup(func() { mapperOmittedInventoryEntryLimit = oldLimit })

	root := t.TempDir()
	writeFile(t, root, "pkg/main.go", "package pkg\n")
	for i := range maxReviewSliceContext + 2 {
		writeFile(t, root, fmt.Sprintf("pkg/file_%02d_test.go", i), "package pkg\n")
	}

	got, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	if len(got.OmittedFiles) != mapperOmittedInventoryEntryLimit {
		t.Fatalf("len(OmittedFiles) = %d, want %d", len(got.OmittedFiles), mapperOmittedInventoryEntryLimit)
	}
	wantRetained := maxReviewSliceContext + 3
	wantSummary := security.MapperInventorySummary{
		EntryLimit:       security.MaxMapperInventoryEntries,
		TotalEntries:     wantRetained,
		RetainedEntries:  wantRetained,
		TruncatedEntries: 0,
		OmissionRecords: &security.MapperOmissionRecordSummary{
			EntryLimit:       mapperOmittedInventoryEntryLimit,
			TotalRecords:     mapperOmittedInventoryEntryLimit + 1,
			RetainedRecords:  mapperOmittedInventoryEntryLimit,
			TruncatedRecords: 1,
			Truncated:        true,
		},
		Truncated: true,
		Reason:    security.MapperCoverageReasonInventoryEntryLimit,
	}
	if !reflect.DeepEqual(got.InventorySummary, wantSummary) {
		t.Fatalf("InventorySummary = %#v, want %#v", got.InventorySummary, wantSummary)
	}
}

func TestBoundedOmittedInventoryPreservesRequiredEntriesBeforeReferences(t *testing.T) {
	oldLimit := mapperOmittedInventoryEntryLimit
	mapperOmittedInventoryEntryLimit = 3
	t.Cleanup(func() { mapperOmittedInventoryEntryLimit = oldLimit })

	collected := []security.MapperFileInventoryEntry{
		inventoryEntry("README.md", security.MapperDispositionExcluded, mapperReasonUnsupported),
	}
	unassigned := []security.MapperFileInventoryEntry{
		inventoryEntry("orphan.go", security.MapperDispositionOmitted, mapperReasonUnassigned),
	}
	references := []security.MapperFileInventoryEntry{
		inventoryEntry("a.go", security.MapperDispositionOmitted, mapperReasonContextCap),
		inventoryEntry("b.go", security.MapperDispositionOmitted, mapperReasonContextCap),
		inventoryEntry("c.go", security.MapperDispositionOmitted, mapperReasonContextCap),
	}

	got, truncated, err := boundedOmittedInventory(collected, unassigned, references)
	if err != nil {
		t.Fatalf("boundedOmittedInventory() error = %v", err)
	}
	if len(got) != mapperOmittedInventoryEntryLimit {
		t.Fatalf("len(boundedOmittedInventory()) = %d, want %d", len(got), mapperOmittedInventoryEntryLimit)
	}
	if truncated != 2 {
		t.Fatalf("truncated = %d, want 2", truncated)
	}
	for _, required := range append(collected, unassigned...) {
		if !slices.Contains(got, required) {
			t.Fatalf("bounded omissions = %#v, missing required entry %#v", got, required)
		}
	}
}

func TestMapRepositorySkipsDependenciesSymlinksAndSecretLikeFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app/main.go", "package app\n")
	writeFile(t, root, "node_modules/pkg/index.js", "module.exports = {}\n")
	writeFile(t, root, "vendor/pkg/vendor.go", "package vendor\n")
	writeFile(t, root, "build/generated.go", "package generated\n")
	writeFile(t, root, ".orka-artifacts/injected.go", "package injected\n")
	writeFile(t, root, "package-lock.json", `{"lockfileVersion":3}`)
	writeFile(t, root, "README.md", "# docs\n")
	writeFile(t, root, ".env", "SECRET=value\n")
	writeFile(t, root, "private.pem", "secret\n")
	writeFile(t, root, "secrets/prod.yaml", "password: value\n")
	writeFile(t, root, "credentials/config.json", `{"token":"value"}`)
	writeFile(t, root, "tokens/config.toml", `token = "value"`)
	target := filepath.Join(root, "app")
	link := filepath.Join(root, "linked-app")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	got, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	paths := []string{}
	for _, slice := range got.Slices {
		for _, file := range slice.OwnedFiles {
			paths = append(paths, file.Path)
		}
	}
	for _, bad := range []string{
		"node_modules/pkg/index.js",
		"vendor/pkg/vendor.go",
		"build/generated.go",
		".orka-artifacts/injected.go",
		"package-lock.json",
		"README.md",
		".env",
		"private.pem",
		"secrets/prod.yaml",
		"credentials/config.json",
		"tokens/config.toml",
		"linked-app/main.go",
	} {
		if slices.Contains(paths, bad) {
			t.Fatalf("mapped paths = %#v, should not include %s", paths, bad)
		}
	}
	wantOmissions := map[string]string{
		"node_modules":            mapperReasonDependencyDirectory,
		"vendor":                  mapperReasonDependencyDirectory,
		"build":                   mapperReasonGeneratedDirectory,
		".orka-artifacts":         mapperReasonGeneratedDirectory,
		"package-lock.json":       mapperReasonLockfile,
		"README.md":               mapperReasonUnsupported,
		".env":                    mapperReasonSecretLike,
		"private.pem":             mapperReasonSecretLike,
		"secrets/prod.yaml":       mapperReasonSecretLike,
		"credentials/config.json": mapperReasonSecretLike,
		"tokens/config.toml":      mapperReasonSecretLike,
		"linked-app":              mapperReasonSymlink,
	}
	for path, reason := range wantOmissions {
		if !hasInventoryEntry(got.OmittedFiles, path, security.MapperDispositionExcluded, reason) {
			t.Fatalf("omittedFiles = %#v, want %s excluded as %s", got.OmittedFiles, path, reason)
		}
	}
}

func TestMapRepositoryBoundsLargeReviewSlices(t *testing.T) {
	root := t.TempDir()
	const sourceFiles = maxReviewSliceOwnedFiles*2 + 5
	for i := range sourceFiles {
		writeFile(t, root, fmt.Sprintf("pkg/large/file_%02d.go", i), "package large\n")
		writeFile(t, root, fmt.Sprintf("pkg/large/file_%02d_test.go", i), "package large\n")
	}

	got, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}

	goPackageParts := 0
	ownedInGoPackageParts := 0
	seenOwned := map[string]struct{}{}
	for _, reviewSlice := range got.Slices {
		if len(reviewSlice.Entrypoints) > maxReviewSliceEntrypoints ||
			len(reviewSlice.OwnedFiles) > maxReviewSliceOwnedFiles ||
			len(reviewSlice.ContextFiles) > maxReviewSliceContext ||
			len(reviewSlice.Tests) > maxReviewSliceTests {
			t.Fatalf("review slice is unbounded: %#v", reviewSlice)
		}
		if reviewSlice.Source != "deterministic-go-package" ||
			!strings.HasPrefix(reviewSlice.Title, "Go package pkg/large") {
			continue
		}
		goPackageParts++
		ownedInGoPackageParts += len(reviewSlice.OwnedFiles)
		for _, file := range reviewSlice.OwnedFiles {
			if _, ok := seenOwned[file.Path]; ok {
				t.Fatalf("owned file %q appears in multiple package parts", file.Path)
			}
			seenOwned[file.Path] = struct{}{}
		}
	}
	wantParts := (sourceFiles + maxReviewSliceOwnedFiles - 1) / maxReviewSliceOwnedFiles
	if goPackageParts != wantParts || ownedInGoPackageParts != sourceFiles {
		t.Fatalf("go package parts/files = %d/%d, want %d/%d", goPackageParts, ownedInGoPackageParts, wantParts, sourceFiles)
	}
	for i := maxReviewSliceContext; i < sourceFiles; i++ {
		path := fmt.Sprintf("pkg/large/file_%02d_test.go", i)
		if !hasInventoryEntry(got.OmittedFiles, path, security.MapperDispositionOmitted, mapperReasonContextCap) {
			t.Fatalf("omittedFiles missing context cap for %s: %#v", path, got.OmittedFiles)
		}
		if !hasInventoryEntry(got.OmittedFiles, path, security.MapperDispositionOmitted, mapperReasonTestCap) {
			t.Fatalf("omittedFiles missing test cap for %s: %#v", path, got.OmittedFiles)
		}
		if !hasInventoryEntry(got.ReviewableFiles, path, security.MapperDispositionAssigned, mapperReasonAssigned) {
			t.Fatalf("reviewableFiles should retain %s through fallback assignment: %#v", path, got.ReviewableFiles)
		}
	}
}

func TestMapRepositoryAccountsForLargeNodeAndGenericGroups(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "ui/package.json", `{"scripts":{"test":"vitest"}}`)
	const nodeFiles = maxReviewSliceOwnedFiles*2 + 7
	for i := range nodeFiles {
		writeFile(t, root, fmt.Sprintf("ui/src/file_%02d.ts", i), "export const value = 1\n")
		writeFile(t, root, fmt.Sprintf("misc/file_%02d.yaml", i), "enabled: true\n")
	}

	got, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	for i := range nodeFiles {
		for _, path := range []string{
			fmt.Sprintf("ui/src/file_%02d.ts", i),
			fmt.Sprintf("misc/file_%02d.yaml", i),
		} {
			if !hasInventoryEntry(got.ReviewableFiles, path, security.MapperDispositionAssigned, mapperReasonAssigned) {
				t.Fatalf("reviewableFiles missing assigned path %s", path)
			}
		}
	}
	if countOwnedPaths(got.Slices, "deterministic-node-package", "ui/src/") != nodeFiles {
		t.Fatalf("node package slices silently lost source files: %#v", got.Slices)
	}
	if countOwnedPaths(got.Slices, "deterministic-generic-directory", "misc/") != nodeFiles {
		t.Fatalf("generic slices silently lost source files: %#v", got.Slices)
	}
}

func TestMapRepositoryAccountsForLargeWorkflowScriptAndConfigGroups(t *testing.T) {
	root := t.TempDir()
	const filesPerGroup = maxReviewSliceOwnedFiles + 9
	for i := range filesPerGroup {
		writeFile(t, root, fmt.Sprintf(".github/workflows/ci_%02d.yml", i), "name: ci\n")
		writeFile(t, root, fmt.Sprintf("scripts/task_%02d.sh", i), "#!/bin/sh\n")
		writeFile(t, root, fmt.Sprintf("configs/%02d/Dockerfile", i), "FROM scratch\n")
	}

	got, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	tests := []struct {
		source string
		prefix string
	}{
		{source: "deterministic-workflows", prefix: ".github/workflows/"},
		{source: "deterministic-scripts", prefix: "scripts/"},
		{source: "deterministic-config", prefix: "configs/"},
	}
	for _, tt := range tests {
		if gotCount := countOwnedPaths(got.Slices, tt.source, tt.prefix); gotCount != filesPerGroup {
			t.Fatalf("%s owned files = %d, want %d", tt.source, gotCount, filesPerGroup)
		}
	}
}

func TestBoundedReviewSlicesAccountsForCappedReferences(t *testing.T) {
	entrypoints := make([]store.ReviewSliceFile, maxReviewSliceEntrypoints+2)
	owned := make([]store.ReviewSliceFile, maxReviewSliceEntrypoints+2)
	for i := range entrypoints {
		path := fmt.Sprintf("pkg/file_%02d.go", i)
		entrypoints[i] = store.ReviewSliceFile{Path: path}
		owned[i] = store.ReviewSliceFile{Path: path}
	}
	reviewSlices, omitted := boundedReviewSlices(store.ReviewSlice{
		SchemaVersion:  1,
		ID:             "slice_test",
		RepositoryScan: "repo",
		Source:         "test",
		Title:          "test",
		OwnedFiles:     owned,
		Entrypoints:    entrypoints,
	})
	if len(reviewSlices) != 1 || len(reviewSlices[0].Entrypoints) != maxReviewSliceEntrypoints {
		t.Fatalf("boundedReviewSlices() = %#v, want one bounded slice", reviewSlices)
	}
	for i := maxReviewSliceEntrypoints; i < len(entrypoints); i++ {
		if !hasInventoryEntry(omitted, entrypoints[i].Path, security.MapperDispositionOmitted, mapperReasonEntrypointCap) {
			t.Fatalf("omitted references = %#v, want entrypoint cap for %s", omitted, entrypoints[i].Path)
		}
	}
}

func TestMapRepositoryScopeRootExcludesOutsidePaths(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "outside/main.go", "package outside\n")
	writeFile(t, repo, "services/api/main.go", "package api\n")

	got, err := MapRepository(filepath.Join(repo, "services/api"), MapperOptions{RepositoryScan: "repo", SubPath: "services/api"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	if hasInventoryPath(got.DiscoveredFiles, "outside/main.go") {
		t.Fatalf("discoveredFiles escaped configured scope: %#v", got.DiscoveredFiles)
	}
	if !hasInventoryPath(got.DiscoveredFiles, "main.go") {
		t.Fatalf("discoveredFiles = %#v, want scoped main.go", got.DiscoveredFiles)
	}
}

func hasInventoryEntry(entries []security.MapperFileInventoryEntry, path, disposition, reason string) bool {
	return slices.Contains(entries, security.MapperFileInventoryEntry{Path: path, Disposition: disposition, Reason: reason})
}

func hasInventoryPath(entries []security.MapperFileInventoryEntry, path string) bool {
	for _, entry := range entries {
		if entry.Path == path {
			return true
		}
	}
	return false
}

func countOwnedPaths(reviewSlices []store.ReviewSlice, source, prefix string) int {
	seen := map[string]struct{}{}
	for _, reviewSlice := range reviewSlices {
		if reviewSlice.Source != source {
			continue
		}
		for _, file := range reviewSlice.OwnedFiles {
			if strings.HasPrefix(file.Path, prefix) {
				seen[file.Path] = struct{}{}
			}
		}
	}
	return len(seen)
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
