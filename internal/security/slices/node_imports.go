package slices

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const (
	maxPackageJSONBytes = 1 << 20
	nodeRequireSyntax   = "require"
)

var (
	nodeSourceExtensions = []string{extTS, extTSX, extJS, extJSX, extMJS, extCJS}
	nodeImportPattern    = regexp.MustCompile(`(\bfrom|\bimport|\brequire)\s*\(?\s*["']([^"'\n]+)["']`)
)

type nodeImportGraph struct {
	edges   map[string]map[string]struct{}
	reverse map[string]map[string]struct{}
}

type nodePackage struct {
	name    string
	dir     string
	main    string
	exports map[string]json.RawMessage
	imports map[string]json.RawMessage
}

func newNodeImportGraph(root string, files []repoFile, lines map[string]int) *nodeImportGraph {
	graph := &nodeImportGraph{edges: map[string]map[string]struct{}{}, reverse: map[string]map[string]struct{}{}}
	known := make(map[string]struct{}, len(files))
	var packages []nodePackage
	aliases := map[string][]nodePathAlias{}
	for _, file := range files {
		known[file.Path] = struct{}{}
		if file.Name == packageJSONFile {
			if pkg, ok := readNodePackage(filepath.Join(root, filepath.FromSlash(file.Path)), file.Dir); ok {
				packages = append(packages, pkg)
			}
		}
		if strings.HasPrefix(file.Name, "tsconfig") && path.Ext(file.Name) == extJSON {
			aliases[file.Dir] = append(aliases[file.Dir], readTSConfigAliases(filepath.Join(root, filepath.FromSlash(file.Path)), file.Dir)...)
		}
	}
	for _, file := range files {
		if _, ok := lines[file.Path]; !ok || !isNodeSource(file.Path) || isTestFile(file) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			continue
		}
		for _, match := range nodeImportPattern.FindAllSubmatch(data, -1) {
			target := resolveNodeImport(file.Dir, string(match[2]), string(match[1]) == nodeRequireSyntax, known, packages, aliases)
			if target == "" || target == file.Path {
				continue
			}
			link(graph.edges, file.Path, target)
			link(graph.reverse, target, file.Path)
		}
	}
	return graph
}

func (g *nodeImportGraph) imports(file string) []string   { return sortedKeys(g.edges[file]) }
func (g *nodeImportGraph) importers(file string) []string { return sortedKeys(g.reverse[file]) }

func resolveNodeImport(fromDir, spec string, require bool, known map[string]struct{}, packages []nodePackage, aliases map[string][]nodePathAlias) string {
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		return resolveNodeBase(path.Join(fromDir, spec), known)
	}
	conditions := nodeImportConditions
	if require {
		conditions = nodeRequireConditions
	}
	if strings.HasPrefix(spec, "#") {
		pkg, ok := nearestNodePackage(fromDir, packages)
		if !ok {
			return ""
		}
		target := nodeMappedTarget(pkg.imports, spec, conditions)
		if strings.HasPrefix(target, "./") {
			return resolveNodeBase(path.Join(pkg.dir, target), known)
		}
		if target == "" || strings.HasPrefix(target, "#") {
			return ""
		}
		return resolveNodeImport(pkg.dir, target, require, known, packages, aliases)
	}
	for dir := fromDir; ; dir = path.Dir(dir) {
		if list, ok := aliases[dir]; ok && len(list) > 0 {
			for _, alias := range list {
				matched, ok := alias.match(spec)
				if !ok {
					continue
				}
				for _, target := range alias.targets {
					if found := resolveNodeBase(strings.Replace(target, "*", matched, 1), known); found != "" {
						return found
					}
				}
			}
			break
		}
		if dir == "." || dir == "/" {
			break
		}
	}
	for _, pkg := range packages {
		if pkg.name == "" || (spec != pkg.name && !strings.HasPrefix(spec, pkg.name+"/")) {
			continue
		}
		if pkg.exports != nil {
			target := pkg.exportTarget("."+strings.TrimPrefix(spec, pkg.name), conditions)
			if target == "" {
				return ""
			}
			if found := resolveNodeBase(target, known); found != "" {
				return found
			}
		}
		if spec == pkg.name && pkg.main != "" {
			if found := resolveNodeBase(pkg.main, known); found != "" {
				return found
			}
		}
		return resolveNodeBase(path.Join(pkg.dir, strings.TrimPrefix(spec, pkg.name)), known)
	}
	return ""
}

