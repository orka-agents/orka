package workspacedelta

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIgnoreRulesMatchGitignoreSemantics(t *testing.T) {
	for _, test := range []struct {
		name   string
		rules  map[string]string // directory ("" for root, or info/exclude) -> file content
		path   string
		isDir  bool
		ignore bool
	}{
		{name: "basename any depth", rules: map[string]string{"": "*.log\n"}, path: "a/b/x.log", ignore: true},
		{name: "basename no match", rules: map[string]string{"": "*.log\n"}, path: "a/x.txt"},
		{name: "star does not cross slash", rules: map[string]string{"": "a/*.o\n"}, path: "a/b/c.o"},
		{name: "anchored leading slash", rules: map[string]string{"": "/build\n"}, path: "build", isDir: true, ignore: true},
		{name: "anchored leading slash not nested", rules: map[string]string{"": "/build\n"}, path: "src/build", isDir: true},
		{name: "middle slash anchors", rules: map[string]string{"": "docs/gen\n"}, path: "x/docs/gen", isDir: true},
		{name: "middle slash matches from root", rules: map[string]string{"": "docs/gen\n"}, path: "docs/gen", isDir: true, ignore: true},
		{name: "dir only skips files", rules: map[string]string{"": "target/\n"}, path: "target"},
		{name: "dir only matches dirs", rules: map[string]string{"": "target/\n"}, path: "crate/target", isDir: true, ignore: true},
		{name: "negation last wins", rules: map[string]string{"": "*.log\n!keep.log\n"}, path: "keep.log"},
		{name: "negation then reignore", rules: map[string]string{"": "*.log\n!keep.log\nkeep.log\n"}, path: "keep.log", ignore: true},
		{name: "leading double star", rules: map[string]string{"": "**/cache\n"}, path: "a/b/cache", isDir: true, ignore: true},
		{name: "middle double star zero", rules: map[string]string{"": "a/**/b\n"}, path: "a/b", ignore: true},
		{name: "middle double star many", rules: map[string]string{"": "a/**/b\n"}, path: "a/x/y/b", ignore: true},
		{name: "trailing double star inside", rules: map[string]string{"": "out/**\n"}, path: "out/x", ignore: true},
		{name: "trailing double star not dir itself", rules: map[string]string{"": "out/**\n"}, path: "out", isDir: true},
		{name: "bracket negation", rules: map[string]string{"": "file[!0-9].txt\n"}, path: "filea.txt", ignore: true},
		{name: "bracket negation excludes", rules: map[string]string{"": "file[!0-9].txt\n"}, path: "file1.txt"},
		{name: "comment and blank", rules: map[string]string{"": "# x.log\n\n"}, path: "x.log"},
		{name: "escaped hash", rules: map[string]string{"": "\\#notes\n"}, path: "#notes", ignore: true},
		{name: "escaped bang", rules: map[string]string{"": "\\!bang\n"}, path: "!bang", ignore: true},
		{name: "trailing spaces trimmed", rules: map[string]string{"": "dist   \n"}, path: "dist", isDir: true, ignore: true},
		{name: "crlf", rules: map[string]string{"": "dist\r\n"}, path: "dist", isDir: true, ignore: true},
		{name: "bom", rules: map[string]string{"": utf8ByteOrderMk + "dist\n"}, path: "dist", isDir: true, ignore: true},
		{name: "posix class", rules: map[string]string{"": "[[:digit:]].log\n"}, path: "1.log", ignore: true},
		{name: "posix class no match", rules: map[string]string{"": "[[:digit:]].log\n"}, path: "a.log"},
		{name: "posix class negated", rules: map[string]string{"": "[![:upper:]]x\n"}, path: "ax", ignore: true},
		{name: "unknown posix class dropped", rules: map[string]string{"": "[[:nope:]]\n"}, path: "n"},
		{name: "unclosed bracket dropped", rules: map[string]string{"": "[\n"}, path: "["},
		{name: "dangling backslash dropped", rules: map[string]string{"": "a\\\n"}, path: "a"},
		{name: "bracket range", rules: map[string]string{"": "v[0-3].bin\n"}, path: "v2.bin", ignore: true},
		{name: "bracket leading close", rules: map[string]string{"": "[]x]y\n"}, path: "]y", ignore: true},
		{name: "escaped star literal", rules: map[string]string{"": "a\\*b\n"}, path: "axb"},
		{name: "star backtracks", rules: map[string]string{"": "a*b*c\n"}, path: "aXbYbZc", ignore: true},
		{name: "question mark is one byte", rules: map[string]string{"": "?.txt\n"}, path: "\u00e9.txt"},
		{name: "two question marks match a two byte rune", rules: map[string]string{"": "??.txt\n"}, path: "\u00e9.txt", ignore: true},
		{name: "bracket is one byte", rules: map[string]string{"": "[\u00e9].txt\n"}, path: "\u00e9.txt"},
		{name: "nested gitignore scoped", rules: map[string]string{"pkg": "gen/\n"}, path: "gen", isDir: true},
		{name: "nested gitignore applies below", rules: map[string]string{"pkg": "gen/\n"}, path: "pkg/a/gen", isDir: true, ignore: true},
		{name: "nested anchored relative to its dir", rules: map[string]string{"pkg": "/gen\n"}, path: "pkg/gen", isDir: true, ignore: true},
		{name: "deeper file overrides root", rules: map[string]string{"": "*.out\n", "keep": "!*.out\n"}, path: "keep/a.out"},
		{name: "gitignore overrides info exclude", rules: map[string]string{"exclude": "*.out\n", "": "!a.out\n"}, path: "a.out"},
		{name: "info exclude applies", rules: map[string]string{"exclude": "*.out\n"}, path: "x/a.out", ignore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rules := &ignoreRules{byDir: map[string][]ignorePattern{}}
			for dir, content := range test.rules {
				patterns := parseIgnorePatterns([]byte(content))
				if dir == "exclude" {
					rules.exclude = patterns
				} else {
					rules.byDir[dir] = patterns
				}
			}
			if got := rules.matches(test.path, test.isDir); got != test.ignore {
				t.Fatalf("matches(%q, dir=%v) = %v, want %v", test.path, test.isDir, got, test.ignore)
			}
		})
	}
}

