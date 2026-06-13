/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/sozercan/orka/internal/security"
	securityslices "github.com/sozercan/orka/internal/security/slices"
	"github.com/sozercan/orka/internal/workerenv"

	"github.com/sozercan/orka/workers/common"
)

var (
	workspaceDir                  = "/workspace"
	setupGitCredentialsForGeneral = common.SetupGitCredentials
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() (err error) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if len(os.Args) > 1 && os.Args[1] == "--prepare-workspace-only" {
		return prepareWorkspace(ctx)
	}
	if len(os.Args) > 1 && os.Args[1] == "--security-mapper" {
		return runSecurityMapper(ctx)
	}

	baseEnv := workerenv.ParseBaseEnv(os.Getenv)
	taskName := baseEnv.TaskName
	taskNamespace := baseEnv.TaskNamespace
	eventRecorder := common.NewHTTPEventRecorderFromEnv()
	defer func() {
		if err != nil {
			recordGeneralWorkerFailed(eventRecorder, taskName, err)
			return
		}
		common.RecordEventWithTimeout(eventRecorder, "WorkerCompleted", 0,
			common.WithEventTaskName(taskName),
			common.WithEventSummary("General worker completed"),
		)
	}()

	transactionLogFields := workerenv.TransactionLogFields(
		baseEnv.TransactionID, baseEnv.TransactionProfile,
	)
	fmt.Printf("Worker general started task=%s/%s%s\n",
		taskNamespace, taskName, transactionLogFields)
	common.RecordEvent(ctx, eventRecorder, "WorkerStarted",
		common.WithEventTaskName(taskName),
		common.WithEventSummary("General worker started"),
	)

	workDir, err := prepareWorkspaceIfConfigured(ctx)
	if err != nil {
		return err
	}

	// Get command from arguments or environment
	var command []string
	if len(os.Args) > 1 {
		command = os.Args[1:]
	} else {
		cmdStr := os.Getenv(workerenv.Command)
		if cmdStr == "" {
			return fmt.Errorf("no command specified")
		}
		command = strings.Fields(cmdStr)
	}

	if len(command) == 0 {
		return fmt.Errorf("command cannot be empty")
	}

	// Execute the command and print output to stdout/stderr.
	// The controller captures pod logs and writes them to a result ConfigMap.
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()
	if workDir != "" {
		cmd.Dir = workDir
	}

	err = cmd.Run()

	if stdout.Len() > 0 {
		fmt.Print(stdout.String())
	}
	if stderr.Len() > 0 {
		fmt.Fprint(os.Stderr, stderr.String())
	}

	output := stdout.String() + stderr.String()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if submitErr := submitResult(workDir, output); submitErr == nil {
				recordGeneralResultSubmitted(eventRecorder, taskName, len(output))
			}
			recordGeneralWorkerFailed(
				eventRecorder,
				taskName,
				fmt.Errorf("command exited with code %d: %w", exitErr.ExitCode(), err),
			)
			os.Exit(exitErr.ExitCode())
		}
		return err
	}

	if err := submitResult(workDir, output); err != nil {
		return err
	}
	recordGeneralResultSubmitted(eventRecorder, taskName, len(output))

	fmt.Printf("Task %s/%s completed successfully%s\n",
		taskNamespace, taskName, transactionLogFields)
	return nil
}

func recordGeneralResultSubmitted(recorder common.EventRecorder, taskName string, resultLength int) {
	common.RecordEventWithTimeout(recorder, "ResultSubmitted", 0,
		common.WithEventTaskName(taskName),
		common.WithEventSummary("General worker submitted result"),
		common.WithEventContent(generalEventContent(map[string]any{"resultLength": resultLength})),
	)
}

func recordGeneralWorkerFailed(recorder common.EventRecorder, taskName string, err error) {
	if err == nil {
		return
	}
	common.RecordEventWithTimeout(recorder, "WorkerFailed", 0,
		common.WithEventSeverity("error"),
		common.WithEventTaskName(taskName),
		common.WithEventSummary(err.Error()),
	)
}

