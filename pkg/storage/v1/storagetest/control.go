package storagetest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// controlEpochName is the only controller epoch name orka.storage.v1 accepts.
const controlEpochName = storagev1.DefaultControllerEpochName

var controlBaseTime = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func controlDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func controlFence(epoch int64, holder string) storagev1.ControllerEpochFence {
	return storagev1.ControllerEpochFence{Name: controlEpochName, Epoch: epoch, HolderID: holder}
}

func mirroredEpoch(epoch int64, holder string) storagev1.ControllerEpoch {
	return storagev1.ControllerEpoch{
		Name: controlEpochName, Epoch: epoch, HolderID: holder,
		RequestDigest: controlDigest(holder), Version: epoch,
		AcquiredAt: controlBaseTime, UpdatedAt: controlBaseTime,
	}
}

func syncEpoch(t *testing.T, provider storagev1.StateProvider, epoch int64, holder string) storagev1.ControllerEpochFence {
	t.Helper()
	require.NoError(t, provider.SyncControllerEpochMirror(t.Context(), mirroredEpoch(epoch, holder)))
	return controlFence(epoch, holder)
}

// requireFenced checks a stale-fence rejection. It also checks the
// compatibility match on ErrConflict that existing Orka callers rely on.
func requireFenced(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, storagev1.ErrFenced)
	require.ErrorIs(t, err, storagev1.ErrConflict, "a fence rejection still matches ErrConflict")
}

// runControllerEpochMirror checks that the mirror accepts exact replays and
// only moves forward.
func runControllerEpochMirror(t *testing.T, newProvider StateFactory) {
	provider := newProvider(t)
	ctx := t.Context()

	invalid := mirroredEpoch(0, "owner-a")
	require.ErrorIs(t, provider.SyncControllerEpochMirror(ctx, invalid), storagev1.ErrValidation)
	invalid = mirroredEpoch(1, "owner-a")
	invalid.RequestDigest = "not-a-digest"
	require.ErrorIs(t, provider.SyncControllerEpochMirror(ctx, invalid), storagev1.ErrValidation)

	syncEpoch(t, provider, 1, "owner-a")
	require.NoError(t, provider.SyncControllerEpochMirror(ctx, mirroredEpoch(1, "owner-a")), "identical replay")

	conflicting := mirroredEpoch(1, "owner-b")
	require.ErrorIs(t, provider.SyncControllerEpochMirror(ctx, conflicting), storagev1.ErrConflict)
	conflicting = mirroredEpoch(1, "owner-a")
	conflicting.RequestDigest = controlDigest("other request")
	require.ErrorIs(t, provider.SyncControllerEpochMirror(ctx, conflicting), storagev1.ErrConflict)

	// A mirror may move forward past epochs it never saw, but never rewinds.
	syncEpoch(t, provider, 3, "owner-c")
	require.ErrorIs(t, provider.SyncControllerEpochMirror(ctx, mirroredEpoch(2, "owner-b")), storagev1.ErrConflict)
	require.ErrorIs(t, provider.SyncControllerEpochMirror(ctx, mirroredEpoch(1, "owner-a")), storagev1.ErrConflict)
	require.NoError(t, provider.SyncControllerEpochMirror(ctx, mirroredEpoch(3, "owner-c")), "the mirror kept epoch 3")
}

func outboxProjection(id string, payload string, availableAt time.Time) *storagev1.OutboxProjection {
	body := json.RawMessage(payload)
	return &storagev1.OutboxProjection{
		ID: id, AggregateKind: "ConformanceAggregate", AggregateID: "aggregate-" + id, ProjectionKind: "ConformanceDelivery",
		Payload: body, PayloadDigest: storagev1.CanonicalBytesDigest(body),
		CreatedAt: controlBaseTime, AvailableAt: availableAt,
	}
}

