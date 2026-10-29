package storagetest

import (
	"slices"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// exportSeedTenant is the namespace the export seeds write to.
const exportSeedTenant = "tenant-export"

// reportsExport reports whether provider reports the export capability, and
// fails when it reports it without implementing the export and import
// surface.
func reportsExport(t *testing.T, provider storagev1.ProviderDescriber) bool {
	t.Helper()
	if !slices.Contains(describe(t, provider).Capabilities, storagev1.ExportCapability) {
		return false
	}
	_, ok := provider.(ExportProvider)
	require.True(t, ok, "%T reports %s without implementing the export and import surface", provider, storagev1.ExportCapability)
	return true
}

// copyDataset exports source and imports it into target, returning the
// imported dataset's description.
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

// stateExportSeed is what seedStateExport wrote that an import must continue
// from.
type stateExportSeed struct {
	// lastMessageID is the highest message ID issued, whose message was then
	// deleted.
	lastMessageID int64
	// lastSessionSeq is the session's latest event sequence, whose Task
	// stream was then deleted.
	lastSessionSeq int64
	// lastTaskSeq is the latest sequence of the remaining Task stream.
	lastTaskSeq int64
}

// seedStateExport writes records through state.v1, including sequences whose
// highest values were deleted, so a copy must carry the sequence heads, not
// only the records.
func seedStateExport(t *testing.T, provider storagev1.StateProvider) stateExportSeed {
	t.Helper()
	ctx := t.Context()
	var seed stateExportSeed
	require.NoError(t, provider.SaveResult(ctx, exportSeedTenant, "task-a", []byte("a result")))

	for _, from := range []string{"worker-a", "worker-c"} {
		require.NoError(t, provider.SendMessage(ctx, &storagev1.Message{
			Namespace: exportSeedTenant, FromTask: from, ToTask: "worker-b", ParentTask: "parent", Content: "from " + from,
		}))
	}
	sent, err := provider.GetMessages(ctx, exportSeedTenant, "worker-b", "parent", false)
	require.NoError(t, err)
	require.Len(t, sent, 2)
	seed.lastMessageID = max(sent[0].ID, sent[1].ID)
	require.NoError(t, provider.DeleteTaskMessages(ctx, exportSeedTenant, "worker-c"))

	createSessionFixture(t, provider, exportSeedTenant, eventsSession, sessionsTypeTask, exportExampleTime)
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
	require.NoError(t, provider.DeleteExecutionEvents(ctx, exportSeedTenant, storagev1.ExecutionEventStreamTypeTask, "task-a"))
	_, seed.lastSessionSeq, err = provider.ListSessionExecutionEvents(ctx, storagev1.SessionExecutionEventFilter{
		Namespace: exportSeedTenant, SessionName: eventsSession,
	})
	require.NoError(t, err)
	return seed
}

func runStateExportImport(t *testing.T, newProvider StateFactory) {
	if !reportsExport(t, newProvider(t)) {
		t.Skipf("the provider does not report %s", storagev1.ExportCapability)
	}
	newExport := func(t *testing.T) ExportProvider { return newProvider(t).(ExportProvider) }
	RunExportImport(t, newExport, func(t *testing.T, provider ExportProvider) {
		seedStateExport(t, provider.(storagev1.StateProvider))
	})

	t.Run("an imported dataset continues its sequences", func(t *testing.T) {
		source := newProvider(t)
		seed := seedStateExport(t, source)
		target := newProvider(t)
		copyDataset(t, source.(ExportProvider), target.(ExportProvider))
		ctx := t.Context()

		result, err := target.GetResult(ctx, exportSeedTenant, "task-a")
		require.NoError(t, err)
		require.Equal(t, []byte("a result"), result)
		require.NoError(t, target.SendMessage(ctx, &storagev1.Message{
			Namespace: exportSeedTenant, FromTask: "worker-d", ToTask: "worker-b", ParentTask: "parent", Content: "after the import",
		}))
		received, err := target.GetMessages(ctx, exportSeedTenant, "worker-b", "parent", false)
		require.NoError(t, err)
		require.Len(t, received, 2)
		require.Greater(t, received[1].ID, seed.lastMessageID, "a message ID issued before the import, even of a deleted message, is never issued again")

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
	})

	t.Run("an importing dataset refuses writes", func(t *testing.T) {
		source := newProvider(t)
		seedStateExport(t, source)
		manifest, _ := exportDataset(t, source.(ExportProvider))
		target := newProvider(t)
		_, err := target.(ExportProvider).BeginImport(t.Context(), beginImportRequest(t, describe(t, source), manifest))
		require.NoError(t, err)
		require.ErrorIs(t, target.SaveResult(t.Context(), exportSeedTenant, "task-b", []byte("written during the import")), storagev1.ErrNotReady)
	})

	t.Run("a migrated contract part refuses writes", func(t *testing.T) {
		source := newProvider(t)
		seedStateExport(t, source)
		info := describe(t, source)
		request := storagev1.MigrationMarkerRequest{DatasetID: info.DatasetID, Contract: storagev1.StateContract, TargetDatasetID: "target-dataset"}
		_, err := source.(ExportProvider).MarkDatasetMigrated(t.Context(), request)
		require.NoError(t, err)
		ctx := t.Context()
		require.ErrorIs(t, source.SaveResult(ctx, exportSeedTenant, "task-b", []byte("written after the migration")), storagev1.ErrDatasetMigrated)
		result, err := source.GetResult(ctx, exportSeedTenant, "task-a")
		require.NoError(t, err, "reads still work")
		require.Equal(t, []byte("a result"), result)
		require.NoError(t, source.(ExportProvider).ClearMigrationMarker(ctx, request))
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
	newExport := func(t *testing.T) ExportProvider { return newProvider(t).(ExportProvider) }
	RunExportImport(t, newExport, func(t *testing.T, provider ExportProvider) {
		seedArtifactsExport(t, provider.(storagev1.ArtifactProvider))
	})

	t.Run("an imported dataset serves the artifacts", func(t *testing.T) {
		source := newProvider(t)
		seedArtifactsExport(t, source)
		target := newProvider(t)
		copyDataset(t, source.(ExportProvider), target.(ExportProvider))
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
		_, err := source.(ExportProvider).MarkDatasetMigrated(t.Context(), request)
		require.NoError(t, err)
		err = source.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{
			Owner: artifactOwner(exportSeedTenant, "task-b"), Filename: "late.txt", ContentType: "text/plain", Data: []byte("late"),
		})
		require.ErrorIs(t, err, storagev1.ErrDatasetMigrated)
	})
}