func nearestNodePackage(dir string, packages []nodePackage) (nodePackage, bool) {
	best, found := nodePackage{}, false
	for _, pkg := range packages {
		if pkg.dir != "." && dir != pkg.dir && !strings.HasPrefix(dir, pkg.dir+"/") {
			continue
		}
		if !found || len(pkg.dir) > len(best.dir) || best.dir == "." {
			best, found = pkg, true
		}
	}
	return best, found
}

func (p nodePackage) exportTarget(subpath string, conditions []string) string {
	if target := nodeMappedTarget(p.exports, subpath, conditions); target != "" {
		return path.Join(p.dir, target)
	}
	return ""
}

func nodeMappedTarget(mapping map[string]json.RawMessage, key string, conditions []string) string {
	subpath := key
	if raw, ok := mapping[subpath]; ok {
		return nodeExportTarget(raw, conditions, 0)
	}
	patterns := make([]string, 0, len(mapping))
	for pattern := range mapping {
		patterns = append(patterns, pattern)
	}
	sort.Slice(patterns, func(i, j int) bool { return morePreciseNodePattern(patterns[i], patterns[j]) })
	for _, pattern := range patterns {
		prefix, suffix, wildcard := strings.Cut(pattern, "*")
		if !wildcard || len(subpath) < len(prefix)+len(suffix) || !strings.HasPrefix(subpath, prefix) || !strings.HasSuffix(subpath, suffix) {
			continue
		}
		target := nodeExportTarget(mapping[pattern], conditions, 0)
		if target == "" {
			return ""
		}
		return strings.Replace(target, "*", subpath[len(prefix):len(subpath)-len(suffix)], 1)
	}
	return ""
}

var (
	nodeImportConditions  = []string{"import", "node", "node-addons", "default"}
	nodeRequireConditions = []string{nodeRequireSyntax, "node", "node-addons", "default"}
)

func nodeExportTarget(raw json.RawMessage, conditions []string, depth int) string {
	var target string
	if json.Unmarshal(raw, &target) == nil {
		return target
	}
	if depth > 2 {
		return ""
	}
	var fallbacks []json.RawMessage
	if json.Unmarshal(raw, &fallbacks) == nil {
		for _, fallback := range fallbacks {
			if target := nodeExportTarget(fallback, conditions, depth+1); target != "" {
				return target
			}
		}
		return ""
	}
	keys, values, ok := orderedJSONObject(raw)
	if !ok {
		return ""
	}
	typesTarget := ""
	for i, key := range keys {
		if slices.Contains(conditions, key) {
			if target := nodeExportTarget(values[i], conditions, depth+1); target != "" {
				return target
			}
		}
		if key == "types" && typesTarget == "" {
			typesTarget = nodeExportTarget(values[i], conditions, depth+1)
		}
	}
	return typesTarget
}

func orderedJSONObject(raw json.RawMessage) ([]string, []json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, nil, false
	}
	var keys []string
	var values []json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, false
		}
		keys = append(keys, key)
		values = append(values, value)
	}
	return keys, values, true
}

func readNodeExports(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var subpaths map[string]json.RawMessage
	if json.Unmarshal(raw, &subpaths) == nil && len(subpaths) > 0 {
		for subpath := range subpaths {
			if !strings.HasPrefix(subpath, ".") {
				return map[string]json.RawMessage{".": raw}
			}
		}
		return subpaths
	}
	return map[string]json.RawMessage{".": raw}
}

func resolveNodeBase(base string, known map[string]struct{}) string {
	if base == "" || base == ".." || strings.HasPrefix(base, "../") {
		return ""
	}
	if path.Ext(base) == extJSON {
		if _, ok := known[base]; ok {
			return base
		}
		return ""
	}
	candidates := []string{base}
	stem := base
	switch path.Ext(base) {
	case extJS, extJSX, extMJS, extCJS:
		stem = strings.TrimSuffix(base, path.Ext(base))
	}
	for _, ext := range nodeSourceExtensions {
		candidates = append(candidates, stem+ext)
	}
	for _, ext := range nodeSourceExtensions {
		candidates = append(candidates, path.Join(base, "index"+ext), path.Join(base, "src", "index"+ext))
	}
	for _, candidate := range candidates {
		if _, ok := known[candidate]; ok && isNodeSource(candidate) {
			return candidate
		}
	}
	return ""
}

func morePreciseNodePattern(a, b string) bool {
	prefixA, prefixB := strings.Index(a, "*"), strings.Index(b, "*")
	if prefixA < 0 {
		prefixA = len(a)
	}
	if prefixB < 0 {
		prefixB = len(b)
	}
	if prefixA != prefixB {
		return prefixA > prefixB
	}
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a < b
}

type nodePathAlias struct {
	pattern string
	targets []string
}

