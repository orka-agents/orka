package storagetest

import (
	"fmt"
	"slices"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

const exportSeedTenant = "tenant-export"

func reportsExport(t *testing.T, provider storagev1.ProviderDescriber) bool {
	t.Helper()
	if !slices.Contains(describe(t, provider).Capabilities, storagev1.ExportCapability) {
		return false
	}
	exporter := harnessProvider(provider)
	_, ok := exporter.(ExportProvider)
	require.True(t, ok, "%T reports %s without implementing the export and import surface", exporter, storagev1.ExportCapability)
	return true
}

func exporter(provider storagev1.ProviderDescriber) ExportProvider {
	return harnessProvider(provider).(ExportProvider)
}

type suiteExport struct {
	ExportProvider
	writes storagev1.ProviderDescriber
}

func newSuiteExport[P storagev1.ProviderDescriber](newProvider func(t *testing.T) P) ExportFactory {
	return func(t *testing.T) ExportProvider {
		provider := newProvider(t)
		return suiteExport{ExportProvider: exporter(provider), writes: provider}
	}
}

func copyDataset(t *testing.T, source, target ExportProvider) storagev1.ProviderInfo {
	t.Helper()
	manifest, records := exportDataset(t, source)
	request := beginImportRequest(t, describe(t, source), manifest)
	session, err := target.BeginImport(t.Context(), request)
	require.NoError(t, err)
	importDataset(t, target, session.SessionID, manifest, records, false)
	info, err := target.CompleteImport(t.Context(), storagev1.CompleteImportRequest{
		SessionID: session.SessionID, ManifestDigest: request.Source.ManifestDigest,
	})
	require.NoError(t, err)
	return info
}

type stateExportSeed struct {
	lastMessageID    int64
	unreadMessageID  int64
	sessionCreatedAt time.Time
	lastSessionSeq   int64
	lastTaskSeq      int64
}

func seedStateExport(t *testing.T, provider storagev1.StateProvider) stateExportSeed {
	t.Helper()
	ctx := t.Context()
	var seed stateExportSeed
	require.NoError(t, provider.SaveResult(ctx, exportSeedTenant, "task-a", []byte("a result")))

	sent := make([]int64, 0, 3)
	for _, from := range []string{"worker-a", "worker-e", "worker-c"} {
		message := &storagev1.Message{
			Namespace: exportSeedTenant, FromTask: from, ToTask: "worker-b", ParentTask: "parent", Content: "from " + from,
		}
		require.NoError(t, provider.SendMessage(ctx, message))
		sent = append(sent, message.ID)
	}
	require.True(t, slices.IsSorted(sent), "message IDs are issued in order")
	seed.lastMessageID, seed.unreadMessageID = sent[2], sent[1]
	consumed, err := provider.ConsumeMessages(ctx, storagev1.ConsumeMessagesRequest{
		Namespace: exportSeedTenant, TaskName: "worker-b", ParentTask: "parent", Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, consumed, 1)
	require.Equal(t, sent[0], consumed[0].ID)
	require.NoError(t, provider.DeleteTaskMessages(ctx, exportSeedTenant, "worker-c"))

	createSessionFixture(t, provider, exportSeedTenant, eventsSession, sessionsTypeTask, exportExampleTime)
	seed.sessionCreatedAt, err = provider.GetSessionCreatedAt(ctx, exportSeedTenant, eventsSession)
	require.NoError(t, err)
	require.NoError(t, provider.AppendMessages(ctx, exportSeedTenant, eventsSession, []storagev1.SessionMessage{
		{ID: "m1", Role: "user", Content: "a question", Timestamp: exportExampleTime},
		{ID: "m2", Role: "assistant", Content: "an answer", Timestamp: exportExampleTime.Add(1)},
	}))
	for _, task := range []string{"task-a", "task-b", "task-a"} {
		event := taskEvent(exportSeedTenant, task, storagev1.ExecutionEventTypeTaskStarted)
		event.SessionName = eventsSession
		appended := appendEvent(t, provider, event)
		if task == "task-b" {
			seed.lastTaskSeq = appended.Seq
		}
	}
	require.NoError(t, provider.UpsertFinding(ctx, workflowFinding("fp-export", "high")))
	_, seed.lastSessionSeq, err = provider.ListSessionExecutionEvents(ctx, storagev1.SessionExecutionEventFilter{
		Namespace: exportSeedTenant, SessionName: eventsSession,
	})
	require.NoError(t, err)
	require.Equal(t, int64(3), seed.lastSessionSeq, "the session issued one sequence per appended event")
	require.NoError(t, provider.DeleteExecutionEvents(ctx, exportSeedTenant, storagev1.ExecutionEventStreamTypeTask, "task-a"))
	return seed
}

func runStateExportImport(t *testing.T, newProvider StateFactory) {
	if !reportsExport(t, newProvider(t)) {
		t.Skipf("the provider does not report %s", storagev1.ExportCapability)
	}
	RunExportImport(t, newSuiteExport(newProvider), func(t *testing.T, provider ExportProvider) {
		seedStateExport(t, provider.(suiteExport).writes.(storagev1.StateProvider))
	})

	t.Run("an imported dataset continues its sequences", func(t *testing.T) {
		source := newProvider(t)
		seed := seedStateExport(t, source)
		target := newProvider(t)
		copyDataset(t, exporter(source), exporter(target))
		ctx := t.Context()

		result, err := target.GetResult(ctx, exportSeedTenant, "task-a")
		require.NoError(t, err)
		require.Equal(t, []byte("a result"), result)
		require.NoError(t, target.SendMessage(ctx, &storagev1.Message{
			Namespace: exportSeedTenant, FromTask: "worker-d", ToTask: "worker-b", ParentTask: "parent", Content: "after the import",
		}))
		received, err := target.ListUnreadMessages(ctx, exportSeedTenant, "worker-b", "parent", 0)
		require.NoError(t, err)
		require.Len(t, received, 2, "a message consumed before the export stays read")
		require.Equal(t, seed.unreadMessageID, received[0].ID, "a message unread before the export stays unread")
		require.Greater(t, received[1].ID, seed.lastMessageID, "a message ID issued before the import, even of a deleted message, is never issued again")
		createdAt, err := target.GetSessionCreatedAt(ctx, exportSeedTenant, eventsSession)
		require.NoError(t, err)
		require.True(t, seed.sessionCreatedAt.Equal(createdAt), "an imported Session keeps its creation time, which identifies its incarnation")

		require.NoError(t, target.AppendMessages(ctx, exportSeedTenant, eventsSession, []storagev1.SessionMessage{
			{ID: "m3", Role: "user", Content: "after the import", Timestamp: exportExampleTime.Add(2)},
		}))
		transcript, err := target.LoadTranscript(ctx, exportSeedTenant, eventsSession, 0)
		require.NoError(t, err)
		require.Len(t, transcript, 3)
		require.True(t, exportExampleTime.Equal(transcript[0].Timestamp), "times keep their nanoseconds")
		require.Equal(t, []string{"m1", "m2", "m3"}, []string{transcript[0].ID, transcript[1].ID, transcript[2].ID})
		require.Greater(t, transcript[2].Order, transcript[1].Order, "a transcript continues after its imported messages")

		event := taskEvent(exportSeedTenant, "task-b", storagev1.ExecutionEventTypeTaskSucceeded)
		event.SessionName = eventsSession
		appended := appendEvent(t, target, event)
		require.Equal(t, seed.lastTaskSeq+1, appended.Seq, "a stream continues after its imported events")
		listed, latest, err := target.ListSessionExecutionEvents(ctx, storagev1.SessionExecutionEventFilter{
			Namespace: exportSeedTenant, SessionName: eventsSession, AfterSeq: seed.lastSessionSeq,
		})
		require.NoError(t, err)
		require.Equal(t, seed.lastSessionSeq+1, latest, "a session sequence continues past a deleted stream's events")
		require.Len(t, listed, 1)
		require.Equal(t, latest, listed[0].SessionSeq)

		finding, err := target.GetFinding(ctx, workflowNamespace, "fp-export")
		require.NoError(t, err)
		require.Equal(t, workflowFinding("fp-export", "high").Evidence, finding.Evidence, "evidence keeps every field, including its role")
		byRun, _, err := target.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, ScanRunID: "run-1"})
		require.NoError(t, err)
		require.Equal(t, []string{"fp-export"}, workflowFindingIDs(byRun), "the scan run filter finds the imported finding")
		otherRun, _, err := target.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, ScanRunID: "run-2"})
		require.NoError(t, err)
		require.Empty(t, otherRun)
	})

	runImportedWriteOrder(t, newProvider, resultOrder())

	t.Run("an imported dataset keeps the admission order of scan runs", func(t *testing.T) {
		source := newProvider(t)
		ctx := t.Context()
		require.NoError(t, source.CreateScanRun(ctx, workflowScanRun("run-z", "running")))
		require.NoError(t, source.CreateScanRun(ctx, workflowScanRun("run-a", "succeeded")))
		target := newProvider(t)
		copyDataset(t, exporter(source), exporter(target))

		listed, _, err := target.ListScanRuns(ctx, workflowNamespace, workflowScan, 10, "")
		require.NoError(t, err)
		require.Equal(t, []string{"run-a", "run-z"}, scanRunIDs(listed), "runs list newest first in their admission order")
		require.ErrorIs(t, target.UpdateScanRun(ctx, workflowScanRun("run-z", "running")), storagev1.ErrConflict,
			"an active run admitted before a newer one does not update as active")
		require.NoError(t, target.UpdateScanRun(ctx, workflowScanRun("run-z", "failed")))
		require.NoError(t, target.CreateScanRun(ctx, workflowScanRun("run-0", "succeeded")))
		listed, _, err = target.ListScanRuns(ctx, workflowNamespace, workflowScan, 10, "")
		require.NoError(t, err)
		require.Equal(t, []string{"run-0", "run-a", "run-z"}, scanRunIDs(listed), "a run admitted after the import is newer than every imported run")
	})

	t.Run("an imported dataset keeps the issuance order of sign-in sessions", func(t *testing.T) {
		source := newProvider(t)
		ctx := t.Context()
		at := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
		last := storagev1.MaxAuthSessionsPerSubject - 1
		for i := range storagev1.MaxAuthSessionsPerSubject {
			id := fmt.Sprintf("sess-%02d", last-i)
			require.NoError(t, source.CreateAuthSession(ctx, testAuthSession(id, "hash-"+id, at)))
		}
		target := newProvider(t)
		copyDataset(t, exporter(source), exporter(target))

		require.NoError(t, target.CreateAuthSession(ctx, testAuthSession("sess-new", "hash-sess-new", at)))
		_, err := target.GetAuthSessionByHash(ctx, fmt.Sprintf("hash-sess-%02d", last))
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a sign-in beyond the bound ends the least recently issued session")
		for i := range last {
			_, err := target.GetAuthSessionByHash(ctx, fmt.Sprintf("hash-sess-%02d", i))
			require.NoError(t, err, "a more recently issued session stays")
		}
		_, err = target.GetAuthSessionByHash(ctx, "hash-sess-new")
		require.NoError(t, err, "a session issued after the import is newer than every imported session")
	})

	t.Run("an importing dataset refuses writes", func(t *testing.T) {
		source := newProvider(t)
		seedStateExport(t, source)
		manifest, _ := exportDataset(t, exporter(source))
		target := newProvider(t)
		_, err := exporter(target).BeginImport(t.Context(), beginImportRequest(t, describe(t, source), manifest))
		require.NoError(t, err)
		data := []byte("written during the import")
		ctx := storagev1.WithWriteEnvelope(t.Context(),
			NewWriteEnvelope(t, describe(t, target).DatasetID, suiteFence, "SaveResult", exportSeedTenant, "task-b", data))
		err = target.SaveResult(ctx, exportSeedTenant, "task-b", data)
		require.ErrorIs(t, err, storagev1.ErrNotReady)
		require.NotErrorIs(t, err, storagev1.ErrFenced)
	})

	t.Run("a migrated contract part refuses writes", func(t *testing.T) {
		source := newProvider(t)
		seedStateExport(t, source)
		info := describe(t, source)
		request := storagev1.MigrationMarkerRequest{DatasetID: info.DatasetID, Contract: storagev1.StateContract, TargetDatasetID: "target-dataset"}
		_, err := exporter(source).MarkDatasetMigrated(t.Context(), request)
		require.NoError(t, err)
		ctx := t.Context()
		require.ErrorIs(t, source.SaveResult(ctx, exportSeedTenant, "task-b", []byte("written after the migration")), storagev1.ErrDatasetMigrated)
		result, err := source.GetResult(ctx, exportSeedTenant, "task-a")
		require.NoError(t, err, "reads still work")
		require.Equal(t, []byte("a result"), result)
		require.NoError(t, exporter(source).ClearMigrationMarker(ctx, request))
		require.NoError(t, source.SaveResult(ctx, exportSeedTenant, "task-b", []byte("written after a rollback")))
	})
}