func generalEventContent(value map[string]any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func prepareWorkspaceIfConfigured(ctx context.Context) (string, error) {
	cfg, err := common.LoadWorkspaceConfig()
	if err != nil {
		return "", err
	}
	if cfg.GitRepo == "" {
		return "", nil
	}
	if cfg.SubPath != os.Getenv(workerenv.WorkspaceSubpath) {
		if err := os.Setenv(workerenv.WorkspaceSubpath, cfg.SubPath); err != nil {
			return "", err
		}
	}
	setupGitCredentialsForGeneral()
	if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); err == nil {
		return workspaceRoot(), nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("stat workspace: %w", err)
	}
	if err := prepareWorkspace(ctx); err != nil {
		return "", err
	}
	return workspaceRoot(), nil
}

func prepareWorkspace(ctx context.Context) error {
	cfg, err := common.LoadWorkspaceConfig()
	if err != nil {
		return err
	}
	if cfg.GitRepo == "" {
		return nil
	}

	setupGitCredentialsForGeneral()
	if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); os.IsNotExist(err) {
		if err := common.CloneRepo(ctx, cfg, workspaceDir); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("stat workspace: %w", err)
	}
	if err := common.PrepareWorkspace(workspaceDir); err != nil {
		return err
	}
	if err := common.PreparePullRequestReviewContext(workspaceDir, cfg); err != nil {
		return err
	}
	return common.EnsureWorkspaceArtifactsLink(workspaceDir)
}

func workspaceRoot() string {
	if subPath := os.Getenv(workerenv.WorkspaceSubpath); subPath != "" {
		return filepath.Join(workspaceDir, subPath)
	}
	return workspaceDir
}

func submitResult(workDir, output string) error {
	if os.Getenv(workerenv.ResultEndpoint) == "" && os.Getenv(workerenv.ControllerURL) == "" {
		return nil
	}
	resultDir := ""
	if workDir != "" {
		resultDir = workspaceDir
	}
	resultBytes, err := common.FinalizeResult(resultDir, output)
	if err != nil {
		return err
	}
	if err := common.SubmitResult(resultBytes); err != nil {
		return err
	}
	return common.UploadArtifacts()
}

func runSecurityMapper(ctx context.Context) error {
	workDir, err := prepareWorkspaceIfConfigured(ctx)
	if err != nil {
		return err
	}
	if workDir == "" {
		return fmt.Errorf("security mapper requires a git workspace")
	}
	repositoryScan := strings.TrimSpace(os.Getenv(security.EnvRepositoryScanName))
	slices, err := securityslices.MapRepository(workDir, securityslices.MapperOptions{
		RepositoryScan: repositoryScan,
		SubPath:        os.Getenv(workerenv.WorkspaceSubpath),
	})
	if err != nil {
		return err
	}
	baseCommit := strings.TrimSpace(os.Getenv(security.EnvScanBaseCommit))
	headCommit := strings.TrimSpace(os.Getenv(security.EnvScanHeadCommit))
	changedFilesComputed, changedFiles, changedFilesError, resolvedHeadCommit :=
		changedFilesForSecurityScan(ctx, workDir, baseCommit, headCommit)
	if headCommit == "" {
		headCommit = resolvedHeadCommit
	}
	artifact := security.ReviewSlicesArtifact{
		SchemaVersion:        security.SchemaVersionReviewSlices,
		BaseCommit:           baseCommit,
		HeadCommit:           headCommit,
		ChangedFilesComputed: changedFilesComputed,
		ChangedFiles:         changedFiles,
		ChangedFilesError:    changedFilesError,
		Slices:               slices,
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	if err := common.WriteArtifactFile(security.ArtifactSlices, data); err != nil {
		return err
	}
	output := fmt.Sprintf("security mapper wrote %d review slices\n", len(slices))
	fmt.Print(output)
	return submitResult(workDir, output)
}

func changedFilesForSecurityScan(
	ctx context.Context,
	workDir, baseCommit, headCommit string,
) (bool, []string, string, string) {
	if headCommit == "" {
		out, err := exec.CommandContext(ctx, "git", "-C", workDir, "rev-parse", "HEAD").CombinedOutput()
		if err == nil {
			headCommit = strings.TrimSpace(string(out))
		}
	}
	if baseCommit == "" || headCommit == "" {
		return false, nil, "", headCommit
	}
	for _, commit := range []string{baseCommit, headCommit} {
		if !safeGitCommitID(commit) {
			return false, nil, fmt.Sprintf("commit %q is not a hex SHA", commit), headCommit
		}
		if err := ensureCommitAvailableForDiff(ctx, workDir, commit); err != nil {
			return false, nil, err.Error(), headCommit
		}
	}

	deletedOut, err := exec.CommandContext(ctx,
		"git", "-C", workDir,
		"diff", "--name-only", "--diff-filter=D", "--relative",
		baseCommit, headCommit, "--", ".",
	).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(deletedOut))
		if message == "" {
			message = err.Error()
		}
		return false, nil, message, headCommit
	}
	deletedFiles := safeChangedFileLines(deletedOut)
	if len(deletedFiles) > 0 {
		message := fmt.Sprintf(
			"changed-file selection disabled because deleted files require full review: %s",
			strings.Join(deletedFiles, ", "),
		)
		return false, nil, message, headCommit
	}

	out, err := exec.CommandContext(ctx,
		"git", "-C", workDir,
		"diff", "--name-only", "--diff-filter=ACMRT", "--relative",
		baseCommit, headCommit, "--", ".",
	).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return false, nil, message, headCommit
	}

	files := safeChangedFileLines(out)
	return true, files, "", headCommit
}

