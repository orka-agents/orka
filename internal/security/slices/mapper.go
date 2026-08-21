package slices

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

type MapperOptions struct {
	RepositoryScan string
	SubPath        string
}

const (
	extGo               = ".go"
	extTS               = ".ts"
	extTSX              = ".tsx"
	extJS               = ".js"
	extJSX              = ".jsx"
	extMJS              = ".mjs"
	extCJS              = ".cjs"
	extPython           = ".py"
	extShell            = ".sh"
	extBash             = ".bash"
	extZsh              = ".zsh"
	extYAML             = ".yaml"
	extYML              = ".yml"
	packageJSONFile     = "package.json"
	pyprojectFile       = "pyproject.toml"
	requirementsTxtFile = "requirements.txt"

	maxReviewSliceEntrypoints = 4
	maxReviewSliceOwnedFiles  = 24
	maxReviewSliceContext     = 24
	maxReviewSliceTests       = 24

	mapperReasonReviewable          = "supported-reviewable-file"
	mapperReasonAssigned            = "assigned-to-review-slice"
	mapperReasonUnassigned          = "no-deterministic-review-slice"
	mapperReasonSymlink             = "symlink"
	mapperReasonSecretLike          = "secret-like-path"
	mapperReasonLockfile            = "lockfile"
	mapperReasonUnsupported         = "unsupported-type"
	mapperReasonVCSDirectory        = "vcs-directory"
	mapperReasonDependencyDirectory = "dependency-directory"
	mapperReasonGeneratedDirectory  = "generated-directory"
	mapperReasonCacheDirectory      = "cache-directory"
	mapperReasonVirtualEnvDirectory = "virtualenv-directory"
	mapperReasonEntrypointCap       = "entrypoint-reference-cap"
	mapperReasonContextCap          = "context-reference-cap"
	mapperReasonTestCap             = "test-reference-cap"
)

var (
	mapperInventoryEntryLimit        = security.MaxMapperInventoryEntries
	mapperOmittedInventoryEntryLimit = security.MaxMapperOmittedInventoryEntries
)

// MapperResult contains deterministic review slices and their coverage inventory.
type MapperResult struct {
	Slices           []store.ReviewSlice
	InventorySummary security.MapperInventorySummary
	DiscoveredFiles  []security.MapperFileInventoryEntry
	ReviewableFiles  []security.MapperFileInventoryEntry
	OmittedFiles     []security.MapperFileInventoryEntry
}

// MapRepository returns deterministic review slices and coverage inventory for
// the supplied checkout root. The caller supplies the configured scope root;
// paths outside that root are therefore never eligible for collection.
func MapRepository(root string, opts MapperOptions) (MapperResult, error) {
	collection, err := collectFiles(root)
	if err != nil {
		return MapperResult{}, err
	}

	builder := newSliceBuilder(opts.RepositoryScan)
	addGoSlices(builder, collection.reviewable)
	addNodeSlices(builder, root, collection.reviewable)
	addPythonSlices(builder, collection.reviewable)
	addWorkflowAndScriptSlices(builder, collection.reviewable)
	addConfigSlices(builder, collection.reviewable)
	addGenericFallbackSlices(builder, collection.reviewable)

	slices := builder.slices()
	reviewableFiles, unassigned := reviewableInventory(collection.reviewable, slices)
	omittedFiles, derivedTruncated, err := boundedOmittedInventory(
		collection.omitted,
		unassigned,
		builder.referenceOmissions,
	)
	if err != nil {
		return MapperResult{}, err
	}

	return MapperResult{
		Slices:           slices,
		InventorySummary: collection.inventorySummary(len(omittedFiles), derivedTruncated),
		DiscoveredFiles:  normalizeInventory(collection.discovered),
		ReviewableFiles:  reviewableFiles,
		OmittedFiles:     omittedFiles,
	}, nil
}

type repoFile struct {
	Path string
	Dir  string
	Name string
	Ext  string
}

type fileCollection struct {
	reviewable []repoFile
	discovered []security.MapperFileInventoryEntry
	omitted    []security.MapperFileInventoryEntry
	total      int
	limit      int
}

