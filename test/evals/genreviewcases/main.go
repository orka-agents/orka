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
	"regexp"
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
	// Check the URL before Git sees it, so a credential pasted into it never
	// reaches a clone or an error message.
	if err := evals.ValidateFixtureRepoURL(source.RepoURL); err != nil {
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
	for i, sc := range source.Cases {
		c, err := resolveCase(source, sc, repo)
		if err != nil {
			// Name the case by position: its id is not validated yet and may
			// carry a credential.
			return nil, fmt.Errorf("cases[%d]: %w", i, err)
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
	head, err := resolveBranch(repo, sc.Branch)
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
		return evals.Case{}, errors.New("branch must end in a commit with exactly one parent")
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
	statusOut, err := gitDiff(repo, base, head, nil, "--name-status", "-z")
	if err != nil {
		return nil, err
	}
	if statusOut == "" {
		return nil, errors.New("branch changes nothing")
	}
	fields := strings.Split(strings.TrimSuffix(statusOut, "\x00"), "\x00")
	var files []evals.ReviewCaseFile
	for i := 0; i < len(fields); {
		code := fields[i]
		i++
		if code == "" {
			return nil, errors.New("empty diff status")
		}
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
		numstat, err := gitDiff(repo, base, head, paths, "--numstat")
		if err != nil {
			return nil, err
		}
		// A pathspec also matches everything under a directory of that name,
		// as when a file becomes a directory. Fail rather than attribute
		// another path's changes to this file.
		if records := strings.Count(strings.TrimSuffix(numstat, "\n"), "\n") + 1; numstat == "" || records != 1 {
			return nil, fmt.Errorf("%s overlaps another changed path; give it its own case", file.Filename)
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
			diff, err := gitDiff(repo, base, head, paths, "-U3")
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
			// Never echo the path: it is not validated yet and may carry a
			// credential. The index identifies the location.
			return fmt.Errorf("reference location %d names a file the change does not touch", i)
		}
		added, err := addedLines(file.Patch)
		if err != nil {
			return err
		}
		// Walk the added lines, not the range: the range is not bounded yet.
		covered := false
		for line := range added {
			if line >= location.StartLine && line <= location.EndLine {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("reference location %d (lines %d-%d) covers no added line",
				i, location.StartLine, location.EndLine)
		}
	}
	return nil
}

// gitDiff runs one git diff from base to head, limited to paths. External
// diff drivers and textconv filters stay off, so the output is the
// committed change.
func gitDiff(repo, base, head string, paths []string, formats ...string) (string, error) {
	args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "-M"}, formats...)
	args = append(args, base, head, "--")
	return git(repo, append(args, paths...)...)
}

// resolveBranch finds a fixture branch as a local branch, as in the mirror
// clone run makes, or as a remote-tracking branch, as in an ordinary clone.
func resolveBranch(repo, branch string) (string, error) {
	// A plain name only: revision syntax such as "main~1" or "x@{1}" would
	// resolve to some other commit. Never echo a rejected value.
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") {
		return "", errors.New("branch must be a plain branch name")
	}
	for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		if sha, err := revParse(repo, ref); err == nil {
			return sha, nil
		}
	}
	return "", errors.New("branch is neither a local nor an origin branch")
}

var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

func revParse(repo, ref string) (string, error) {
	out, err := git(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return strings.TrimSpace(out), err
}

// git runs Git without the caller's configuration or credentials: no
// global or system config, no inherited GIT_* overrides, no credential
// helper and no prompt. The fixture repository is public and read
// anonymously, and the output must not depend on local settings. Paths are
// literal, never pathspec patterns. The output is returned unmodified.
func git(dir string, args ...string) (string, error) {
	args = append([]string{"-c", "credential.helper=", "--literal-pathspecs"}, args...)
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = filepath.Clean(dir)
	}
	cmd.Env = gitEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Name only the subcommand: arguments can hold a repository URL.
		return "", fmt.Errorf("git %s: %w: %s", gitSubcommand(args), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// gitEnv drops every inherited GIT_* variable and points Git's global and
// system configuration at nothing. HOME points at nothing too, so the HTTPS
// transport can't read the caller's ~/.netrc: a clone that needs credentials
// fails here as it would in a credential-free trial.
func gitEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+5)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "GIT_") || name == "HOME" || name == "CURL_HOME" || name == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
}

func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return ""
}
