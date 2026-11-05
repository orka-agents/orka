/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Command genreviewcases generates the orka.eval.case.v1 cases of the PR
// review eval from the fixture repository, so each case's files match exactly
// what a trial clones.
//
// The source file names each case's branch, title, body and reference. Each
// branch is one commit; the command pins it as the head SHA and its parent as
// the base SHA, so where the fixture repository's main branch moves never
// changes a case. It renders each
// changed file the way the forge pull request files API does, checks that
// every reference location points at a line the change adds, and validates
// the result with the case parser Orka uses.
//
//	go run ./test/evals/genreviewcases                # rewrite cases.jsonl
//	go run ./test/evals/genreviewcases -check         # fail if it is stale
//	go run ./test/evals/genreviewcases -repo <clone>  # use a local clone
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
)

const (
	defaultSource = "test/evals/pr-review/cases.yaml"
	defaultOut    = "test/evals/pr-review/cases.jsonl"
)

// Source is the hand-written description of the case set.
type Source struct {
	RepoURL string       `json:"repoURL"`
	Cases   []SourceCase `json:"cases"`
}

// SourceCase is one case before its SHAs and files are resolved.
type SourceCase struct {
	ID        string              `json:"id"`
	Branch    string              `json:"branch"`
	Tags      []string            `json:"tags,omitempty"`
	Title     string              `json:"title"`
	Body      string              `json:"body,omitempty"`
	Reference evals.CaseReference `json:"reference"`
}

func main() {
	source := flag.String("source", defaultSource, "case source file")
	out := flag.String("out", defaultOut, "generated JSONL file")
	repo := flag.String("repo", "", "local clone of the fixture repository; empty clones repoURL anonymously")
	check := flag.Bool("check", false, "fail when -out differs from the generated cases instead of rewriting it")
	flag.Parse()
	if err := run(*source, *out, *repo, *check); err != nil {
		fmt.Fprintln(os.Stderr, "genreviewcases:", err)
		os.Exit(1)
	}
}

