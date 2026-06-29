/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sozercan/orka/internal/events"
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

var newGeneralEventRecorder = common.NewHTTPEventRecorderFromEnv

var exitProcess = os.Exit

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	eventRecorder := newGeneralEventRecorder()

	if len(os.Args) > 1 && os.Args[1] == "--prepare-workspace-only" {
		return prepareWorkspace(ctx)
	}
	if len(os.Args) > 1 && os.Args[1] == "--security-mapper" {
		return runSecurityMapper(ctx)
	}

	baseEnv := workerenv.ParseBaseEnv(os.Getenv)
	transactionLogFields := workerenv.TransactionLogFields(
		baseEnv.TransactionID, baseEnv.TransactionProfile,
	)
	fmt.Printf("Worker general started task=%s/%s%s\n",
		baseEnv.TaskNamespace, baseEnv.TaskName, transactionLogFields)
	recordGeneralEvent(ctx, eventRecorder, baseEnv, events.ExecutionEventTypeWorkerStarted,
		events.ExecutionEventSeverityInfo,
		"general worker started",
		map[string]any{"worker": "general"},
	)

	workDir, err := prepareWorkspaceIfConfigured(ctx)
	if err != nil {
		recordGeneralWorkerFailure(ctx, eventRecorder, baseEnv, "prepare workspace failed", err, nil)
		return err
	}

	// Get command from arguments or environment
	var command []string
	if len(os.Args) > 1 {
		command = os.Args[1:]
	} else {
		cmdStr := os.Getenv(workerenv.Command)
		if cmdStr == "" {
			err := fmt.Errorf("no command specified")
			recordGeneralWorkerFailure(ctx, eventRecorder, baseEnv, "no command specified", err, nil)
			return fmt.Errorf("no command specified")
		}
		command = strings.Fields(cmdStr)
	}

	if len(command) == 0 {
		err := fmt.Errorf("command cannot be empty")
		recordGeneralWorkerFailure(ctx, eventRecorder, baseEnv, "command cannot be empty", err, nil)
		return err
	}

	// Execute the command and print output to stdout/stderr.
	// The controller captures pod logs and writes them to a result ConfigMap.
	var stdout, stderr bytes.Buffer
	startedAt := time.Now()
	recordGeneralEvent(ctx, eventRecorder, baseEnv, events.ExecutionEventTypeContainerCommandStarted,
		events.ExecutionEventSeverityInfo,
		fmt.Sprintf("container command started: %s", commandExecutableSummary(command)),
		generalCommandEventContent(command, workDir, commandRunMetadata{}),
	)

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()
	if workDir != "" {
		cmd.Dir = workDir
	}

	err = cmd.Run()
	duration := time.Since(startedAt)
	runMetadata := commandRunMetadata{
		Duration:    duration,
		StdoutBytes: stdout.Len(),
		StderrBytes: stderr.Len(),
	}
	exitCode := 0
	if err != nil {
		exitCode = -1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		runMetadata.ExitCode = &exitCode
	} else {
		runMetadata.ExitCode = &exitCode
	}

	if stdout.Len() > 0 {
		fmt.Print(stdout.String())
	}
	if stderr.Len() > 0 {
		fmt.Fprint(os.Stderr, stderr.String())
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			recordGeneralEvent(ctx, eventRecorder, baseEnv, events.ExecutionEventTypeContainerCommandFailed,
				events.ExecutionEventSeverityError,
				fmt.Sprintf("container command failed: %s", commandExecutableSummary(command)),
				generalCommandEventContent(command, workDir, runMetadata),
			)
			if submitErr := submitResult(workDir, stdout.String()+stderr.String()); submitErr == nil {
				recordGeneralEvent(ctx, eventRecorder, baseEnv, events.ExecutionEventTypeResultSubmitted,
					events.ExecutionEventSeverityInfo,
					"command result submitted",
					map[string]any{"stdoutBytes": stdout.Len(), "stderrBytes": stderr.Len()},
				)
			}
			recordGeneralWorkerFailure(ctx, eventRecorder, baseEnv, "general worker failed", err,
				generalCommandEventContent(command, workDir, runMetadata))
			exitProcess(exitErr.ExitCode())
			return nil
		}
		recordGeneralEvent(ctx, eventRecorder, baseEnv, events.ExecutionEventTypeContainerCommandFailed,
			events.ExecutionEventSeverityError,
			fmt.Sprintf("container command failed: %s", commandExecutableSummary(command)),
			generalCommandEventContent(command, workDir, runMetadata),
		)
		recordGeneralWorkerFailure(ctx, eventRecorder, baseEnv, "general worker failed", err,
			generalCommandEventContent(command, workDir, runMetadata))
		return err
	}
	recordGeneralEvent(ctx, eventRecorder, baseEnv, events.ExecutionEventTypeContainerCommandCompleted,
		events.ExecutionEventSeverityInfo,
		fmt.Sprintf("container command completed: %s", commandExecutableSummary(command)),
		generalCommandEventContent(command, workDir, runMetadata),
	)

	if err := submitResult(workDir, stdout.String()+stderr.String()); err != nil {
		recordGeneralWorkerFailure(ctx, eventRecorder, baseEnv, "submit result failed", err,
			generalCommandEventContent(command, workDir, runMetadata))
		return err
	}
	recordGeneralEvent(ctx, eventRecorder, baseEnv, events.ExecutionEventTypeResultSubmitted,
		events.ExecutionEventSeverityInfo,
		"command result submitted",
		map[string]any{"stdoutBytes": stdout.Len(), "stderrBytes": stderr.Len()},
	)

	fmt.Printf("Task %s/%s completed successfully%s\n",
		baseEnv.TaskNamespace, baseEnv.TaskName, transactionLogFields)
	recordGeneralEvent(ctx, eventRecorder, baseEnv, events.ExecutionEventTypeWorkerCompleted,
		events.ExecutionEventSeverityInfo,
		"general worker completed",
		map[string]any{"worker": "general", "stdoutBytes": stdout.Len(), "stderrBytes": stderr.Len()},
	)
	return nil
}