func (a nodePathAlias) match(spec string) (string, bool) {
	prefix, suffix, wildcard := strings.Cut(a.pattern, "*")
	if !wildcard {
		return "", spec == a.pattern
	}
	if len(spec) < len(prefix)+len(suffix) || !strings.HasPrefix(spec, prefix) || !strings.HasSuffix(spec, suffix) {
		return "", false
	}
	return spec[len(prefix) : len(spec)-len(suffix)], true
}

func readTSConfigAliases(fullPath, dir string) []nodePathAlias {
	options := readTSConfigOptions(fullPath, dir, 0)
	root := options.pathsDir
	if options.baseURL != "" {
		root = options.baseURL
	}
	out := make([]nodePathAlias, 0, len(options.paths)+1)
	for pattern, targets := range options.paths {
		alias := nodePathAlias{pattern: pattern}
		for _, target := range targets {
			alias.targets = append(alias.targets, path.Join(root, target))
		}
		out = append(out, alias)
	}
	sort.Slice(out, func(i, j int) bool { return morePreciseNodePattern(out[i].pattern, out[j].pattern) })
	if options.baseURL != "" {
		out = append(out, nodePathAlias{pattern: "*", targets: []string{path.Join(options.baseURL, "*")}})
	}
	return out
}

const maxTSConfigExtendsDepth = 4

type tsconfigOptions struct {
	paths    map[string][]string
	pathsDir string
	baseURL  string
}

func readTSConfigOptions(fullPath, dir string, depth int) tsconfigOptions {
	var options tsconfigOptions
	info, err := os.Lstat(fullPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPackageJSONBytes {
		return options
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return options
	}
	var config struct {
		Extends         json.RawMessage `json:"extends"`
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if json.Unmarshal(stripJSONComments(data), &config) != nil {
		return options
	}
	if depth < maxTSConfigExtendsDepth {
		var bases []string
		var single string
		if json.Unmarshal(config.Extends, &single) == nil {
			bases = []string{single}
		} else if json.Unmarshal(config.Extends, &bases) != nil {
			bases = nil
		}
		for _, base := range bases {
			base = strings.TrimSpace(base)
			if !strings.HasPrefix(base, "./") && !strings.HasPrefix(base, "../") {
				continue
			}
			if path.Ext(base) != extJSON {
				base += extJSON
			}
			baseDir := path.Join(dir, path.Dir(base))
			if baseDir == ".." || strings.HasPrefix(baseDir, "../") {
				continue
			}
			inherited := readTSConfigOptions(filepath.Join(filepath.Dir(fullPath), filepath.FromSlash(base)), baseDir, depth+1)
			if len(inherited.paths) > 0 {
				options.paths, options.pathsDir = inherited.paths, inherited.pathsDir
			}
			if inherited.baseURL != "" {
				options.baseURL = inherited.baseURL
			}
		}
	}
	if len(config.CompilerOptions.Paths) > 0 {
		options.paths, options.pathsDir = config.CompilerOptions.Paths, dir
	}
	if config.CompilerOptions.BaseURL != "" {
		options.baseURL = path.Join(dir, config.CompilerOptions.BaseURL)
	}
	return options
}

func stripJSONComments(data []byte) []byte {
	return stripJSONCommentsOnce(stripJSONCommentsOnce(data))
}

func stripJSONCommentsOnce(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case inString:
			out = append(out, c)
			if c == '\\' && i+1 < len(data) {
				i++
				out = append(out, data[i])
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && (data[i] != '*' || data[i+1] != '/') {
				i++
			}
			i++
		case c == ',':
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

func readNodePackage(fullPath, dir string) (nodePackage, bool) {
	info, err := os.Lstat(fullPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPackageJSONBytes {
		return nodePackage{}, false
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nodePackage{}, false
	}
	var manifest struct {
		Name    string                     `json:"name"`
		Main    string                     `json:"main"`
		Exports json.RawMessage            `json:"exports"`
		Imports map[string]json.RawMessage `json:"imports"`
	}
	if json.Unmarshal(data, &manifest) != nil || (strings.TrimSpace(manifest.Name) == "" && len(manifest.Imports) == 0) {
		return nodePackage{}, false
	}
	pkg := nodePackage{name: strings.TrimSpace(manifest.Name), dir: dir, exports: readNodeExports(manifest.Exports), imports: manifest.Imports}
	if manifest.Main != "" {
		pkg.main = path.Join(dir, manifest.Main)
	}
	return pkg, true
}

func isNodeSource(filePath string) bool {
	return slices.Contains(nodeSourceExtensions, path.Ext(filePath))
}
