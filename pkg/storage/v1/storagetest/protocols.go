package storagetest

import (
	"context"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// protocolRun drives the store half of one Kubernetes control protocol on a
// provider whose dataset starts under suiteFence. The protocols are numbered
// as in docs/development/storage-contracts.md; Kubernetes runs the other
// steps, which the cases name but do not perform.
type protocolRun struct {
	t        *testing.T
	provider storagev1.StateProvider
	dataset  string
}

func newProtocolRun(t *testing.T, newProvider StateFactory) *protocolRun {
	t.Helper()
	provider := newProvider(t)
	advanceEpoch(t, provider, suiteFence)
	return &protocolRun{t: t, provider: provider, dataset: datasetID(t, provider)}
}

// op returns a context carrying a new operation of method under fence. A
// caller whose response was lost retries the same operation by calling
// again with the same context.
func (r *protocolRun) op(fence storagev1.ControllerEpochFence, method string, args ...any) context.Context {
	r.t.Helper()
	return storagev1.WithWriteEnvelope(r.t.Context(), NewWriteEnvelope(r.t, r.dataset, fence, method, args...))
}

// takeover advances the dataset to nextFence, as a replacement controller's
// epoch manager does before it opens its fence.
func (r *protocolRun) takeover() {
	r.t.Helper()
	advanceEpoch(r.t, r.provider, nextFence)
}

// runProtocols checks the store half of the outbox delivery (P3), Session
// cleanup (P4), prompt result receipt (P5) and Gateway Task cleanup receipt
// (P7) protocols. For each store step it loses the response and retries the
// same operation, runs the step again as a new operation, replaces the
// controller between steps, and delivers a step the replaced controller
// issued after the replacement. Every retry must return the original
// outcome, every rerun must be recognized by the step's natural key or the
// state it changes, and every delayed step must be refused without a change.
func runProtocols(t *testing.T, newProvider StateFactory) {
	t.Run("P3 outbox projection delivery", func(t *testing.T) { runOutboxProtocol(t, newProvider) })
	t.Run("P4 Session cleanup", func(t *testing.T) { runSessionCleanupProtocol(t, newProvider) })
	t.Run("P4 transcript-only Session cleanup", func(t *testing.T) { runTranscriptCleanupProtocol(t, newProvider) })
	t.Run("P5 prompt result receipt", func(t *testing.T) { runPromptResultProtocol(t, newProvider) })
	t.Run("P7 Gateway Task cleanup receipts", func(t *testing.T) { runGatewayTaskCleanupProtocol(t, newProvider) })
	t.Run("P7 delayed compaction", func(t *testing.T) { runDelayedGatewayCompaction(t, newProvider) })
}

func runOutboxProtocol(t *testing.T, newProvider StateFactory) {
	r := newProtocolRun(t, newProvider)
	p, ctx := r.provider, t.Context()
	const id = "p3-projection"
	projection := func(payload string) *storagev1.OutboxProjection {
		return outboxProjection(id, payload, controlBaseTime)
	}

	// Step 1: enqueue. A lost response is retried with the same operation.
	enqueue := r.op(suiteFence, "EnqueueOutboxProjectionRecord", projection(`{"n":1}`), suiteFence)
	first, err := p.EnqueueOutboxProjectionRecord(enqueue, projection(`{"n":1}`), suiteFence)
	require.NoError(t, err)
	require.EqualValues(t, 1, first.Version)
	replayed, err := p.EnqueueOutboxProjectionRecord(enqueue, projection(`{"n":1}`), suiteFence)
	require.NoError(t, err)
	require.EqualValues(t, 1, replayed.Version)
	// Run again as a new operation, the projection ID and payload digest
	// identify it.
	again, err := p.EnqueueOutboxProjectionRecord(
		r.op(suiteFence, "EnqueueOutboxProjectionRecord", projection(`{"n":1}`), suiteFence), projection(`{"n":1}`), suiteFence)
	require.NoError(t, err)
	require.EqualValues(t, 1, again.Version)
	_, err = p.EnqueueOutboxProjectionRecord(
		r.op(suiteFence, "EnqueueOutboxProjectionRecord", projection(`{"n":2}`), suiteFence), projection(`{"n":2}`), suiteFence)
	require.ErrorIs(t, err, storagev1.ErrConflict, "the projection ID with another payload")
	requireBacklog(t, p, 1)

	// Step 2: claim. A lost claim response is retried with the same
	// operation and returns the same claim without claiming again.
	claim := storagev1.ClaimOutboxProjectionsRequest{
		Fence: suiteFence, WorkerID: "worker-a", Limit: 10, LeaseDuration: time.Minute, Now: controlBaseTime,
	}
	claimCtx := r.op(suiteFence, "ClaimOutboxProjectionRecords", claim)
	claimed, err := p.ClaimOutboxProjectionRecords(claimCtx, claim)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.EqualValues(t, 1, claimed[0].Attempts)
	require.EqualValues(t, 2, claimed[0].Version)
	reclaimed, err := p.ClaimOutboxProjectionRecords(claimCtx, claim)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	require.EqualValues(t, 1, reclaimed[0].Attempts, "a replayed claim does not claim again")
	require.EqualValues(t, 2, reclaimed[0].Version)
	// A claim is not recognized by a natural key: run again as a new
	// operation before the lease expires, it finds nothing due, so a claim
	// whose outcome stays unknown only delays the record until its lease
	// expires.
	early := claim
	early.Now = controlBaseTime.Add(30 * time.Second)
	none, err := p.ClaimOutboxProjectionRecords(r.op(suiteFence, "ClaimOutboxProjectionRecords", early), early)
	require.NoError(t, err)
	require.Empty(t, none)

	// Step 3 applies the effect in Kubernetes. The controller is replaced
	// before step 4, and the replaced controller's completion arrives late.
	complete := storagev1.CompleteOutboxProjectionRequest{
		ID: id, Fence: suiteFence, ExpectedVersion: 2, LeaseOwner: "worker-a",
		OperationID: "deliver-a", OperationDigest: controlDigest("deliver-a"),
		NewState: storagev1.OutboxProjectionDelivered, DeliveryDigest: controlDigest("delivery"),
		UpdatedAt: controlBaseTime.Add(time.Second),
	}
	delayed := r.op(suiteFence, "CompleteOutboxProjectionRecord", complete)
	r.takeover()
	_, err = p.CompleteOutboxProjectionRecord(delayed, complete)
	requireFenced(t, err)
	stored, err := p.GetOutboxProjection(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storagev1.OutboxProjectionDelivering, stored.State, "a delayed completion changed the projection")
	require.EqualValues(t, 2, stored.Version)

	// The successor claims once the lease expired, applies the effect again
	// (idempotent in Kubernetes) and completes.
	successorClaim := storagev1.ClaimOutboxProjectionsRequest{
		Fence: nextFence, WorkerID: "worker-b", Limit: 10, LeaseDuration: time.Minute, Now: controlBaseTime.Add(2 * time.Minute),
	}
	successor, err := p.ClaimOutboxProjectionRecords(r.op(nextFence, "ClaimOutboxProjectionRecords", successorClaim), successorClaim)
	require.NoError(t, err)
	require.Len(t, successor, 1)
	require.Equal(t, "worker-b", successor[0].LeaseOwner)
	require.EqualValues(t, 2, successor[0].Attempts)
	done := storagev1.CompleteOutboxProjectionRequest{
		ID: id, Fence: nextFence, ExpectedVersion: successor[0].Version, LeaseOwner: "worker-b",
		OperationID: "deliver-b", OperationDigest: controlDigest("deliver-b"),
		NewState: storagev1.OutboxProjectionDelivered, DeliveryDigest: controlDigest("delivery"),
		UpdatedAt: controlBaseTime.Add(3 * time.Minute),
	}
	doneCtx := r.op(nextFence, "CompleteOutboxProjectionRecord", done)
	completed, err := p.CompleteOutboxProjectionRecord(doneCtx, done)
	require.NoError(t, err)
	require.Equal(t, storagev1.OutboxProjectionDelivered, completed.State)
	replayedDone, err := p.CompleteOutboxProjectionRecord(doneCtx, done)
	require.NoError(t, err)
	require.Equal(t, completed.Version, replayedDone.Version, "a replayed completion changed the projection")
	// Run again as a new operation, the completion's operation ID and digest
	// identify it.
	rerun, err := p.CompleteOutboxProjectionRecord(r.op(nextFence, "CompleteOutboxProjectionRecord", done), done)
	require.NoError(t, err)
	require.Equal(t, completed.Version, rerun.Version)
	requireBacklog(t, p, 0)

	// The replaced controller stays refused after the successor completed.
	_, err = p.CompleteOutboxProjectionRecord(r.op(suiteFence, "CompleteOutboxProjectionRecord", complete), complete)
	requireFenced(t, err)
	final, err := p.GetOutboxProjection(ctx, id)
	require.NoError(t, err)
	require.Equal(t, completed.Version, final.Version)
}

func runSessionCleanupProtocol(t *testing.T, newProvider StateFactory) {
	r := newProtocolRun(t, newProvider)
	p, ctx := r.provider, t.Context()
	const namespace, name, uid = "tenant", "agent", "p4-session-uid"
	createControlSession(t, p, namespace, name, "task")
	require.NoError(t, p.BindSessionCleanupIdentity(ctx, namespace, name, uid))
	intent := storagev1.SessionCleanupIntent{
		Namespace: namespace, SessionName: name, SessionUID: uid, OperationID: "delete-agent",
		OperationDigest: controlDigest("delete-agent"), PreparedAt: controlBaseTime,
	}

	// Step 1 reads the intent and completion; neither exists. Step 2
	// prepares the intent, and a lost response is retried.
	prepare := r.op(suiteFence, "PrepareSessionCleanup", intent)
	prepared, err := p.PrepareSessionCleanup(prepare, intent)
	require.NoError(t, err)
	require.Equal(t, intent.OperationID, prepared.OperationID)
	replayed, err := p.PrepareSessionCleanup(prepare, intent)
	require.NoError(t, err)
	require.Equal(t, prepared.OperationDigest, replayed.OperationDigest)
	// While it is pending, the same intent as a new operation returns it, and
	// a different operation is refused.
	_, err = p.PrepareSessionCleanup(r.op(suiteFence, "PrepareSessionCleanup", intent), intent)
	require.NoError(t, err)
	other := intent
	other.OperationID, other.OperationDigest = "delete-again", controlDigest("delete-again")
	_, err = p.PrepareSessionCleanup(r.op(suiteFence, "PrepareSessionCleanup", other), other)
	require.ErrorIs(t, err, storagev1.ErrConflict)

	// The intent blocks new work on the Session and fences its UID.
	messages := []storagev1.SessionMessage{{ID: "after-intent", Role: "user", Content: "late"}}
	err = p.AppendMessages(r.op(suiteFence, "AppendMessages", namespace, name, messages), namespace, name, messages)
	require.ErrorIs(t, err, storagev1.ErrConflict, "an append while cleanup is pending")
	err = p.AcquireLock(r.op(suiteFence, "AcquireLock", namespace, name, "task-a", "task-uid-a"), namespace, name, "task-a", "task-uid-a")
	require.ErrorIs(t, err, storagev1.ErrConflict, "a Task lock while cleanup is pending")
	fenced, err := p.HasSessionCleanupFenceForUID(ctx, uid)
	require.NoError(t, err)
	require.True(t, fenced)

	// Steps 3 and 4 retire the runtime and delete the Kubernetes records. The
	// controller is replaced before step 5 completes: this is the window
	// between the Kubernetes deletes and the transcript delete.
	complete := storagev1.CompleteSessionCleanupRequest{
		Namespace: namespace, SessionName: name, OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
	}
	delayed := r.op(suiteFence, "CompleteSessionCleanup", complete)
	r.takeover()
	requireFenced(t, p.CompleteSessionCleanup(delayed, complete))
	_, err = p.GetSession(ctx, namespace, name)
	require.NoError(t, err, "a delayed completion deleted the transcript")
	pending, err := p.GetSessionCleanupIntent(ctx, namespace, name)
	require.NoError(t, err)
	require.Equal(t, intent.OperationID, pending.OperationID)

	// The successor resumes the intent with the operation ID it holds. Its
	// lost response is retried, and running it again as a new operation is
	// recognized by the cleanup operation.
	resumed := r.op(nextFence, "CompleteSessionCleanup", complete)
	require.NoError(t, p.CompleteSessionCleanup(resumed, complete))
	require.NoError(t, p.CompleteSessionCleanup(resumed, complete))
	require.NoError(t, p.CompleteSessionCleanup(r.op(nextFence, "CompleteSessionCleanup", complete), complete))
	_, err = p.GetSession(ctx, namespace, name)
	require.ErrorIs(t, err, storagev1.ErrNotFound)
	_, err = p.GetSessionCleanupIntent(ctx, namespace, name)
	require.ErrorIs(t, err, storagev1.ErrNotFound)
	completion, err := p.GetSessionCleanupCompletion(ctx, namespace, name)
	require.NoError(t, err)
	require.Equal(t, intent.OperationID, completion.OperationID)
	require.Equal(t, intent.OperationDigest, completion.OperationDigest)
	require.Equal(t, uid, completion.SessionUID)

	// A step 2 the replaced controller issued arrives after the successor
	// completed and is refused.
	requireFencedPrepare := func(request storagev1.SessionCleanupIntent) {
		t.Helper()
		_, err := p.PrepareSessionCleanup(r.op(suiteFence, "PrepareSessionCleanup", request), request)
		requireFenced(t, err)
		_, err = p.GetSessionCleanupIntent(ctx, namespace, name)
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a delayed preparation recorded an intent")
	}
	requireFencedPrepare(intent)
	requireFencedPrepare(other)

	// The completion keeps fencing the UID and reserves the name of a
	// Session with a Kubernetes identity.
	fenced, err = p.HasSessionCleanupFenceForUID(ctx, uid)
	require.NoError(t, err)
	require.True(t, fenced)
	err = p.CreateSession(ctx, &storagev1.SessionRecord{
		Namespace: namespace, Name: name, SessionType: "task", CreatedAt: controlBaseTime, UpdatedAt: controlBaseTime,
	})
	require.ErrorIs(t, err, storagev1.ErrConflict)
}

// runTranscriptCleanupProtocol checks the documented exception to name
// reservation: a transcript-only Session, which has no Kubernetes identity,
// may be created again once its cleanup completed.
func runTranscriptCleanupProtocol(t *testing.T, newProvider StateFactory) {
	r := newProtocolRun(t, newProvider)
	p, ctx := r.provider, t.Context()
	createControlSession(t, p, "tenant", "chat", "chat")
	intent := storagev1.SessionCleanupIntent{
		Namespace: "tenant", SessionName: "chat", OperationID: "delete-chat",
		OperationDigest: controlDigest("delete-chat"), PreparedAt: controlBaseTime,
	}
	_, err := p.PrepareSessionCleanup(r.op(suiteFence, "PrepareSessionCleanup", intent), intent)
	require.NoError(t, err)
	_, err = p.AcquireChatTurn(r.op(suiteFence, "AcquireChatTurn", chatSession(), "turn-1", writeCaseFuture),
		chatSession(), "turn-1", writeCaseFuture)
	require.ErrorIs(t, err, storagev1.ErrConflict, "a chat turn while cleanup is pending")
	complete := storagev1.CompleteSessionCleanupRequest{
		Namespace: "tenant", SessionName: "chat", OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
	}
	require.NoError(t, p.CompleteSessionCleanup(r.op(suiteFence, "CompleteSessionCleanup", complete), complete))
	completion, err := p.GetSessionCleanupCompletion(ctx, "tenant", "chat")
	require.NoError(t, err)
	require.Empty(t, completion.SessionUID)

	session := &storagev1.SessionRecord{
		Namespace: "tenant", Name: "chat", SessionType: "chat", CreatedAt: controlBaseTime, UpdatedAt: controlBaseTime,
	}
	require.NoError(t, p.CreateSession(r.op(suiteFence, "CreateSession", session), session))
	_, err = p.GetSessionCleanupCompletion(ctx, "tenant", "chat")
	require.ErrorIs(t, err, storagev1.ErrNotFound, "reusing a transcript-only name removes its completion")
}

func runPromptResultProtocol(t *testing.T, newProvider StateFactory) {
	r := newProtocolRun(t, newProvider)
	p, ctx := r.provider, t.Context()
	receipt := storagev1.PromptResultReceipt{
		AttemptID: "p5-attempt", Namespace: "tenant", TaskName: "p5-task",
		OperationID: "settle-1", OperationDigest: controlDigest("settle-1"), Data: []byte("the result"),
	}

	// Step 1: the receipt. A lost response is retried, and the same receipt
	// as a new operation is recognized by its attempt ID.
	save := r.op(suiteFence, "SavePromptResultReceipt", receipt)
	require.NoError(t, p.SavePromptResultReceipt(save, receipt))
	require.NoError(t, p.SavePromptResultReceipt(save, receipt))
	require.NoError(t, p.SavePromptResultReceipt(r.op(suiteFence, "SavePromptResultReceipt", receipt), receipt))
	changed := receipt
	changed.Data = []byte("another result")
	err := p.SavePromptResultReceipt(r.op(suiteFence, "SavePromptResultReceipt", changed), changed)
	require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch, "a receipt is immutable per attempt")

	// Step 2 moves the PromptAttempt to Settling in Kubernetes. The
	// controller is replaced before its step 3 result write arrives.
	delayed := r.op(suiteFence, "SaveResult", receipt.Namespace, receipt.TaskName, receipt.Data)
	r.takeover()
	requireFenced(t, p.SaveResult(delayed, receipt.Namespace, receipt.TaskName, receipt.Data))
	_, err = p.GetResult(ctx, receipt.Namespace, receipt.TaskName)
	require.ErrorIs(t, err, storagev1.ErrNotFound, "a delayed result write committed")

	// The successor repairs the interrupted result write from the receipt.
	stored, err := p.GetPromptResultReceipt(ctx, receipt.AttemptID)
	require.NoError(t, err)
	require.Equal(t, receipt.Data, stored.Data)
	repair := r.op(nextFence, "SaveResult", stored.Namespace, stored.TaskName, stored.Data)
	require.NoError(t, p.SaveResult(repair, stored.Namespace, stored.TaskName, stored.Data))
	require.NoError(t, p.SaveResult(repair, stored.Namespace, stored.TaskName, stored.Data))
	result, err := p.GetResult(ctx, receipt.Namespace, receipt.TaskName)
	require.NoError(t, err)
	require.Equal(t, receipt.Data, result)

	// A receipt the replaced controller issued for another attempt is refused.
	late := receipt
	late.AttemptID, late.OperationID = "p5-attempt-2", "settle-2"
	requireFenced(t, p.SavePromptResultReceipt(r.op(suiteFence, "SavePromptResultReceipt", late), late))
	_, err = p.GetPromptResultReceipt(ctx, late.AttemptID)
	require.ErrorIs(t, err, storagev1.ErrNotFound)
}