func run(sourcePath, outPath, repo string, check bool) error {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	var source Source
	if err := yaml.UnmarshalStrict(raw, &source); err != nil {
		return fmt.Errorf("%s: %w", sourcePath, err)
	}
	if repo == "" {
		dir, err := os.MkdirTemp("", "orka-eval-fixtures-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		// A bare mirror keeps every branch as a local ref.
		if _, err := git("", "clone", "--quiet", "--mirror", source.RepoURL, dir); err != nil {
			return err
		}
		repo = dir
	}
	generated, err := Generate(source, repo)
	if err != nil {
		return err
	}
	if check {
		current, err := os.ReadFile(outPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, generated) {
			return fmt.Errorf("%s is stale; run go run ./test/evals/genreviewcases", outPath)
		}
		return nil
	}
	return os.WriteFile(outPath, generated, 0o644)
}

// Generate resolves every case against the repository at repo and returns
// the validated JSONL document.
func Generate(source Source, repo string) ([]byte, error) {
	if len(source.Cases) == 0 {
		return nil, errors.New("source has no cases")
	}
	var buf bytes.Buffer
	for _, sc := range source.Cases {
		c, err := resolveCase(source, sc, repo)
		if err != nil {
			return nil, fmt.Errorf("case %q: %w", sc.ID, err)
		}
		encoder := json.NewEncoder(&buf)
		// Patches are code; keep <, > and & readable.
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(c); err != nil {
			return nil, err
		}
	}
	if _, err := evals.ParseCases(buf.Bytes(), corev1alpha1.EvalSubjectRepositoryMonitorReview); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func resolveCase(source Source, sc SourceCase, repo string) (evals.Case, error) {
	head, err := revParse(repo, "refs/heads/"+sc.Branch)
	if err != nil {
		return evals.Case{}, err
	}
	// A case is one commit: a merge or a root commit has no single base.
	parents, err := git(repo, "rev-list", "--parents", "-n", "1", head)
	if err != nil {
		return evals.Case{}, err
	}
	commits := strings.Fields(parents)
	if len(commits) != 2 {
		return evals.Case{}, fmt.Errorf("branch %s must end in a commit with exactly one parent", sc.Branch)
	}
	base := commits[1]
	files, err := changedFiles(repo, base, head)
	if err != nil {
		return evals.Case{}, err
	}
	if err := checkReference(sc.Reference, files); err != nil {
		return evals.Case{}, err
	}
	return evals.Case{
		SchemaVersion: corev1alpha1.EvalCaseSchemaVersion,
		ID:            sc.ID,
		Tags:          sc.Tags,
		Input: evals.CaseInput{RepositoryMonitorReview: &evals.ReviewCaseInput{
			RepoURL: source.RepoURL,
			BaseSHA: base,
			HeadSHA: head,
			Title:   sc.Title,
			Body:    strings.TrimRight(sc.Body, "\n"),
			Files:   files,
		}},
		Reference: sc.Reference,
	}, nil
}

// changedFiles renders the diff from base to head like the forge pull request
// files API: one entry per file, with hunks but no file headers.
func changedFiles(repo, base, head string) ([]evals.ReviewCaseFile, error) {
	statusOut, err := git(repo, "diff", "--no-color", "--no-ext-diff", "-M", "--name-status", "-z", base, head)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(statusOut, "\x00"), "\x00")
	var files []evals.ReviewCaseFile
	for i := 0; i < len(fields); {
		code := fields[i]
		i++
		file := evals.ReviewCaseFile{}
		paths := []string{}
		switch code[0] {
		case 'A':
			file.Status = "added"
		case 'D':
			file.Status = "removed"
		case 'M', 'T':
			file.Status = "modified"
		case 'R':
			file.Status = "renamed"
		case 'C':
			file.Status = "copied"
		default:
			return nil, fmt.Errorf("unsupported diff status %q", code)
		}
		if file.Status == "renamed" || file.Status == "copied" {
			if i+1 >= len(fields) {
				return nil, errors.New("truncated rename in diff status")
			}
			file.PreviousFilename, file.Filename = fields[i], fields[i+1]
			paths = append(paths, fields[i], fields[i+1])
			i += 2
		} else {
			if i >= len(fields) {
				return nil, errors.New("truncated diff status")
			}
			file.Filename = fields[i]
			paths = append(paths, fields[i])
			i++
		}
		numstat, err := gitDiff(repo, base, head, "--numstat", paths)
		if err != nil {
			return nil, err
		}
		counts := strings.Fields(numstat)
		if len(counts) < 2 {
			return nil, fmt.Errorf("no numstat for %s", file.Filename)
		}
		// Binary files report "-" and carry no patch.
		if counts[0] != "-" {
			if file.Additions, err = strconv.Atoi(counts[0]); err != nil {
				return nil, err
			}
			if file.Deletions, err = strconv.Atoi(counts[1]); err != nil {
				return nil, err
			}
			diff, err := gitDiff(repo, base, head, "-U3", paths)
			if err != nil {
				return nil, err
			}
			file.Patch = hunks(diff)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, errors.New("branch changes nothing")
	}
	return files, nil
}

// hunks drops the file header of a one-file git diff, keeping everything from
// the first hunk header on, without the final newline.
func hunks(diff string) string {
	start := strings.Index(diff, "\n@@ ")
	if start < 0 {
		return ""
	}
	return strings.TrimSuffix(diff[start+1:], "\n")
}

// addedLines returns the head line numbers a patch adds.
func addedLines(patch string) (map[int64]bool, error) {
	added := map[int64]bool{}
	var line int64
	for text := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(text, "@@ "):
			// @@ -a,b +c,d @@
			plus := strings.Fields(text)
			if len(plus) < 3 || !strings.HasPrefix(plus[2], "+") {
				return nil, fmt.Errorf("bad hunk header %q", text)
			}
			start, _, _ := strings.Cut(strings.TrimPrefix(plus[2], "+"), ",")
			n, err := strconv.ParseInt(start, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad hunk header %q", text)
			}
			line = n
		case strings.HasPrefix(text, "+"):
			added[line] = true
			line++
		case strings.HasPrefix(text, "-"), strings.HasPrefix(text, `\`):
		default:
			line++
		}
	}
	return added, nil
}

// checkReference requires every defect location to cover a line the change
// adds, so a reference can't point at code the reviewer never sees change.
func checkReference(reference evals.CaseReference, files []evals.ReviewCaseFile) error {
	byName := make(map[string]evals.ReviewCaseFile, len(files))
	for _, file := range files {
		byName[file.Filename] = file
	}
	for i, location := range reference.Locations {
		file, ok := byName[location.File]
		if !ok {
			return fmt.Errorf("reference location %d names %s, which the change does not touch", i, location.File)
		}
		added, err := addedLines(file.Patch)
		if err != nil {
			return err
		}
		covered := false
		for line := location.StartLine; line <= location.EndLine; line++ {
			if added[line] {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("reference location %d (%s:%d-%d) covers no added line",
				i, location.File, location.StartLine, location.EndLine)
		}
	}
	return nil
}

// gitDiff runs one git diff from base to head, limited to paths.
func gitDiff(repo, base, head, format string, paths []string) (string, error) {
	args := append([]string{"diff", "--no-color", "--no-ext-diff", "-M", format, base, head, "--"}, paths...)
	return git(repo, args...)
}

func revParse(repo, ref string) (string, error) {
	return git(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = filepath.Clean(dir)
	}
	// Never prompt: the fixture repository is public and read anonymously.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
