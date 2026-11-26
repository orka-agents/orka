/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package scan

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

const BaselinePath = "hack/commentlint/baseline.json"

const (
	langGo     = "go"
	langShell  = "shell"
	langPython = "python"
)

const (
	kindDoc      = "doc"
	kindLine     = "line"
	kindTrailing = "trailing"
)

type Comment struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"endLine"`
	Lines   int    `json:"lines"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Context string `json:"context,omitempty"`

	spans []span
}

type span struct {
	start, end int
	insert     string
}

type Target struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Category string `json:"category,omitempty"`
}

var goDirective = regexp.MustCompile(`^(?://(?:go:|\s*nolint(?:$|[:\s])|lint:|export |line |extern |` +
	`\+|\s*\+[a-z]|revive:|\s*#nosec)|/\*line )`)

var deprecation = regexp.MustCompile(`(?m)^Deprecated: `)

var shellDirective = regexp.MustCompile(`^#\s*(shellcheck|shfmt)\b`)

var exampleOutput = regexp.MustCompile(`(?i)^[[:space:]]*(unordered )?output:`)

var pythonDirective = regexp.MustCompile(`^#\s*(nosec|noqa|type:|pylint:|pyright:|mypy:|-\*-|fmt:|isort:)`)

var shellAliases = regexp.MustCompile(`\balias\b|expand_aliases`)

//go:embed notices.json
var noticesJSON []byte

var noticeParagraphs = func() []*regexp.Regexp {
	var spec struct {
		Paragraphs []string `json:"paragraphs"`
	}
	if err := json.Unmarshal(noticesJSON, &spec); err != nil {
		panic(err)
	}
	out := make([]*regexp.Regexp, 0, len(spec.Paragraphs))
	for _, pattern := range spec.Paragraphs {
		out = append(out, regexp.MustCompile(pattern))
	}
	return out
}()

var pythonInterpreter = regexp.MustCompile(`^python(3(\.\d+)?)?$`)

func language(path string, src []byte) string {
	switch filepath.Ext(path) {
	case ".go":
		return langGo
	case ".sh", ".bash":
		return langShell
	case ".py":
		return langPython
	}
	switch name := interpreter(src); {
	case name == "bash" || name == "sh":
		return langShell
	case pythonInterpreter.MatchString(name):
		return langPython
	}
	return ""
}

func interpreter(src []byte) string {
	line, _, _ := bytes.Cut(src, []byte{'\n'})
	rest, ok := bytes.CutPrefix(line, []byte("#!"))
	if !ok {
		return ""
	}
	fields := strings.Fields(string(rest))
	if len(fields) == 0 {
		return ""
	}
	name := filepath.Base(fields[0])
	if name != "env" {
		return name
	}
	split := false
	for i := 1; i < len(fields); i++ {
		switch arg := fields[i]; {
		case arg == "-S" || arg == "--split-string":
			split = true
		case strings.HasPrefix(arg, "--split-string="):
			split = true
			fields = slices.Insert(fields, i+1, strings.Fields(strings.TrimPrefix(arg, "--split-string="))...)
		case strings.HasPrefix(arg, "-S") && len(arg) > 2:
			split = true
			fields = slices.Insert(fields, i+1, strings.Fields(arg[2:])...)
		case arg == "-u" || arg == "--unset" || arg == "-C" || arg == "--chdir":
			i++
		case strings.HasPrefix(arg, "-") || strings.Contains(arg, "="):
		default:
			name := filepath.Base(strings.Trim(arg, `"'`))
			if !split || supportedInterpreter(name) {
				return name
			}
		}
	}
	return ""
}

func supportedInterpreter(name string) bool {
	return name == "bash" || name == "sh" || pythonInterpreter.MatchString(name)
}

func fileLanguage(full, path string) string {
	if lang := language(path, nil); lang != "" {
		return lang
	}
	f, err := os.Open(full)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 256)
	n, _ := f.Read(head)
	return language(path, head[:n])
}

