/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// fixtureRepo builds a repository whose branch case/x is one commit on main
// that modifies, adds, renames and removes a file. main then moves on;
// case/two-commits adds a second commit to case/x and case/root is a root
// commit. It returns the repository and case/x's base and head.
func fixtureRepo(t *testing.T) (repo, base, head string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := git(repo, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "fixtures@example.com")
	run("config", "user.name", "fixtures")
	run("config", "commit.gpgsign", "false")
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
