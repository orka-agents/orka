package slices

import (
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

func TestReferenceFilesFollowInRepositoryGoImports(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/app\n\ngo 1.22\n")
	writeFile(t, root, "main.go", "package main\n\nimport (\n\t\"fmt\"\n\t\"example.com/app/internal/core\"\n)\n\nfunc main() { fmt.Println(core.Run()) }\n")
	writeFile(t, root, "internal/core/core.go", "package core\n\nimport \"example.com/app/apis/types\"\n\nfunc Run() types.Value { return types.Value{} }\n")
	writeFile(t, root, "internal/core/core_test.go", "package core\n")
	writeFile(t, root, "apis/types/types.go", "package types\n\ntype Value struct{}\n")
	writeFile(t, root, "unrelated/other.go", "package unrelated\n")
	writeFile(t, root, "third_party/lib/lib.go", "package lib\n")

	slices, err := MapRepository(root, MapperOptions{RepositoryScan: "repo", BaselineAudit: true})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	refs, err := ReferenceFiles(root, slices)
	if err != nil {
		t.Fatalf("ReferenceFiles() error = %v", err)
	}

	core := sliceByTitle(t, slices, "Go package internal/core")
	got := referenceSummary(refs[core.ID])
	for _, want := range []string{
		"internal/core/core.go=slice:5",
		"internal/core/core_test.go=slice:1",
		"apis/types/types.go=imports:3",
		"main.go=imported-by:8",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("core references = %s, want %s", got, want)
		}
	}
	if strings.Contains(got, "unrelated/") || strings.Contains(got, "third_party/") {
		t.Fatalf("core references include unrelated or vendored files: %s", got)
	}

	baseline := sliceByTitle(t, slices, "Independent repository audit")
	if baseline.Kind != security.SliceKindBaseline || len(baseline.OwnedFiles) == 0 || baseline.OwnedFiles[0].Path != "main.go" {
		t.Fatalf("baseline slice = %#v, want main.go as its entrypoint", baseline)
	}
	baselineRefs := referenceSummary(refs[baseline.ID])
	if !strings.Contains(baselineRefs, "unrelated/other.go=repository") || strings.Contains(baselineRefs, "third_party/") {
		t.Fatalf("baseline references = %s, want the repository index without vendored code", baselineRefs)
	}
}

func TestMapRepositoryAddsInvestigationSlices(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/flow/a.go", "package flow\n")
	writeFile(t, root, "pkg/flow/a_test.go", "package flow\n")
	writeFile(t, root, "pkg/sink/b.go", "package sink\n")
	writeFile(t, root, "cmd/app/main.go", "package main\n")

	slices, err := MapRepository(root, MapperOptions{RepositoryScan: "repo", Investigations: []security.SecurityInvestigation{
		{Title: "Flow to sink", Question: "Does flow input reach the sink unchecked?", Paths: []string{"pkg/flow", "pkg/sink/b.go", "missing.go"}},
		{Title: "No paths", Question: "Is the entrypoint authenticated?", Paths: []string{"missing"}},
	}})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	flow := sliceByTitle(t, slices, "Investigation: Flow to sink")
	if flow.Kind != security.SliceKindInvestigation || flow.Summary != "Does flow input reach the sink unchecked?" ||
		!strings.HasPrefix(flow.ID, "slice_inv_") {
		t.Fatalf("investigation slice = %#v", flow)
	}
	if got := filePaths(flow.OwnedFiles); got != "pkg/flow/a.go,pkg/sink/b.go" {
		t.Fatalf("investigation files = %s, want test files skipped and directory expanded", got)
	}
	if got := filePaths(sliceByTitle(t, slices, "Investigation: No paths").OwnedFiles); got != "cmd/app/main.go" {
		t.Fatalf("pathless investigation files = %s, want repository entrypoint", got)
	}
}

func sliceByTitle(t *testing.T, slices []store.ReviewSlice, title string) store.ReviewSlice {
	t.Helper()
	for _, slice := range slices {
		if slice.Title == title {
			return slice
		}
	}
	t.Fatalf("no slice titled %q", title)
	return store.ReviewSlice{}
}