func collectFiles(root string) (fileCollection, error) {
	root = filepath.Clean(root)
	collection := fileCollection{
		reviewable: []repoFile{},
		discovered: []security.MapperFileInventoryEntry{},
		omitted:    []security.MapperFileInventoryEntry{},
		limit:      effectiveMapperInventoryEntryLimit(),
	}
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filePath == root {
			return nil
		}
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if reason := skippedDirectoryReason(rel, entry); reason != "" {
				collection.addExcluded(rel, reason)
				return filepath.SkipDir
			}
			return nil
		}
		if reason := excludedFileReason(rel, entry); reason != "" {
			collection.addExcluded(rel, reason)
			return nil
		}
		collection.addReviewable(repoFile{
			Path: rel,
			Dir:  path.Dir(rel),
			Name: path.Base(rel),
			Ext:  strings.ToLower(path.Ext(rel)),
		})
		return nil
	})
	if err != nil {
		return fileCollection{}, err
	}
	sort.Slice(collection.reviewable, func(i, j int) bool { return collection.reviewable[i].Path < collection.reviewable[j].Path })
	return collection, nil
}

func effectiveMapperInventoryEntryLimit() int {
	if mapperInventoryEntryLimit <= 0 || mapperInventoryEntryLimit > security.MaxMapperInventoryEntries {
		return security.MaxMapperInventoryEntries
	}
	return mapperInventoryEntryLimit
}

func effectiveMapperOmittedInventoryEntryLimit() int {
	if mapperOmittedInventoryEntryLimit <= 0 ||
		mapperOmittedInventoryEntryLimit > security.MaxMapperOmittedInventoryEntries {
		return security.MaxMapperOmittedInventoryEntries
	}
	return mapperOmittedInventoryEntryLimit
}

func (c *fileCollection) retainInventoryEntry() bool {
	c.total++
	return len(c.discovered) < c.limit
}

func (c *fileCollection) addExcluded(filePath, reason string) {
	if !c.retainInventoryEntry() {
		return
	}
	excluded := inventoryEntry(filePath, security.MapperDispositionExcluded, reason)
	c.discovered = append(c.discovered, excluded)
	c.omitted = append(c.omitted, excluded)
}

func (c *fileCollection) addReviewable(file repoFile) {
	if !c.retainInventoryEntry() {
		return
	}
	c.reviewable = append(c.reviewable, file)
	c.discovered = append(c.discovered,
		inventoryEntry(file.Path, security.MapperDispositionReviewable, mapperReasonReviewable))
}

func (c fileCollection) inventorySummary(
	retainedOmissionRecords, truncatedOmissionRecords int,
) security.MapperInventorySummary {
	retainedPaths := len(c.discovered)
	truncatedPaths := c.total - retainedPaths
	truncatedOmissionRecords = max(truncatedOmissionRecords, 0)
	totalOmissionRecords := retainedOmissionRecords + truncatedOmissionRecords
	summary := security.MapperInventorySummary{
		EntryLimit:       c.limit,
		TotalEntries:     c.total,
		RetainedEntries:  retainedPaths,
		TruncatedEntries: truncatedPaths,
		Truncated:        truncatedPaths > 0 || truncatedOmissionRecords > 0,
	}
	if totalOmissionRecords > 0 {
		summary.OmissionRecords = &security.MapperOmissionRecordSummary{
			EntryLimit:       effectiveMapperOmittedInventoryEntryLimit(),
			TotalRecords:     totalOmissionRecords,
			RetainedRecords:  retainedOmissionRecords,
			TruncatedRecords: truncatedOmissionRecords,
			Truncated:        truncatedOmissionRecords > 0,
		}
	}
	if summary.Truncated {
		summary.Reason = security.MapperCoverageReasonInventoryEntryLimit
	}
	return summary
}

func skippedDirectoryReason(rel string, entry fs.DirEntry) string {
	if entry.Type()&os.ModeSymlink != 0 {
		return mapperReasonSymlink
	}
	name := path.Base(rel)
	switch name {
	case ".git", ".hg", ".svn":
		return mapperReasonVCSDirectory
	case "node_modules", "vendor":
		return mapperReasonDependencyDirectory
	case "dist", "build", "coverage", ".next", "target", "bin", ".orka-artifacts":
		return mapperReasonGeneratedDirectory
	case ".cache", ".turbo", "__pycache__", ".pytest_cache":
		return mapperReasonCacheDirectory
	case ".venv", "venv":
		return mapperReasonVirtualEnvDirectory
	default:
		return ""
	}
}

