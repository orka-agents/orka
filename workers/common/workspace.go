/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/workerenv"
)

const requirePushBranchEnvVar = workerenv.RequirePushBranch

var waitForRemoteBranchVisibility = waitForRemoteBranchVisibilityWithGit

// PrepareWorkspace applies the diff from a prior task's result to the working
// directory. It is called after git clone and before the LLM agent starts.
// If ORKA_PRIOR_TASK is not set the function is a no-op.
func PrepareWorkspace(workDir string) error {
	priorTask := os.Getenv(workerenv.PriorTask)
	if priorTask == "" {
		return nil
	}

	ns := os.Getenv(workerenv.PriorTaskNamespace)
	if ns == "" {
		ns = os.Getenv(workerenv.TaskNamespace)
	}

	controllerURL := os.Getenv(workerenv.ControllerURL)
	if controllerURL == "" {
		return fmt.Errorf("%s must be set when %s is specified", workerenv.ControllerURL, workerenv.PriorTask)
	}
	controllerURL = strings.TrimRight(controllerURL, "/")

	// Read SA token for authentication.
	saToken := workerServiceAccountToken()

	// Fetch the prior task's result.
	url := fmt.Sprintf("%s/api/v1/tasks/%s/result?namespace=%s", controllerURL, priorTask, ns)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for prior task result: %w", err)
	}
	if saToken != "" {
		req.Header.Set("Authorization", "Bearer "+saToken)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch prior task result: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("prior task result HTTP %d: %s", resp.StatusCode, string(body))
	}

	var resultResp struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resultResp); err != nil {
		return fmt.Errorf("failed to decode prior task result: %w", err)
	}

	sr := ParseStructuredResult(resultResp.Result)
	if sr.Diff == "" {
		return nil
	}

	// Warn if HEAD doesn't match the base SHA of the prior result.
	if sr.BaseSHA != "" {
		headOut, err := execGit(workDir, "rev-parse", "HEAD")
		if err == nil {
			headSHA := strings.TrimSpace(headOut)
			if headSHA != sr.BaseSHA {
				fmt.Fprintf(os.Stderr, "warning: current HEAD %s does not match prior task baseSHA %s\n", headSHA, sr.BaseSHA)
			}
		}
	}

	// Write diff to a temp file.
	diffPath := filepath.Join(workDir, ".orka-prior.patch")
	if err := os.WriteFile(diffPath, []byte(sr.Diff), 0o600); err != nil {
		return fmt.Errorf("failed to write diff to temp file: %w", err)
	}
	defer os.Remove(diffPath) //nolint:errcheck

	// Dry-run check.
	if out, err := execGit(workDir, "apply", "--check", diffPath); err != nil {
		return fmt.Errorf("git apply --check failed: %s: %w", out, err)
	}

	// Apply the diff.
	if out, err := execGit(workDir, "apply", diffPath); err != nil {
		return fmt.Errorf("git apply failed: %s: %w", out, err)
	}

	fmt.Fprintf(os.Stderr, "successfully applied prior task diff from %s\n", priorTask)
	return nil
}

