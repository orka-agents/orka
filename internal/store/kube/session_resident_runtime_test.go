package kube

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	controlstore "github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// residentRuntimeHarness drives the Kubernetes-authoritative composite store
// through whole Session turns for the ADR 0059 handoff record.
type residentRuntimeHarness struct {
	t       *testing.T
	ctx     context.Context
	store   *Store
	raw     client.Client
	sqlite  *sqlitestore.Store
	fence   controlstore.ControllerEpochFence
	session controlstore.SessionControl
}

type residentRuntimeTurn struct {
	control *controlstore.SessionControl
	attempt *controlstore.PromptAttempt
	turn    *controlstore.SessionTurn
	key     controlstore.SessionTurnKey
}

func newResidentRuntimeHarness(t *testing.T) *residentRuntimeHarness {
	t.Helper()
	ctx := context.Background()
	_, rawClient, fence := newTestStoreWithEpoch(t)
	db, err := sqlitestore.NewDB(filepath.Join(t.TempDir(), "resident.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqliteStore := sqlitestore.NewStore(db, "")
	if err := sqliteStore.CreateSession(ctx, &controlstore.SessionRecord{Namespace: "tenant-a", Name: "resident", SessionType: "task"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	kubeStore, err := NewComposite(rawClient, testControlNamespace, storageclienttest.Client(t, sqliteStore))
	if err != nil {
		t.Fatalf("NewComposite: %v", err)
	}
	syncTestEpochMirror(t, kubeStore, sqliteStore)
	control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "resident", SessionUID: "resident-uid", RequestDigest: testDigest("resident"),
	}, fence)
	if err != nil {
		t.Fatalf("CreateSessionControl: %v", err)
	}
	return &residentRuntimeHarness{t: t, ctx: ctx, store: kubeStore, raw: rawClient, sqlite: sqliteStore, fence: fence, session: *control}
}

func (h *residentRuntimeHarness) current() *controlstore.SessionControl {
	h.t.Helper()
	control, err := h.store.GetSessionControl(h.ctx, h.session.Namespace, h.session.SessionName)
	if err != nil {
		h.t.Fatalf("GetSessionControl: %v", err)
	}
	return control
}

func (h *residentRuntimeHarness) acquireRequest(control *controlstore.SessionControl, taskUID string, claim *controlstore.ClaimSessionResidentRuntime, fence controlstore.ControllerEpochFence, acquiredAt time.Time) controlstore.AcquireSessionMutationLeaseRequest {
	h.t.Helper()
	promptID := "prompt-" + taskUID
	digest, err := controlstore.SessionMutationLeaseRequestDigest(control.SessionUID, control.LeaseGeneration+1, taskUID, 1, promptID, testDigest("prompt-"+taskUID))
	if err != nil {
		h.t.Fatal(err)
	}
	expires := acquiredAt.Add(time.Hour)
	return controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		TaskUID: taskUID, Attempt: 1, PromptID: promptID, RequestDigest: digest,
		AcquiredAt: acquiredAt, ExpiresAt: &expires, Lineage: testSessionLineageClaim(control),
		ClaimResidentRuntime: claim,
	}
}

// openTurn acquires the next lease, creates a succeeded PromptAttempt, and
// opens its SessionTurn.
func (h *residentRuntimeHarness) openTurn(taskUID string, claim *controlstore.ClaimSessionResidentRuntime) residentRuntimeTurn {
	h.t.Helper()
	control, err := h.store.AcquireSessionMutationLease(h.ctx, h.acquireRequest(h.current(), taskUID, claim, h.fence, testNow))
	if err != nil {
		h.t.Fatalf("AcquireSessionMutationLease(%s): %v", taskUID, err)
	}
	return h.openTurnUnderLease(control, taskUID)
}

func (h *residentRuntimeHarness) openTurnUnderLease(control *controlstore.SessionControl, taskUID string) residentRuntimeTurn {
	h.t.Helper()
	key := controlstore.PromptAttemptKey{Namespace: control.Namespace, TaskUID: taskUID, Attempt: 1, PromptID: "prompt-" + taskUID}
	ensureActiveAgentTask(h.t, h.ctx, h.raw, key.Namespace, taskUID, taskUID)
	attempt, err := h.store.CreatePromptAttempt(h.ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: key, RequestDigest: testDigest("prompt-" + taskUID)}), h.fence)
	if err != nil {
		h.t.Fatalf("CreatePromptAttempt: %v", err)
	}
	attempt = advancePromptAttemptToSuccess(h.t, h.ctx, h.store, h.fence, attempt, control)
	turnKey := controlstore.SessionTurnKey{SessionUID: control.SessionUID, LeaseGeneration: control.LeaseGeneration, TaskUID: taskUID, Attempt: 1, PromptID: key.PromptID}
	turn, err := h.store.CreateSessionTurn(h.ctx, controlstore.CreateSessionTurnRequest{
		Turn:  controlstore.SessionTurn{Key: turnKey, PromptAttemptID: attempt.ID, RequestDigest: testDigest("turn-" + taskUID), UserPrompt: "continue"},
		Fence: h.fence, ExpectedSessionVersion: control.Version,
	})
	if err != nil {
		h.t.Fatalf("CreateSessionTurn: %v", err)
	}
	return residentRuntimeTurn{control: control, attempt: attempt, turn: turn, key: turnKey}
}

