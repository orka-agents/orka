/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
)

// TestCommittedCasesMatchTheSource checks the committed JSONL without the
// network: it parses, and every case keeps the id, tags, title, body and
// reference its source names. `-check` against the fixture repository covers
// the SHAs and files.
func TestCommittedCasesMatchTheSource(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "pr-review", "cases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var source Source
	if err := yaml.UnmarshalStrict(raw, &source); err != nil {
		t.Fatal(err)
	}
	jsonl, err := os.ReadFile(filepath.Join("..", "pr-review", "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := evals.ParseCases(jsonl, corev1alpha1.EvalSubjectRepositoryMonitorReview)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != len(source.Cases) {
		t.Fatalf("cases.jsonl has %d cases, source has %d", len(cases), len(source.Cases))
	}
	verdicts := map[string]int{}
	for i, c := range cases {
		sc := source.Cases[i]
		in := c.Input.RepositoryMonitorReview
		if c.ID != sc.ID || strings.Join(c.Tags, ",") != strings.Join(sc.Tags, ",") ||
			in.Title != sc.Title || in.Body != strings.TrimRight(sc.Body, "\n") || in.RepoURL != source.RepoURL {
			t.Fatalf("case %d (%s) differs from its source; regenerate cases.jsonl", i, c.ID)
		}
		if c.Reference.Verdict != sc.Reference.Verdict || len(c.Reference.Locations) != len(sc.Reference.Locations) {
			t.Fatalf("case %s reference differs from its source", c.ID)
		}
		for j := range c.Reference.Locations {
			if c.Reference.Locations[j] != sc.Reference.Locations[j] {
				t.Fatalf("case %s location %d differs from its source", c.ID, j)
			}
		}
		if err := checkReference(c.Reference, in.Files); err != nil {
			t.Fatalf("case %s: %v", c.ID, err)
		}
		verdicts[c.Reference.Verdict]++
	}
	// The set stays balanced so a reviewer that blocks everything, or passes
	// everything, scores half.
	if verdicts[evals.ReferenceVerdictPassed] != verdicts[evals.ReferenceVerdictNotPassed] {
		t.Fatalf("verdicts are unbalanced: %v", verdicts)
	}
}

// testRepo is a scratch Git repository on branch main.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) testRepo {
	t.Helper()
	r := testRepo{t: t, dir: t.TempDir()}
	r.run("init", "-q", "-b", "main")
	r.run("config", "user.email", "fixtures@example.com")
	r.run("config", "user.name", "fixtures")
	r.run("config", "commit.gpgsign", "false")
	return r
}