func runOutboxPersistence(t *testing.T, newProvider StateFactory) {
	provider := newProvider(t)
	ctx := t.Context()
	fence := syncEpoch(t, provider, 1, "owner-a")

	_, err := provider.GetOutboxProjection(ctx, "missing")
	require.ErrorIs(t, err, storagev1.ErrNotFound)
	mismatched := outboxProjection("bad-digest", `{"n":1}`, controlBaseTime)
	mismatched.PayloadDigest = controlDigest("different")
	_, err = provider.EnqueueOutboxProjectionRecord(ctx, mismatched, fence)
	require.ErrorIs(t, err, storagev1.ErrValidation)

	due, err := provider.EnqueueOutboxProjectionRecord(ctx, outboxProjection("due", `{"n":1}`, controlBaseTime), fence)
	require.NoError(t, err)
	require.Equal(t, storagev1.OutboxProjectionPending, due.State)
	require.EqualValues(t, 1, due.Version)
	stored, err := provider.GetOutboxProjection(ctx, "due")
	require.NoError(t, err)
	require.Equal(t, due.ID, stored.ID)
	require.JSONEq(t, `{"n":1}`, string(stored.Payload))
	require.Equal(t, storagev1.OutboxProjectionPending, stored.State)
	require.Zero(t, stored.Attempts)
	require.True(t, stored.AvailableAt.Equal(controlBaseTime))

	replayed, err := provider.EnqueueOutboxProjectionRecord(ctx, outboxProjection("due", `{"n":1}`, controlBaseTime), fence)
	require.NoError(t, err, "identical enqueue is idempotent")
	require.EqualValues(t, 1, replayed.Version)
	_, err = provider.EnqueueOutboxProjectionRecord(ctx, outboxProjection("due", `{"n":2}`, controlBaseTime), fence)
	require.ErrorIs(t, err, storagev1.ErrConflict)

	later := controlBaseTime.Add(time.Hour)
	_, err = provider.EnqueueOutboxProjectionRecord(ctx, outboxProjection("later", `{"n":3}`, later), fence)
	require.NoError(t, err)
	requireBacklog(t, provider, 2)

	claimed, err := provider.ClaimOutboxProjectionRecords(ctx, storagev1.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "worker-a", Limit: 10, LeaseDuration: time.Minute, Now: controlBaseTime,
	})
	require.NoError(t, err)
	require.Len(t, claimed, 1, "only due projections are claimed")
	require.Equal(t, "due", claimed[0].ID)
	require.Equal(t, storagev1.OutboxProjectionDelivering, claimed[0].State)
	require.EqualValues(t, 1, claimed[0].Attempts)
	require.Equal(t, "worker-a", claimed[0].LeaseOwner)
	require.EqualValues(t, 2, claimed[0].Version)
	requireBacklog(t, provider, 2)

	delivered := storagev1.CompleteOutboxProjectionRequest{
		ID: "due", Fence: fence, ExpectedVersion: claimed[0].Version, LeaseOwner: "worker-a",
		OperationID: "deliver-due", OperationDigest: controlDigest("deliver-due"),
		NewState: storagev1.OutboxProjectionDelivered, DeliveryDigest: controlDigest("delivery"),
		UpdatedAt: controlBaseTime.Add(time.Second),
	}
	staleVersion := delivered
	staleVersion.ExpectedVersion = 1
	_, err = provider.CompleteOutboxProjectionRecord(ctx, staleVersion)
	require.ErrorIs(t, err, storagev1.ErrConflict)
	wrongOwner := delivered
	wrongOwner.LeaseOwner = "worker-b"
	_, err = provider.CompleteOutboxProjectionRecord(ctx, wrongOwner)
	require.ErrorIs(t, err, storagev1.ErrConflict)

	completed, err := provider.CompleteOutboxProjectionRecord(ctx, delivered)
	require.NoError(t, err)
	require.Equal(t, storagev1.OutboxProjectionDelivered, completed.State)
	require.NotNil(t, completed.DeliveredAt)
	require.EqualValues(t, 3, completed.Version)
	again, err := provider.CompleteOutboxProjectionRecord(ctx, delivered)
	require.NoError(t, err, "completion replay is idempotent")
	require.EqualValues(t, 3, again.Version)
	reusedOperation := delivered
	reusedOperation.OperationDigest = controlDigest("different completion")
	_, err = provider.CompleteOutboxProjectionRecord(ctx, reusedOperation)
	require.ErrorIs(t, err, storagev1.ErrConflict)
	requireBacklog(t, provider, 1)

	// An expired lease is reclaimed by another worker; retry and dead-letter
	// completions settle it.
	firstClaim, err := provider.ClaimOutboxProjectionRecords(ctx, storagev1.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "worker-a", Limit: 10, LeaseDuration: time.Minute, Now: later,
	})
	require.NoError(t, err)
	require.Len(t, firstClaim, 1)
	reclaimed, err := provider.ClaimOutboxProjectionRecords(ctx, storagev1.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "worker-b", Limit: 10, LeaseDuration: time.Minute, Now: later.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	require.Equal(t, "worker-b", reclaimed[0].LeaseOwner)
	require.EqualValues(t, 2, reclaimed[0].Attempts)
	_, err = provider.CompleteOutboxProjectionRecord(ctx, storagev1.CompleteOutboxProjectionRequest{
		ID: "later", Fence: fence, ExpectedVersion: firstClaim[0].Version, LeaseOwner: "worker-a",
		OperationID: "late-worker", OperationDigest: controlDigest("late-worker"),
		NewState: storagev1.OutboxProjectionDelivered, DeliveryDigest: controlDigest("late"),
		UpdatedAt: later.Add(3 * time.Minute),
	})
	require.ErrorIs(t, err, storagev1.ErrConflict, "a superseded lease cannot complete")

	retryAt := later.Add(10 * time.Minute)
	retried, err := provider.CompleteOutboxProjectionRecord(ctx, storagev1.CompleteOutboxProjectionRequest{
		ID: "later", Fence: fence, ExpectedVersion: reclaimed[0].Version, LeaseOwner: "worker-b",
		OperationID: "retry-later", OperationDigest: controlDigest("retry-later"),
		NewState: storagev1.OutboxProjectionPending, LastError: "adapter unavailable", AvailableAt: retryAt,
		UpdatedAt: later.Add(3 * time.Minute),
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OutboxProjectionPending, retried.State)
	require.True(t, retried.AvailableAt.Equal(retryAt))
	notYet, err := provider.ClaimOutboxProjectionRecords(ctx, storagev1.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "worker-b", Limit: 10, LeaseDuration: time.Minute, Now: retryAt.Add(-time.Second),
	})
	require.NoError(t, err)
	require.Empty(t, notYet)
	final, err := provider.ClaimOutboxProjectionRecords(ctx, storagev1.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "worker-b", Limit: 10, LeaseDuration: time.Minute, Now: retryAt,
	})
	require.NoError(t, err)
	require.Len(t, final, 1)
	deadLettered, err := provider.CompleteOutboxProjectionRecord(ctx, storagev1.CompleteOutboxProjectionRequest{
		ID: "later", Fence: fence, ExpectedVersion: final[0].Version, LeaseOwner: "worker-b",
		OperationID: "dead-letter-later", OperationDigest: controlDigest("dead-letter-later"),
		NewState: storagev1.OutboxProjectionDeadLetter, LastError: "permanent failure",
		UpdatedAt: retryAt.Add(time.Second),
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OutboxProjectionDeadLetter, deadLettered.State)
	requireBacklog(t, provider, 0)
}