type commandRunMetadata struct {
	Duration    time.Duration
	ExitCode    *int
	StdoutBytes int
	StderrBytes int
}

func recordGeneralWorkerFailure(
	ctx context.Context,
	recorder common.EventRecorder,
	baseEnv workerenv.BaseEnv,
	summary string,
	err error,
	content map[string]any,
) {
	if content == nil {
		content = map[string]any{}
	}
	content["worker"] = "general"
	if err != nil {
		content["error"] = err.Error()
	}
	recordGeneralEvent(ctx, recorder, baseEnv, events.ExecutionEventTypeWorkerFailed,
		events.ExecutionEventSeverityError, summary, content)
}

func recordGeneralEvent(
	ctx context.Context,
	recorder common.EventRecorder,
	baseEnv workerenv.BaseEnv,
	typ string,
	severity string,
	summary string,
	content map[string]any,
) {
	var raw json.RawMessage
	if len(content) > 0 {
		if data, err := json.Marshal(content); err == nil {
			raw = data
		}
	}
	opts := []common.EventOption{
		common.WithEventSeverity(severity),
		common.WithEventTaskName(baseEnv.TaskName),
		common.WithEventSessionName(os.Getenv(workerenv.SessionName)),
		common.WithEventSummary(summary),
		common.WithEventContent(raw),
	}
	if ctx != nil && ctx.Err() != nil {
		common.RecordEventWithTimeout(recorder, typ, 0, opts...)
		return
	}
	common.RecordEvent(ctx, recorder, typ, opts...)
}

func generalCommandEventContent(command []string, workDir string, run commandRunMetadata) map[string]any {
	content := map[string]any{
		"executable":        commandExecutableSummary(command),
		"argsCount":         max(len(command)-1, 0),
		"workdirConfigured": workDir != "",
	}
	if run.Duration > 0 {
		content["durationMs"] = run.Duration.Milliseconds()
	}
	if run.ExitCode != nil {
		content["exitCode"] = *run.ExitCode
	}
	if run.StdoutBytes > 0 {
		content["stdoutBytes"] = run.StdoutBytes
	}
	if run.StderrBytes > 0 {
		content["stderrBytes"] = run.StderrBytes
	}
	return content
}