func seedArtifactsExport(t *testing.T, provider storagev1.ArtifactProvider) {
	t.Helper()
	saveArtifact(t, provider, artifactOwner(exportSeedTenant, "task-a"), "report.txt", "text/plain", []byte("report"))
	saveArtifact(t, provider, artifactOwner(exportSeedTenant, "task-a"), "data.bin", "application/octet-stream", []byte{0, 1, 2})
}

func runArtifactsExportImport(t *testing.T, newProvider ArtifactFactory) {
	if !reportsExport(t, newProvider(t)) {
		t.Skipf("the provider does not report %s", storagev1.ExportCapability)
	}
	RunExportImport(t, newSuiteExport(newProvider), func(t *testing.T, provider ExportProvider) {
		seedArtifactsExport(t, provider.(suiteExport).writes.(storagev1.ArtifactProvider))
	})

	runImportedWriteOrder(t, newProvider, artifactOrder())

	t.Run("an imported dataset serves the artifacts", func(t *testing.T) {
		source := newProvider(t)
		seedArtifactsExport(t, source)
		target := newProvider(t)
		copyDataset(t, exporter(source), exporter(target))
		data, contentType, err := target.GetArtifact(t.Context(), artifactKey(exportSeedTenant, "task-a", "data.bin"))
		require.NoError(t, err)
		require.Equal(t, []byte{0, 1, 2}, data)
		require.Equal(t, "application/octet-stream", contentType)
	})

	t.Run("a migrated contract part refuses writes", func(t *testing.T) {
		source := newProvider(t)
		seedArtifactsExport(t, source)
		info := describe(t, source)
		request := storagev1.MigrationMarkerRequest{DatasetID: info.DatasetID, Contract: storagev1.ArtifactsContract, TargetDatasetID: "target-dataset"}
		_, err := exporter(source).MarkDatasetMigrated(t.Context(), request)
		require.NoError(t, err)
		err = source.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{
			Owner: artifactOwner(exportSeedTenant, "task-b"), Filename: "late.txt", ContentType: "text/plain", Data: []byte("late"),
		})
		require.ErrorIs(t, err, storagev1.ErrDatasetMigrated)
	})
}