func TestMatchIgnoreSegmentsStaysLinearOnManyDoubleStars(t *testing.T) {
	pattern := parseIgnorePatterns([]byte(strings.Repeat("**/a/", 40) + "b\n"))
	if len(pattern) != 1 {
		t.Fatalf("patterns = %d, want 1", len(pattern))
	}
	parts := strings.Split(strings.Repeat("a/", 1500)+"c", "/")
	if matchIgnoreSegments(pattern[0].segments, parts) {
		t.Fatal("pattern unexpectedly matched")
	}
}

func TestOversizedIgnoreRulesFallBackToFullCapture(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "too many patterns", content: strings.Repeat("x\n", maxIgnorePatterns) + "dist/\n"},
		{name: "too many bytes", content: "dist/\n#" + strings.Repeat("x", maxIgnoreSourceBytes)},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, root, ".gitignore", test.content, 0o644)
			baseline := captureTestBaseline(t, root, Options{})
			if baseline.ignore != nil {
				t.Fatal("oversized ignore files produced rules")
			}
			writeTestFile(t, root, "dist/a.js", "a\n", 0o644)
			result, err := Build(baseline, root, IntentWrite)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertChangedPaths(t, result, "dist", "dist/a.js")
		})
	}
}

func TestBuildSkipsIgnoredOutputAndPublishesSourceChanges(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "node_modules/\ndist/\n*.log\n", 0o644)
	writeTestFile(t, root, "src/app.js", "old\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})

	writeTestFile(t, root, "src/app.js", "new\n", 0o644)
	writeTestFile(t, root, "src/util.js", "util\n", 0o644)
	writeTestFile(t, root, "node_modules/left-pad/index.js", "module.exports = 1\n", 0o644)
	writeTestFile(t, root, "node_modules/left-pad/package.json", "{}\n", 0o644)
	writeTestFile(t, root, "dist/app.min.js", "minified\n", 0o644)
	writeTestFile(t, root, "src/debug.log", "trace\n", 0o644)

	result, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertChangedPaths(t, result, "src/app.js", "src/util.js")
	if err := result.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	archived, _ := readTestArchive(t, result.Artifact)
	for name := range archived {
		for _, ignored := range []string{"node_modules", "dist", "debug.log"} {
			if strings.Contains(name, ignored) {
				t.Fatalf("artifact contains ignored path %s", name)
			}
		}
	}
	if _, found := archived["files/src/util.js"]; !found {
		t.Fatalf("artifact lacks files/src/util.js: %v", archived)
	}
}

func TestBuildPublishesEditsToTrackedIgnoredPaths(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "*.log\nbuild/\n", 0o644)
	writeTestFile(t, root, "keep.log", "tracked\n", 0o644)
	writeTestFile(t, root, "build/keep.txt", "tracked\n", 0o644)
	writeTestFile(t, root, "build/remove.txt", "tracked\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})

	writeTestFile(t, root, "keep.log", "edited\n", 0o644)
	writeTestFile(t, root, "new.log", "untracked\n", 0o644)
	writeTestFile(t, root, "build/keep.txt", "edited\n", 0o644)
	writeTestFile(t, root, "build/out.o", "object\n", 0o644)
	writeTestFile(t, root, "build/obj/a.o", "object\n", 0o644)
	if err := os.Remove(filepath.Join(root, "build/remove.txt")); err != nil {
		t.Fatalf("remove tracked file: %v", err)
	}

	result, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertChangedPaths(t, result, "build/keep.txt", "keep.log")
	if !reflect.DeepEqual(result.Deletions, []Deletion{{Path: "build/remove.txt", Kind: EntryFile}}) {
		t.Fatalf("deletions = %#v", result.Deletions)
	}
}

func TestBuildUsesBaselineIgnoreRulesNotAgentEdits(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "*.tmp\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})

	writeTestFile(t, root, ".gitignore", "*.tmp\nnotes.txt\n", 0o644)
	writeTestFile(t, root, "notes.txt", "agent notes\n", 0o644)
	writeTestFile(t, root, "scratch.tmp", "scratch\n", 0o644)

	result, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertChangedPaths(t, result, ".gitignore", "notes.txt")
}