func excludedFileReason(rel string, entry fs.DirEntry) string {
	switch {
	case entry.Type()&os.ModeSymlink != 0:
		return mapperReasonSymlink
	case likelySecretPath(rel):
		return mapperReasonSecretLike
	case isLockfile(rel):
		return mapperReasonLockfile
	case !isReviewableFile(rel):
		return mapperReasonUnsupported
	default:
		return ""
	}
}

func isLockfile(rel string) bool {
	switch path.Base(rel) {
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock":
		return true
	default:
		return false
	}
}

func likelySecretPath(rel string) bool {
	name := strings.ToLower(path.Base(rel))
	for segment := range strings.SplitSeq(path.Dir(rel), "/") {
		if likelySecretDirName(segment) && !isSourceCodePath(rel) {
			return true
		}
	}
	if strings.HasPrefix(name, ".env") {
		return true
	}
	switch {
	case strings.HasSuffix(name, ".pem"), strings.HasSuffix(name, ".key"), strings.HasSuffix(name, ".p12"),
		strings.HasSuffix(name, ".pfx"), strings.HasSuffix(name, "id_rsa"), strings.HasSuffix(name, "id_ed25519"):
		return true
	case !isSourceCodePath(rel) && (strings.Contains(name, "secret") ||
		strings.Contains(name, "token") ||
		strings.Contains(name, "credential")):
		return true
	default:
		return false
	}
}

func likelySecretDirName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ".env", ".secret", ".secrets", "secret", "secrets", "credential", "credentials", "token", "tokens":
		return true
	default:
		return false
	}
}

func isSourceCodePath(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case extGo, extTS, extTSX, extJS, extJSX, extMJS, extCJS, extPython, extShell, extBash, extZsh:
		return true
	default:
		return false
	}
}

func isReviewableFile(rel string) bool {
	name := path.Base(rel)
	if isLockfile(rel) {
		return false
	}
	switch strings.ToLower(path.Ext(rel)) {
	case extGo, extTS, extTSX, extJS, extJSX, extMJS, extCJS, extPython, extShell, extBash, extZsh,
		extYAML, extYML, ".json", ".toml", ".cfg":
		return true
	}
	switch name {
	case "Dockerfile", "Containerfile", "Makefile", "Taskfile.yml", "Taskfile.yaml", "go.mod", "go.sum",
		packageJSONFile, pyprojectFile, "setup.py", "setup.cfg", requirementsTxtFile:
		return true
	default:
		return false
	}
}

type sliceBuilder struct {
	repositoryScan     string
	byID               map[string]store.ReviewSlice
	claimed            map[string]struct{}
	referenceOmissions []security.MapperFileInventoryEntry
}

func newSliceBuilder(repositoryScan string) *sliceBuilder {
	return &sliceBuilder{
		repositoryScan: repositoryScan,
		byID:           map[string]store.ReviewSlice{},
		claimed:        map[string]struct{}{},
	}
}

func (b *sliceBuilder) add(slice store.ReviewSlice) {
	if slice.SchemaVersion == 0 {
		slice.SchemaVersion = 1
	}
	if slice.RepositoryScan == "" {
		slice.RepositoryScan = b.repositoryScan
	}
	if slice.Kind == "" {
		slice.Kind = "unknown"
	}
	if slice.Confidence == "" {
		slice.Confidence = "medium"
	}
	if slice.Status == "" {
		slice.Status = "pending"
	}
	normalizeSliceFiles(&slice)
	boundedSlices, omitted := boundedReviewSlices(slice)
	b.referenceOmissions = append(b.referenceOmissions, omitted...)
	for _, bounded := range boundedSlices {
		if bounded.ID == "" {
			bounded.ID = sliceID(b.repositoryScan, bounded.Source, bounded.Title)
		}
		b.byID[bounded.ID] = bounded
		for _, file := range bounded.OwnedFiles {
			b.claimed[file.Path] = struct{}{}
		}
	}
}