func commandExecutableSummary(command []string) string {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return "unknown"
	}
	executable := filepath.Base(command[0])
	if executable == "." || executable == string(filepath.Separator) || executable == "" {
		executable = strings.TrimSpace(command[0])
	}
	return executable
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
	changedFilesComputed, changedFiles, changedLineRanges, diffSummary, changedFilesError, resolvedHeadCommit :=
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
		ChangedLineRanges:    changedLineRanges,
		DiffSummary:          diffSummary,
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
) (bool, []string, []security.ChangedLineRange, string, string, string) {
	if headCommit == "" {
		out, err := exec.CommandContext(ctx, "git", "-C", workDir, "rev-parse", "HEAD").CombinedOutput()
		if err == nil {
			headCommit = strings.TrimSpace(string(out))
		}
	}
	if baseCommit == "" || headCommit == "" {
		return false, nil, nil, "", "", headCommit
	}
	for _, commit := range []string{baseCommit, headCommit} {
		if !safeGitCommitID(commit) {
			return false, nil, nil, "", fmt.Sprintf("commit %q is not a hex SHA", commit), headCommit
		}
		if err := ensureCommitAvailableForDiff(ctx, workDir, commit); err != nil {
			return false, nil, nil, "", err.Error(), headCommit
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
		return false, nil, nil, "", message, headCommit
	}
	deletedFiles := safeChangedFileLines(deletedOut)
	if len(deletedFiles) > 0 {
		message := fmt.Sprintf(
			"changed-file selection disabled because deleted files require full review: %s",
			strings.Join(deletedFiles, ", "),
		)
		return false, nil, nil, "", message, headCommit
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
		return false, nil, nil, "", message, headCommit
	}

	files := safeChangedFileLines(out)
	lineRanges, err := changedLineRangesForSecurityScan(ctx, workDir, baseCommit, headCommit)
	if err != nil {
		if errors.Is(err, errChangedDiffTooLarge) {
			diffSummary := fmt.Sprintf(
				"%d changed files; changed line ranges omitted because diff exceeded safety cap",
				len(files),
			)
			return true, files, nil, diffSummary, "", headCommit
		}
		diffSummary := fmt.Sprintf(
			"%d changed files; changed line ranges omitted because diff could not be parsed: %s",
			len(files), err,
		)
		return true, files, nil, diffSummary, "", headCommit
	}
	diffSummary := fmt.Sprintf("%d changed files; %d changed line ranges", len(files), len(lineRanges))
	return true, files, lineRanges, diffSummary, "", headCommit
}

const (
	maxChangedLineRangesForArtifact  = 2000
	maxChangedDiffBytesForLineRanges = 2 * 1024 * 1024
	maxChangedDiffLinesForLineRanges = 20000
)

var (
	errChangedDiffTooLarge           = errors.New("changed diff exceeds changed-line metadata safety cap")
	changedLineRangesForSecurityScan = defaultChangedLineRangesForSecurityScan
)

func defaultChangedLineRangesForSecurityScan(
	ctx context.Context,
	workDir, baseCommit, headCommit string,
) ([]security.ChangedLineRange, error) {
	cmd := exec.CommandContext(ctx,
		"git", "-C", workDir,
		"diff", "--unified=0", "--diff-filter=ACMRT", "--relative",
		baseCommit, headCommit, "--", ".",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	lineRanges, parseErr := parseChangedLineRangesFromUnifiedDiffReader(stdout)
	if parseErr != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return nil, parseErr
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		return nil, fmt.Errorf("git diff changed line ranges: %s", message)
	}
	return lineRanges, nil
}

var unifiedDiffHunkRE = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)

func parseChangedLineRangesFromUnifiedDiff(diff []byte) ([]security.ChangedLineRange, error) {
	return parseChangedLineRangesFromUnifiedDiffReader(bytes.NewReader(diff))
}