var vendoredDirs = []string{".agents/skills/autoreview/"}

func skipped(path string) bool {
	if slices.ContainsFunc(vendoredDirs, func(dir string) bool { return strings.HasPrefix(path, dir) }) {
		return true
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "testdata" || part == "third_party" || part == "vendor" || part == "node_modules" {
			return true
		}
	}
	base := filepath.Base(path)
	return strings.HasPrefix(base, "zz_generated") ||
		strings.HasSuffix(base, "_gen.go") ||
		strings.HasSuffix(base, ".pb.go")
}

func TrackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		path := string(entry)
		if path == "" || skipped(path) {
			continue
		}
		full := filepath.Join(root, path)
		if info, err := os.Lstat(full); err != nil || !info.Mode().IsRegular() || fileLanguage(full, path) == "" {
			continue
		}
		files = append(files, path)
	}
	sort.Strings(files)
	return files, nil
}

func Scan(root, path string) ([]Comment, error) {
	src, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return nil, err
	}
	switch language(path, src) {
	case langGo:
		return scanGo(path, src)
	case langShell:
		return scanHash(path, src, shellDirective)
	case langPython:
		return scanHash(path, src, pythonDirective)
	}
	return nil, nil
}

func scanGo(path string, src []byte) ([]Comment, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if ast.IsGenerated(file) {
		return nil, nil
	}
	docs := map[*ast.CommentGroup]bool{}
	exemptDocs := map[*ast.CommentGroup]bool{}
	isAPI := strings.HasPrefix(path, "api/")
	isTest := strings.HasSuffix(path, "_test.go")
	var examples []*ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			docs[node.Doc] = true
			if isTest && strings.HasPrefix(node.Name.Name, "Example") && node.Body != nil {
				examples = append(examples, node.Body)
			}
		case *ast.GenDecl:
			docs[node.Doc] = true
			if isAPI && node.Tok == token.TYPE || importsC(node) {
				exemptDocs[node.Doc] = true
			}
		case *ast.ImportSpec:
			if node.Path.Value == `"C"` {
				exemptDocs[node.Doc] = true
			}
		case *ast.TypeSpec:
			docs[node.Doc] = true
			if isAPI {
				exemptDocs[node.Doc] = true
			}
		case *ast.ValueSpec:
			docs[node.Doc] = true
		case *ast.Field:
			docs[node.Doc] = true
			if isAPI {
				exemptDocs[node.Doc] = true
			}
		}
		return true
	})
	docs[file.Doc] = true
	packageLine := fset.PositionFor(file.Package, false).Line
	tf := fset.File(file.Pos())
	var out []Comment
	for _, group := range file.Comments {
		if exemptDocs[group] {
			continue
		}
		startLine := fset.PositionFor(group.Pos(), false).Line
		if startLine < packageLine && onlyNotices(group.Text()) {
			continue
		}
		if deprecation.MatchString(group.Text()) || exampleOutputGroup(group, examples) {
			continue
		}
		var c Comment
		var texts []string
		for _, item := range group.List {
			if goDirective.MatchString(item.Text) {
				continue
			}
			start := tf.Offset(item.Pos())
			end := tf.Offset(item.End())
			c.spans = append(c.spans, span{start: start, end: end})
			c.Lines += strings.Count(item.Text, "\n") + 1
			texts = append(texts, item.Text)
		}
		if len(c.spans) == 0 {
			continue
		}
		c.File = path
		c.Line = tf.PositionFor(tf.Pos(c.spans[0].start), false).Line
		c.EndLine = tf.PositionFor(tf.Pos(c.spans[len(c.spans)-1].end), false).Line
		c.Text = strings.Join(texts, "\n")
		switch {
		case !onlySpaceBefore(src, c.spans[0].start):
			c.Kind = kindTrailing
		case docs[group]:
			c.Kind = kindDoc
		default:
			c.Kind = kindLine
		}
		out = append(out, c)
	}
	return out, nil
}