// compactableGatewayEvent links one Gateway event to a Task and expires it
// before cutoff, so the next maintenance after cutoff compacts it.
func compactableGatewayEvent(t *testing.T, p storagev1.StateProvider, suffix string, created, cutoff time.Time) storagev1.GatewayEvent {
	t.Helper()
	event := linkGatewayTask(t, p, gatewayEvent(created, eventsTenant, suffix), "task-uid-"+suffix)
	delivered := gatewayDelivery(event, "gdl-"+suffix, "error", cutoff)
	delivered.State, delivered.ProviderMessageID, delivered.DeliveredAt = storagev1.GatewayDeliveryDelivered, "provider", &cutoff
	_, _, err := p.ExpireGatewayEventWithDelivery(t.Context(), storagev1.GatewayExpiryProjection{
		EventID: event.ID, Reason: "expired", CompletedAt: cutoff, Delivery: delivered,
	})
	require.NoError(t, err)
	return event
}

func runGatewayTaskCleanupProtocol(t *testing.T, newProvider StateFactory) {
	r := newProtocolRun(t, newProvider)
	p, ctx := r.provider, t.Context()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Hour)
	event := compactableGatewayEvent(t, p, "p7", now.Add(-2*time.Hour), cutoff)

	// Step 1: compaction writes the receipt. A lost response is retried and
	// returns the original counts; compaction run again as a new operation
	// finds nothing left to compact.
	compactedAt, compactCutoff := now.Add(time.Second), cutoff.Add(time.Second)
	maintain := r.op(suiteFence, "MaintainGatewayRecords", event.Namespace, compactedAt, compactCutoff)
	result, err := p.MaintainGatewayRecords(maintain, event.Namespace, compactedAt, compactCutoff)
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedEvents)
	replayed, err := p.MaintainGatewayRecords(maintain, event.Namespace, compactedAt, compactCutoff)
	require.NoError(t, err)
	require.Equal(t, result, replayed)
	again, err := p.MaintainGatewayRecords(
		r.op(suiteFence, "MaintainGatewayRecords", event.Namespace, compactedAt, compactCutoff),
		event.Namespace, compactedAt, compactCutoff)
	require.NoError(t, err)
	require.Zero(t, again.DeletedEvents)
	receipt, err := p.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.NoError(t, err)
	require.NoError(t, receipt.Validate(event.Namespace, event.TaskName, event.TaskUID))

	// Step 2 proves in Kubernetes that the exact Task is gone. The
	// controller is replaced before step 3, and the replaced controller's
	// receipt deletion arrives late.
	delayed := r.op(suiteFence, "DeleteGatewayTaskCleanupReceipt", event.Namespace, event.TaskName, event.TaskUID)
	r.takeover()
	requireFenced(t, p.DeleteGatewayTaskCleanupReceipt(delayed, event.Namespace, event.TaskName, event.TaskUID))
	_, err = p.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.NoError(t, err, "a delayed deletion removed the receipt")

	// Step 3: the successor deletes the receipt. A lost response is retried,
	// and the deletion run again as a new operation changes nothing.
	remove := r.op(nextFence, "DeleteGatewayTaskCleanupReceipt", event.Namespace, event.TaskName, event.TaskUID)
	require.NoError(t, p.DeleteGatewayTaskCleanupReceipt(remove, event.Namespace, event.TaskName, event.TaskUID))
	require.NoError(t, p.DeleteGatewayTaskCleanupReceipt(remove, event.Namespace, event.TaskName, event.TaskUID))
	require.NoError(t, p.DeleteGatewayTaskCleanupReceipt(
		r.op(nextFence, "DeleteGatewayTaskCleanupReceipt", event.Namespace, event.TaskName, event.TaskUID),
		event.Namespace, event.TaskName, event.TaskUID))
	_, err = p.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, storagev1.ErrNotFound)
}

