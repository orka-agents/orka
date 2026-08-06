/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/security"
	securityslices "github.com/orka-agents/orka/internal/security/slices"
	"github.com/orka-agents/orka/internal/workerenv"

	"github.com/orka-agents/orka/workers/common"
)

var (
	workspaceDir                  = "/workspace"
	setupGitCredentialsForGeneral = common.SetupGitCredentials
	mapperTreeIndexLimit          = security.MaxMapperTreeIndexEntries
)

func gitCommandNoReplace(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	return cmd
}

const maxMapperTreePathBytes = 4096

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
	baseCommit := strings.TrimSpace(os.Getenv(security.EnvScanBaseCommit))
	headCommit := strings.TrimSpace(os.Getenv(security.EnvScanHeadCommit))
	pinnedTargets, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(security.EnvPinnedScanTargetsEnabled)))
	artifact, err := buildSecurityMapperArtifact(ctx, workDir, repositoryScan, baseCommit, headCommit, pinnedTargets)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	if err := common.WriteArtifactFile(security.ArtifactSlices, data); err != nil {
		return err
	}
	output := fmt.Sprintf("security mapper wrote %d review slices\n", len(artifact.Slices))
	fmt.Print(output)
	return submitResult(workDir, output)
}

func buildSecurityMapperArtifact(
	ctx context.Context,
	workDir string,
	repositoryScan string,
	baseCommit string,
	headCommit string,
	pinnedTargets bool,
) (*security.ReviewSlicesArtifact, error) {
	mapped, err := securityslices.MapRepository(workDir, securityslices.MapperOptions{
		RepositoryScan: repositoryScan,
		SubPath:        os.Getenv(workerenv.WorkspaceSubpath),
	})
	if err != nil {
		return nil, err
	}
	inventorySummary := mapped.InventorySummary
	coverageStatus, coverageReasonCodes := mapperCoverageForInventory(inventorySummary)
	if !pinnedTargets {
		resolvedHead := strings.TrimSpace(headCommit)
		if resolvedHead == "" {
			resolvedHead, _ = gitTextOutput(ctx, workDir, "rev-parse", "HEAD")
		}
		changedFilesComputed, changedFiles, changedLineRanges, diffSummary, changedFilesError, _ :=
			changedFilesForSecurityScan(ctx, workDir, baseCommit, resolvedHead)
		return &security.ReviewSlicesArtifact{
			SchemaVersion: security.SchemaVersionReviewSlices, CoverageStatus: security.MapperCoverageUnknown,
			BaseCommit: baseCommit, HeadCommit: resolvedHead, ChangedFilesComputed: changedFilesComputed,
			ChangedFiles: changedFiles, ChangedLineRanges: changedLineRanges, DiffSummary: diffSummary,
			ChangedFilesError: changedFilesError, CoverageReasonCodes: coverageReasonCodes,
			InventorySummary: &inventorySummary, DiscoveredFiles: mapped.DiscoveredFiles,
			ReviewableFiles: mapped.ReviewableFiles, OmittedFiles: mapped.OmittedFiles, Slices: mapped.Slices,
		}, nil
	}
	targetReceipt, err := buildMapperTargetReceipt(ctx, workDir, baseCommit, headCommit)
	if err != nil {
		return nil, err
	}
	changedFilesComputed, changedFiles, changedLineRanges, diffSummary, changedFilesError, _ :=
		changedFilesForSecurityScan(ctx, workDir, baseCommit, targetReceipt.HeadOID)
	return &security.ReviewSlicesArtifact{
		SchemaVersion:        security.SchemaVersionReviewSlicesV2,
		CoverageStatus:       coverageStatus,
		BaseCommit:           baseCommit,
		HeadCommit:           targetReceipt.HeadOID,
		ChangedFilesComputed: changedFilesComputed,
		ChangedFiles:         changedFiles,
		ChangedLineRanges:    changedLineRanges,
		DiffSummary:          diffSummary,
		ChangedFilesError:    changedFilesError,
		CoverageReasonCodes:  coverageReasonCodes,
		InventorySummary:     &inventorySummary,
		DiscoveredFiles:      mapped.DiscoveredFiles,
		ReviewableFiles:      mapped.ReviewableFiles,
		OmittedFiles:         mapped.OmittedFiles,
		TargetReceipt:        targetReceipt,
		Slices:               mapped.Slices,
	}, nil
}

func mapperCoverageForInventory(summary security.MapperInventorySummary) (string, []string) {
	if summary.Truncated {
		return security.MapperCoveragePartial, []string{security.MapperCoverageReasonInventoryEntryLimit}
	}
	return security.MapperCoverageAccountable, nil
}