func (b *sliceBuilder) slices() []store.ReviewSlice {
	out := make([]store.ReviewSlice, 0, len(b.byID))
	for _, slice := range b.byID {
		out = append(out, slice)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func normalizeSliceFiles(slice *store.ReviewSlice) {
	sort.Slice(slice.Entrypoints, func(i, j int) bool { return slice.Entrypoints[i].Path < slice.Entrypoints[j].Path })
	sort.Slice(slice.OwnedFiles, func(i, j int) bool { return slice.OwnedFiles[i].Path < slice.OwnedFiles[j].Path })
	sort.Slice(slice.ContextFiles, func(i, j int) bool { return slice.ContextFiles[i].Path < slice.ContextFiles[j].Path })
	sort.Slice(slice.Tests, func(i, j int) bool { return slice.Tests[i].Path < slice.Tests[j].Path })
	sort.Strings(slice.Tags)
	sort.Strings(slice.TrustBoundaries)
}

func boundedReviewSlices(slice store.ReviewSlice) ([]store.ReviewSlice, []security.MapperFileInventoryEntry) {
	omitted := make([]security.MapperFileInventoryEntry, 0, len(slice.Entrypoints)+len(slice.ContextFiles)+len(slice.Tests))
	var droppedFiles []store.ReviewSliceFile
	var droppedTests []store.ReviewSliceTest
	slice.Entrypoints, droppedFiles = capFileRefs(slice.Entrypoints, maxReviewSliceEntrypoints)
	omitted = appendReferenceOmissions(omitted, droppedFiles, mapperReasonEntrypointCap)
	slice.ContextFiles, droppedFiles = capFileRefs(slice.ContextFiles, maxReviewSliceContext)
	omitted = appendReferenceOmissions(omitted, droppedFiles, mapperReasonContextCap)
	slice.Tests, droppedTests = capTestRefs(slice.Tests, maxReviewSliceTests)
	for _, test := range droppedTests {
		if test.Path == "" {
			continue
		}
		omitted = append(omitted, inventoryEntry(test.Path, security.MapperDispositionOmitted, mapperReasonTestCap))
	}
	if len(slice.OwnedFiles) <= maxReviewSliceOwnedFiles {
		return []store.ReviewSlice{slice}, omitted
	}

	total := (len(slice.OwnedFiles) + maxReviewSliceOwnedFiles - 1) / maxReviewSliceOwnedFiles
	out := make([]store.ReviewSlice, 0, total)
	baseID := slice.ID
	for part, start := 0, 0; start < len(slice.OwnedFiles); part, start = part+1, start+maxReviewSliceOwnedFiles {
		end := min(start+maxReviewSliceOwnedFiles, len(slice.OwnedFiles))
		bounded := slice
		bounded.OwnedFiles = append([]store.ReviewSliceFile(nil), slice.OwnedFiles[start:end]...)
		bounded.Entrypoints = entrypointsForOwnedFiles(slice.Entrypoints, bounded.OwnedFiles)
		if len(bounded.Entrypoints) == 0 {
			bounded.Entrypoints = []store.ReviewSliceFile{bounded.OwnedFiles[0]}
		}
		bounded.Title = fmt.Sprintf("%s (part %d/%d)", slice.Title, part+1, total)
		if baseID != "" {
			bounded.ID = fmt.Sprintf("%s_part_%03d", baseID, part+1)
		}
		out = append(out, bounded)
	}
	return out, omitted
}

func appendReferenceOmissions(
	omitted []security.MapperFileInventoryEntry,
	files []store.ReviewSliceFile,
	reason string,
) []security.MapperFileInventoryEntry {
	for _, file := range files {
		if file.Path == "" {
			continue
		}
		omitted = append(omitted, inventoryEntry(file.Path, security.MapperDispositionOmitted, reason))
	}
	return omitted
}

func entrypointsForOwnedFiles(entrypoints, owned []store.ReviewSliceFile) []store.ReviewSliceFile {
	ownedPaths := make(map[string]struct{}, len(owned))
	for _, file := range owned {
		ownedPaths[file.Path] = struct{}{}
	}
	out := make([]store.ReviewSliceFile, 0, len(entrypoints))
	for _, file := range entrypoints {
		if _, ok := ownedPaths[file.Path]; !ok {
			continue
		}
		out = append(out, file)
		if len(out) == maxReviewSliceEntrypoints {
			return out
		}
	}
	return out
}

func capFileRefs(files []store.ReviewSliceFile, max int) ([]store.ReviewSliceFile, []store.ReviewSliceFile) {
	if max <= 0 || len(files) <= max {
		return files, nil
	}
	return append([]store.ReviewSliceFile(nil), files[:max]...), append([]store.ReviewSliceFile(nil), files[max:]...)
}

func capTestRefs(tests []store.ReviewSliceTest, max int) ([]store.ReviewSliceTest, []store.ReviewSliceTest) {
	if max <= 0 || len(tests) <= max {
		return tests, nil
	}
	return append([]store.ReviewSliceTest(nil), tests[:max]...), append([]store.ReviewSliceTest(nil), tests[max:]...)
}

func inventoryEntry(filePath, disposition, reason string) security.MapperFileInventoryEntry {
	return security.MapperFileInventoryEntry{Path: filePath, Disposition: disposition, Reason: reason}
}

func normalizeInventory(entries []security.MapperFileInventoryEntry) []security.MapperFileInventoryEntry {
	if entries == nil {
		return []security.MapperFileInventoryEntry{}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		if entries[i].Disposition != entries[j].Disposition {
			return entries[i].Disposition < entries[j].Disposition
		}
		return entries[i].Reason < entries[j].Reason
	})
	out := make([]security.MapperFileInventoryEntry, 0, len(entries))
	for _, entry := range entries {
		if len(out) > 0 && out[len(out)-1] == entry {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func boundedOmittedInventory(
	collected, unassigned, references []security.MapperFileInventoryEntry,
) ([]security.MapperFileInventoryEntry, int, error) {
	limit := effectiveMapperOmittedInventoryEntryLimit()
	required := make([]security.MapperFileInventoryEntry, 0, len(collected)+len(unassigned))
	required = append(required, collected...)
	required = append(required, unassigned...)
	required = normalizeInventory(required)
	if len(required) > limit {
		return nil, 0, fmt.Errorf(
			"required mapper omissions exceed %d entries",
			security.MaxMapperOmittedInventoryEntries,
		)
	}

	seen := make(map[security.MapperFileInventoryEntry]struct{}, len(required))
	for _, entry := range required {
		seen[entry] = struct{}{}
	}
	optional := normalizeInventory(append([]security.MapperFileInventoryEntry(nil), references...))
	retainedOptional := optional[:0]
	for _, entry := range optional {
		if _, ok := seen[entry]; ok {
			continue
		}
		retainedOptional = append(retainedOptional, entry)
	}

	remaining := limit - len(required)
	truncated := max(len(retainedOptional)-remaining, 0)
	retainedOptional = retainedOptional[:min(len(retainedOptional), remaining)]
	bounded := append(required, retainedOptional...)
	return normalizeInventory(bounded), truncated, nil
}

func reviewableInventory(
	files []repoFile,
	slices []store.ReviewSlice,
) ([]security.MapperFileInventoryEntry, []security.MapperFileInventoryEntry) {
	assigned := referencedSlicePaths(slices)
	reviewable := make([]security.MapperFileInventoryEntry, 0, len(files))
	omitted := []security.MapperFileInventoryEntry{}
	for _, file := range files {
		if _, ok := assigned[file.Path]; ok {
			reviewable = append(reviewable, inventoryEntry(file.Path, security.MapperDispositionAssigned, mapperReasonAssigned))
			continue
		}
		entry := inventoryEntry(file.Path, security.MapperDispositionOmitted, mapperReasonUnassigned)
		reviewable = append(reviewable, entry)
		omitted = append(omitted, entry)
	}
	return normalizeInventory(reviewable), normalizeInventory(omitted)
}

func referencedSlicePaths(slices []store.ReviewSlice) map[string]struct{} {
	paths := map[string]struct{}{}
	for _, slice := range slices {
		for _, file := range slice.Entrypoints {
			paths[file.Path] = struct{}{}
		}
		for _, file := range slice.OwnedFiles {
			paths[file.Path] = struct{}{}
		}
		for _, file := range slice.ContextFiles {
			paths[file.Path] = struct{}{}
		}
		for _, test := range slice.Tests {
			if test.Path != "" {
				paths[test.Path] = struct{}{}
			}
		}
	}
	return paths
}

func sliceID(repositoryScan, source, title string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{repositoryScan, source, title}, "|")))
	return "slice_" + hex.EncodeToString(sum[:])[:16]
}

func fileRef(file repoFile, reason string) store.ReviewSliceFile {
	return store.ReviewSliceFile{Path: file.Path, Reason: reason}
}

func testRef(file repoFile, command string) store.ReviewSliceTest {
	return store.ReviewSliceTest{Path: file.Path, Command: command}
}

func addGoSlices(builder *sliceBuilder, files []repoFile) {
	byDir := map[string][]repoFile{}
	for _, file := range files {
		if file.Ext != extGo {
			continue
		}
		byDir[file.Dir] = append(byDir[file.Dir], file)
	}
	for dir, dirFiles := range byDir {
		owned := []store.ReviewSliceFile{}
		contextFiles := []store.ReviewSliceFile{}
		tests := []store.ReviewSliceTest{}
		for _, file := range dirFiles {
			if strings.HasSuffix(file.Name, "_test.go") {
				contextFiles = append(contextFiles, fileRef(file, "package test"))
				tests = append(tests, testRef(file, "go test ./"+dir))
			} else {
				owned = append(owned, fileRef(file, "primary package source"))
			}
		}
		if len(owned) == 0 {
			continue
		}
		titleDir := dir
		if titleDir == "." {
			titleDir = "root"
		}
		builder.add(store.ReviewSlice{
			Source:          "deterministic-go-package",
			Title:           "Go package " + titleDir,
			Summary:         "Go package source and tests.",
			Kind:            "package",
			Entrypoints:     []store.ReviewSliceFile{owned[0]},
			OwnedFiles:      owned,
			ContextFiles:    contextFiles,
			Tests:           dedupeTests(tests),
			Tags:            []string{"language:go", "project-root:."},
			TrustBoundaries: goTrustBoundaries(dirFiles),
			Confidence:      "high",
		})
	}
}

func goTrustBoundaries(files []repoFile) []string {
	boundaries := map[string]struct{}{}
	for _, file := range files {
		switch {
		case strings.Contains(file.Path, "api"), strings.Contains(file.Path, "http"), strings.Contains(file.Path, "controller"):
			boundaries["network"] = struct{}{}
		case strings.Contains(file.Path, "store"), strings.Contains(file.Path, "db"):
			boundaries["persistence"] = struct{}{}
		}
	}
	out := make([]string, 0, len(boundaries))
	for boundary := range boundaries {
		out = append(out, boundary)
	}
	return out
}

func dedupeTests(tests []store.ReviewSliceTest) []store.ReviewSliceTest {
	seen := map[string]struct{}{}
	out := []store.ReviewSliceTest{}
	for _, test := range tests {
		key := test.Path + "|" + test.Command
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, test)
	}
	return out
}

