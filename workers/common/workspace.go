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
)

const requirePushBranchEnvVar = "ORKA_REQUIRE_PUSH_BRANCH"

var waitForRemoteBranchVisibility = waitForRemoteBranchVisibilityWithGit

// PrepareWorkspace applies the diff from a prior task's result to the working
// directory. It is called after git clone and before the LLM agent starts.
// If ORKA_PRIOR_TASK is not set the function is a no-op.
func PrepareWorkspace(workDir string) error {
	priorTask := os.Getenv("ORKA_PRIOR_TASK")
	if priorTask == "" {
		return nil
	}

	ns := os.Getenv("ORKA_PRIOR_TASK_NAMESPACE")
	if ns == "" {
		ns = os.Getenv("ORKA_TASK_NAMESPACE")
	}

	controllerURL := os.Getenv("ORKA_CONTROLLER_URL")
	if controllerURL == "" {
		return fmt.Errorf("ORKA_CONTROLLER_URL must be set when ORKA_PRIOR_TASK is specified")
	}
	controllerURL = strings.TrimRight(controllerURL, "/")

	// Read SA token for authentication.
	token, _ := os.ReadFile(saTokenPath)
	saToken := strings.TrimSpace(string(token))

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

	// Auto-push if ORKA_PUSH_BRANCH is set and there are changes
	pushBranch := os.Getenv("ORKA_PUSH_BRANCH")
	requirePushBranch := strings.EqualFold(os.Getenv(requirePushBranchEnvVar), "true")
	if requirePushBranch && pushBranch == "" {
		return nil, fmt.Errorf("%s is true but ORKA_PUSH_BRANCH is empty", requirePushBranchEnvVar)
	}
	if pushBranch != "" {
		if diff == "" {
			if requirePushBranch {
				return nil, fmt.Errorf("ORKA_PUSH_BRANCH=%s but no workspace diff was produced", pushBranch)
			}
		} else if pushErr := pushChanges(workDir, pushBranch); pushErr != nil {
			if requirePushBranch {
				return nil, fmt.Errorf("failed to push to %s: %w", pushBranch, pushErr)
			}
			fmt.Fprintf(os.Stderr, "warning: failed to push to %s: %v\n", pushBranch, pushErr)
		} else {
			fmt.Fprintf(os.Stderr, "pushed changes to branch %s\n", pushBranch)
		}
	}

	sr := &StructuredResult{
		Summary:    agentOutput,
		BaseSHA:    baseSHA,
		PushBranch: pushBranch,
	}
	if diff != "" {
		sr.Diff = diff
		sr.Files = files
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
