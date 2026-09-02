package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const testJournalDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestJournalReclaimsWorkspaceObjectAfterTerminalReplayIsDurable(t *testing.T) {
	t.Parallel()
	store := newTestJournalStore(t)
	metadata := OperationMetadata{Namespace: "default", OperationID: "workspace-prepare-1", TaskID: "task-1"}
	requestDigest := journalTestDigest("b")
	if _, _, err := store.begin(context.Background(), OperationWorkspacePrepare, metadata, requestDigest); err != nil {
		t.Fatalf("begin: %v", err)
	}

	source := writeJournalWorkspaceObject(t, "workspace archive")
	prepared := WorkspacePrepareResponse{
		OperationID: metadata.OperationID,
		Artifact: harnessv2.ArtifactReference{
			Digest:    testJournalDigest,
			SizeBytes: int64(len("workspace archive")),
		},
	}
	if err := store.setWorkspaceObjectFile(
		context.Background(), metadata.OperationID, requestDigest, prepared, source, int64(len("workspace archive")),
	); err != nil {
		t.Fatalf("persist workspace: %v", err)
	}
	objectPath, err := store.workspaceObjectPath(testJournalDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(objectPath); err != nil {
		t.Fatalf("workspace object before completion: %v", err)
	}

	response := []byte(`{"operationId":"workspace-prepare-1"}`)
	if err := store.complete(context.Background(), metadata.OperationID, requestDigest, 200, response); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := os.Stat(objectPath); !os.IsNotExist(err) {
		t.Fatalf("workspace object after completion err = %v, want not exist", err)
	}

	replayed, found, err := store.begin(context.Background(), OperationWorkspacePrepare, metadata, requestDigest)
	if err != nil {
		t.Fatalf("replay begin: %v", err)
	}
	if !found || replayed.State != journalCompleted || string(replayed.Response) != string(response) {
		t.Fatalf("replayed record = %#v, found=%v", replayed, found)
	}
}

func TestJournalKeepsSharedWorkspaceObjectUntilEveryUploadSettles(t *testing.T) {
	t.Parallel()
	store := newTestJournalStore(t)
	objectPath, err := store.workspaceObjectPath(testJournalDigest)
	if err != nil {
		t.Fatal(err)
	}

	type operation struct {
		id     string
		taskID string
		digest string
	}
	operations := make([]operation, 0, 2)
	for index, taskID := range []string{"task-1", "task-2"} {
		operationID := "workspace-prepare-" + taskID
		digest := journalTestDigest(string(rune('b' + index)))
		metadata := OperationMetadata{Namespace: "default", OperationID: operationID, TaskID: taskID}
		if _, _, err := store.begin(context.Background(), OperationWorkspacePrepare, metadata, digest); err != nil {
			t.Fatalf("begin %s: %v", taskID, err)
		}
		operations = append(operations, operation{id: operationID, taskID: taskID, digest: digest})
	}

	source := writeJournalWorkspaceObject(t, "shared archive")
	for _, operation := range operations {
		prepared := WorkspacePrepareResponse{
			OperationID: operation.id,
			Artifact: harnessv2.ArtifactReference{
				Digest:    testJournalDigest,
				SizeBytes: int64(len("shared archive")),
			},
		}
		if err := store.setWorkspaceObjectFile(
			context.Background(), operation.id, operation.digest, prepared, source, int64(len("shared archive")),
		); err != nil {
			t.Fatalf("persist workspace %s: %v", operation.taskID, err)
		}
	}

	firstDigest := journalTestDigest("b")
	if err := store.complete(context.Background(), "workspace-prepare-task-1", firstDigest, 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete first owner: %v", err)
	}
	if _, err := os.Stat(objectPath); err != nil {
		t.Fatalf("shared object was reclaimed while referenced: %v", err)
	}

	secondDigest := journalTestDigest("c")
	if err := store.complete(context.Background(), "workspace-prepare-task-2", secondDigest, 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete second owner: %v", err)
	}
	if _, err := os.Stat(objectPath); !os.IsNotExist(err) {
		t.Fatalf("shared object after final completion err = %v, want not exist", err)
	}
}

func TestJournalReclaimsEarlierTerminalRecordsAfterOwnerAdvances(t *testing.T) {
	t.Parallel()
	store := newTestJournalStore(t)
	resolveMetadata := OperationMetadata{Namespace: "default", OperationID: "workspace-resolve-1", TaskID: "task-1"}
	resolveDigest := journalTestDigest("b")
	if _, _, err := store.begin(context.Background(), OperationWorkspaceResolve, resolveMetadata, resolveDigest); err != nil {
		t.Fatalf("begin resolve: %v", err)
	}
	if err := store.complete(context.Background(), resolveMetadata.OperationID, resolveDigest, 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete resolve: %v", err)
	}
	resolvePath := store.recordPath(resolveMetadata.OperationID)
	if _, err := os.Stat(resolvePath); err != nil {
		t.Fatalf("resolve record before owner advance: %v", err)
	}

	prepareMetadata := OperationMetadata{Namespace: "default", OperationID: "workspace-prepare-1", TaskID: "task-1"}
	prepareDigest := journalTestDigest("c")
	if _, _, err := store.begin(context.Background(), OperationWorkspacePrepare, prepareMetadata, prepareDigest); err != nil {
		t.Fatalf("begin prepare: %v", err)
	}
	if _, err := os.Stat(resolvePath); err != nil {
		t.Fatalf("recent resolve replay was pruned when a later operation merely started: %v", err)
	}
	resolveRecord, found, err := store.readLocked(resolveMetadata.OperationID)
	if err != nil || !found {
		t.Fatalf("read resolve record = found %t err %v", found, err)
	}
	resolveRecord.UpdatedAt = resolveRecord.UpdatedAt.Add(-journalReplayRetention - time.Hour)
	if err := store.writeLocked(resolveRecord); err != nil {
		t.Fatalf("age resolve record: %v", err)
	}
	if err := store.reclaimLocked(prepareMetadata.OperationID, ""); err != nil {
		t.Fatalf("reclaim aged resolve record: %v", err)
	}
	if _, err := os.Stat(resolvePath); !os.IsNotExist(err) {
		t.Fatalf("resolve record after owner advance err = %v, want not exist", err)
	}
	if _, err := os.Stat(store.recordPath(prepareMetadata.OperationID)); err != nil {
		t.Fatalf("active prepare record: %v", err)
	}
}

func TestJournalDoesNotPruneAcrossTaskIncarnations(t *testing.T) {
	t.Parallel()
	store := newTestJournalStore(t)
	oldPrepare := OperationMetadata{Namespace: "default", OperationID: "workspace-prepare-prompt-old", TaskID: "task-1"}
	oldDigest := journalTestDigest("d")
	if _, _, err := store.begin(context.Background(), OperationWorkspacePrepare, oldPrepare, oldDigest); err != nil {
		t.Fatal(err)
	}
	if err := store.complete(context.Background(), oldPrepare.OperationID, oldDigest, 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}

	newResolve := OperationMetadata{Namespace: "default", OperationID: "workspace-resolve-prompt-new", TaskID: "task-1"}
	newDigest := journalTestDigest("e")
	if _, _, err := store.begin(context.Background(), OperationWorkspaceResolve, newResolve, newDigest); err != nil {
		t.Fatal(err)
	}
	if err := store.complete(context.Background(), newResolve.OperationID, newDigest, 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.recordPath(newResolve.OperationID)); err != nil {
		t.Fatalf("new incarnation resolve replay was pruned: %v", err)
	}
}

func journalTestDigest(hexDigit string) string {
	return "sha256:" + strings.Repeat(hexDigit, 64)
}

func newTestJournalStore(t *testing.T) *journalStore {
	t.Helper()
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	store, err := newJournalStore(t.TempDir(), 16<<20, func() time.Time {
		now = now.Add(time.Second)
		return now
	})
	if err != nil {
		t.Fatalf("new journal store: %v", err)
	}
	return store
}

func writeJournalWorkspaceObject(t *testing.T, contents string) string {
	t.Helper()
	path := t.TempDir() + "/workspace.tar"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write workspace object: %v", err)
	}
	return path
}