func addNodeSlices(builder *sliceBuilder, root string, files []repoFile) {
	for _, file := range files {
		if file.Name != packageJSONFile {
			continue
		}
		scripts := packageScripts(filepath.Join(root, filepath.FromSlash(file.Path)))
		tests := []store.ReviewSliceTest{}
		for _, name := range []string{"test", "lint", "typecheck", "build"} {
			if _, ok := scripts[name]; ok {
				cmd := "npm run " + name
				if file.Dir != "." {
					cmd = fmt.Sprintf("cd %s && npm run %s", file.Dir, name)
				}
				tests = append(tests, store.ReviewSliceTest{Path: file.Path, Command: cmd})
			}
		}
		owned := []store.ReviewSliceFile{fileRef(file, "package manifest")}
		for _, candidate := range nodeFilesForDir(file.Dir, files) {
			owned = append(owned, fileRef(candidate, "node/typescript source"))
		}
		titleDir := file.Dir
		if titleDir == "." {
			titleDir = "root"
		}
		builder.add(store.ReviewSlice{
			Source:          "deterministic-node-package",
			Title:           "Node package " + titleDir,
			Summary:         "Node or TypeScript package manifest, scripts, and nearby source.",
			Kind:            nodeKind(file.Dir),
			Entrypoints:     []store.ReviewSliceFile{fileRef(file, "package manifest")},
			OwnedFiles:      owned,
			Tests:           tests,
			Tags:            []string{"language:typescript", "language:javascript"},
			TrustBoundaries: []string{"process-execution"},
			Confidence:      "medium",
		})
	}
	addNodeRouteSlices(builder, files)
}