func (h *residentRuntimeHarness) record(turn residentRuntimeTurn, keptAt time.Time) *controlstore.SessionResidentRuntime {
	h.t.Helper()
	record, err := controlstore.NewSessionResidentRuntime(controlstore.SessionResidentRuntime{
		SourceTaskUID: turn.key.TaskUID, SourceAttempt: turn.key.Attempt, SourcePromptAttemptID: turn.attempt.ID,
		SourceTurnID: turn.turn.ID, SourceLeaseGeneration: turn.key.LeaseGeneration,
		ControllerEpochName: h.fence.Name, ControllerEpoch: h.fence.Epoch,
		WorkspaceName: "ws", WorkspaceUID: "ws-uid", WorkspaceAttachmentEpoch: 1,
		RuntimePoolName: "acp-ws-pool", RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1,
		RuntimeInstanceID: "instance-1", SupervisorBootID: "boot-1", ServedRecordDigest: testDigest("served"),
		RuntimeSessionUID: turn.key.SessionUID, RuntimeSessionGeneration: 1,
		ProfileDigest: testDigest("profile"), MCPDigest: testDigest("mcp"), WorkspaceDigest: testDigest("workspace"),
		LineageConfigDigest: testDigest("lineage"), ReuseDigest: testDigest("reuse"),
		TranscriptHeadDigest: testDigest("head"), PromptDelivered: true,
		KeptAt: keptAt, ExpiresAt: keptAt.Add(5 * time.Minute),
	})
	if err != nil {
		h.t.Fatalf("NewSessionResidentRuntime: %v", err)
	}
	return &record
}

func (h *residentRuntimeHarness) finalizeRequest(turn residentRuntimeTurn, record *controlstore.SessionResidentRuntime) controlstore.FinalizeSessionTurnRequest {
	payload := []byte(`{"phase":"Succeeded"}`)
	return controlstore.FinalizeSessionTurnRequest{
		Key: turn.key, Fence: h.fence, ExpectedSessionVersion: turn.control.Version, ExpectedTurnVersion: turn.turn.Version,
		FinalizationDigest: testDigest("finalize-" + turn.key.TaskUID), TerminalKind: controlstore.SessionTurnAssistantResult,
		TerminalContent: "done " + turn.key.TaskUID,
		Projection: controlstore.OutboxProjection{
			ID: "projection-" + turn.key.TaskUID, AggregateKind: sessionTurnAggregateKind, AggregateID: turn.turn.ID,
			ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: testBytesDigest(payload),
		},
		FinalizedAt:     testNow.Add(time.Hour),
		ResidentRuntime: record,
	}
}

// keep runs turn N ("task-n") to a finalization that keeps its runtime.
func (h *residentRuntimeHarness) keep() *controlstore.SessionResidentRuntime {
	h.t.Helper()
	turn := h.openTurn("task-n", nil)
	record := h.record(turn, testNow)
	if _, err := h.store.FinalizeSessionTurn(h.ctx, h.finalizeRequest(turn, record)); err != nil {
		h.t.Fatalf("FinalizeSessionTurn with resident runtime: %v", err)
	}
	kept := h.current().ResidentRuntime
	if kept == nil || kept.HandoffID != record.HandoffID || kept.State != controlstore.SessionResidentRuntimeResident {
		h.t.Fatalf("kept record = %#v, want Resident %s", kept, record.HandoffID)
	}
	return kept
}