func referenceSummary(refs []security.ReviewContextReferenceFile) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, ref.Path+"="+ref.Relation+":"+strconv.Itoa(ref.Lines))
	}
	return strings.Join(parts, " ")
}

func filePaths(files []store.ReviewSliceFile) string {
	parts := make([]string, 0, len(files))
	for _, file := range files {
		parts = append(parts, file.Path)
	}
	return strings.Join(parts, ",")
}

func TestMapRepositorySkipsBuildOutputOnlyAtTheRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/target/matcher.go", "package target\n")
	writeFile(t, root, "cmd/build/main.go", "package main\n")
	writeFile(t, root, "target/debug/gen.go", "package gen\n")
	writeFile(t, root, "build/out.go", "package out\n")
	writeFile(t, root, "web/dist/app.js", "var a=1\n")
	writeFile(t, root, "apps/site/.next/chunk.js", "var b=2\n")
	slices, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	owned := map[string]bool{}
	for _, slice := range slices {
		for _, file := range slice.OwnedFiles {
			owned[file.Path] = true
		}
	}
	if !owned["pkg/target/matcher.go"] || !owned["cmd/build/main.go"] {
		t.Fatalf("owned = %v, want nested target and build source reviewed", owned)
	}
	if owned["target/debug/gen.go"] || owned["build/out.go"] {
		t.Fatalf("owned = %v, want root build output skipped", owned)
	}
	if owned["web/dist/app.js"] || owned["apps/site/.next/chunk.js"] {
		t.Fatalf("owned = %v, want generated output skipped at any depth", owned)
	}
}

func TestGoImportGraphParsesOnlyIndexedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module example.com/app\n")
	writeFile(t, root, "a/a.go", "package a\n\nimport _ \"example.com/app/b\"\n")
	writeFile(t, root, "b/b.go", "package b\n")
	files, err := collectFiles(root)
	if err != nil {
		t.Fatalf("collectFiles() error = %v", err)
	}
	lines := referenceLineCounts(root, files)
	if got := newGoImportGraph(root, files, lines).imports("a"); len(got) != 1 || got[0] != "b" {
		t.Fatalf("imports(a) = %v, want [b]", got)
	}
	// A file over the reference size bound has no line count and is not read.
	delete(lines, "a/a.go")
	if got := newGoImportGraph(root, files, lines).imports("a"); len(got) != 0 {
		t.Fatalf("imports(a) = %v, want none for an unindexed file", got)
	}
}

// Large packages are split into bounded parts; no file is left unreviewed.
func TestMapRepositoryCoversEveryFileOfLargePackages(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "backend/package.json", `{"name":"backend"}`)
	want := make([]string, 0, 3*maxReviewSliceOwnedFiles+8)
	for i := range 2*maxReviewSliceOwnedFiles + 5 {
		path := "backend/src/services/s" + strconv.Itoa(100+i) + ".ts"
		writeFile(t, root, path, "export const x = 1\n")
		want = append(want, path)
	}
	for i := range maxReviewSliceOwnedFiles + 3 {
		path := "tools/t" + strconv.Itoa(100+i) + ".sh"
		writeFile(t, root, path, "echo hi\n")
		want = append(want, path)
	}
	slices, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	owned := map[string]struct{}{}
	for _, slice := range slices {
		if len(slice.OwnedFiles) > maxReviewSliceOwnedFiles {
			t.Fatalf("slice %q owns %d files, want at most %d", slice.Title, len(slice.OwnedFiles), maxReviewSliceOwnedFiles)
		}
		for _, file := range slice.OwnedFiles {
			owned[file.Path] = struct{}{}
		}
	}
	for _, path := range want {
		if _, ok := owned[path]; !ok {
			t.Fatalf("%s is not owned by any slice", path)
		}
	}
}