func safeChangedFileLines(out []byte) []string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	seen := make(map[string]struct{}, len(lines))
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		file := strings.TrimSpace(strings.ReplaceAll(line, "\\", "/"))
		if file == "" || !security.SafeRepoPath(file) {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}

func ensureCommitAvailableForDiff(ctx context.Context, workDir, commit string) error {
	if !safeGitCommitID(commit) {
		return fmt.Errorf("commit %q is not a hex SHA", commit)
	}
	if gitCommitAvailable(ctx, workDir, commit) {
		return nil
	}

	out, err := exec.CommandContext(
		ctx,
		"git",
		"-C",
		workDir,
		"fetch",
		"--no-tags",
		"--depth=1",
		"origin",
		commit,
	).CombinedOutput()
	if err == nil && gitCommitAvailable(ctx, workDir, commit) {
		return nil
	}
	firstMessage := strings.TrimSpace(string(out))
	if firstMessage == "" && err != nil {
		firstMessage = err.Error()
	}

	args := []string{"fetch", "--no-tags", "origin"}
	if isShallowGitRepository(ctx, workDir) {
		args = []string{"fetch", "--no-tags", "--unshallow", "origin"}
	}
	out, err = exec.CommandContext(ctx, "git", append([]string{"-C", workDir}, args...)...).CombinedOutput()
	if err == nil && gitCommitAvailable(ctx, workDir, commit) {
		return nil
	}
	message := strings.TrimSpace(string(out))
	if message == "" && err != nil {
		message = err.Error()
	}
	if firstMessage != "" && message != "" {
		message = firstMessage + "; " + message
	} else if message == "" {
		message = firstMessage
	}
	if message == "" {
		message = "commit is not available after fetching origin"
	}
	return fmt.Errorf("fetch commit for incremental diff: %s", message)
}

func gitCommitAvailable(ctx context.Context, workDir, commit string) bool {
	if strings.TrimSpace(commit) == "" {
		return false
	}
	err := exec.CommandContext(ctx, "git", "-C", workDir, "cat-file", "-e", commit+"^{commit}").Run()
	return err == nil
}

func safeGitCommitID(commit string) bool {
	commit = strings.TrimSpace(commit)
	if len(commit) < 7 || len(commit) > 64 {
		return false
	}
	for _, ch := range commit {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') {
			continue
		}
		return false
	}
	return true
}

func isShallowGitRepository(ctx context.Context, workDir string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", workDir, "rev-parse", "--is-shallow-repository").CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}