// run runs git in the repository and returns its trimmed output.
func (r testRepo) run(args ...string) string {
	r.t.Helper()
	out, err := git(r.dir, args...)
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func (r testRepo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// commitCase commits the work tree on a new branch from main and returns
// the branch head.
func (r testRepo) commitCase(branch string, change func()) string {
	r.t.Helper()
	r.run("checkout", "-q", "-b", branch, "main")
	change()
	r.run("add", "-A")
	r.run("commit", "-q", "--allow-empty", "-m", branch)
	head := r.run("rev-parse", "HEAD")
	r.run("checkout", "-q", "main")
	return head
}

func sourceFor(branch string, reference evals.CaseReference) Source {
	return Source{
		RepoURL: "https://github.com/orka-agents/orka-eval-fixtures",
		Cases:   []SourceCase{{ID: "x", Branch: branch, Title: "change", Reference: reference}},
	}
}

var cleanReference = evals.CaseReference{Verdict: evals.ReferenceVerdictPassed}

// fixtureRepo builds a repository whose branch case/x is one commit on main
// that modifies, adds, renames and removes a file. main then moves on;
// case/two-commits adds a second commit to case/x and case/root is a root
// commit. It returns the repository and case/x's base and head.
func fixtureRepo(t *testing.T) (repo, base, head string) {
	t.Helper()
	r := newTestRepo(t)
	repo, run, write := r.dir, r.run, r.write
	write("pkg/a.go", "package pkg\n\nfunc A() int {\n\treturn 1\n}\n")
	write("pkg/old.go", "package pkg\n\n// Old stays the same when renamed.\nfunc Old() {}\n")
	write("gone.txt", "bye\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base = run("rev-parse", "HEAD")
	run("checkout", "-q", "-b", "case/x")
	write("pkg/a.go", "package pkg\n\nfunc A() int {\n\treturn 2\n}\n")
	write("pkg/new.go", "package pkg\n\nfunc New() {}\n")
	run("mv", "pkg/old.go", "pkg/renamed.go")
	run("rm", "-q", "gone.txt")
	run("add", "-A")
	run("commit", "-q", "-m", "change")
	head = run("rev-parse", "HEAD")
	// The base branch moving on, or being squash-merged into another
	// branch, must not change what the case reviews.
	run("checkout", "-q", "main")
	write("later.txt", "later\n")
	run("add", "-A")
	run("commit", "-q", "-m", "later")
	run("checkout", "-q", "-b", "case/two-commits", head)
	write("pkg/a.go", "package pkg\n\nfunc A() int {\n\treturn 3\n}\n")
	run("commit", "-q", "-am", "second")
	run("checkout", "-q", "--orphan", "case/root")
	run("commit", "-q", "-m", "root")
	return repo, base, head
}

func testSource() Source {
	return Source{
		RepoURL: "https://github.com/orka-agents/orka-eval-fixtures",
		Cases: []SourceCase{{
			ID: "x", Branch: "case/x", Tags: []string{"defect"}, Title: "change", Body: "body\n",
			Reference: evals.CaseReference{Verdict: evals.ReferenceVerdictNotPassed, Locations: []evals.ReferenceLocation{
				{File: "pkg/a.go", StartLine: 4, EndLine: 4},
			}},
		}},
	}
}

func TestGenerateRendersForgeStylePatches(t *testing.T) {
	repo, base, head := fixtureRepo(t)
	out, err := Generate(testSource(), repo)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := evals.ParseCases(out, corev1alpha1.EvalSubjectRepositoryMonitorReview)
	if err != nil {
		t.Fatal(err)
	}
	in := cases[0].Input.RepositoryMonitorReview
	if in.BaseSHA != base || in.HeadSHA != head || in.Body != "body" {
		t.Fatalf("base %s head %s body %q", in.BaseSHA, in.HeadSHA, in.Body)
	}
	byName := map[string]evals.ReviewCaseFile{}
	for _, f := range in.Files {
		byName[f.Filename] = f
	}
	if f := byName["pkg/a.go"]; f.Status != "modified" || f.Additions != 1 || f.Deletions != 1 ||
		f.Patch != "@@ -1,5 +1,5 @@\n package pkg\n \n func A() int {\n-\treturn 1\n+\treturn 2\n }" {
		t.Fatalf("modified file = %+v", f)
	}
	if f := byName["pkg/new.go"]; f.Status != "added" || f.Additions != 3 ||
		!strings.HasPrefix(f.Patch, "@@ -0,0 +1,3 @@") {
		t.Fatalf("added file = %+v", f)
	}
	if f := byName["gone.txt"]; f.Status != "removed" || f.Deletions != 1 {
		t.Fatalf("removed file = %+v", f)
	}
	if f := byName["pkg/renamed.go"]; f.Status != "renamed" || f.PreviousFilename != "pkg/old.go" || f.Patch != "" {
		t.Fatalf("renamed file = %+v", f)
	}
	if _, ok := byName["later.txt"]; ok {
		t.Fatal("a later base commit leaked into the case")
	}
}

func TestGenerateChecksReferencesAndCommits(t *testing.T) {
	repo, _, head := fixtureRepo(t)
	source := testSource()
	// A reference must point at a line the change adds.
	source.Cases[0].Reference.Locations[0] = evals.ReferenceLocation{File: "pkg/a.go", StartLine: 1, EndLine: 3}
	if _, err := Generate(source, repo); err == nil || !strings.Contains(err.Error(), "covers no added line") {
		t.Fatalf("Generate with an unchanged reference line: %v", err)
	}
	source.Cases[0].Reference.Locations[0] = evals.ReferenceLocation{File: "pkg/other.go", StartLine: 1, EndLine: 1}
	if _, err := Generate(source, repo); err == nil || !strings.Contains(err.Error(), "does not touch") {
		t.Fatalf("Generate with an untouched reference file: %v", err)
	}

	// A two-commit branch reviews only its last commit; a root commit has no
	// base and is rejected.
	source.Cases[0].Reference.Locations[0] = evals.ReferenceLocation{File: "pkg/a.go", StartLine: 4, EndLine: 4}
	source.Cases[0].Branch = "case/two-commits"
	out, err := Generate(source, repo)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := evals.ParseCases(out, corev1alpha1.EvalSubjectRepositoryMonitorReview)
	if err != nil {
		t.Fatal(err)
	}
	if in := cases[0].Input.RepositoryMonitorReview; in.BaseSHA != head || len(in.Files) != 1 {
		t.Fatalf("two-commit branch: base %s, %d files", in.BaseSHA, len(in.Files))
	}
	source.Cases[0].Branch = "case/root"
	if _, err := Generate(source, repo); err == nil || !strings.Contains(err.Error(), "exactly one parent") {
		t.Fatalf("Generate with a root commit: %v", err)
	}
}

func TestGenerateKeepsPatchBytesAndLiteralPaths(t *testing.T) {
	r := newTestRepo(t)
	r.write("pkg/a[1].go", "package pkg\n")
	r.write("pkg/a1.go", "package pkg\n")
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.commitCase("case/x", func() {
		// The bracketed name would match a1.go as a pathspec pattern, and the
		// added lines end in whitespace a trim would drop.
		r.write("pkg/a[1].go", "package pkg\n\nvar A = 1  \n\t\n")
		r.write("pkg/a1.go", "package pkg\n\nvar B = 2\n")
	})
	out, err := Generate(sourceFor("case/x", cleanReference), r.dir)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := evals.ParseCases(out, corev1alpha1.EvalSubjectRepositoryMonitorReview)
	if err != nil {
		t.Fatal(err)
	}
	files := cases[0].Input.RepositoryMonitorReview.Files
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	for _, f := range files {
		switch f.Filename {
		case "pkg/a[1].go":
			if f.Additions != 3 || f.Patch != "@@ -1 +1,4 @@\n package pkg\n+\n+var A = 1  \n+\t" {
				t.Fatalf("bracketed file = %+v", f)
			}
		case "pkg/a1.go":
			if f.Additions != 2 || strings.Contains(f.Patch, "var A") {
				t.Fatalf("plain file = %+v", f)
			}
		default:
			t.Fatalf("unexpected file %s", f.Filename)
		}
	}
}

func TestGenerateResolvesBranchesInOrdinaryClones(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	head := r.commitCase("case/x", func() { r.write("a.txt", "b\n") })
	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := git("", "clone", "-q", r.dir, clone); err != nil {
		t.Fatal(err)
	}
	// An ordinary clone has case/x only as origin/case/x.
	if _, err := revParse(clone, "refs/heads/case/x"); err == nil {
		t.Fatal("the clone unexpectedly has a local case/x")
	}
	out, err := Generate(sourceFor("case/x", cleanReference), clone)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), head) {
		t.Fatalf("case does not pin %s: %s", head, out)
	}
	if _, err := Generate(sourceFor("case/missing", cleanReference), clone); err == nil {
		t.Fatal("Generate resolved a missing branch")
	}
}

func TestGenerateRejectsEmptyCommits(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.commitCase("case/empty", func() {})
	if _, err := Generate(sourceFor("case/empty", cleanReference), r.dir); err == nil ||
		!strings.Contains(err.Error(), "changes nothing") {
		t.Fatalf("Generate with an empty commit: %v", err)
	}
}

func TestRunRejectsCredentialURLsBeforeGit(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "cases.yaml")
	secret := "ghp_" + strings.Repeat("a", 36)
	content := "repoURL: https://x:" + secret + "@github.com/orka-agents/orka-eval-fixtures\ncases: []\n"
	if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(source, filepath.Join(dir, "out.jsonl"), "", false)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "anonymous") {
		t.Fatalf("run with a credential URL: %v", err)
	}
}

