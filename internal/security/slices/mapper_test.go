package slices

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
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
	for _, slice := range got {
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
}

func TestMapRepositorySkipsDependenciesSymlinksAndSecretLikeFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app/main.go", "package app\n")
	writeFile(t, root, "node_modules/pkg/index.js", "module.exports = {}\n")
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
	for _, slice := range got {
		for _, file := range slice.OwnedFiles {
			paths = append(paths, file.Path)
		}
	}
	for _, bad := range []string{
		"node_modules/pkg/index.js",
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
	for _, reviewSlice := range got {
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