func TestResidentRuntimeIsKeptClaimedOnceAndConsumedByTheNextFinalization(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	kept := h.keep()

	claim := &controlstore.ClaimSessionResidentRuntime{HandoffID: kept.HandoffID}
	next := h.openTurn("task-n1", claim)
	if !controlstore.ResidentRuntimeClaimedBy(next.control.ResidentRuntime, next.key) {
		t.Fatalf("lease acquisition did not claim the record: %#v", next.control.ResidentRuntime)
	}
	// The idempotent re-entry of the same lease returns the same claim.
	retry, err := h.store.AcquireSessionMutationLease(h.ctx, controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: next.control.Namespace, SessionName: next.control.SessionName, SessionUID: next.control.SessionUID,
		Fence: h.fence, ExpectedVersion: next.control.Version, ExpectedLeaseGeneration: next.control.LeaseGeneration,
		TaskUID: "task-n1", Attempt: 1, PromptID: "prompt-task-n1", RequestDigest: next.control.Lease.RequestDigest,
		AcquiredAt: testNow, Lineage: testSessionLineageClaim(next.control), ClaimResidentRuntime: claim,
	})
	if err != nil || !controlstore.ResidentRuntimeClaimedBy(retry.ResidentRuntime, next.key) {
		t.Fatalf("idempotent re-acquire = %#v, %v", retry, err)
	}
	if _, err := h.store.FinalizeSessionTurn(h.ctx, h.finalizeRequest(next, nil)); err != nil {
		t.Fatalf("FinalizeSessionTurn: %v", err)
	}
	if record := h.current().ResidentRuntime; record != nil {
		t.Fatalf("finalization without a record must clear the consumed claim, got %#v", record)
	}
}

func TestResidentRuntimeUnclaimedAcquisitionsRetireTheRecord(t *testing.T) {
	cases := map[string]func(h *residentRuntimeHarness, kept *controlstore.SessionResidentRuntime) controlstore.AcquireSessionMutationLeaseRequest{
		"ordinary acquisition": func(h *residentRuntimeHarness, _ *controlstore.SessionResidentRuntime) controlstore.AcquireSessionMutationLeaseRequest {
			return h.acquireRequest(h.current(), "task-other", nil, h.fence, testNow)
		},
		"other handoff": func(h *residentRuntimeHarness, _ *controlstore.SessionResidentRuntime) controlstore.AcquireSessionMutationLeaseRequest {
			return h.acquireRequest(h.current(), "task-other", &controlstore.ClaimSessionResidentRuntime{HandoffID: testDigest("other")}, h.fence, testNow)
		},
		"expired record": func(h *residentRuntimeHarness, kept *controlstore.SessionResidentRuntime) controlstore.AcquireSessionMutationLeaseRequest {
			return h.acquireRequest(h.current(), "task-other", &controlstore.ClaimSessionResidentRuntime{HandoffID: kept.HandoffID}, h.fence, kept.ExpiresAt)
		},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			h := newResidentRuntimeHarness(t)
			kept := h.keep()
			control, err := h.store.AcquireSessionMutationLease(h.ctx, request(h, kept))
			if err != nil {
				t.Fatalf("AcquireSessionMutationLease: %v", err)
			}
			if record := control.ResidentRuntime; record == nil || record.State != controlstore.SessionResidentRuntimeRetiring {
				t.Fatalf("record after an unclaimed acquisition = %#v, want Retiring", record)
			}
		})
	}
}