func buildMapperTargetReceipt(
	ctx context.Context,
	workDir string,
	baseRef string,
	expectedHeadOID string,
) (*security.MapperTargetReceipt, error) {
	repoRoot, err := gitTextOutput(ctx, workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("resolve mapper repository root: %w", err)
	}
	objectFormat, err := gitTextOutput(ctx, repoRoot, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, fmt.Errorf("resolve mapper git object format: %w", err)
	}
	objectFormat = strings.ToLower(objectFormat)
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return nil, fmt.Errorf("unsupported git object format %q", objectFormat)
	}
	headOID, err := gitTextOutput(ctx, repoRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("resolve mapper HEAD: %w", err)
	}
	headOID = strings.ToLower(headOID)
	if !fullGitObjectID(headOID, objectFormat) {
		return nil, fmt.Errorf("resolved mapper HEAD is not a full %s object ID", objectFormat)
	}
	if expected := strings.ToLower(strings.TrimSpace(expectedHeadOID)); expected != "" {
		if !fullGitObjectID(expected, objectFormat) {
			return nil, fmt.Errorf("expected mapper head %q must be a full %s object ID", expectedHeadOID, objectFormat)
		}
		if expected != headOID {
			return nil, fmt.Errorf("expected mapper head %s does not match checked out HEAD %s", expected, headOID)
		}
	}

	clean, err := cleanTrackedWorktree(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	if !clean {
		return nil, fmt.Errorf("mapper tracked worktree is not clean at HEAD %s", headOID)
	}
	treeOID, err := gitTextOutput(ctx, repoRoot, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return nil, fmt.Errorf("resolve mapper HEAD tree: %w", err)
	}
	treeOID = strings.ToLower(treeOID)
	if !fullGitObjectID(treeOID, objectFormat) {
		return nil, fmt.Errorf("resolved mapper tree is not a full %s object ID", objectFormat)
	}

	treeIndex, treeEntryCount, treeDigest, err := mapperTreeIndex(ctx, repoRoot, objectFormat)
	if err != nil {
		return nil, err
	}
	receipt := &security.MapperTargetReceipt{
		HeadOID:              headOID,
		RequestedBranch:      strings.TrimSpace(os.Getenv(workerenv.GitBranch)),
		RequestedRef:         strings.TrimSpace(os.Getenv(workerenv.GitRef)),
		BaseRef:              strings.TrimSpace(baseRef),
		CleanTrackedWorktree: true,
		ObjectFormat:         objectFormat,
		TreeOID:              treeOID,
		TreeDigest:           treeDigest,
		TreeEntryCount:       treeEntryCount,
		TreeIndexTruncated:   treeEntryCount > len(treeIndex),
		TreeIndex:            treeIndex,
	}
	receipt.SnapshotDigest = mapperSnapshotDigest(receipt)
	return receipt, nil
}

func gitTextOutput(ctx context.Context, workDir string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", workDir}, args...)
	out, err := gitCommandNoReplace(ctx, cmdArgs...).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return strings.TrimSpace(string(out)), nil
}

func fullGitObjectID(value, objectFormat string) bool {
	expectedLength := 0
	switch objectFormat {
	case "sha1":
		expectedLength = 40
	case "sha256":
		expectedLength = 64
	default:
		return false
	}
	if len(value) != expectedLength {
		return false
	}
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}

func cleanTrackedWorktree(ctx context.Context, repoRoot string) (bool, error) {
	out, err := gitCommandNoReplace(ctx, "-C", repoRoot,
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching",
		"--ignore-submodules=none").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("check mapper tracked worktree: %s", strings.TrimSpace(string(out)))
	}
	for record := range bytes.SplitSeq(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		if bytes.Equal(record, []byte("?? .orka-artifacts")) || bytes.HasPrefix(record, []byte("?? .orka-artifacts/")) ||
			bytes.Equal(record, []byte("!! .orka-artifacts")) || bytes.HasPrefix(record, []byte("!! .orka-artifacts/")) {
			continue
		}
		return false, nil
	}
	return true, nil
}