func TestReferenceFilesFollowTypeScriptImports(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "shared/package.json", `{"name":"@app/shared","main":"./types/index.ts"}`)
	writeFile(t, root, "shared/types/index.ts", "export type Value = string\n")
	writeFile(t, root, "shared/api/index.ts", "export const api = 1\n")
	writeFile(t, root, "backend/package.json", `{"name":"@app/backend"}`)
	writeFile(t, root, "backend/src/routes/admin.ts", "import { helm } from '../services/helm'\nimport type { Value } from '@app/shared'\nimport { api } from \"@app/shared/api\"\nconst k = require('../lib/kube.js')\n")
	writeFile(t, root, "backend/src/services/helm.ts", "export const helm = 1\n")
	writeFile(t, root, "backend/src/lib/kube.ts", "export const kube = 1\n")
	writeFile(t, root, "backend/src/services/helm.test.ts", "import { helm } from './helm'\n")
	writeFile(t, root, "backend/src/__tests__/admin.ts", "import { helm } from '../services/helm'\n")
	slices := []store.ReviewSlice{
		{ID: "routes", OwnedFiles: []store.ReviewSliceFile{{Path: "backend/src/routes/admin.ts"}}},
		{ID: "services", OwnedFiles: []store.ReviewSliceFile{{Path: "backend/src/services/helm.ts"}}},
	}
	refs, err := ReferenceFiles(root, slices)
	if err != nil {
		t.Fatalf("ReferenceFiles() error = %v", err)
	}
	routes := referenceSummary(refs["routes"])
	for _, want := range []string{
		"backend/src/services/helm.ts=imports", "shared/types/index.ts=imports",
		"shared/api/index.ts=imports", "backend/src/lib/kube.ts=imports",
	} {
		if !strings.Contains(routes, want) {
			t.Fatalf("routes references = %s, want %s", routes, want)
		}
	}
	services := referenceSummary(refs["services"])
	if !strings.Contains(services, "backend/src/routes/admin.ts=imported-by") || strings.Contains(services, "helm.test.ts") || strings.Contains(services, "__tests__") {
		t.Fatalf("services references = %s, want the route as importer and no test files", services)
	}
}

func TestReferenceFilesFollowTSConfigPathAliases(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "frontend/package.json", `{"name":"@app/frontend"}`)
	writeFile(t, root, "frontend/tsconfig.app.json", `{
  // Vite templates keep comments in tsconfig.
  "compilerOptions": {
    /* Aliases */
    "paths": { "@/*": ["./src/*"], },
  },
}`)
	writeFile(t, root, "frontend/src/pages/Deploy.tsx", "import { api } from '@/lib/api'\nimport { Button } from \"@/components/ui/button\"\n")
	writeFile(t, root, "frontend/src/lib/api.ts", "export const api = 1\n")
	writeFile(t, root, "frontend/src/components/ui/button.tsx", "export const Button = 1\n")
	refs, err := ReferenceFiles(root, []store.ReviewSlice{
		{ID: "pages", OwnedFiles: []store.ReviewSliceFile{{Path: "frontend/src/pages/Deploy.tsx"}}},
	})
	if err != nil {
		t.Fatalf("ReferenceFiles() error = %v", err)
	}
	got := referenceSummary(refs["pages"])
	for _, want := range []string{"frontend/src/lib/api.ts=imports", "frontend/src/components/ui/button.tsx=imports"} {
		if !strings.Contains(got, want) {
			t.Fatalf("references = %s, want %s", got, want)
		}
	}
}

