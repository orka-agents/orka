/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

// PrepareWorkspace applies the diff from a prior task's result to the working
// directory. It is called after git clone and before the LLM agent starts.
// If MERCAN_PRIOR_TASK is not set the function is a no-op.
func PrepareWorkspace(workDir string) error {
	priorTask := os.Getenv("MERCAN_PRIOR_TASK")
	if priorTask == "" {
		return nil
	}

	ns := os.Getenv("MERCAN_PRIOR_TASK_NAMESPACE")
	if ns == "" {
		ns = os.Getenv("MERCAN_TASK_NAMESPACE")
	}

	controllerURL := os.Getenv("MERCAN_CONTROLLER_URL")
	if controllerURL == "" {
		return fmt.Errorf("MERCAN_CONTROLLER_URL must be set when MERCAN_PRIOR_TASK is specified")
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
	defer resp.Body.Close()

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
	diffPath := filepath.Join(workDir, ".mercan-prior.patch")
	if err := os.WriteFile(diffPath, []byte(sr.Diff), 0o600); err != nil {
		return fmt.Errorf("failed to write diff to temp file: %w", err)
	}
	defer os.Remove(diffPath)

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
// When MERCAN_PUSH_BRANCH is set, changes are committed and pushed to that branch.
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
	execGit(workDir, "add", "-A")

	diff, err := execGit(workDir, "diff", "--cached", "--binary", "--full-index")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: git diff failed in %s, returning plain text result\n", workDir)
		return []byte(agentOutput), nil
	}
	// Also include unstaged changes
	unstaged, _ := execGit(workDir, "diff", "--binary", "--full-index")
	if unstaged != "" {
		diff += unstaged
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

	// Auto-push if MERCAN_PUSH_BRANCH is set and there are changes
	pushBranch := os.Getenv("MERCAN_PUSH_BRANCH")
	if pushBranch != "" && diff != "" {
		if pushErr := pushChanges(workDir, pushBranch); pushErr != nil {
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

	// Check if there's anything to commit
	status, _ := execGit(workDir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		return nil // nothing to commit
	}

	// Configure committer identity
	execGit(workDir, "config", "user.email", "mercan@ai")
	execGit(workDir, "config", "user.name", "Mercan AI")

	// Commit
	if out, err := execGit(workDir, "commit", "-m", "feat: changes from mercan agent"); err != nil {
		return fmt.Errorf("git commit failed: %s: %w", out, err)
	}

	// Create and push the branch
	if out, err := execGit(workDir, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("git checkout -b %s failed: %s: %w", branch, out, err)
	}

	if out, err := execGit(workDir, "push", "origin", branch); err != nil {
		return fmt.Errorf("git push failed: %s: %w", out, err)
	}

	return nil
}

// execGit runs a git command in the given directory and returns combined output.
func execGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// parseDiffStatFiles extracts file paths from `git diff --stat` output.
// Each line looks like: " path/to/file | 5 ++-" and the last line is a summary.
func parseDiffStatFiles(stat string) []string {
	var files []string
	for _, line := range strings.Split(stat, "\n") {
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
