/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/orka-agents/orka/internal/security"
	storepkg "github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func parseChangedLineRangesFromUnifiedDiff(diff []byte) ([]security.ChangedLineRange, error) {
	return parseChangedLineRangesFromUnifiedDiffReader(bytes.NewReader(diff))
}

func TestRun_Success(t *testing.T) {
	os.Args = []string{"worker", "echo", "hello"}
	err := run()
	if err != nil {
		t.Errorf("run() returned error: %v", err)
	}
}

func TestRun_NoCommand(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"worker"}
	os.Unsetenv("ORKA_COMMAND") //nolint:errcheck
	err := run()
	if err == nil {
		t.Error("run() should return error when no command specified")
	}
}

func TestRun_CommandFromEnv(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"worker"}
	os.Setenv("ORKA_COMMAND", "echo hello") //nolint:errcheck
	defer os.Unsetenv("ORKA_COMMAND")       //nolint:errcheck

	err := run()
	if err != nil {
		t.Errorf("run() returned error: %v", err)
	}
}

func TestRun_EmitsWorkerEventsOnSuccess(t *testing.T) {
	eventTypes := captureGeneralWorkerHTTPEvents(t, "general-success-task", func() {
		origArgs := os.Args
		t.Cleanup(func() { os.Args = origArgs })
		os.Args = []string{"worker", "printf", "hello"}

		if err := run(); err != nil {
			t.Fatalf("run() error = %v", err)
		}
	})

	want := []string{"WorkerStarted", "ResultSubmitted", "WorkerCompleted"}
	if !reflect.DeepEqual(eventTypes, want) {
		t.Fatalf("event types = %#v, want %#v", eventTypes, want)
	}
}

func TestRun_EmitsWorkerFailedEventOnError(t *testing.T) {
	eventTypes := captureGeneralWorkerHTTPEvents(t, "general-failure-task", func() {
		origArgs := os.Args
		t.Cleanup(func() { os.Args = origArgs })
		os.Args = []string{"worker", "nonexistent_command_12345"}

		if err := run(); err == nil {
			t.Fatal("run() error = nil, want command error")
		}
	})

	want := []string{"WorkerStarted", "WorkerFailed"}
	if !reflect.DeepEqual(eventTypes, want) {
		t.Fatalf("event types = %#v, want %#v", eventTypes, want)
	}
}