func parseChangedLineRangesFromUnifiedDiffReader(r io.Reader) ([]security.ChangedLineRange, error) {
	reader := bufio.NewReaderSize(r, 64*1024)
	currentPath := ""
	expectPlusHeader := false
	inHunk := false
	ranges := make([]security.ChangedLineRange, 0)
	atLineStart := true
	bytesRead := 0
	linesRead := 0
	for {
		chunk, err := reader.ReadSlice('\n')
		bytesRead += len(chunk)
		if bytesRead > maxChangedDiffBytesForLineRanges {
			return nil, errChangedDiffTooLarge
		}
		if len(chunk) > 0 && atLineStart {
			linesRead++
			if linesRead > maxChangedDiffLinesForLineRanges {
				return nil, errChangedDiffTooLarge
			}
			line := strings.TrimRight(string(chunk), "\r\n")
			switch {
			case strings.HasPrefix(line, "diff --git "):
				currentPath = ""
				expectPlusHeader = false
				inHunk = false
			case !inHunk && strings.HasPrefix(line, "--- "):
				expectPlusHeader = true
			case expectPlusHeader && strings.HasPrefix(line, "+++ "):
				pathValue := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
				currentPath = normalizeDiffPath(pathValue)
				if currentPath != "" && !security.SafeRepoPath(currentPath) {
					currentPath = ""
				}
				expectPlusHeader = false
			case strings.HasPrefix(line, "@@ "):
				inHunk = true
				expectPlusHeader = false
				if currentPath == "" {
					break
				}
				matches := unifiedDiffHunkRE.FindStringSubmatch(line)
				if len(matches) == 0 {
					return nil, fmt.Errorf("parse changed line ranges: unsupported hunk header %q", line)
				}
				start := atoiDiffNumber(matches[1])
				count := 1
				if matches[2] != "" {
					count = atoiDiffNumber(matches[2])
				}
				if len(ranges) >= maxChangedLineRangesForArtifact {
					return nil, errChangedDiffTooLarge
				}
				if count <= 0 {
					if start <= 0 {
						start = 1
					}
					count = 1
				}
				if start <= 0 {
					break
				}
				ranges = append(ranges, security.ChangedLineRange{Path: currentPath, StartLine: start, EndLine: start + count - 1})
			}
		}
		if err == nil {
			atLineStart = true
			continue
		}
		if err == bufio.ErrBufferFull {
			atLineStart = false
			continue
		}
		if err == io.EOF {
			break
		}
		return nil, err
	}
	return mergeChangedLineRanges(ranges), nil
}

func normalizeDiffPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "/dev/null" || value == "" {
		return ""
	}
	if strings.HasPrefix(value, "b/") || strings.HasPrefix(value, "a/") {
		value = value[2:]
	}
	return value
}

func atoiDiffNumber(value string) int {
	out := 0
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0
		}
		out = out*10 + int(ch-'0')
	}
	return out
}

func mergeChangedLineRanges(ranges []security.ChangedLineRange) []security.ChangedLineRange {
	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Path != ranges[j].Path {
			return ranges[i].Path < ranges[j].Path
		}
		if ranges[i].StartLine != ranges[j].StartLine {
			return ranges[i].StartLine < ranges[j].StartLine
		}
		return ranges[i].EndLine < ranges[j].EndLine
	})
	out := make([]security.ChangedLineRange, 0, len(ranges))
	for _, lineRange := range ranges {
		if lineRange.Path == "" || lineRange.StartLine <= 0 || lineRange.EndLine < lineRange.StartLine {
			continue
		}
		if len(out) == 0 || out[len(out)-1].Path != lineRange.Path || lineRange.StartLine > out[len(out)-1].EndLine+1 {
			out = append(out, lineRange)
			continue
		}
		if lineRange.EndLine > out[len(out)-1].EndLine {
			out[len(out)-1].EndLine = lineRange.EndLine
		}
	}
	return out
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