func requireBacklog(t *testing.T, provider storagev1.StateProvider, want int64) {
	t.Helper()
	got, err := provider.CountUnsettledOutboxProjections(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func createControlSession(t *testing.T, provider storagev1.StateProvider, namespace, name, sessionType string) {
	t.Helper()
	require.NoError(t, provider.CreateSession(t.Context(), &storagev1.SessionRecord{
		Namespace: namespace, Name: name, SessionType: sessionType,
		CreatedAt: controlBaseTime, UpdatedAt: controlBaseTime,
	}))
}

func runSessionCleanupPersistence(t *testing.T, newProvider StateFactory) {
	t.Run("transcript-only cleanup leaves a completion receipt", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		createControlSession(t, provider, "tenant-a", "chat", "chat")
		createControlSession(t, provider, "tenant-b", "chat", "chat")

		_, err := provider.GetSessionCleanupIntent(ctx, "tenant-a", "chat")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = provider.GetSessionCleanupCompletion(ctx, "tenant-a", "chat")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		intent := storagev1.SessionCleanupIntent{
			Namespace: "tenant-a", SessionName: "chat", OperationID: "delete-chat",
			OperationDigest: controlDigest("delete-chat"), PreparedAt: controlBaseTime,
		}
		missingTime := intent
		missingTime.PreparedAt = time.Time{}
		_, err = provider.PrepareSessionCleanup(ctx, missingTime)
		require.ErrorIs(t, err, storagev1.ErrValidation)

		prepared, err := provider.PrepareSessionCleanup(ctx, intent)
		require.NoError(t, err)
		require.Equal(t, intent.OperationDigest, prepared.OperationDigest)
		replayed, err := provider.PrepareSessionCleanup(ctx, intent)
		require.NoError(t, err, "identical intent is idempotent")
		require.Equal(t, intent.OperationID, replayed.OperationID)
		otherOperation := intent
		otherOperation.OperationID, otherOperation.OperationDigest = "delete-again", controlDigest("delete-again")
		_, err = provider.PrepareSessionCleanup(ctx, otherOperation)
		require.ErrorIs(t, err, storagev1.ErrConflict)

		pending, err := provider.HasSessionCleanupIntent(ctx, "tenant-a", "chat")
		require.NoError(t, err)
		require.True(t, pending)
		pending, err = provider.HasSessionCleanupIntent(ctx, "tenant-b", "chat")
		require.NoError(t, err)
		require.False(t, pending, "intents are namespace scoped")
		stored, err := provider.GetSessionCleanupIntent(ctx, "tenant-a", "chat")
		require.NoError(t, err)
		require.Equal(t, intent.OperationID, stored.OperationID)
		listed, err := provider.ListSessionCleanupIntents(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.Equal(t, "tenant-a", listed[0].Namespace)
		turns, err := provider.ListSessionCleanupTurns(ctx, intent)
		require.NoError(t, err)
		require.Empty(t, turns)

		request := storagev1.CompleteSessionCleanupRequest{
			Namespace: intent.Namespace, SessionName: intent.SessionName,
			OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
		}
		wrong := request
		wrong.OperationID, wrong.OperationDigest = otherOperation.OperationID, otherOperation.OperationDigest
		require.ErrorIs(t, provider.CompleteSessionCleanup(ctx, wrong), storagev1.ErrConflict)
		require.NoError(t, provider.CompleteSessionCleanup(ctx, request))
		require.NoError(t, provider.CompleteSessionCleanup(ctx, request), "completion replay is idempotent")
		require.ErrorIs(t, provider.CompleteSessionCleanup(ctx, wrong), storagev1.ErrConflict)

		_, err = provider.GetSession(ctx, "tenant-a", "chat")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = provider.GetSession(ctx, "tenant-b", "chat")
		require.NoError(t, err, "other namespace is untouched")
		_, err = provider.GetSessionCleanupIntent(ctx, "tenant-a", "chat")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		listed, err = provider.ListSessionCleanupIntents(ctx)
		require.NoError(t, err)
		require.Empty(t, listed)
		completion, err := provider.GetSessionCleanupCompletion(ctx, "tenant-a", "chat")
		require.NoError(t, err)
		require.Equal(t, intent.OperationID, completion.OperationID)
		require.Equal(t, intent.OperationDigest, completion.OperationDigest)
		require.Empty(t, completion.SessionUID)
		require.False(t, completion.CompletedAt.IsZero())
		_, err = provider.GetSessionTurnCleanupReceipt(ctx, "tenant-a", "chat", "attempt-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("bound Session identity fences its UID", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		createControlSession(t, provider, "tenant", "agent", "task")

		require.ErrorIs(t, provider.BindSessionCleanupIdentity(ctx, "tenant", "missing", "uid-1"), storagev1.ErrNotFound)
		require.ErrorIs(t, provider.BindSessionCleanupIdentity(ctx, "tenant", "agent", ""), storagev1.ErrValidation)
		identity, err := provider.GetSessionCleanupIdentity(ctx, "tenant", "agent")
		require.NoError(t, err)
		require.Empty(t, identity)
		require.NoError(t, provider.BindSessionCleanupIdentity(ctx, "tenant", "agent", "uid-1"))
		require.NoError(t, provider.BindSessionCleanupIdentity(ctx, "tenant", "agent", "uid-1"), "rebinding is idempotent")
		require.ErrorIs(t, provider.BindSessionCleanupIdentity(ctx, "tenant", "agent", "uid-2"), storagev1.ErrConflict)
		identity, err = provider.GetSessionCleanupIdentity(ctx, "tenant", "agent")
		require.NoError(t, err)
		require.Equal(t, "uid-1", identity)

		fenced, err := provider.HasSessionCleanupFenceForUID(ctx, "uid-1")
		require.NoError(t, err)
		require.False(t, fenced)
		intent := storagev1.SessionCleanupIntent{
			Namespace: "tenant", SessionName: "agent", SessionUID: "uid-1", OperationID: "delete-agent",
			OperationDigest: controlDigest("delete-agent"), PreparedAt: controlBaseTime,
		}
		_, err = provider.PrepareSessionCleanup(ctx, intent)
		require.NoError(t, err)
		fenced, err = provider.HasSessionCleanupFenceForUID(ctx, "uid-1")
		require.NoError(t, err)
		require.True(t, fenced, "a pending intent fences its Session UID")
		fenced, err = provider.HasSessionCleanupFenceForUID(ctx, "uid-2")
		require.NoError(t, err)
		require.False(t, fenced)
		require.ErrorIs(t, provider.BindSessionCleanupIdentity(ctx, "tenant", "agent", "uid-1"), storagev1.ErrConflict,
			"identity cannot be rebound while deletion is pending")

		require.NoError(t, provider.CompleteSessionCleanup(ctx, storagev1.CompleteSessionCleanupRequest{
			Namespace: "tenant", SessionName: "agent", OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
		}))
		completion, err := provider.GetSessionCleanupCompletion(ctx, "tenant", "agent")
		require.NoError(t, err)
		require.Equal(t, "uid-1", completion.SessionUID)
		fenced, err = provider.HasSessionCleanupFenceForUID(ctx, "uid-1")
		require.NoError(t, err)
		require.True(t, fenced, "a completed deletion keeps fencing its Session UID")
		err = provider.CreateSession(ctx, &storagev1.SessionRecord{
			Namespace: "tenant", Name: "agent", SessionType: "task", CreatedAt: controlBaseTime, UpdatedAt: controlBaseTime,
		})
		require.ErrorIs(t, err, storagev1.ErrConflict, "a deleted Kubernetes identity reserves its Session name")
	})
}

func sessionLineage(namespace, name, sessionUID string) storagev1.SessionLineage {
	return storagev1.SessionLineage{
		Namespace: namespace, SessionName: name, NamespaceUID: "ns-uid-" + namespace, SessionUID: sessionUID,
		ContractVersion: "orka.harness.v2", LineageGeneration: 1, RuntimeIdentity: "codex",
		ConfigDigest: controlDigest("config-" + name), Version: 1,
		CreatedAt: controlBaseTime, UpdatedAt: controlBaseTime,
	}
}

func runSessionLineage(t *testing.T, newProvider StateFactory) {
	provider := newProvider(t)
	ctx := t.Context()

	_, err := provider.GetSessionLineage(ctx, "tenant", "session")
	require.ErrorIs(t, err, storagev1.ErrNotFound)
	_, err = provider.GetSessionLineage(ctx, "", "session")
	require.ErrorIs(t, err, storagev1.ErrValidation)
	invalid := sessionLineage("tenant", "session", "uid-1")
	invalid.ContractVersion = "orka.harness.v1"
	_, err = provider.ProjectSessionLineage(ctx, invalid)
	require.ErrorIs(t, err, storagev1.ErrValidation)

	lineage := sessionLineage("tenant", "session", "uid-1")
	projected, err := provider.ProjectSessionLineage(ctx, lineage)
	require.NoError(t, err)
	require.Equal(t, lineage.SessionUID, projected.SessionUID)
	stored, err := provider.GetSessionLineage(ctx, "tenant", "session")
	require.NoError(t, err)
	require.Equal(t, lineage.NamespaceUID, stored.NamespaceUID)
	require.Equal(t, lineage.ContractVersion, stored.ContractVersion)
	require.Equal(t, lineage.RuntimeIdentity, stored.RuntimeIdentity)
	require.Equal(t, lineage.ConfigDigest, stored.ConfigDigest)
	require.Equal(t, lineage.LineageGeneration, stored.LineageGeneration)
	require.True(t, stored.CreatedAt.Equal(lineage.CreatedAt))

	_, err = provider.ProjectSessionLineage(ctx, lineage)
	require.NoError(t, err, "identical projection is idempotent")
	for name, mutate := range map[string]func(*storagev1.SessionLineage){
		"session UID":   func(l *storagev1.SessionLineage) { l.SessionUID = "uid-2" },
		"namespace UID": func(l *storagev1.SessionLineage) { l.NamespaceUID = "ns-uid-other" },
		"runtime":       func(l *storagev1.SessionLineage) { l.RuntimeIdentity = "claude" },
		"config":        func(l *storagev1.SessionLineage) { l.ConfigDigest = controlDigest("other config") },
		"generation":    func(l *storagev1.SessionLineage) { l.LineageGeneration = 2 },
	} {
		changed := lineage
		mutate(&changed)
		_, err := provider.ProjectSessionLineage(ctx, changed)
		require.ErrorIs(t, err, storagev1.ErrConflict, name)
	}

	other, err := provider.ProjectSessionLineage(ctx, sessionLineage("other-tenant", "session", "uid-9"))
	require.NoError(t, err, "lineage is namespace scoped")
	require.Equal(t, "uid-9", other.SessionUID)
	stored, err = provider.GetSessionLineage(ctx, "tenant", "session")
	require.NoError(t, err)
	require.Equal(t, "uid-1", stored.SessionUID)
}

func runTaskJobAuthority(t *testing.T, newProvider StateFactory) {
	provider := newProvider(t)
	ctx := t.Context()
	job := storagev1.TaskJobIdentity{Namespace: "tenant", TaskUID: "task-uid", JobUID: "job-1"}
	sibling := storagev1.TaskJobIdentity{Namespace: "tenant", TaskUID: "task-uid", JobUID: "job-2"}
	otherNamespace := storagev1.TaskJobIdentity{Namespace: "other", TaskUID: "task-uid", JobUID: "job-1"}

	require.ErrorIs(t, provider.CheckTaskJobAuthority(ctx, storagev1.TaskJobIdentity{Namespace: "tenant", TaskUID: "task-uid"}),
		storagev1.ErrValidation)
	require.ErrorIs(t, provider.RevokeTaskJob(ctx, storagev1.TaskJobIdentity{Namespace: "tenant", JobUID: "job-1"}),
		storagev1.ErrValidation)
	require.NoError(t, provider.CheckTaskJobAuthority(ctx, job))

	require.NoError(t, provider.RevokeTaskJob(ctx, job))
	require.NoError(t, provider.RevokeTaskJob(ctx, job), "revocation is idempotent")
	require.ErrorIs(t, provider.CheckTaskJobAuthority(ctx, job), storagev1.ErrTaskJobRevoked)
	require.NoError(t, provider.CheckTaskJobAuthority(ctx, sibling), "revocation is exact to one Job")
	require.NoError(t, provider.CheckTaskJobAuthority(ctx, otherNamespace), "revocation is namespace scoped")

	require.ErrorIs(t, provider.DeleteTaskJobRevocations(ctx, "tenant", "", "task-uid"), storagev1.ErrValidation)
	require.NoError(t, provider.DeleteTaskJobRevocations(ctx, "tenant", "task", "task-uid"))
	require.NoError(t, provider.CheckTaskJobAuthority(ctx, job), "finalizer reclamation clears revocations")
}
