package slices

import (
	"bufio"
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const (
	maxSliceReferenceFiles   = 200
	maxReferenceFileBytes    = 4 << 20
	maxGoModuleFileBytes     = 1 << 20
	referenceLineBufferBytes = 64 << 10
)

func ReferenceFiles(root string, reviewSlices []store.ReviewSlice) (map[string][]security.ReviewContextReferenceFile, error) {
	files, err := collectFiles(root)
	if err != nil {
		return nil, err
	}
	lines := referenceLineCounts(root, files)
	graph := newGoImportGraph(root, files, lines)
	nodeGraph := newNodeImportGraph(root, files, lines)
	repository := repositoryReferenceFiles(files, lines)

	out := make(map[string][]security.ReviewContextReferenceFile, len(reviewSlices))
	for _, slice := range reviewSlices {
		if slice.Kind == security.SliceKindInvestigation || slice.Kind == security.SliceKindBaseline {
			out[slice.ID] = repository
			continue
		}
		refs := newReferenceList(lines)
		sliceDirs := map[string]struct{}{}
		for _, file := range slice.OwnedFiles {
			refs.add(file.Path, security.ReferenceRelationSlice)
			if path.Ext(file.Path) == extGo {
				sliceDirs[path.Dir(file.Path)] = struct{}{}
			}
		}
		for _, file := range slice.ContextFiles {
			refs.add(file.Path, security.ReferenceRelationSlice)
		}
		for _, test := range slice.Tests {
			refs.add(test.Path, security.ReferenceRelationSlice)
		}
		for _, dir := range sortedKeys(sliceDirs) {
			for _, imported := range graph.imports(dir) {
				refs.addDir(graph, imported, security.ReferenceRelationImports)
			}
		}
		for _, dir := range sortedKeys(sliceDirs) {
			for _, importer := range graph.importers(dir) {
				refs.addDir(graph, importer, security.ReferenceRelationImportedBy)
			}
		}
		for _, file := range slice.OwnedFiles {
			for _, imported := range nodeGraph.imports(file.Path) {
				refs.add(imported, security.ReferenceRelationImports)
			}
		}
		for _, file := range slice.OwnedFiles {
			for _, importer := range nodeGraph.importers(file.Path) {
				refs.add(importer, security.ReferenceRelationImportedBy)
			}
		}
		out[slice.ID] = refs.files
	}
	return out, nil
}

type referenceList struct {
	lines map[string]int
	seen  map[string]struct{}
	files []security.ReviewContextReferenceFile
}

func newReferenceList(lines map[string]int) *referenceList {
	return &referenceList{lines: lines, seen: map[string]struct{}{}}
}

func (l *referenceList) add(filePath, relation string) {
	if len(l.files) >= maxSliceReferenceFiles {
		return
	}
	count, ok := l.lines[filePath]
	if !ok {
		return
	}
	if _, ok := l.seen[filePath]; ok {
		return
	}
	l.seen[filePath] = struct{}{}
	l.files = append(l.files, security.ReviewContextReferenceFile{Path: filePath, Lines: count, Relation: relation})
}

func (l *referenceList) addDir(graph *goImportGraph, dir, relation string) {
	for _, file := range graph.sources[dir] {
		l.add(file, relation)
	}
}

func repositoryReferenceFiles(files []repoFile, lines map[string]int) []security.ReviewContextReferenceFile {
	ordered := make([]repoFile, 0, len(files))
	for _, file := range files {
		if isSourceCodePath(file.Path) {
			ordered = append(ordered, file)
		}
	}
	for _, file := range files {
		if !isSourceCodePath(file.Path) {
			ordered = append(ordered, file)
		}
	}
	out := make([]security.ReviewContextReferenceFile, 0, min(len(ordered), security.MaxReviewContextReferenceFiles))
	for _, file := range ordered {
		count, ok := lines[file.Path]
		if !ok {
			continue
		}
		out = append(out, security.ReviewContextReferenceFile{Path: file.Path, Lines: count, Relation: security.ReferenceRelationRepository})
		if len(out) == security.MaxReviewContextReferenceFiles {
			break
		}
	}
	return out
}

func referenceLineCounts(root string, files []repoFile) map[string]int {
	out := make(map[string]int, len(files))
	for _, file := range files {
		if !security.SafeRepoPath(file.Path) {
			continue
		}
		if count := countFileLines(filepath.Join(root, filepath.FromSlash(file.Path))); count > 0 {
			out[file.Path] = count
		}
	}
	return out
}

func countFileLines(fullPath string) int {
	info, err := os.Lstat(fullPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxReferenceFileBytes {
		return 0
	}
	file, err := os.Open(fullPath)
	if err != nil {
		return 0
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReaderSize(file, referenceLineBufferBytes)
	count := 0
	last := byte('\n')
	buf := make([]byte, referenceLineBufferBytes)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			count += bytes.Count(buf[:n], []byte{'\n'})
			last = buf[n-1]
		}
		if err != nil {
			break
		}
	}
	if last != '\n' {
		count++
	}
	return count
}

type goImportGraph struct {
	sources   map[string][]string
	edges     map[string]map[string]struct{}
	reverse   map[string]map[string]struct{}
	modules   []goModule
	goDirsSet map[string]struct{}
}

type goModule struct {
	path string
	dir  string
}

func newGoImportGraph(root string, files []repoFile, lines map[string]int) *goImportGraph {
	graph := &goImportGraph{
		sources:   map[string][]string{},
		edges:     map[string]map[string]struct{}{},
		reverse:   map[string]map[string]struct{}{},
		goDirsSet: map[string]struct{}{},
	}
	for _, file := range files {
		if file.Name == goModFile {
			if module := readGoModulePath(filepath.Join(root, filepath.FromSlash(file.Path))); module != "" {
				graph.modules = append(graph.modules, goModule{path: module, dir: file.Dir})
			}
		}
		if _, ok := lines[file.Path]; ok && file.Ext == extGo && !strings.HasSuffix(file.Name, "_test.go") {
			graph.sources[file.Dir] = append(graph.sources[file.Dir], file.Path)
			graph.goDirsSet[file.Dir] = struct{}{}
		}
	}
	sort.Slice(graph.modules, func(i, j int) bool { return len(graph.modules[i].path) > len(graph.modules[j].path) })
	if len(graph.modules) == 0 {
		return graph
	}
	fset := token.NewFileSet()
	for dir, sources := range graph.sources {
		for _, source := range sources {
			parsed, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(source)), nil, parser.ImportsOnly)
			if err != nil {
				continue
			}
			for _, spec := range parsed.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					continue
				}
				target, ok := graph.resolve(importPath)
				if !ok || target == dir {
					continue
				}
				link(graph.edges, dir, target)
				link(graph.reverse, target, dir)
			}
		}
	}
	return graph
}