func mapperTreeIndex(
	ctx context.Context,
	repoRoot string,
	objectFormat string,
) ([]security.MapperTreeIndexEntry, int, string, error) {
	cmd := gitCommandNoReplace(ctx, "-C", repoRoot, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, "", fmt.Errorf("read mapper tree: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, 0, "", fmt.Errorf("start mapper tree read: %w", err)
	}
	hasher := sha256.New()
	reader := bufio.NewReader(io.TeeReader(stdout, hasher))
	limit := mapperTreeIndexLimit
	if limit <= 0 || limit > security.MaxMapperTreeIndexEntries {
		limit = security.MaxMapperTreeIndexEntries
	}
	entries := make([]security.MapperTreeIndexEntry, 0, min(limit, 256))
	total := 0
	for {
		record, readErr := reader.ReadBytes(0)
		if len(record) > 0 {
			entry, parseErr := parseMapperTreeEntry(record, objectFormat)
			if parseErr != nil {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_ = cmd.Wait()
				return nil, 0, "", parseErr
			}
			total++
			if len(entries) < limit {
				entries = append(entries, entry)
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return nil, 0, "", fmt.Errorf("read mapper tree: %w", readErr)
	}
	if err := cmd.Wait(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, 0, "", fmt.Errorf("git ls-tree: %s", message)
	}
	if err := markMapperLFSEntries(ctx, repoRoot, entries); err != nil {
		return nil, 0, "", err
	}
	if err := populateMapperTreeEntryMetadata(repoRoot, entries); err != nil {
		return nil, 0, "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, total, "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func parseMapperTreeEntry(record []byte, objectFormat string) (security.MapperTreeIndexEntry, error) {
	if len(record) == 0 || record[len(record)-1] != 0 {
		return security.MapperTreeIndexEntry{}, fmt.Errorf("mapper tree record is not NUL terminated")
	}
	record = record[:len(record)-1]
	tab := bytes.IndexByte(record, '\t')
	if tab <= 0 || tab == len(record)-1 {
		return security.MapperTreeIndexEntry{}, fmt.Errorf("mapper tree record has invalid format")
	}
	header := strings.Fields(string(record[:tab]))
	if len(header) != 3 {
		return security.MapperTreeIndexEntry{}, fmt.Errorf("mapper tree record has invalid header")
	}
	pathBytes := record[tab+1:]
	if len(pathBytes) > maxMapperTreePathBytes || !utf8.Valid(pathBytes) {
		return security.MapperTreeIndexEntry{}, fmt.Errorf("mapper tree path is oversized or invalid UTF-8")
	}
	entry := security.MapperTreeIndexEntry{
		Mode:     header[0],
		Type:     header[1],
		ObjectID: strings.ToLower(header[2]),
		Path:     string(pathBytes),
	}
	if !security.SafeRepoPath(entry.Path) || !fullGitObjectID(entry.ObjectID, objectFormat) {
		return security.MapperTreeIndexEntry{}, fmt.Errorf(
			"mapper tree entry %q has unsafe path or invalid object ID", entry.Path,
		)
	}
	switch {
	case entry.Mode == "120000" && entry.Type == "blob":
		entry.Disposition = security.MapperTreeDispositionSymlink
	case entry.Mode == "160000" && entry.Type == "commit":
		entry.Disposition = security.MapperTreeDispositionSubmodule
	case (entry.Mode == "100644" || entry.Mode == "100755") && entry.Type == "blob":
		entry.Disposition = security.MapperTreeDispositionRegular
	default:
		return security.MapperTreeIndexEntry{}, fmt.Errorf(
			"mapper tree entry %q has unsupported mode/type %s/%s", entry.Path, entry.Mode, entry.Type,
		)
	}
	return entry, nil
}

func markMapperLFSEntries(ctx context.Context, repoRoot string, entries []security.MapperTreeIndexEntry) error {
	var input bytes.Buffer
	for _, entry := range entries {
		if entry.Disposition != security.MapperTreeDispositionRegular {
			continue
		}
		input.WriteString(entry.Path)
		input.WriteByte(0)
	}
	if input.Len() == 0 {
		return nil
	}
	cmd := gitCommandNoReplace(ctx, "-C", repoRoot, "check-attr", "-z", "--stdin", "filter")
	cmd.Stdin = &input
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("classify mapper LFS entries: %s", strings.TrimSpace(string(out)))
	}
	parts := bytes.Split(out, []byte{0})
	if len(parts) == 0 || len(parts)%3 != 1 || len(parts[len(parts)-1]) != 0 {
		return fmt.Errorf("classify mapper LFS entries: invalid git check-attr output")
	}
	lfsPaths := map[string]struct{}{}
	for i := 0; i+2 < len(parts)-1; i += 3 {
		if string(parts[i+1]) == "filter" && string(parts[i+2]) == "lfs" {
			lfsPaths[string(parts[i])] = struct{}{}
		}
	}
	for i := range entries {
		if _, ok := lfsPaths[entries[i].Path]; ok && entries[i].Disposition == security.MapperTreeDispositionRegular {
			entries[i].Disposition = security.MapperTreeDispositionLFS
		}
	}
	return nil
}

func populateMapperTreeEntryMetadata(repoRoot string, entries []security.MapperTreeIndexEntry) error {
	const maxLineCountBytes = 10 << 20
	for i := range entries {
		entry := &entries[i]
		if entry.Disposition != security.MapperTreeDispositionRegular {
			continue
		}
		fullPath := filepath.Join(repoRoot, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(fullPath)
		if err != nil {
			return fmt.Errorf("stat mapper tree entry %q: %w", entry.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("mapper tree entry %q is not a regular checked-out file", entry.Path)
		}
		entry.ContentSize = info.Size()
		if info.Size() > maxLineCountBytes {
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("read mapper tree entry %q: %w", entry.Path, err)
		}
		entry.LineCount = bytes.Count(data, []byte{'\n'})
		if len(data) > 0 && data[len(data)-1] != '\n' {
			entry.LineCount++
		}
	}
	return nil
}

func mapperSnapshotDigest(receipt *security.MapperTargetReceipt) string {
	payload := struct {
		Version              int    `json:"version"`
		ObjectFormat         string `json:"objectFormat"`
		HeadOID              string `json:"headOID"`
		TreeOID              string `json:"treeOID"`
		TreeDigest           string `json:"treeDigest"`
		CleanTrackedWorktree bool   `json:"cleanTrackedWorktree"`
	}{
		Version:              1,
		ObjectFormat:         receipt.ObjectFormat,
		HeadOID:              receipt.HeadOID,
		TreeOID:              receipt.TreeOID,
		TreeDigest:           receipt.TreeDigest,
		CleanTrackedWorktree: receipt.CleanTrackedWorktree,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func changedFilesForSecurityScan(
	ctx context.Context,
	workDir, baseCommit, headCommit string,
) (bool, []string, []security.ChangedLineRange, string, string, string) {
	if headCommit == "" {
		out, err := gitCommandNoReplace(ctx, "-C", workDir, "rev-parse", "HEAD").CombinedOutput()
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
	ancestor, err := gitCommitIsAncestor(ctx, workDir, baseCommit, headCommit)
	if err != nil {
		return false, nil, nil, "", err.Error(), headCommit
	}
	if !ancestor {
		return false, nil, nil, "", changedFilesDivergedError, headCommit
	}

	deletedOut, err := gitCommandNoReplace(ctx,
		"-C", workDir,
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

	out, err := gitCommandNoReplace(ctx,
		"-C", workDir,
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

func gitCommitIsAncestor(ctx context.Context, workDir, baseCommit, headCommit string) (bool, error) {
	ancestor, err := runGitCommitIsAncestor(ctx, workDir, baseCommit, headCommit)
	if ancestor || !isShallowGitRepository(ctx, workDir) {
		return ancestor, err
	}
	out, fetchErr := gitCommandNoReplace(ctx, "-C", workDir,
		"fetch", "--no-tags", "--unshallow", "origin").CombinedOutput()
	if fetchErr != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = fetchErr.Error()
		}
		message = redact.SensitiveText(message)
		if len(message) > 1024 {
			message = message[:1024]
			for len(message) > 0 && !utf8.ValidString(message) {
				message = message[:len(message)-1]
			}
		}
		return false, fmt.Errorf("verify incremental base ancestry: fetch complete history: %s", message)
	}
	return runGitCommitIsAncestor(ctx, workDir, baseCommit, headCommit)
}

func runGitCommitIsAncestor(ctx context.Context, workDir, baseCommit, headCommit string) (bool, error) {
	err := gitCommandNoReplace(ctx, "-C", workDir,
		"merge-base", "--is-ancestor", baseCommit, headCommit).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("verify incremental base ancestry: %w", err)
}

const (
	maxChangedLineRangesForArtifact  = 2000
	maxChangedDiffBytesForLineRanges = 2 * 1024 * 1024
	maxChangedDiffLinesForLineRanges = 20000
	changedFilesDivergedError        = "incremental base is not an ancestor of head; full scan required"
)

var (
	errChangedDiffTooLarge           = errors.New("changed diff exceeds changed-line metadata safety cap")
	changedLineRangesForSecurityScan = defaultChangedLineRangesForSecurityScan
)

func defaultChangedLineRangesForSecurityScan(
	ctx context.Context,
	workDir, baseCommit, headCommit string,
) ([]security.ChangedLineRange, error) {
	cmd := gitCommandNoReplace(ctx,
		"-C", workDir,
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

	out, err := gitCommandNoReplace(
		ctx,
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
	out, err = gitCommandNoReplace(ctx, append([]string{"-C", workDir}, args...)...).CombinedOutput()
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
	err := gitCommandNoReplace(ctx, "-C", workDir, "cat-file", "-e", commit+"^{commit}").Run()
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
	out, err := gitCommandNoReplace(ctx, "-C", workDir, "rev-parse", "--is-shallow-repository").CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}