// runDelayedGatewayCompaction checks that a compaction the replaced
// controller issued is refused after the takeover and leaves the event, so
// the successor's own compaction writes the receipt.
func runDelayedGatewayCompaction(t *testing.T, newProvider StateFactory) {
	r := newProtocolRun(t, newProvider)
	p, ctx := r.provider, t.Context()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Hour)
	event := compactableGatewayEvent(t, p, "p7-delayed", now.Add(-2*time.Hour), cutoff)

	compactedAt, compactCutoff := now.Add(time.Second), cutoff.Add(time.Second)
	delayed := r.op(suiteFence, "MaintainGatewayRecords", event.Namespace, compactedAt, compactCutoff)
	r.takeover()
	_, err := p.MaintainGatewayRecords(delayed, event.Namespace, compactedAt, compactCutoff)
	requireFenced(t, err)
	_, err = p.GetGatewayEvent(ctx, event.Namespace, event.ID)
	require.NoError(t, err, "a delayed compaction removed the event")
	_, err = p.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, storagev1.ErrNotFound, "a delayed compaction wrote a receipt")

	result, err := p.MaintainGatewayRecords(
		r.op(nextFence, "MaintainGatewayRecords", event.Namespace, compactedAt, compactCutoff),
		event.Namespace, compactedAt, compactCutoff)
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedEvents)
	_, err = p.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.NoError(t, err)
}