func captureGeneralWorkerHTTPEvents(t *testing.T, taskName string, runWorker func()) []string {
	t.Helper()

	var mu sync.Mutex
	var eventTypes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/internal/v1/events/default/task/"+taskName):
			defer r.Body.Close() //nolint:errcheck
			var body struct {
				Type     string `json:"type"`
				TaskName string `json:"taskName"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode event body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.TaskName != taskName {
				t.Errorf("event taskName = %q, want %q", body.TaskName, taskName)
			}
			mu.Lock()
			eventTypes = append(eventTypes, body.Type)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(r.URL.Path, "/internal/v1/results/default/"+taskName):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskName, taskName)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv("ORKA_ARTIFACTS_DIR", filepath.Join(t.TempDir(), "artifacts"))
	runWorker()

	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), eventTypes...)
}

func TestWorkspaceRootUsesSubPath(t *testing.T) {
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "src")
	if got := workspaceRoot(); got != filepath.Join(workspaceDir, "src") {
		t.Fatalf("workspaceRoot() = %q", got)
	}
}

func TestBuildSecurityMapperArtifactEmitsCoverageAccountableV2(t *testing.T) {
	root := newMapperGitRepo(t)
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.go) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# app\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(README.md) error = %v", err)
	}
	headOID := commitMapperRepo(t, root, "initial")
	t.Setenv(workerenv.GitBranch, "main")
	t.Setenv(workerenv.GitRef, "refs/heads/main")

	artifact, err := buildSecurityMapperArtifact(context.Background(), root, "repo", "", headOID, true)
	if err != nil {
		t.Fatalf("buildSecurityMapperArtifact() error = %v", err)
	}
	if artifact.SchemaVersion != security.SchemaVersionReviewSlicesV2 {
		t.Fatalf("schemaVersion = %d, want %d", artifact.SchemaVersion, security.SchemaVersionReviewSlicesV2)
	}
	if artifact.CoverageStatus != security.MapperCoverageAccountable {
		t.Fatalf("coverageStatus = %q, want %q", artifact.CoverageStatus, security.MapperCoverageAccountable)
	}
	if artifact.DiscoveredFiles == nil || artifact.ReviewableFiles == nil || artifact.OmittedFiles == nil {
		t.Fatalf("v2 inventories must be arrays: %#v", artifact)
	}
	if artifact.InventorySummary == nil || artifact.InventorySummary.Truncated ||
		artifact.InventorySummary.TotalEntries != len(artifact.DiscoveredFiles) {
		t.Fatalf("inventorySummary = %#v, want complete retained inventory", artifact.InventorySummary)
	}
	if artifact.TargetReceipt == nil || artifact.TargetReceipt.HeadOID != headOID ||
		!artifact.TargetReceipt.CleanTrackedWorktree || artifact.TargetReceipt.RequestedBranch != "main" ||
		artifact.TargetReceipt.RequestedRef != "refs/heads/main" {
		t.Fatalf("targetReceipt = %#v, want exact clean HEAD and requested refs", artifact.TargetReceipt)
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	parsed, err := security.ParseReviewSlicesArtifact(data)
	if err != nil {
		t.Fatalf("ParseReviewSlicesArtifact() error = %v", err)
	}
	if parsed.CoverageStatus != security.MapperCoverageAccountable {
		t.Fatalf("parsed coverageStatus = %q, want accountable", parsed.CoverageStatus)
	}
}

func TestMapperCoverageForInventoryMarksTruncationPartial(t *testing.T) {
	status, reasons := mapperCoverageForInventory(security.MapperInventorySummary{
		EntryLimit:       2,
		TotalEntries:     5,
		RetainedEntries:  2,
		TruncatedEntries: 3,
		Truncated:        true,
		Reason:           security.MapperCoverageReasonInventoryEntryLimit,
	})
	if status != security.MapperCoveragePartial {
		t.Fatalf("coverage status = %q, want %q", status, security.MapperCoveragePartial)
	}
	if len(reasons) != 1 || reasons[0] != security.MapperCoverageReasonInventoryEntryLimit {
		t.Fatalf("coverage reasons = %#v, want stable inventory limit reason", reasons)
	}
}

func TestBuildMapperTargetReceiptRejectsAbbreviatedHeadAndDirtyWorktree(t *testing.T) {
	t.Run("abbreviated head", func(t *testing.T) {
		root := newMapperGitRepo(t)
		if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(app.go) error = %v", err)
		}
		headOID := commitMapperRepo(t, root, "initial")
		if _, err := buildMapperTargetReceipt(context.Background(), root, "", headOID[:12]); err == nil {
			t.Fatal("buildMapperTargetReceipt() error = nil, want abbreviated head rejected")
		} else if !strings.Contains(err.Error(), "must be a full") {
			t.Fatalf("buildMapperTargetReceipt() error = %v, want full object ID rejection", err)
		}
	})

	t.Run("untracked reviewable file", func(t *testing.T) {
		root := newMapperGitRepo(t)
		if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package app\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		commitMapperRepo(t, root, "initial")
		if err := os.WriteFile(filepath.Join(root, "untracked.go"), []byte("package app\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := buildMapperTargetReceipt(context.Background(), root, "", "")
		if err == nil || !strings.Contains(err.Error(), "not clean") {
			t.Fatalf("untracked worktree error = %v", err)
		}
	})

	t.Run("dirty tracked worktree", func(t *testing.T) {
		root := newMapperGitRepo(t)
		path := filepath.Join(root, "app.go")
		if err := os.WriteFile(path, []byte("package app\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(app.go) error = %v", err)
		}
		commitMapperRepo(t, root, "initial")
		if err := os.WriteFile(path, []byte("package app\n\nfunc dirty() {}\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(dirty app.go) error = %v", err)
		}
		if _, err := buildMapperTargetReceipt(context.Background(), root, "", ""); err == nil {
			t.Fatal("buildMapperTargetReceipt() error = nil, want dirty tracked worktree rejected")
		} else if !strings.Contains(err.Error(), "not clean") {
			t.Fatalf("buildMapperTargetReceipt() error = %v, want dirty worktree rejection", err)
		}
	})
}

func TestBuildMapperTargetReceiptClassifiesAndBoundsTreeIndex(t *testing.T) {
	root := newMapperGitRepo(t)
	for path, content := range map[string]string{
		".gitattributes": "large.bin filter=lfs\n",
		"app.go":         "package app\n",
		"large.bin":      "version https://git-lfs.github.com/spec/v1\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	runGit(t, root, "config", "filter.lfs.clean", "cat")
	runGit(t, root, "config", "filter.lfs.smudge", "cat")
	runGit(t, root, "config", "filter.lfs.required", "false")
	symlinkAdded := true
	if err := os.Symlink("app.go", filepath.Join(root, "app-link")); err != nil {
		symlinkAdded = false
	}
	headOID := commitMapperRepo(t, root, "tree fixtures")

	receipt, err := buildMapperTargetReceipt(context.Background(), root, "base-ref", headOID)
	if err != nil {
		t.Fatalf("buildMapperTargetReceipt() error = %v", err)
	}
	if receipt.TreeDigest == "" || receipt.SnapshotDigest == "" || receipt.TreeOID == "" {
		t.Fatalf("target receipt missing digests: %#v", receipt)
	}
	if !treeEntryHasDisposition(receipt.TreeIndex, "large.bin", security.MapperTreeDispositionLFS) {
		t.Fatalf("treeIndex = %#v, want large.bin classified as LFS", receipt.TreeIndex)
	}
	if symlinkAdded && !treeEntryHasDisposition(receipt.TreeIndex, "app-link", security.MapperTreeDispositionSymlink) {
		t.Fatalf("treeIndex = %#v, want app-link classified as symlink", receipt.TreeIndex)
	}
	repeated, err := buildMapperTargetReceipt(context.Background(), root, "base-ref", headOID)
	if err != nil {
		t.Fatalf("buildMapperTargetReceipt(repeated) error = %v", err)
	}
	if !reflect.DeepEqual(receipt, repeated) {
		t.Fatalf("target receipt is not deterministic:\nfirst=%#v\nsecond=%#v", receipt, repeated)
	}

	oldLimit := mapperTreeIndexLimit
	mapperTreeIndexLimit = 2
	t.Cleanup(func() { mapperTreeIndexLimit = oldLimit })
	bounded, err := buildMapperTargetReceipt(context.Background(), root, "base-ref", headOID)
	if err != nil {
		t.Fatalf("buildMapperTargetReceipt(bounded) error = %v", err)
	}
	if !bounded.TreeIndexTruncated || len(bounded.TreeIndex) != 2 || bounded.TreeEntryCount <= len(bounded.TreeIndex) {
		t.Fatalf("bounded target receipt = %#v, want a two-entry truncated index", bounded)
	}
}

func TestParseMapperTreeEntryClassifiesSubmodule(t *testing.T) {
	record := []byte("160000 commit " + strings.Repeat("a", 40) + "\tdeps/submodule\x00")
	entry, err := parseMapperTreeEntry(record, "sha1")
	if err != nil {
		t.Fatalf("parseMapperTreeEntry() error = %v", err)
	}
	if entry.Disposition != security.MapperTreeDispositionSubmodule {
		t.Fatalf("disposition = %q, want %q", entry.Disposition, security.MapperTreeDispositionSubmodule)
	}
}

func treeEntryHasDisposition(entries []security.MapperTreeIndexEntry, path, disposition string) bool {
	for _, entry := range entries {
		if entry.Path == path && entry.Disposition == disposition {
			return true
		}
	}
	return false
}

func TestPrepareWorkspaceIfConfiguredSetsCredentialsForExistingCheckout(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatalf("Mkdir(.git) error = %v", err)
	}
	oldWorkspaceDir := workspaceDir
	oldSetupGitCredentials := setupGitCredentialsForGeneral
	t.Cleanup(func() {
		workspaceDir = oldWorkspaceDir
		setupGitCredentialsForGeneral = oldSetupGitCredentials
	})
	workspaceDir = workspace
	calls := 0
	setupGitCredentialsForGeneral = func() {
		calls++
	}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "./src")

	got, err := prepareWorkspaceIfConfigured(context.Background())
	if err != nil {
		t.Fatalf("prepareWorkspaceIfConfigured() error = %v", err)
	}
	if got != filepath.Join(workspace, "src") {
		t.Fatalf("prepareWorkspaceIfConfigured() = %q, want sanitized subpath root", got)
	}
	if calls != 1 {
		t.Fatalf("setupGitCredentialsForGeneral calls = %d, want 1", calls)
	}
	if os.Getenv("ORKA_WORKSPACE_SUBPATH") != "src" {
		t.Fatalf("ORKA_WORKSPACE_SUBPATH = %q, want sanitized src", os.Getenv("ORKA_WORKSPACE_SUBPATH"))
	}
}

func TestPrepareWorkspaceIfConfiguredRejectsTraversalSubPath(t *testing.T) {
	oldSetupGitCredentials := setupGitCredentialsForGeneral
	t.Cleanup(func() {
		setupGitCredentialsForGeneral = oldSetupGitCredentials
	})
	setupGitCredentialsForGeneral = func() {
		t.Fatal("setupGitCredentialsForGeneral should not be called for invalid subpath")
	}
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "../outside")

	if _, err := prepareWorkspaceIfConfigured(context.Background()); err == nil {
		t.Fatal("prepareWorkspaceIfConfigured() error = nil, want traversal rejection")
	} else if !strings.Contains(err.Error(), "contains path traversal") {
		t.Fatalf("prepareWorkspaceIfConfigured() error = %v, want traversal rejection", err)
	}
}

func TestConfigurePublicationRemotePushesToPublicationRepository(t *testing.T) {
	sourceBare := filepath.Join(t.TempDir(), "source.git")
	targetBare := filepath.Join(t.TempDir(), "target.git")
	seed := t.TempDir()
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "init", "--bare", sourceBare)
	runGit(t, t.TempDir(), "init", "--bare", targetBare)
	runGit(t, seed, "init", "-b", "main")
	runGit(t, seed, "config", "user.email", "orka@example.com")
	runGit(t, seed, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "file.txt")
	runGit(t, seed, "commit", "-m", "source")
	runGit(t, seed, "remote", "add", "origin", "file://"+sourceBare)
	runGit(t, seed, "push", "-u", "origin", "main")

	clone := exec.Command("git", "clone", "--branch", "main", "file://"+sourceBare, work)
	if output, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("git clone failed: %s: %v", strings.TrimSpace(string(output)), err)
	}
	t.Setenv(workerenv.ForkRepo, "file://"+targetBare)
	t.Setenv(workerenv.PushBranch, "orka/publish")
	t.Setenv(workerenv.RequirePushBranch, "true")
	if err := configurePublicationRemote(work); err != nil {
		t.Fatalf("configurePublicationRemote() error = %v", err)
	}
	if got := runGit(t, work, "remote", "get-url", "origin"); got != "file://"+targetBare {
		t.Fatalf("origin URL = %q, want publication repository", got)
	}
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("published\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := common.FinalizeResult(work, "published"); err != nil {
		t.Fatalf("FinalizeResult() error = %v", err)
	}
	targetHead := runGit(t, targetBare, "rev-parse", "refs/heads/orka/publish")
	if targetHead == "" {
		t.Fatal("publication repository branch is empty")
	}
	command := exec.Command(
		"git", "-C", sourceBare, "rev-parse", "--verify", "refs/heads/orka/publish",
	)
	if command.Run() == nil {
		t.Fatal("source repository unexpectedly received the publication branch")
	}
}

func TestRun_CommandNotFound(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	// run() calls os.Exit for exec failures, so we test the underlying exec
	os.Args = []string{"worker", "nonexistent_command_12345"}
	err := run()
	if err == nil {
		t.Error("run() should return error for nonexistent command")
	}
	if _, ok := err.(*exec.Error); !ok {
		t.Errorf("expected *exec.Error, got %T", err)
	}
}

func TestChangedFilesForSecurityScanFetchesMissingBaseInShallowClone(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "orka@example.com")
	runGit(t, source, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base) error = %v", err)
	}
	runGit(t, source, "add", "app.go")
	runGit(t, source, "commit", "-m", "base")
	baseCommit := runGit(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(
		filepath.Join(source, "app.go"),
		[]byte("package main\n\nfunc main() {}\n"),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile(head) error = %v", err)
	}
	runGit(t, source, "commit", "-am", "head")
	headCommit := runGit(t, source, "rev-parse", "HEAD")

	clone := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", "--depth=1", "file://"+source, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone shallow failed: %s: %v", strings.TrimSpace(string(out)), err)
	}
	if gitCommitAvailable(context.Background(), clone, baseCommit) {
		t.Fatal("base commit is already available in shallow clone; fixture is invalid")
	}

	computed, files, lineRanges, diffSummary, message, resolvedHead := changedFilesForSecurityScan(
		context.Background(), clone, baseCommit, "",
	)
	if !computed {
		t.Fatalf("changedFilesForSecurityScan() computed=false message=%q", message)
	}
	if resolvedHead != headCommit {
		t.Fatalf("resolved head = %q, want %q", resolvedHead, headCommit)
	}
	if !reflect.DeepEqual(files, []string{"app.go"}) {
		t.Fatalf("changed files = %#v, want app.go", files)
	}
	if len(lineRanges) != 1 || lineRanges[0].Path != "app.go" ||
		lineRanges[0].StartLine != 2 || lineRanges[0].EndLine != 3 {
		t.Fatalf("changed line ranges = %#v, want app.go:2-3", lineRanges)
	}
	if !strings.Contains(diffSummary, "1 changed files") {
		t.Fatalf("diff summary = %q, want changed-file count", diffSummary)
	}
	if !gitCommitAvailable(context.Background(), clone, baseCommit) {
		t.Fatal("base commit was not fetched into shallow clone")
	}
}

func TestChangedFilesForSecurityScanFallsBackToFullReviewForDeletedFiles(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "orka@example.com")
	runGit(t, source, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(source, "auth.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base) error = %v", err)
	}
	runGit(t, source, "add", "auth.go")
	runGit(t, source, "commit", "-m", "base")
	baseCommit := runGit(t, source, "rev-parse", "HEAD")
	runGit(t, source, "rm", "auth.go")
	runGit(t, source, "commit", "-m", "remove auth")
	headCommit := runGit(t, source, "rev-parse", "HEAD")

	computed, files, lineRanges, _, message, resolvedHead := changedFilesForSecurityScan(
		context.Background(),
		source,
		baseCommit,
		headCommit,
	)
	if computed {
		t.Fatal("changedFilesForSecurityScan() computed=true, want false when deleted files require full review")
	}
	if len(files) != 0 || len(lineRanges) != 0 {
		t.Fatalf("changed files/ranges = %#v/%#v, want none when falling back to full review", files, lineRanges)
	}
	if resolvedHead != headCommit {
		t.Fatalf("resolved head = %q, want %q", resolvedHead, headCommit)
	}
	if !strings.Contains(message, "deleted files require full review") || !strings.Contains(message, "auth.go") {
		t.Fatalf("message = %q, want deleted-file full-review fallback", message)
	}
}

func TestChangedFilesForSecurityScanFallsBackForDivergedBase(t *testing.T) {
	source := newMapperGitRepo(t)
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(common) error = %v", err)
	}
	commonCommit := commitMapperRepo(t, source, "common")

	runGit(t, source, "checkout", "-b", "rewritten")
	rewritten := []byte("package main\n\nfunc rewritten() {}\n")
	if err := os.WriteFile(filepath.Join(source, "app.go"), rewritten, 0o644); err != nil {
		t.Fatalf("WriteFile(rewritten) error = %v", err)
	}
	rewrittenHead := commitMapperRepo(t, source, "rewritten head")

	runGit(t, source, "checkout", "main")
	original := []byte("package main\n\nfunc original() {}\n")
	if err := os.WriteFile(filepath.Join(source, "app.go"), original, 0o644); err != nil {
		t.Fatalf("WriteFile(original) error = %v", err)
	}
	divergedBase := commitMapperRepo(t, source, "original head")
	if commonCommit == divergedBase || commonCommit == rewrittenHead {
		t.Fatal("divergent history fixture did not create distinct commits")
	}

	computed, files, lineRanges, diffSummary, message, resolvedHead := changedFilesForSecurityScan(
		context.Background(), source, divergedBase, rewrittenHead,
	)
	if computed || len(files) != 0 || len(lineRanges) != 0 || diffSummary != "" {
		t.Fatalf(
			"diverged incremental result = computed:%v files:%#v ranges:%#v summary:%q, want full-scan fallback",
			computed, files, lineRanges, diffSummary,
		)
	}
	if message != changedFilesDivergedError {
		t.Fatalf("changedFilesError = %q, want stable %q", message, changedFilesDivergedError)
	}
	if resolvedHead != rewrittenHead {
		t.Fatalf("resolved head = %q, want %q", resolvedHead, rewrittenHead)
	}
}

func TestChangedFilesForSecurityScanRejectsNonSHARevisions(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "orka@example.com")
	runGit(t, source, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base) error = %v", err)
	}
	runGit(t, source, "add", "app.go")
	runGit(t, source, "commit", "-m", "base")

	computed, files, lineRanges, _, message, _ := changedFilesForSecurityScan(context.Background(), source, "HEAD", "")
	if computed {
		t.Fatal("changedFilesForSecurityScan() computed=true, want false for non-SHA base")
	}
	if len(files) != 0 || len(lineRanges) != 0 {
		t.Fatalf("changed files/ranges = %#v/%#v, want none for rejected base", files, lineRanges)
	}
	if !strings.Contains(message, "not a hex SHA") {
		t.Fatalf("message = %q, want non-SHA rejection", message)
	}
}

func TestChangedLineRangesForSecurityScanParsesUnifiedDiff(t *testing.T) {
	diff := []byte("diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1,0 +2,2 @@\n+one\n+two\n")
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{{Path: "app.go", StartLine: 2, EndLine: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}

func TestChangedLineRangesHandlesMultipleHunksSameFile(t *testing.T) {
	diff := []byte(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -1 +1 @@",
		"+one",
		"@@ -10 +12,2 @@",
		"+two",
		"+three",
		"",
	}, "\n"))
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{
		{Path: "app.go", StartLine: 1, EndLine: 1},
		{Path: "app.go", StartLine: 12, EndLine: 13},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}

func TestChangedLineRangesIgnoresAddedLinesThatLookLikeFileHeaders(t *testing.T) {
	diff := []byte(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -1 +1,2 @@",
		"+++ not a file header",
		"+ordinary addition",
		"@@ -10 +11 @@",
		"+later addition",
		"",
	}, "\n"))
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{
		{Path: "app.go", StartLine: 1, EndLine: 2},
		{Path: "app.go", StartLine: 11, EndLine: 11},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}

func TestChangedLineRangesPreservesDeletionOnlyHunkAnchor(t *testing.T) {
	diff := []byte(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -6 +6,0 @@",
		"-removedSecurityCheck()",
		"",
	}, "\n"))
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{{Path: "app.go", StartLine: 6, EndLine: 6}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want deletion anchor %#v", got, want)
	}
}

func TestChangedLineRangesPreservesFirstLineDeletionAnchor(t *testing.T) {
	diff := []byte(strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -1 +0,0 @@",
		"-removedSecurityCheck()",
		"",
	}, "\n"))
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	want := []security.ChangedLineRange{{Path: "app.go", StartLine: 1, EndLine: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %#v, want deletion anchor %#v", got, want)
	}
}

func TestChangedFilesForSecurityScanKeepsFilesWhenLineRangeParseFails(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.email", "orka@example.com")
	runGit(t, source, "config", "user.name", "Orka Test")
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(base) error = %v", err)
	}
	runGit(t, source, "add", "app.go")
	runGit(t, source, "commit", "-m", "base")
	baseCommit := runGit(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "app.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(head) error = %v", err)
	}
	runGit(t, source, "commit", "-am", "head")
	headCommit := runGit(t, source, "rev-parse", "HEAD")

	original := changedLineRangesForSecurityScan
	changedLineRangesForSecurityScan = func(context.Context, string, string, string) ([]security.ChangedLineRange, error) {
		return nil, fmt.Errorf("parse changed line ranges: unsupported hunk header")
	}
	t.Cleanup(func() { changedLineRangesForSecurityScan = original })

	computed, files, lineRanges, diffSummary, message, _ := changedFilesForSecurityScan(
		context.Background(), source, baseCommit, headCommit,
	)
	if !computed || message != "" {
		t.Fatalf("computed=%v message=%q, want file-level fallback without error message", computed, message)
	}
	if !reflect.DeepEqual(files, []string{"app.go"}) || len(lineRanges) != 0 {
		t.Fatalf("files/ranges = %#v/%#v, want app.go and no ranges", files, lineRanges)
	}
	if !strings.Contains(diffSummary, "changed line ranges omitted") {
		t.Fatalf("diffSummary = %q, want line-range omission", diffSummary)
	}
}

func TestChangedLineRangeParserStopsAtSafetyCap(t *testing.T) {
	diff := "diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1 @@\n+" +
		strings.Repeat("x", maxChangedDiffBytesForLineRanges+1)
	_, err := parseChangedLineRangesFromUnifiedDiff([]byte(diff))
	if !errors.Is(err, errChangedDiffTooLarge) {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v, want diff safety cap", err)
	}
}

func TestChangedLineRangeParserStopsAtRangeCountCap(t *testing.T) {
	var diff strings.Builder
	diff.WriteString("diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n")
	for i := 1; i <= maxChangedLineRangesForArtifact+1; i++ {
		fmt.Fprintf(&diff, "@@ -%d +%d @@\n+line\n", i, i)
	}
	_, err := parseChangedLineRangesFromUnifiedDiff([]byte(diff.String()))
	if !errors.Is(err, errChangedDiffTooLarge) {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v, want range count cap", err)
	}
}

func TestChangedLineRangesRejectsUnsafePaths(t *testing.T) {
	diff := []byte("diff --git a/../secret b/../secret\n--- a/../secret\n+++ b/../secret\n@@ -1 +1 @@\n+secret\n")
	got, err := parseChangedLineRangesFromUnifiedDiff(diff)
	if err != nil {
		t.Fatalf("parseChangedLineRangesFromUnifiedDiff() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ranges = %#v, want unsafe path ignored", got)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %s: %v", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out))
}

func newMapperGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	runGit(t, root, "config", "user.email", "orka@example.com")
	runGit(t, root, "config", "user.name", "Orka Test")
	return root
}

func commitMapperRepo(t *testing.T, root, message string) string {
	t.Helper()
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", message)
	return runGit(t, root, "rev-parse", "HEAD")
}

func TestCleanTrackedWorktreeIgnoresArtifactDirectoryContents(t *testing.T) {
	repo := newMapperGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".orka-artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(repo, ".orka-artifacts", "security-review-slices.json")
	if err := os.WriteFile(artifactPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err := cleanTrackedWorktree(context.Background(), repo)
	if err != nil || !clean {
		t.Fatalf("cleanTrackedWorktree(artifacts) = %v, %v", clean, err)
	}
	ignoreFile := filepath.Join(repo, ".gitignore")
	if err := os.WriteFile(ignoreFile, []byte(".orka-artifacts/\nignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitMapperRepo(t, repo, "add ignores")
	if err := os.WriteFile(filepath.Join(repo, "ignored.txt"), []byte("ignored dirty input"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err = cleanTrackedWorktree(context.Background(), repo)
	if err != nil || clean {
		t.Fatalf("cleanTrackedWorktree(ignored input) = %v, %v", clean, err)
	}
	if err := os.Remove(filepath.Join(repo, "ignored.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "unexpected.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err = cleanTrackedWorktree(context.Background(), repo)
	if err != nil || clean {
		t.Fatalf("cleanTrackedWorktree(unexpected) = %v, %v", clean, err)
	}
}

func TestBuildSecurityMapperArtifactLegacyModeSkipsPinnedReceipt(t *testing.T) {
	repo := newMapperGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := commitMapperRepo(t, repo, "initial")
	artifact, err := buildSecurityMapperArtifact(context.Background(), repo, "repo", "", head, false)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.SchemaVersion != security.SchemaVersionReviewSlices || artifact.TargetReceipt != nil {
		t.Fatalf("legacy mapper artifact = %#v", artifact)
	}
}

func TestMarshalMapperArtifactWithinBudgetTruncatesInventories(t *testing.T) {
	artifact := &security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlices,
		Slices:        []storepkg.ReviewSlice{{ID: "slice_api"}},
	}
	longPath := strings.Repeat("a", 4000)
	for i := range 3000 {
		artifact.DiscoveredFiles = append(artifact.DiscoveredFiles, security.MapperFileInventoryEntry{
			Path: fmt.Sprintf("%s/%d.go", longPath, i), Disposition: "discovered", Reason: "source",
		})
	}
	data, err := marshalMapperArtifactWithinBudget(artifact)
	if err != nil {
		t.Fatalf("marshalMapperArtifactWithinBudget() error = %v", err)
	}
	if len(data) > maxMapperArtifactBytes {
		t.Fatalf("len(data) = %d, want <= %d", len(data), maxMapperArtifactBytes)
	}
	if len(artifact.DiscoveredFiles) != 0 || len(artifact.ReviewableFiles) != 0 || len(artifact.OmittedFiles) != 0 {
		t.Fatalf("inventories were not truncated: %d/%d/%d",
			len(artifact.DiscoveredFiles), len(artifact.ReviewableFiles), len(artifact.OmittedFiles))
	}
	truncated := false
	for _, code := range artifact.CoverageReasonCodes {
		if code == "inventory_truncated" {
			truncated = true
		}
	}
	if !truncated {
		t.Fatalf("coverage reason codes = %v, want inventory_truncated", artifact.CoverageReasonCodes)
	}
	var decoded security.ReviewSlicesArtifact
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(decoded.Slices) != 1 {
		t.Fatalf("len(slices) = %d, want slices preserved", len(decoded.Slices))
	}
}

func TestMarshalMapperArtifactWithinBudgetKeepsSmallInventories(t *testing.T) {
	artifact := &security.ReviewSlicesArtifact{
		SchemaVersion:   security.SchemaVersionReviewSlices,
		DiscoveredFiles: []security.MapperFileInventoryEntry{{Path: "main.go", Disposition: "discovered", Reason: "source"}},
	}
	data, err := marshalMapperArtifactWithinBudget(artifact)
	if err != nil {
		t.Fatalf("marshalMapperArtifactWithinBudget() error = %v", err)
	}
	var decoded security.ReviewSlicesArtifact
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(decoded.DiscoveredFiles) != 1 {
		t.Fatalf("small inventory was truncated: %v", decoded.DiscoveredFiles)
	}
}

func TestMarshalMapperArtifactWithinBudgetFailsClosedForPinnedV2(t *testing.T) {
	artifact := &security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlicesV2,
	}
	longPath := strings.Repeat("b", 4000)
	for i := range 3000 {
		artifact.DiscoveredFiles = append(artifact.DiscoveredFiles, security.MapperFileInventoryEntry{
			Path: fmt.Sprintf("%s/%d.go", longPath, i), Disposition: "reviewable", Reason: "source",
		})
	}
	if _, err := marshalMapperArtifactWithinBudget(artifact); err == nil ||
		!strings.Contains(err.Error(), "cannot be truncated") {
		t.Fatalf("marshalMapperArtifactWithinBudget(v2 oversized) error = %v, want fail-closed budget error", err)
	}
	if len(artifact.DiscoveredFiles) == 0 {
		t.Fatal("v2 inventories must not be truncated")
	}
}