func packageScripts(filePath string) map[string]string {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	var payload struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	return payload.Scripts
}

func nodeFilesForDir(dir string, files []repoFile) []repoFile {
	out := []repoFile{}
	for _, file := range files {
		if file.Ext != extTS && file.Ext != extTSX && file.Ext != extJS && file.Ext != extJSX &&
			file.Ext != extMJS && file.Ext != extCJS {
			continue
		}
		if dir == "." {
			if topLevel(file.Path) == "src" || topLevel(file.Path) == "app" || topLevel(file.Path) == "pages" || topLevel(file.Path) == "server" {
				out = append(out, file)
			}
			continue
		}
		if strings.HasPrefix(file.Path, dir+"/") {
			out = append(out, file)
		}
	}
	return out
}

func nodeKind(dir string) string {
	switch topLevel(dir) {
	case "apps", "services":
		return "service"
	default:
		return "library"
	}
}

func addNodeRouteSlices(builder *sliceBuilder, files []repoFile) {
	for _, root := range []string{"app", "pages"} {
		owned := []store.ReviewSliceFile{}
		for _, file := range files {
			if (file.Ext == extTS || file.Ext == extTSX || file.Ext == extJS || file.Ext == extJSX) &&
				(topLevel(file.Path) == root || strings.Contains(file.Path, "/"+root+"/")) {
				owned = append(owned, fileRef(file, "route source"))
			}
		}
		if len(owned) == 0 {
			continue
		}
		builder.add(store.ReviewSlice{
			Source:          "deterministic-node-routes",
			Title:           "Node routes " + root,
			Summary:         "Frontend or API routes with request handling behavior.",
			Kind:            "route",
			Entrypoints:     []store.ReviewSliceFile{owned[0]},
			OwnedFiles:      owned,
			Tags:            []string{"language:typescript", "route"},
			TrustBoundaries: []string{"network"},
			Confidence:      "medium",
		})
	}
}