// Node package tests are slice tests, as Go package tests are: they do not
// take excerpt budget from the package's source or form fallback slices.
func TestMapRepositoryKeepsNodeTestsOutOfOwnedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "backend/package.json", `{"name":"backend","scripts":{"test":"bun test"}}`)
	writeFile(t, root, "backend/src/helm.ts", "export const helm = 1\n")
	writeFile(t, root, "backend/src/helm.test.ts", "import { helm } from './helm'\n")
	writeFile(t, root, "backend/src/__tests__/fixtures.ts", "export const f = 1\n")
	slices, err := MapRepository(root, MapperOptions{RepositoryScan: "repo"})
	if err != nil {
		t.Fatalf("MapRepository() error = %v", err)
	}
	var packageSlice *store.ReviewSlice
	for i := range slices {
		for _, file := range slices[i].OwnedFiles {
			if strings.Contains(file.Path, ".test.") || strings.Contains(file.Path, "__tests__") {
				t.Fatalf("slice %q owns test file %s", slices[i].Title, file.Path)
			}
		}
		if slices[i].Title == "Node package backend" {
			packageSlice = &slices[i]
		}
	}
	if packageSlice == nil || !strings.Contains(filePaths(packageSlice.OwnedFiles), "backend/src/helm.ts") {
		t.Fatalf("package slice = %#v, want it to own the source", packageSlice)
	}
	tests := map[string]string{}
	for _, test := range packageSlice.Tests {
		tests[test.Path] = test.Command
	}
	if tests["backend/src/helm.test.ts"] != "cd backend && npm run test" {
		t.Fatalf("tests = %v, want helm.test.ts with the package test command", tests)
	}
}

func TestReferenceFilesFollowPackageExports(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/package.json", `{"name":"@app/lib","exports":{
  ".": {"types": "./src/index.ts", "import": "./src/index.ts"},
  "./client": "./src/client/main.ts",
  "./utils/*": "./src/utils/*.ts"
}}`)
	writeFile(t, root, "lib/src/index.ts", "export const lib = 1\n")
	writeFile(t, root, "lib/src/client/main.ts", "export const client = 1\n")
	writeFile(t, root, "lib/src/utils/token.ts", "export const token = 1\n")
	writeFile(t, root, "app/package.json", `{"name":"@app/app"}`)
	writeFile(t, root, "app/server.ts", "import { lib } from '@app/lib'\nimport { client } from '@app/lib/client'\nimport { token } from '@app/lib/utils/token'\n")
	refs, err := ReferenceFiles(root, []store.ReviewSlice{{ID: "app", OwnedFiles: []store.ReviewSliceFile{{Path: "app/server.ts"}}}})
	if err != nil {
		t.Fatalf("ReferenceFiles() error = %v", err)
	}
	got := referenceSummary(refs["app"])
	for _, want := range []string{"lib/src/index.ts=imports", "lib/src/client/main.ts=imports", "lib/src/utils/token.ts=imports"} {
		if !strings.Contains(got, want) {
			t.Fatalf("references = %s, want %s", got, want)
		}
	}
}

func TestReferenceFilesFollowRequireExportConditions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/package.json", `{"name":"@app/lib","exports":{".":{"import":"./src/esm.ts","require":"./src/cjs.ts"}}}`)
	writeFile(t, root, "lib/src/esm.ts", "export const esm = 1\n")
	writeFile(t, root, "lib/src/cjs.ts", "module.exports = 1\n")
	writeFile(t, root, "app/package.json", `{"name":"@app/app"}`)
	writeFile(t, root, "app/modern.ts", "import { esm } from '@app/lib'\n")
	writeFile(t, root, "app/legacy.js", "const lib = require('@app/lib')\n")
	refs, err := ReferenceFiles(root, []store.ReviewSlice{
		{ID: "modern", OwnedFiles: []store.ReviewSliceFile{{Path: "app/modern.ts"}}},
		{ID: "legacy", OwnedFiles: []store.ReviewSliceFile{{Path: "app/legacy.js"}}},
	})
	if err != nil {
		t.Fatalf("ReferenceFiles() error = %v", err)
	}
	if got := referenceSummary(refs["modern"]); !strings.Contains(got, "lib/src/esm.ts=imports") || strings.Contains(got, "cjs.ts") {
		t.Fatalf("modern references = %s, want the import target", got)
	}
	if got := referenceSummary(refs["legacy"]); !strings.Contains(got, "lib/src/cjs.ts=imports") || strings.Contains(got, "esm.ts") {
		t.Fatalf("legacy references = %s, want the require target", got)
	}
}