func TestGitIgnoresCallerConfiguration(t *testing.T) {
	env := gitEnv([]string{
		"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_DIR=/x",
		"HOME=/h", "CURL_HOME=/c", "XDG_CONFIG_HOME=/x",
	})
	joined := strings.Join(env, "\n")
	// HOME holds ~/.netrc, which the HTTPS transport would use.
	dropped := []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_DIR", "HOME=/h", "CURL_HOME", "XDG_CONFIG_HOME"}
	for _, dropped := range dropped {
		if strings.Contains(joined, dropped) {
			t.Fatalf("%s survived: %v", dropped, env)
		}
	}
	kept := []string{"PATH=/bin", "HOME=" + os.DevNull, "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"}
	for _, kept := range kept {
		if !strings.Contains(joined, kept) {
			t.Fatalf("%s missing: %v", kept, env)
		}
	}
	// A global extraHeader must not reach Git.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Bearer leaked")
	r := newTestRepo(t)
	if out, err := git(r.dir, "config", "--get", "http.extraHeader"); err == nil || strings.TrimSpace(out) != "" {
		t.Fatalf("git saw the caller's extraHeader: %q, %v", out, err)
	}
}

func TestGenerateRejectsAFileThatBecomesADirectory(t *testing.T) {
	r := newTestRepo(t)
	r.write("config", "x\n")
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.commitCase("case/x", func() {
		r.run("rm", "-q", "config")
		r.write("config/defaults.go", "package config\n")
	})
	if _, err := Generate(sourceFor("case/x", cleanReference), r.dir); err == nil ||
		!strings.Contains(err.Error(), "overlaps another changed path") {
		t.Fatalf("Generate with a file replaced by a directory: %v", err)
	}
}