// FinalizeResult builds a structured result from the agent output and any
// uncommitted changes in workDir. If workDir is empty, it returns the raw
// agent output as plain text bytes.
// When ORKA_PUSH_BRANCH is set, changes are committed and pushed to that branch.
func FinalizeResult(workDir string, agentOutput string) ([]byte, error) {
	if workDir == "" {
		return []byte(agentOutput), nil
	}

	baseSHA, err := execGit(workDir, "rev-parse", "HEAD")
	if err != nil {
		// Not a git repo or git not available — fall back to plain text
		fmt.Fprintf(os.Stderr, "warning: git rev-parse HEAD failed in %s, returning plain text result\n", workDir)
		return []byte(agentOutput), nil
	}
	baseSHA = strings.TrimSpace(baseSHA)

	// Stage any new untracked files so they appear in the diff
	execGit(workDir, "add", "-A") //nolint:errcheck
	resetReservedWorkspacePaths(workDir)

	diff, err := execGit(workDir, "diff", "--cached", "--binary", "--full-index")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: git diff failed in %s, returning plain text result\n", workDir)
		return []byte(agentOutput), nil
	}

	var files []string
	stat, err := execGit(workDir, "diff", "--cached", "--stat")
	if err == nil {
		files = parseDiffStatFiles(stat)
	}
	unstagedStat, _ := execGit(workDir, "diff", "--stat")
	if unstagedStat != "" {
		files = append(files, parseDiffStatFiles(unstagedStat)...)
	}

	sr := &StructuredResult{
		Summary: TruncateStructuredSummary(agentOutput),
		BaseSHA: baseSHA,
		HeadSHA: baseSHA,
	}
	if diff != "" {
		sr.Diff = diff
		sr.Files = files
	}

	// Auto-push if ORKA_PUSH_BRANCH is set and there are changes.
	pushBranch := os.Getenv(workerenv.PushBranch)
	requirePushBranch := strings.EqualFold(os.Getenv(requirePushBranchEnvVar), "true")
	allowEmptyPushBranch := strings.EqualFold(os.Getenv(workerenv.AllowEmptyPushBranch), "true")
	if requirePushBranch && pushBranch == "" {
		return nil, fmt.Errorf("%s is true but %s is empty", requirePushBranchEnvVar, workerenv.PushBranch)
	}
	if pushBranch != "" {
		if diff == "" {
			if requirePushBranch && !allowEmptyPushBranch {
				return nil, fmt.Errorf("%s=%s but no workspace diff was produced", workerenv.PushBranch, pushBranch)
			}
			if requirePushBranch && allowEmptyPushBranch {
				if pushErr := validateEmptyPushBranch(
					workDir,
					pushBranch,
					os.Getenv(workerenv.PRBaseBranch),
					os.Getenv(workerenv.PRBaseRepo),
					os.Getenv(workerenv.PRBaseSHA),
				); pushErr != nil {
					sr.PushError = pushErr.Error()
					return nil, fmt.Errorf("%s=%s but no workspace diff was produced: %w", workerenv.PushBranch, pushBranch, pushErr)
				}
				if pushErr := pushCurrentHEAD(workDir, pushBranch); pushErr != nil {
					sr.PushError = pushErr.Error()
					return nil, fmt.Errorf("failed to push to %s: %w", pushBranch, pushErr)
				}
				fmt.Fprintf(os.Stderr, "pushed current HEAD to branch %s\n", pushBranch)
				sr.PushBranch = pushBranch
			}
		} else if pushErr := pushChanges(workDir, pushBranch); pushErr != nil {
			sr.PushError = pushErr.Error()
			if requirePushBranch {
				return nil, fmt.Errorf("failed to push to %s: %w", pushBranch, pushErr)
			}
			fmt.Fprintf(os.Stderr, "warning: failed to push to %s: %v\n", pushBranch, pushErr)
		} else {
			fmt.Fprintf(os.Stderr, "pushed changes to branch %s\n", pushBranch)
			sr.PushBranch = pushBranch
		}
	}

	if out, err := execGit(workDir, "rev-parse", "HEAD"); err == nil {
		sr.HeadSHA = strings.TrimSpace(out)
	}

	return FormatStructuredResult(sr)
}

// pushChanges commits all changes and pushes to the given remote branch.
func pushChanges(workDir, branch string) error {
	// Ensure everything is staged
	if _, err := execGit(workDir, "add", "-A"); err != nil {
		return fmt.Errorf("git add failed: %w", err)
	}
	resetReservedWorkspacePaths(workDir)

	// Check if there's anything to commit
	status, _ := execGit(workDir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		return nil // nothing to commit
	}

	// Configure committer identity
	execGit(workDir, "config", "user.email", "orka@ai") //nolint:errcheck
	execGit(workDir, "config", "user.name", "Orka AI")  //nolint:errcheck

	// Commit
	if out, err := execGit(workDir, "commit", "-m", "feat: changes from orka agent"); err != nil {
		return fmt.Errorf("git commit failed: %s: %w", out, err)
	}

	return pushCurrentHEAD(workDir, branch)
}

// pushCurrentHEAD pushes the current commit without creating a new commit.
func pushCurrentHEAD(workDir, branch string) error {
	// Create/reset the branch locally (handles pre-existing local branch names)
	if out, err := execGit(workDir, "checkout", "-B", branch); err != nil {
		return fmt.Errorf("git checkout -B %s failed: %s: %w", branch, out, err)
	}

	if out, err := execGit(workDir, "push", "-u", "origin", branch); err != nil {
		return fmt.Errorf("git push failed: %s: %w", out, err)
	}
	if err := waitForRemoteBranchVisibility(workDir, "origin", branch, 15*time.Second); err != nil {
		return fmt.Errorf("remote branch %s not visible after push: %w", branch, err)
	}

	return nil
}