func TestResidentRuntimeClaimIsRefusedAcrossAControllerEpochTakeover(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	kept := h.keep()
	// ADR 0058 takeover: a successor advances the epoch between N and N+1.
	epoch, err := h.store.GetControllerEpoch(h.ctx, h.fence.Name)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := h.store.CompareAndSwapControllerEpoch(h.ctx, controlstore.ControllerEpochCAS{
		ExpectedVersion: epoch.Version, ExpectedEpoch: epoch.Epoch, NewEpoch: epoch.Epoch + 1, HolderID: "controller-successor",
		RequestDigest: testDigest("takeover"), UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("advance controller epoch: %v", err)
	}
	syncTestEpochMirror(t, h.store, h.sqlite)
	successor := controlstore.ControllerEpochFence{Name: advanced.Name, Epoch: advanced.Epoch, HolderID: advanced.HolderID}
	if _, err := h.store.AcquireSessionMutationLease(h.ctx, h.acquireRequest(h.current(), "task-n1", &controlstore.ClaimSessionResidentRuntime{HandoffID: kept.HandoffID}, h.fence, testNow)); err == nil {
		t.Fatal("the predecessor's fence must no longer acquire after the takeover")
	}
	control, err := h.store.AcquireSessionMutationLease(h.ctx, h.acquireRequest(h.current(), "task-n1", &controlstore.ClaimSessionResidentRuntime{HandoffID: kept.HandoffID}, successor, testNow))
	if err != nil {
		t.Fatalf("successor acquisition: %v", err)
	}
	if record := control.ResidentRuntime; record == nil || record.State != controlstore.SessionResidentRuntimeRetiring || record.Claim != nil {
		t.Fatalf("record after takeover = %#v, want unclaimed Retiring", record)
	}
}

func TestResidentRuntimeClaimIsExactlyOnceUnderConcurrentAcquisitions(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	kept := h.keep()
	base := h.current()
	const contenders = 8
	var wg sync.WaitGroup
	results := make([]*controlstore.SessionControl, contenders)
	errs := make([]error, contenders)
	for i := range contenders {
		request := h.acquireRequest(base, fmt.Sprintf("task-rival-%d", i), &controlstore.ClaimSessionResidentRuntime{HandoffID: kept.HandoffID}, h.fence, testNow)
		wg.Go(func() {
			results[i], errs[i] = h.store.AcquireSessionMutationLease(h.ctx, request)
		})
	}
	wg.Wait()
	winners := 0
	var winner *controlstore.SessionControl
	for i := range contenders {
		if errs[i] == nil {
			winners++
			winner = results[i]
		}
	}
	if winners != 1 {
		t.Fatalf("%d concurrent acquisitions succeeded, want exactly one: %v", winners, errs)
	}
	final := h.current()
	if final.ResidentRuntime == nil || final.ResidentRuntime.State != controlstore.SessionResidentRuntimeClaimed ||
		final.ResidentRuntime.Claim == nil || final.ResidentRuntime.Claim.TaskUID != winner.Lease.TaskUID ||
		final.ResidentRuntime.Claim.LeaseGeneration != final.LeaseGeneration {
		t.Fatalf("record after the race = %#v, want claimed by the winner %s", final.ResidentRuntime, winner.Lease.TaskUID)
	}
}

func TestResidentRuntimeClaimCommitsOnceWhenTheLeaseStatusWriteCrashes(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	kept := h.keep()
	request := h.acquireRequest(h.current(), "task-n1", &controlstore.ClaimSessionResidentRuntime{HandoffID: kept.HandoffID}, h.fence, testNow)
	withWatch := h.raw.(client.WithWatch)
	failOnce := true
	h.store.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*corev1alpha1.RuntimeSessionControl); ok && failOnce {
				failOnce = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "runtimesessioncontrols"}, obj.GetName(), errors.New("simulated crash after the Lease CAS"))
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	if _, err := h.store.AcquireSessionMutationLease(h.ctx, request); err == nil {
		t.Fatal("the first acquisition must fail at the simulated status crash")
	}
	if record := h.current().ResidentRuntime; record == nil || record.State != controlstore.SessionResidentRuntimeResident {
		t.Fatalf("a crash before the status CAS must leave the record untouched, got %#v", record)
	}
	h.store.client = h.raw
	control, err := h.store.AcquireSessionMutationLease(h.ctx, request)
	if err != nil {
		t.Fatalf("retried acquisition: %v", err)
	}
	key := controlstore.SessionTurnKey{SessionUID: control.SessionUID, LeaseGeneration: control.LeaseGeneration, TaskUID: "task-n1", Attempt: 1, PromptID: "prompt-task-n1"}
	if !controlstore.ResidentRuntimeClaimedBy(control.ResidentRuntime, key) {
		t.Fatalf("retried acquisition did not claim exactly once: %#v", control.ResidentRuntime)
	}
}