func addPythonSlices(builder *sliceBuilder, files []repoFile) {
	hasPythonProject := false
	for _, file := range files {
		switch file.Name {
		case pyprojectFile, "setup.cfg", "setup.py", "requirements.txt":
			hasPythonProject = true
		}
	}
	if !hasPythonProject {
		return
	}
	byDir := map[string][]repoFile{}
	for _, file := range files {
		if file.Ext == ".py" {
			byDir[file.Dir] = append(byDir[file.Dir], file)
		}
	}
	for dir, dirFiles := range byDir {
		owned := []store.ReviewSliceFile{}
		contextFiles := []store.ReviewSliceFile{}
		tests := []store.ReviewSliceTest{}
		for _, file := range dirFiles {
			if strings.HasPrefix(file.Name, "test_") || strings.HasSuffix(file.Name, "_test.py") {
				contextFiles = append(contextFiles, fileRef(file, "python test"))
				tests = append(tests, testRef(file, "pytest "+dir))
			} else {
				owned = append(owned, fileRef(file, "python source"))
			}
		}
		if len(owned) == 0 {
			continue
		}
		builder.add(store.ReviewSlice{
			Source:          "deterministic-python-package",
			Title:           "Python module " + dir,
			Summary:         "Python source and tests.",
			Kind:            "service",
			Entrypoints:     []store.ReviewSliceFile{owned[0]},
			OwnedFiles:      owned,
			ContextFiles:    contextFiles,
			Tests:           dedupeTests(tests),
			Tags:            []string{"language:python"},
			TrustBoundaries: []string{"network"},
			Confidence:      "medium",
		})
	}
}