func TestBuildIgnoresAgentAddedGitignoreWithoutBaselineRules(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md", "readme\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})
	if baseline.ignore != nil {
		t.Fatal("baseline without ignore files has rules")
	}

	writeTestFile(t, root, ".gitignore", "out/\n", 0o644)
	writeTestFile(t, root, "out/a.txt", "a\n", 0o644)

	result, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertChangedPaths(t, result, ".gitignore", "out", "out/a.txt")
}

func TestBuildHonoursNestedGitignoreAndInfoExclude(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".git/info/exclude", "*.swp\n", 0o644)
	writeTestFile(t, root, ".gitignore", "*.out\n", 0o644)
	writeTestFile(t, root, "pkg/.gitignore", "gen/\n!wanted.out\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})

	writeTestFile(t, root, "main.swp", "swap\n", 0o644)
	writeTestFile(t, root, "a.out", "binary\n", 0o644)
	writeTestFile(t, root, "pkg/wanted.out", "kept\n", 0o644)
	writeTestFile(t, root, "pkg/gen/types.go", "generated\n", 0o644)
	writeTestFile(t, root, "gen/types.go", "not ignored at root\n", 0o644)

	result, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertChangedPaths(t, result, "gen", "gen/types.go", "pkg/wanted.out")
}

func TestBuildStillEnforcesProtectedAndReservedNamesUnderIgnoreRules(t *testing.T) {
	t.Run("protected", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, ".gitignore", ".ssh/\n.config\n", 0o644)
		baseline := captureTestBaseline(t, root, Options{})
		writeTestFile(t, root, ".ssh/id_ed25519", "key\n", 0o600)
		_, err := Build(baseline, root, IntentWrite)
		if !errors.Is(err, ErrExcludedPathModified) {
			t.Fatalf("Build error = %v, want ErrExcludedPathModified", err)
		}
	})
	t.Run("protected baseline content", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, ".gitignore", ".git/\n", 0o644)
		writeTestFile(t, root, ".git/config", "[core]\n", 0o644)
		baseline := captureTestBaseline(t, root, Options{})
		writeTestFile(t, root, ".git/config", "[core]\n\thooksPath = x\n", 0o644)
		_, err := Build(baseline, root, IntentWrite)
		if !errors.Is(err, ErrExcludedPathModified) {
			t.Fatalf("Build error = %v, want ErrExcludedPathModified", err)
		}
	})
	t.Run("reserved", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, ".gitignore", ".orka-artifacts/\n", 0o644)
		baseline := captureTestBaseline(t, root, Options{})
		writeTestFile(t, root, ".orka-artifacts/x", "x\n", 0o644)
		_, err := Build(baseline, root, IntentWrite)
		if !errors.Is(err, ErrReservedPath) {
			t.Fatalf("Build error = %v, want ErrReservedPath", err)
		}
	})
}

func TestBuildDoesNotCountIgnoredTreesAgainstLimits(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "node_modules/\n", 0o644)
	writeTestFile(t, root, "index.js", "old\n", 0o644)
	options := Options{Limits: Limits{MaxEntries: 8, MaxTotalBytes: 1 << 10, MaxFileBytes: 512}}
	baseline := captureTestBaseline(t, root, options)

	writeTestFile(t, root, "index.js", "new\n", 0o644)
	for i := range 20 {
		writeTestFile(t, root, fmt.Sprintf("node_modules/pkg%d/index.js", i), strings.Repeat("x", 400), 0o644)
	}
	result, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertChangedPaths(t, result, "index.js")

	// Without the rule the same tree exceeds the limits.
	if err := os.Remove(filepath.Join(root, ".gitignore")); err != nil {
		t.Fatalf("remove .gitignore: %v", err)
	}
	if _, err := Capture(root, options); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("Capture without rules error = %v, want ErrLimitExceeded", err)
	}
}

func TestBuildReadIntentTreatsIgnoredOutputAsNoChange(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "target/\n", 0o644)
	writeTestFile(t, root, "src/lib.rs", "fn main() {}\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})

	writeTestFile(t, root, "target/debug/app", "binary\n", 0o755)
	result, err := Build(baseline, root, IntentRead)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if result.Classification != ClassificationNoChange {
		t.Fatalf("classification = %q, want %q", result.Classification, ClassificationNoChange)
	}
}

func TestBaselineSnapshotDropsRetainedIgnoreContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "dist/\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})
	if baseline.ignore == nil || len(baseline.ignore.byDir[""]) != 1 {
		t.Fatalf("baseline rules = %#v", baseline.ignore)
	}
	if content := baseline.entries[".gitignore"].content; content != nil {
		t.Fatalf("baseline kept .gitignore content %q", content)
	}
}

func assertChangedPaths(t *testing.T, result Result, want ...string) {
	t.Helper()
	got := make([]string, 0, len(result.Changes))
	for _, change := range result.Changes {
		got = append(got, change.Path)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed paths = %q, want %q", got, want)
	}
}