func TestResidentRuntimeRecoveredFinalizationNeverWritesARecord(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	turn := h.openTurn("task-n", nil)
	record := h.record(turn, testNow)
	withWatch := h.raw.(client.WithWatch)
	failOnce := true
	h.store.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*corev1alpha1.RuntimeSessionControl); ok && failOnce {
				failOnce = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "runtimesessioncontrols"}, obj.GetName(), errors.New("simulated crash at the record write"))
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	if _, err := h.store.FinalizeSessionTurn(h.ctx, h.finalizeRequest(turn, record)); err == nil {
		t.Fatal("the live finalization must fail at the simulated crash")
	}
	h.store.client = h.raw
	committed, err := h.sqlite.GetSessionTurn(h.ctx, turn.turn.ID)
	if err != nil || committed.State != controlstore.SessionTurnFinalized {
		t.Fatalf("SQLite turn after the crash = %#v, %v", committed, err)
	}
	if _, err := h.store.ResumeSessionTurnFinalization(h.ctx, controlstore.ResumeSessionTurnFinalizationRequest{
		Key: committed.Key, PromptAttemptID: committed.PromptAttemptID, FinalizationDigest: committed.FinalizationDigest, Fence: h.fence,
	}); err != nil {
		t.Fatalf("ResumeSessionTurnFinalization: %v", err)
	}
	control := h.current()
	if control.Lease != nil || control.ResidentRuntime != nil {
		t.Fatalf("recovery released the lease = %v and must keep no record, got %#v", control.Lease == nil, control.ResidentRuntime)
	}
}

func TestResidentRuntimeLiveFinalizationRetryKeepsTheRecord(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	turn := h.openTurn("task-n", nil)
	record := h.record(turn, testNow)
	withWatch := h.raw.(client.WithWatch)
	failOnce := true
	h.store.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*corev1alpha1.RuntimeSessionControl); ok && failOnce {
				failOnce = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "runtimesessioncontrols"}, obj.GetName(), errors.New("simulated crash at the record write"))
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	request := h.finalizeRequest(turn, record)
	if _, err := h.store.FinalizeSessionTurn(h.ctx, request); err == nil {
		t.Fatal("the live finalization must fail at the simulated crash")
	}
	h.store.client = h.raw
	if _, err := h.store.FinalizeSessionTurn(h.ctx, request); err != nil {
		t.Fatalf("live finalization retry: %v", err)
	}
	if kept := h.current().ResidentRuntime; kept == nil || kept.HandoffID != record.HandoffID {
		t.Fatalf("live retry record = %#v", kept)
	}
}