func scanHash(path string, src []byte, directive *regexp.Regexp) ([]Comment, error) {
	var starts []int
	var err error
	if language(path, src) == langShell {
		starts, err = shellComments(path, src)
	} else {
		starts, err = pythonComments(src)
	}
	if err != nil {
		return nil, err
	}
	var out []Comment
	var current *Comment
	prevLine := 0
	for _, start := range starts {
		end := len(src)
		if i := bytes.IndexByte(src[start:], '\n'); i >= 0 {
			end = start + i
		}
		text := strings.TrimRight(string(src[start:end]), " \t\r")
		line := bytes.Count(src[:start], []byte{'\n'}) + 1
		if (line == 1 && strings.HasPrefix(text, "#!")) || directive.MatchString(text) {
			continue
		}
		fullLine := onlySpaceBefore(src, start)
		if current != nil && (!fullLine || current.Kind == kindTrailing || line != prevLine+1) {
			out = append(out, *current)
			current = nil
		}
		if current == nil {
			kind := kindLine
			if !fullLine {
				kind = kindTrailing
			}
			current = &Comment{File: path, Line: line, Kind: kind}
		} else {
			current.Text += "\n"
		}
		current.EndLine = line
		current.Lines++
		current.Text += text
		current.spans = append(current.spans, span{start: start, end: start + len(text)})
		prevLine = line
	}
	if current != nil {
		out = append(out, *current)
	}
	return slices.DeleteFunc(out, func(c Comment) bool { return licenseHeader(src, c) }), nil
}

func licenseHeader(src []byte, c Comment) bool {
	var plain strings.Builder
	for line := range strings.SplitSeq(c.Text, "\n") {
		plain.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "#")) + "\n")
	}
	if !onlyNotices(plain.String()) {
		return false
	}
	for i, line := range strings.Split(string(src), "\n")[:c.Line-1] {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && (i != 0 || !strings.HasPrefix(trimmed, "#!")) {
			return false
		}
	}
	return true
}

func onlyNotices(text string) bool {
	var plain strings.Builder
	for line := range strings.SplitSeq(text, "\n") {
		plain.WriteString(strings.TrimPrefix(strings.TrimSpace(line), "*") + "\n")
	}
	found := false
	for paragraph := range strings.SplitSeq(plain.String(), "\n\n") {
		normalized := strings.Join(strings.Fields(paragraph), " ")
		if normalized == "" {
			continue
		}
		if !slices.ContainsFunc(noticeParagraphs, func(re *regexp.Regexp) bool { return re.MatchString(normalized) }) {
			return false
		}
		found = true
	}
	return found
}

func exampleOutputGroup(group *ast.CommentGroup, examples []*ast.BlockStmt) bool {
	if !exampleOutput.MatchString(strings.TrimSpace(group.Text())) {
		return false
	}
	return slices.ContainsFunc(examples, func(body *ast.BlockStmt) bool {
		return body.Lbrace < group.Pos() && group.End() < body.Rbrace
	})
}

func importsC(decl *ast.GenDecl) bool {
	if decl.Tok != token.IMPORT {
		return false
	}
	return slices.ContainsFunc(decl.Specs, func(spec ast.Spec) bool {
		imp, ok := spec.(*ast.ImportSpec)
		return ok && imp.Path.Value == `"C"`
	})
}

func shellComments(path string, src []byte) ([]int, error) {
	file, err := syntax.NewParser(syntax.KeepComments(true), syntax.Variant(syntax.LangBash)).
		Parse(bytes.NewReader(src), path)
	if err != nil {
		return nil, err
	}
	var starts []int
	syntax.Walk(file, func(node syntax.Node) bool {
		if c, ok := node.(*syntax.Comment); ok {
			starts = append(starts, int(c.Hash.Offset()))
		}
		return true
	})
	sort.Ints(starts)
	return slices.Compact(starts), nil
}