func TestCheckReferenceIsBoundedByAddedLines(t *testing.T) {
	files := []evals.ReviewCaseFile{{Filename: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b"}}
	huge := evals.CaseReference{Verdict: evals.ReferenceVerdictNotPassed, Locations: []evals.ReferenceLocation{
		{File: "a.go", StartLine: 2, EndLine: math.MaxInt64},
	}}
	done := make(chan error, 1)
	go func() { done <- checkReference(huge, files) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a range past every added line was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("checkReference walked the reference range")
	}
	huge.Locations[0].StartLine = 1
	if err := checkReference(huge, files); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateResolvesOnlyPlainBranchNames(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.commitCase("case/x", func() { r.write("a.txt", "b\n") })
	secret := "sk-" + strings.Repeat("a", 24)
	for _, branch := range []string{"case/x~1", "case/x^", "case/x@{0}", "case/x:a.txt", "case/../x", "-x", secret} {
		_, err := Generate(sourceFor(branch, cleanReference), r.dir)
		if err == nil {
			t.Fatalf("Generate resolved branch %q", branch)
		}
		if strings.Contains(err.Error(), branch) {
			t.Fatalf("error echoes the branch value: %v", err)
		}
	}
}

func TestGenerateIgnoresTextconvFilters(t *testing.T) {
	r := newTestRepo(t)
	// A local textconv driver would rewrite the diff if Git ran it.
	r.run("config", "diff.upper.textconv", "tr a-z A-Z <")
	r.write(".gitattributes", "*.txt diff=upper\n")
	r.write("a.txt", "a\n")
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.commitCase("case/x", func() { r.write("a.txt", "b\n") })
	out, err := Generate(sourceFor("case/x", cleanReference), r.dir)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := evals.ParseCases(out, corev1alpha1.EvalSubjectRepositoryMonitorReview)
	if err != nil {
		t.Fatal(err)
	}
	if patch := cases[0].Input.RepositoryMonitorReview.Files[0].Patch; patch != "@@ -1 +1 @@\n-a\n+b" {
		t.Fatalf("patch = %q", patch)
	}
}

func TestCheckReferenceNeverEchoesTheFile(t *testing.T) {
	secret := "ghp_" + strings.Repeat("b", 36)
	files := []evals.ReviewCaseFile{{Filename: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b"}}
	for _, location := range []evals.ReferenceLocation{
		{File: secret + "/a.go", StartLine: 1, EndLine: 1},
		{File: "a.go", StartLine: 5, EndLine: 6},
	} {
		reference := evals.CaseReference{
			Verdict: evals.ReferenceVerdictNotPassed, Locations: []evals.ReferenceLocation{location},
		}
		err := checkReference(reference, files)
		if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "a.go") {
			t.Fatalf("checkReference(%s): %v", location.File, err)
		}
	}
}

func TestGenerateIgnoresLocalDiffSettings(t *testing.T) {
	lines := func(changed map[int]string) string {
		var b strings.Builder
		for i := 1; i <= 20; i++ {
			if text, ok := changed[i]; ok {
				b.WriteString(text + "\n")
				continue
			}
			fmt.Fprintf(&b, "line %d\n", i)
		}
		return b.String()
	}
	build := func(t *testing.T, config ...string) string {
		r := newTestRepo(t)
		for i := 0; i+1 < len(config); i += 2 {
			r.run("config", config[i], config[i+1])
		}
		r.write("a.txt", lines(nil))
		r.run("add", "-A")
		r.run("commit", "-q", "-m", "base")
		// Two edits nine lines apart: separate hunks with default settings,
		// one hunk if the clone's diff.interHunkContext were honored.
		r.commitCase("case/x", func() { r.write("a.txt", lines(map[int]string{3: "three", 12: "twelve"})) })
		out, err := Generate(sourceFor("case/x", cleanReference), r.dir)
		if err != nil {
			t.Fatal(err)
		}
		cases, err := evals.ParseCases(out, corev1alpha1.EvalSubjectRepositoryMonitorReview)
		if err != nil {
			t.Fatal(err)
		}
		return cases[0].Input.RepositoryMonitorReview.Files[0].Patch
	}
	plain := build(t)
	if strings.Count(plain, "\n@@ ")+1 != 2 {
		t.Fatalf("expected two hunks:\n%s", plain)
	}
	if tuned := build(t, "diff.algorithm", "patience", "diff.indentHeuristic", "false",
		"diff.interHunkContext", "10", "diff.relative", "true"); tuned != plain {
		t.Fatalf("local diff settings changed the patch:\n%s\nvs\n%s", plain, tuned)
	}
}

func TestGenerateIgnoresLocalFileOrder(t *testing.T) {
	order := func(t *testing.T, orderFile bool) []string {
		r := newTestRepo(t)
		if orderFile {
			r.write(".git/order", "z.txt\n")
			r.run("config", "diff.orderFile", ".git/order")
		}
		r.write("a.txt", "a\n")
		r.write("z.txt", "z\n")
		r.run("add", "-A")
		r.run("commit", "-q", "-m", "base")
		r.commitCase("case/x", func() {
			r.write("a.txt", "a2\n")
			r.write("z.txt", "z2\n")
		})
		out, err := Generate(sourceFor("case/x", cleanReference), r.dir)
		if err != nil {
			t.Fatal(err)
		}
		cases, err := evals.ParseCases(out, corev1alpha1.EvalSubjectRepositoryMonitorReview)
		if err != nil {
			t.Fatal(err)
		}
		files := cases[0].Input.RepositoryMonitorReview.Files
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, f.Filename)
		}
		return names
	}
	if plain, ordered := order(t, false), order(t, true); strings.Join(plain, ",") != "a.txt,z.txt" ||
		strings.Join(ordered, ",") != strings.Join(plain, ",") {
		t.Fatalf("file order = %v with diff.orderFile, %v without", ordered, plain)
	}
}

func TestOverlapErrorNamesTheFileByPosition(t *testing.T) {
	r := newTestRepo(t)
	secret := "ghp_" + strings.Repeat("c", 36)
	r.write(secret, "x\n")
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.commitCase("case/x", func() {
		r.run("rm", "-q", secret)
		r.write(secret+"/a.go", "package a\n")
	})
	_, err := Generate(sourceFor("case/x", cleanReference), r.dir)
	if err == nil || !strings.Contains(err.Error(), "overlaps another changed path") ||
		strings.Contains(err.Error(), secret) {
		t.Fatalf("Generate error = %v", err)
	}
}

// A clone's info/attributes can mark text as binary; generation reads a fresh
// mirror, so local attributes never reach the diff.
func TestGenerateIgnoresLocalAttributes(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	r.run("add", "-A")
	r.run("commit", "-q", "-m", "base")
	r.commitCase("case/x", func() { r.write("a.go", "package a\n\nvar A = 1\n") })
	r.write(".git/info/attributes", "*.go -diff\n")
	out, err := Generate(sourceFor("case/x", cleanReference), r.dir)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := evals.ParseCases(out, corev1alpha1.EvalSubjectRepositoryMonitorReview)
	if err != nil {
		t.Fatal(err)
	}
	if f := cases[0].Input.RepositoryMonitorReview.Files[0]; f.Additions != 2 || f.Patch == "" {
		t.Fatalf("file = %+v, want the text diff", f)
	}
}