func TestResidentRuntimePrePromptReleaseNeverRevivesAConsumedClaim(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	kept := h.keep()
	claim := &controlstore.ClaimSessionResidentRuntime{HandoffID: kept.HandoffID}
	control, err := h.store.AcquireSessionMutationLease(h.ctx, h.acquireRequest(h.current(), "task-n1", claim, h.fence, testNow))
	if err != nil {
		t.Fatal(err)
	}
	key := controlstore.SessionTurnKey{SessionUID: control.SessionUID, LeaseGeneration: control.LeaseGeneration, TaskUID: "task-n1", Attempt: 1, PromptID: "prompt-task-n1"}
	turnID, _ := key.CanonicalID()
	releaseDigest, err := controlstore.SessionLeaseReleaseOperationDigest(turnID, control.Lease.RequestDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ReleaseSessionMutationLease(h.ctx, controlstore.ReleaseSessionMutationLeaseRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID, Key: key,
		Fence: h.fence, ExpectedSessionVersion: control.Version, LeaseRequestDigest: control.Lease.RequestDigest,
		OperationID: prePromptLeaseReleasePrefix + turnID, OperationDigest: releaseDigest, ReleasedAt: testNow.Add(time.Minute),
	}); err != nil {
		t.Fatalf("ReleaseSessionMutationLease: %v", err)
	}
	again, err := h.store.AcquireSessionMutationLease(h.ctx, h.acquireRequest(h.current(), "task-n2", claim, h.fence, testNow.Add(2*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if again.ResidentRuntime == nil || again.ResidentRuntime.State != controlstore.SessionResidentRuntimeRetiring {
		t.Fatalf("a consumed claim must retire, never be claimed again: %#v", again.ResidentRuntime)
	}
}

func TestResidentRuntimeFinalizationRefusesARecordForAnotherTurn(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	turn := h.openTurn("task-n", nil)
	record := h.record(turn, testNow)
	foreign := *record
	foreign.SourceTaskUID = "task-other"
	foreignRecord, err := controlstore.NewSessionResidentRuntime(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.FinalizeSessionTurn(h.ctx, h.finalizeRequest(turn, &foreignRecord)); !errors.Is(err, controlstore.ErrValidation) {
		t.Fatalf("foreign record finalization = %v, want validation error", err)
	}
	marker := h.finalizeRequest(turn, record)
	marker.TerminalKind = controlstore.SessionTurnOutcomeMarker
	if _, err := h.store.FinalizeSessionTurn(h.ctx, marker); !errors.Is(err, controlstore.ErrValidation) {
		t.Fatalf("outcome-marker finalization with a record = %v, want validation error", err)
	}
	if open, err := h.sqlite.GetSessionTurn(h.ctx, turn.turn.ID); err != nil || open.State != controlstore.SessionTurnOpen {
		t.Fatalf("a refused record must not commit the turn: %#v, %v", open, err)
	}
}

func TestRetireSessionResidentRuntimeOnlyRetiresTheExactRecord(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	kept := h.keep()
	request := controlstore.RetireSessionResidentRuntimeRequest{
		Namespace: h.session.Namespace, SessionName: h.session.SessionName, SessionUID: h.session.SessionUID,
		HandoffID: testDigest("someone-else"), Fence: h.fence, RetiredAt: testNow.Add(time.Minute),
	}
	if control, err := h.store.RetireSessionResidentRuntime(h.ctx, request); err != nil || control.ResidentRuntime.State != controlstore.SessionResidentRuntimeResident {
		t.Fatalf("retiring another handoff = %#v, %v; want a no-op", control, err)
	}
	request.HandoffID = kept.HandoffID
	retired, err := h.store.RetireSessionResidentRuntime(h.ctx, request)
	if err != nil || retired.ResidentRuntime.State != controlstore.SessionResidentRuntimeRetiring {
		t.Fatalf("RetireSessionResidentRuntime = %#v, %v", retired, err)
	}
	again, err := h.store.RetireSessionResidentRuntime(h.ctx, request)
	if err != nil || again.Version != retired.Version {
		t.Fatalf("retirement must be idempotent: %#v, %v", again, err)
	}
	if !h.store.SupportsSessionResidentRuntime() {
		t.Fatal("the Kubernetes control store persists resident runtimes")
	}
}

func TestResidentRuntimeRecordCarriesNoCredentialMaterial(t *testing.T) {
	h := newResidentRuntimeHarness(t)
	h.keep()
	object := &corev1alpha1.RuntimeSessionControl{}
	if err := h.raw.Get(h.ctx, client.ObjectKey{Namespace: h.session.Namespace, Name: runtimeSessionObjectName(h.session.SessionName)}, object); err != nil {
		t.Fatal(err)
	}
	if object.Status.ResidentRuntime == nil {
		t.Fatal("record was not mirrored into RuntimeSessionControl status")
	}
	assertNoCredentialFields(t, object.Status.ResidentRuntime)
}

// assertNoCredentialFields fails when a record type grows a field that could
// carry a credential, a transaction token, or a token digest (ADR 0059).
func assertNoCredentialFields(t *testing.T, value any) {
	t.Helper()
	forbidden := []string{"token", "secret", "credential", "password", "bearer", "authorization", "capability"}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || typ.PkgPath() == "time" || strings.HasSuffix(typ.PkgPath(), "apis/meta/v1") {
			return
		}
		for field := range typ.Fields() {
			name := strings.ToLower(field.Name)
			for _, word := range forbidden {
				if strings.Contains(name, word) {
					t.Fatalf("resident runtime field %s%s may carry credential material", path, field.Name)
				}
			}
			walk(field.Type, path+field.Name+".")
		}
	}
	walk(reflect.TypeOf(value), "")
}