const pythonTokenizer = `import io, json, sys, tokenize
if sys.version_info < (3, 12):
    sys.exit("python3 3.12 or newer is required to tokenize f-strings")
data = sys.stdin.buffer.read()
lines = data.split(b"\n")
offsets = [0]
for line in lines:
    offsets.append(offsets[-1] + len(line) + 1)
out = []
encoding = "utf-8"
for tok in tokenize.tokenize(io.BytesIO(data).readline):
    if tok.type == tokenize.ENCODING:
        encoding = tok.string
    if tok.type == tokenize.COMMENT:
        row, col = tok.start
        line, base = lines[row - 1], offsets[row - 1]
        if row == 1 and line.startswith(b"\xef\xbb\xbf"):
            line, base = line[3:], base + 3
        out.append(base + len(line.decode(encoding)[:col].encode(encoding)))
print(json.dumps(out))
`

func pythonComments(src []byte) ([]int, error) {
	out, err := runPython(src, pythonTokenizer)
	if err != nil {
		return nil, err
	}
	var starts []int
	if err := json.Unmarshal(out, &starts); err != nil {
		return nil, fmt.Errorf("python tokenizer output: %w", err)
	}
	return starts, nil
}

func runPython(src []byte, program string) ([]byte, error) {
	bin, err := exec.LookPath("python3")
	if err != nil {
		return nil, errors.New("python3 is required to scan Python comments")
	}
	cmd := exec.Command(bin, "-I", "-c", program)
	cmd.Stdin = bytes.NewReader(src)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("python3 tokenize: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func onlySpaceBefore(src []byte, at int) bool {
	lineStart := bytes.LastIndexByte(src[:at], '\n') + 1
	return len(bytes.TrimSpace(src[lineStart:at])) == 0
}

func WithContext(root string, comments []Comment, lines int) {
	cache := map[string][]string{}
	for i := range comments {
		c := &comments[i]
		src, ok := cache[c.File]
		if !ok {
			data, err := os.ReadFile(filepath.Join(root, c.File))
			if err != nil {
				continue
			}
			src = strings.Split(string(data), "\n")
			cache[c.File] = src
		}
		first := c.EndLine
		if c.Kind == kindTrailing {
			first = c.Line - 1
		}
		var ctx []string
		for j := first; j < len(src) && len(ctx) < lines; j++ {
			if strings.TrimSpace(src[j]) != "" {
				ctx = append(ctx, src[j])
			}
		}
		c.Context = strings.Join(ctx, "\n")
	}
}

func Inventory(root string, prefixes []string) ([]Comment, error) {
	files, err := TrackedFiles(root)
	if err != nil {
		return nil, err
	}
	var out []Comment
	var errs []error
	for _, path := range files {
		if len(prefixes) > 0 && !slices.ContainsFunc(prefixes, func(p string) bool {
			p = filepath.ToSlash(filepath.Clean(p))
			return p == "." || path == p || strings.HasPrefix(path, p+"/")
		}) {
			continue
		}
		comments, err := Scan(root, path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		out = append(out, comments...)
	}
	return out, errors.Join(errs...)
}

func Counts(comments []Comment) map[string]int {
	counts := map[string]int{}
	for _, c := range comments {
		counts[c.File] += c.Lines
	}
	return counts
}

func Delete(root string, targets []Target) error {
	byFile := map[string][]int{}
	for _, t := range targets {
		file := filepath.ToSlash(filepath.Clean(t.File))
		if filepath.IsAbs(file) || file == ".." || strings.HasPrefix(file, "../") {
			return fmt.Errorf("target %q must be relative to the repository root", t.File)
		}
		byFile[file] = append(byFile[file], t.Line)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	var errs []error
	for _, path := range files {
		if err := deleteInFile(root, path, byFile[path]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func deleteInFile(root, path string, lines []int) error {
	full := filepath.Join(root, path)
	if err := canonicalFile(root, full); err != nil {
		return err
	}
	src, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	comments, err := Scan(root, path)
	if err != nil {
		return err
	}
	var spans []span
	for _, line := range lines {
		idx := slices.IndexFunc(comments, func(c Comment) bool { return c.Line == line })
		if idx < 0 {
			return fmt.Errorf("no deletable comment starts at line %d", line)
		}
		if slices.IndexFunc(comments[idx+1:], func(c Comment) bool { return c.Line == line }) >= 0 {
			return fmt.Errorf("several comments start at line %d; delete them by hand", line)
		}
		for _, s := range comments[idx].spans {
			spans = append(spans, removalSpan(src, s))
		}
	}
	spans = mergeSpans(spans)
	lang := language(path, src)
	if lang == langShell && (shellAliases.Match(src) || shellSources(path, src)) {
		return errors.New("aliases can change how this script parses; delete its comments by hand")
	}
	isGo := lang == langGo
	out := src
	for _, s := range slices.Backward(spans) {
		if !isGo && continuesInto(src, s.start) {
			return errors.New("a comment follows a line continuation; delete it by hand")
		}
		if !isGo {
			s = absorbBlankLine(src, s)
		}
		out = append(append(out[:s.start:s.start], s.insert...), out[s.end:]...)
	}
	if err := verifyDeletion(path, src, out); err != nil {
		return err
	}
	if isGo {
		formatted, err := format.Source(out)
		if err != nil {
			return fmt.Errorf("gofmt after deletion: %w", err)
		}
		out = formatted
	}
	info, err := os.Stat(full)
	if err != nil {
		return err
	}
	return os.WriteFile(full, out, info.Mode().Perm())
}

func shellSources(path string, src []byte) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(bytes.NewReader(src), path)
	if err != nil {
		return true
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok && !found {
			found = changesParsing(path, call)
		}
		return !found
	})
	return found
}

func changesParsing(path string, call *syntax.CallExpr) bool {
	wrapped := false
	for i, word := range call.Args {
		name := staticWord(word)
		switch {
		case name == "command" || name == "builtin":
			wrapped = true
		case wrapped && strings.HasPrefix(name, "-"):
		case name == "trap":
			return trapChangesParsing(path, call.Args[i+1:])
		default:
			return name == "" || name == "alias" || name == "source" || name == "." || name == "eval"
		}
	}
	return false
}

func trapChangesParsing(path string, args []*syntax.Word) bool {
	for i, word := range args {
		action := staticWord(word)
		switch {
		case action == "--":
			continue
		case action == "-p" || action == "-l" || i == len(args)-1:
			return false
		case action == "":
			return !quotedEmpty(word)
		}
		return shellSources(path, []byte(action))
	}
	return false
}

func quotedEmpty(word *syntax.Word) bool {
	for _, part := range word.Parts {
		switch quoted := part.(type) {
		case *syntax.SglQuoted:
			if quoted.Value != "" {
				return false
			}
		case *syntax.DblQuoted:
			if len(quoted.Parts) != 0 {
				return false
			}
		default:
			return false
		}
	}
	return len(word.Parts) > 0
}

func staticWord(word *syntax.Word) string {
	dynamic := false
	syntax.Walk(word, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst, *syntax.ExtGlob:
			dynamic = true
		}
		return !dynamic
	})
	if dynamic {
		return ""
	}
	for _, part := range word.Parts {
		if lit, ok := part.(*syntax.Lit); ok && strings.ContainsAny(lit.Value, "*?[") {
			return ""
		}
	}
	fields, err := expand.Fields(&expand.Config{}, word)
	if err != nil || len(fields) != 1 {
		return ""
	}
	return fields[0]
}

func continuesInto(src []byte, start int) bool {
	if start == 0 || src[start-1] != '\n' {
		return false
	}
	prev := bytes.TrimRight(src[:start-1], "\r")
	return len(prev) > 0 && prev[len(prev)-1] == '\\' && !escaped(prev, len(prev)-1)
}

func canonicalFile(root, full string) error {
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || resolved != filepath.Join(resolvedRoot, rel) {
		return errors.New("path goes through a symlink; delete comments at the file's real path")
	}
	return nil
}

func removalSpan(src []byte, s span) span {
	lineStart := bytes.LastIndexByte(src[:s.start], '\n') + 1
	lineEnd := len(src)
	if i := bytes.IndexByte(src[s.end:], '\n'); i >= 0 {
		lineEnd = s.end + i
	}
	before := len(bytes.TrimSpace(src[lineStart:s.start])) == 0
	after := len(bytes.TrimSpace(src[s.end:lineEnd])) == 0
	switch {
	case before && after:
		end := lineEnd
		if end < len(src) {
			end++
		}
		return span{start: lineStart, end: end}
	case after:
		start := s.start
		for start > lineStart && isBlank(src[start-1]) && !escaped(src, start-1) {
			start--
		}
		return span{start: start, end: lineEnd}
	default:
		end := s.end
		for end < lineEnd && isBlank(src[end]) {
			end++
		}
		out := span{start: s.start, end: end}
		if s.start > lineStart && !isBlank(src[s.start-1]) && end == s.end {
			out.insert = " "
		}
		return out
	}
}

func isBlank(b byte) bool {
	return b == ' ' || b == '\t'
}

func escaped(src []byte, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && src[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

func mergeSpans(spans []span) []span {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var out []span
	for _, s := range spans {
		if n := len(out); n > 0 && s.start <= out[n-1].end {
			out[n-1].end = max(out[n-1].end, s.end)
			continue
		}
		out = append(out, s)
	}
	return out
}

func absorbBlankLine(src []byte, s span) span {
	fullLines := (s.start == 0 || src[s.start-1] == '\n') && s.end > 0 && src[s.end-1] == '\n'
	if !fullLines || s.start == 0 {
		return s
	}
	prevStart := bytes.LastIndexByte(src[:s.start-1], '\n') + 1
	if len(bytes.TrimSpace(src[prevStart:s.start])) != 0 {
		return s
	}
	nextEnd := len(src)
	if i := bytes.IndexByte(src[s.end:], '\n'); i >= 0 {
		nextEnd = s.end + i + 1
	}
	if s.end < len(src) && len(bytes.TrimSpace(src[s.end:nextEnd])) == 0 {
		s.end = nextEnd
	}
	return s
}

func LoadBaseline(root string) (map[string]int, error) {
	data, err := os.ReadFile(filepath.Join(root, BaselinePath))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	baseline := map[string]int{}
	if err := json.Unmarshal(data, &baseline); err != nil {
		return nil, fmt.Errorf("%s: %w", BaselinePath, err)
	}
	return baseline, nil
}

const fixAdvice = "Delete the new comments: name things so the code explains itself, " +
	"file an issue for a known hack, or record a decision in docs/adr."

type Violation struct {
	File     string
	Have     int
	Allowed  int
	Increase bool
}

func (v Violation) String() string {
	if v.Increase {
		return fmt.Sprintf("%s: %d comment lines, baseline allows %d. %s List them with: "+
			"go run ./hack/commentlint list %s", v.File, v.Have, v.Allowed, fixAdvice, v.File)
	}
	return fmt.Sprintf("%s: %d comment lines, baseline still allows %d. "+
		"Lock in the reduction with: make comment-baseline", v.File, v.Have, v.Allowed)
}

func Compare(counts, baseline map[string]int) []Violation {
	files := map[string]bool{}
	for f := range counts {
		files[f] = true
	}
	for f := range baseline {
		files[f] = true
	}
	var out []Violation
	for f := range files {
		have, allowed := counts[f], baseline[f]
		if have != allowed {
			out = append(out, Violation{File: f, Have: have, Allowed: allowed, Increase: have > allowed})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

func WriteBaseline(root string, counts map[string]int) error {
	trimmed := map[string]int{}
	for f, n := range counts {
		if n > 0 {
			trimmed[f] = n
		}
	}
	data, err := json.MarshalIndent(trimmed, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, BaselinePath), append(data, '\n'), 0o644)
}