func (g *goImportGraph) resolve(importPath string) (string, bool) {
	for _, module := range g.modules {
		if importPath != module.path && !strings.HasPrefix(importPath, module.path+"/") {
			continue
		}
		dir := path.Clean(path.Join(module.dir, strings.TrimPrefix(importPath, module.path)))
		if _, ok := g.goDirsSet[dir]; ok {
			return dir, true
		}
		return "", false
	}
	return "", false
}

func (g *goImportGraph) imports(dir string) []string   { return sortedKeys(g.edges[dir]) }
func (g *goImportGraph) importers(dir string) []string { return sortedKeys(g.reverse[dir]) }

func link(edges map[string]map[string]struct{}, from, to string) {
	if edges[from] == nil {
		edges[from] = map[string]struct{}{}
	}
	edges[from][to] = struct{}{}
}

func readGoModulePath(fullPath string) string {
	info, err := os.Lstat(fullPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxGoModuleFileBytes {
		return ""
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`)
		}
	}
	return ""
}

func annotateGoSliceBlastRadius(root string, files []repoFile, reviewSlices []store.ReviewSlice) {
	lines := referenceLineCounts(root, files)
	graph := newGoImportGraph(root, files, lines)
	for i := range reviewSlices {
		slice := &reviewSlices[i]
		if slice.Source != "deterministic-go-package" || len(slice.OwnedFiles) == 0 {
			continue
		}
		dir := path.Dir(slice.OwnedFiles[0].Path)
		importers := graph.importers(dir)
		if len(importers) == 0 {
			continue
		}
		tag := "importers:" + strconv.Itoa(len(importers))
		if !containsString(slice.Tags, tag) {
			slice.Tags = append(slice.Tags, tag)
			sort.Strings(slice.Tags)
		}
		if len(importers) >= 5 && !containsString(slice.Tags, "high-fan-in") {
			slice.Tags = append(slice.Tags, "high-fan-in")
			sort.Strings(slice.Tags)
		}
	}
}

func containsString(items []string, target string) bool {
	return slices.Contains(items, target)
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