func addWorkflowAndScriptSlices(builder *sliceBuilder, files []repoFile) {
	workflows := []store.ReviewSliceFile{}
	scripts := []store.ReviewSliceFile{}
	for _, file := range files {
		if strings.HasPrefix(file.Path, ".github/workflows/") && (file.Ext == extYML || file.Ext == extYAML) {
			workflows = append(workflows, fileRef(file, "github workflow"))
		}
		if strings.HasPrefix(file.Path, "scripts/") &&
			(file.Ext == extShell || file.Ext == extBash || file.Ext == extZsh) {
			scripts = append(scripts, fileRef(file, "script source"))
		}
	}
	if len(workflows) > 0 {
		builder.add(store.ReviewSlice{
			Source:          "deterministic-workflows",
			Title:           "GitHub Actions workflows",
			Summary:         "CI/CD workflows and shell run blocks.",
			Kind:            "workflow",
			Entrypoints:     []store.ReviewSliceFile{workflows[0]},
			OwnedFiles:      workflows,
			Tags:            []string{"workflow", "ci-cd"},
			TrustBoundaries: []string{"process-execution", "supply-chain"},
			Confidence:      "high",
		})
	}
	if len(scripts) > 0 {
		builder.add(store.ReviewSlice{
			Source:          "deterministic-scripts",
			Title:           "Repository scripts",
			Summary:         "Shell scripts and process execution surfaces.",
			Kind:            "service",
			Entrypoints:     []store.ReviewSliceFile{scripts[0]},
			OwnedFiles:      scripts,
			Tags:            []string{"shell"},
			TrustBoundaries: []string{"process-execution", "filesystem"},
			Confidence:      "medium",
		})
	}
}

func addConfigSlices(builder *sliceBuilder, files []repoFile) {
	owned := []store.ReviewSliceFile{}
	for _, file := range files {
		switch file.Name {
		case "Dockerfile", "Containerfile", "Makefile", "go.mod", packageJSONFile, pyprojectFile, requirementsTxtFile:
			owned = append(owned, fileRef(file, "security-relevant config"))
		}
	}
	if len(owned) == 0 {
		return
	}
	builder.add(store.ReviewSlice{
		Source:          "deterministic-config",
		Title:           "Build and dependency configuration",
		Summary:         "Build, dependency, and packaging configuration files.",
		Kind:            "config",
		Entrypoints:     []store.ReviewSliceFile{owned[0]},
		OwnedFiles:      owned,
		Tags:            []string{"config", "supply-chain"},
		TrustBoundaries: []string{"supply-chain"},
		Confidence:      "medium",
	})
}

func addGenericFallbackSlices(builder *sliceBuilder, files []repoFile) {
	byTop := map[string][]repoFile{}
	for _, file := range files {
		if _, claimed := builder.claimed[file.Path]; claimed {
			continue
		}
		if !genericSourceExtension(file.Ext) {
			continue
		}
		byTop[topLevel(file.Path)] = append(byTop[topLevel(file.Path)], file)
	}
	for top, topFiles := range byTop {
		if len(topFiles) == 0 {
			continue
		}
		owned := make([]store.ReviewSliceFile, 0, len(topFiles))
		for _, file := range topFiles {
			owned = append(owned, fileRef(file, "generic source"))
		}
		builder.add(store.ReviewSlice{
			Source:      "deterministic-generic-directory",
			Title:       "Source group " + top,
			Summary:     "Fallback bounded source group for reviewable files not covered by language-specific mappers.",
			Kind:        "unknown",
			Entrypoints: []store.ReviewSliceFile{owned[0]},
			OwnedFiles:  owned,
			Tags:        []string{"fallback"},
			Confidence:  "low",
		})
	}
}

func genericSourceExtension(ext string) bool {
	switch ext {
	case extGo, extTS, extTSX, extJS, extJSX, extPython, extShell, extYAML, extYML:
		return true
	default:
		return false
	}
}

func topLevel(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" || p == "." {
		return "."
	}
	parts := strings.Split(p, "/")
	return parts[0]
}