func validateEmptyPushBranch(workDir, pushBranch, baseBranch, baseRepo, baseSHA string) error {
	advanced, err := headAdvancedFromRemoteBranch(workDir, pushBranch)
	if err != nil {
		return err
	}
	if advanced {
		return nil
	}

	baseSHA = strings.TrimSpace(baseSHA)
	if baseSHA != "" {
		containsBase, err := headContainsBaseSHA(workDir, baseSHA, baseRepo)
		if err != nil {
			return err
		}
		if !containsBase {
			return fmt.Errorf(
				"current HEAD is unchanged from origin/%s and does not contain PR base SHA %s",
				pushBranch,
				shortGitSHA(baseSHA),
			)
		}
		return nil
	}

	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return fmt.Errorf(
			"current HEAD is unchanged from origin/%s and neither %s nor %s is set",
			pushBranch,
			workerenv.PRBaseSHA,
			workerenv.PRBaseBranch,
		)
	}
	containsBase, err := headContainsRemoteBranch(workDir, baseBranch)
	if err != nil {
		return err
	}
	if !containsBase {
		return fmt.Errorf("current HEAD is unchanged from origin/%s and does not contain origin/%s", pushBranch, baseBranch)
	}
	return nil
}

func headAdvancedFromRemoteBranch(workDir, branch string) (bool, error) {
	head, err := execGit(workDir, "rev-parse", "HEAD")
	if err != nil {
		return false, fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}
	head = strings.TrimSpace(head)

	remoteSHA, exists, err := fetchRemoteBranch(workDir, branch)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	if head == remoteSHA {
		return false, nil
	}
	if _, err := execGit(workDir, "merge-base", "--is-ancestor", remoteSHA, "HEAD"); err != nil {
		return false, nil
	}
	return true, nil
}

func headContainsRemoteBranch(workDir, branch string) (bool, error) {
	remoteSHA, exists, err := fetchRemoteBranch(workDir, branch)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("remote branch origin/%s not found", branch)
	}
	if _, err := execGit(workDir, "merge-base", "--is-ancestor", remoteSHA, "HEAD"); err != nil {
		return false, nil
	}
	return true, nil
}

func headContainsBaseSHA(workDir, baseSHA, baseRepo string) (bool, error) {
	if !commitExists(workDir, baseSHA) {
		if repo := strings.TrimSpace(baseRepo); repo != "" {
			if _, err := execGit(workDir, "fetch", "--depth=1", repo, baseSHA); err != nil && !commitExists(workDir, baseSHA) {
				return false, fmt.Errorf("fetching PR base SHA %s failed: %w", shortGitSHA(baseSHA), err)
			}
		}
	}
	if !commitExists(workDir, baseSHA) {
		return false, nil
	}
	if _, err := execGit(workDir, "merge-base", "--is-ancestor", baseSHA, "HEAD"); err != nil {
		return false, nil
	}
	return true, nil
}

func commitExists(workDir, sha string) bool {
	if strings.TrimSpace(sha) == "" {
		return false
	}
	_, err := execGit(workDir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func shortGitSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func fetchRemoteBranch(workDir, branch string) (string, bool, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", false, nil
	}
	ref := "refs/heads/" + branch
	out, err := execGit(workDir, "ls-remote", "--heads", "origin", ref)
	if err != nil {
		return "", false, fmt.Errorf("git ls-remote %s failed: %s: %w", ref, out, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", false, nil
	}
	remoteSHA := fields[0]
	if _, err := execGit(workDir, "fetch", "origin", "+"+ref+":refs/remotes/origin/"+branch); err != nil {
		return "", true, fmt.Errorf("git fetch origin %s failed: %w", ref, err)
	}
	return remoteSHA, true, nil
}

func waitForRemoteBranchVisibilityWithGit(workDir, remote, branch string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ref := "refs/heads/" + branch
	var lastErr error
	for {
		out, err := execGit(workDir, "ls-remote", "--heads", remote, ref)
		if err == nil && strings.TrimSpace(out) != "" {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("branch %s not advertised by remote %s yet", branch, remote)
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(1 * time.Second)
	}
}

func resetReservedWorkspacePaths(workDir string) {
	// Runtime agents expose a repo-local symlink to /tmp/artifacts so scan and
	// patch jobs can write required artifacts from inside the workspace. That
	// symlink is infrastructure, not a user-facing code change.
	execGit(workDir, "reset", "-q", "--", workspaceArtifactsDirName) //nolint:errcheck
}

// execGit runs a git command in the given directory and returns combined output.
func execGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", gitSafeDirectoryArgs(dir, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// parseDiffStatFiles extracts file paths from `git diff --stat` output.
// Each line looks like: " path/to/file | 5 ++-" and the last line is a summary.
func parseDiffStatFiles(stat string) []string {
	var files []string
	for line := range strings.SplitSeq(stat, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		f := strings.TrimSpace(parts[0])
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}
