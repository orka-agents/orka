/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	persistentCompositeNamespace    = "orka-system"
	persistentCompositeNamespaceUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	persistentCompositeSession      = "persistent-session"
)

// persistentCompositeHarness runs whole Session turns through the dispatcher
// against the Kubernetes-authoritative composite control store.
type persistentCompositeHarness struct {
	t            *testing.T
	kube         client.WithWatch
	controlStore *storekube.Store
	sqlite       *sealedTestStore
	epochs       *ControllerEpochManager
	stopEpochs   func()
	dispatcher   *ACPDispatcher
	profile      harnessv2.ProfileDigest
	mcp          string
	lineage      acpSessionLineageIdentity
}

func newPersistentCompositeHarness(t *testing.T, tasks ...*corev1alpha1.Task) *persistentCompositeHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := make([]client.Object, 0, 1+len(tasks))
	objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: persistentCompositeNamespace, UID: types.UID(persistentCompositeNamespaceUID),
	}})
	for _, task := range tasks {
		objects = append(objects, task.DeepCopy())
	}
	kubeClient := withControllerEpochLeaseUIDs(t, fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{}, &corev1alpha1.RuntimeSessionControl{}).
		WithObjects(objects...).Build())
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "persistent-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqliteStore := sealedStore(t, sqlite.NewStore(db, "persistent-session-test"))
	controlStore, err := storekube.NewComposite(kubeClient, persistentCompositeNamespace, sqliteStore, storekube.WithAPIReader(kubeClient))
	if err != nil {
		t.Fatal(err)
	}
	h := &persistentCompositeHarness{
		t: t, kube: kubeClient, controlStore: controlStore, sqlite: sqliteStore,
		profile: harnessv2.ProfileDigest(testControlDigestForDispatcher("persistent-profile")),
		mcp:     testControlDigestForDispatcher("persistent-mcp"),
	}
	h.lineage = acpSessionLineageIdentity{NamespaceUID: persistentCompositeNamespaceUID, RuntimeIdentity: "claude", ConfigDigest: string(h.profile)}
	h.startController("persistent-controller-a")
	return h
}

// startController starts a controller epoch manager and a dispatcher with an
// empty in-memory binding cache, as a controller restart does.
func (h *persistentCompositeHarness) startController(holder string) {
	h.t.Helper()
	if h.stopEpochs != nil {
		h.stopEpochs()
	}
	epochs := NewControllerEpochManager(h.controlStore, holder).WithStorage(storagev1.StateContract, h.sqlite)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- epochs.Start(ctx) }()
	stopped := false
	h.stopEpochs = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
	}
	h.t.Cleanup(h.stopEpochs)
	h.epochs = epochs
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: h.controlStore, Transcripts: h.sqlite, GatewayEvents: h.sqlite,
		Publications: h.controlStore, BranchClaims: h.controlStore, Lineages: h.sqlite,
		NewSessionUID: func() (string, error) { return "persistent-session-uid", nil },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.dispatcher = &ACPDispatcher{
		Client: h.kube, APIReader: h.kube, Store: h.controlStore, Sessions: continuity, Epochs: epochs,
		PersistentSessionEnabled: true,
	}
}

func (h *persistentCompositeHarness) fence() store.ControllerEpochFence {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fence, err := h.epochs.CurrentFence(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return fence
}

func persistentCompositeTask(name, uid string) *corev1alpha1.Task {
	task := runtimePoolReservationTestTask(name, uid, "runtime-pool-uid")
	task.Namespace = persistentCompositeNamespace
	task.Spec.Prompt = "prompt from " + name
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: persistentCompositeSession, Create: true, Append: true}
	task.Status.Attempts = 1
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateReserved
	return task
}

func (h *persistentCompositeHarness) reserve(task *corev1alpha1.Task, fence store.ControllerEpochFence) string {
	h.t.Helper()
	ctx := context.Background()
	attempt, err := h.controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		Key: store.PromptAttemptKey{
			Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID,
		},
		RequestDigest: task.Status.Execution.RequestDigest,
	}), fence)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.dispatcher.transitionAttempt(ctx, attempt.ID, fence, store.PromptExecutionQueued, store.PromptExecutionReserved, "reserve", nil); err != nil {
		h.t.Fatal(err)
	}
	return attempt.ID
}

func (h *persistentCompositeHarness) claimInput(fence store.ControllerEpochFence, reuseDigest string) *acpPersistentSessionClaimInput {
	return &acpPersistentSessionClaimInput{
		fence: fence,
		runtimeFence: harnessv2.Fence{
			RuntimeInstanceID: "runtime-instance", SupervisorBootID: "supervisor-boot", RuntimeProfileDigest: h.profile,
		},
		mcpDigest: h.mcp, reuseDigest: reuseDigest, lineageDigest: h.lineage.ConfigDigest,
		pool: &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Name: persistentTestPoolName, UID: "pool-uid", Generation: 4}},
		now:  time.Now().UTC(),
	}
}

func (h *persistentCompositeHarness) prepare(task *corev1alpha1.Task, fence store.ControllerEpochFence, claim *acpPersistentSessionClaimInput, reuse *acpPersistentSessionReuse) (*acpTaskSession, error) {
	return h.dispatcher.prepareTaskSessionWithClaim(
		context.Background(), task, fence, h.profile, h.mcp, "runtime-instance", "supervisor-boot", h.lineage, false, claim, reuse,
	)
}

// keep runs turn N to a succeeded finalization that keeps its runtime.
func (h *persistentCompositeHarness) keep(task *corev1alpha1.Task, reuseDigest string) *store.SessionResidentRuntime {
	h.t.Helper()
	const answer = "answer n"
	ctx := context.Background()
	fence := h.fence()
	attemptID := h.reserve(task, fence)
	session, err := h.prepare(task, fence, nil, nil)
	if err != nil {
		h.t.Fatalf("prepare turn N: %v", err)
	}
	from := store.PromptExecutionSessionStarting
	for _, to := range []store.PromptExecutionState{
		store.PromptExecutionPlanned, store.PromptExecutionSubmitting, store.PromptExecutionAccepted,
		store.PromptExecutionRunning, store.PromptExecutionSettling, store.PromptExecutionSucceeded,
	} {
		if err := h.dispatcher.transitionAttempt(ctx, attemptID, fence, from, to, "complete-"+string(to), nil); err != nil {
			h.t.Fatal(err)
		}
		from = to
	}
	keptAt := time.Now().UTC()
	record, err := store.NewSessionResidentRuntime(store.SessionResidentRuntime{
		SourceTaskUID: string(task.UID), SourceAttempt: 1, SourcePromptAttemptID: attemptID,
		SourceTurnID: session.Turn.Turn.ID, SourceLeaseGeneration: session.Turn.Turn.Key.LeaseGeneration,
		ControllerEpochName: fence.Name, ControllerEpoch: fence.Epoch,
		WorkspaceName: persistentTestWorkspace, WorkspaceUID: "workspace-uid", WorkspaceAttachmentEpoch: 1,
		RuntimePoolName: persistentTestPoolName, RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4,
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "supervisor-boot", ServedRecordDigest: testControlDigestForDispatcher("served"),
		RuntimeSessionUID: session.Binding.SessionUID, RuntimeSessionGeneration: int64(session.Binding.Generation),
		ProfileDigest: string(h.profile), MCPDigest: h.mcp, WorkspaceDigest: testControlDigestForDispatcher("workspace"),
		LineageConfigDigest: h.lineage.ConfigDigest, ReuseDigest: reuseDigest,
		TranscriptHeadDigest: acpPersistentSessionTranscriptHeadDigest("", []acpTranscriptHeadMessage{
			{Role: acpBootstrapRoleUser, Content: task.Spec.Prompt}, {Role: acpSessionTurnAssistantRole, Content: answer},
		}),
		PromptDelivered: true, KeptAt: keptAt, ExpiresAt: keptAt.Add(5 * time.Minute),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	session.KeepRecord = &record
	if err := h.dispatcher.finalizeTaskSessionResult(ctx, task, fence, session, answer, "", corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateReadValidated, Outcome: corev1alpha1.TaskDeliveryOutcomeReadValidated,
	}); err != nil {
		h.t.Fatalf("finalize turn N: %v", err)
	}
	if !session.kept {
		h.t.Fatal("the finalization write did not keep the record")
	}
	return &record
}

func (h *persistentCompositeHarness) control() *store.SessionControl {
	h.t.Helper()
	control, err := h.controlStore.GetSessionControl(context.Background(), persistentCompositeNamespace, persistentCompositeSession)
	if err != nil {
		h.t.Fatal(err)
	}
	return control
}

func TestPersistentSessionTurnClaimsTheKeptRuntimeAndSendsOnlyItsOwnPrompt(t *testing.T) {
	turnN := persistentCompositeTask("turn-n", "11111111-0000-0000-0000-000000000001")
	turnN1 := persistentCompositeTask("turn-n1", "11111111-0000-0000-0000-000000000002")
	h := newPersistentCompositeHarness(t, turnN, turnN1)
	reuse := testControlDigestForDispatcher("reuse")
	kept := h.keep(turnN, reuse)

	fence := h.fence()
	h.reserve(turnN1, fence)
	decision := &acpPersistentSessionReuse{attempted: true}
	session, err := h.prepare(turnN1, fence, h.claimInput(fence, reuse), decision)
	if err != nil {
		t.Fatalf("prepare turn N+1: %v", err)
	}
	if session.ResidentClaimed == nil || session.ResidentClaimed.HandoffID != kept.HandoffID || !session.Reused {
		t.Fatalf("turn N+1 did not claim the kept runtime: claimed=%v reused=%v reason=%q", session.ResidentClaimed != nil, session.Reused, decision.reason)
	}
	if session.Bootstrap != nil || session.Binding.Generation != uint64(kept.RuntimeSessionGeneration) || !session.Binding.PromptDelivered {
		t.Fatalf("a reused process must get no bootstrap and keep its generation: %#v", session.Binding)
	}
	if session.ClaimedTranscriptHeadID == "" {
		t.Fatal("a claimed reuse must record the last canonical message the process holds")
	}
	if session.UserPrompt != turnN1.Spec.Prompt {
		t.Fatalf("turn N+1 sends %q, want only its own prompt %q", session.UserPrompt, turnN1.Spec.Prompt)
	}
	if session.FallbackBootstrap == nil || session.FallbackBootstrap.MessageCount != 2 {
		t.Fatalf("a refused gate must be able to recreate from the canonical transcript: %#v", session.FallbackBootstrap)
	}
	if !store.ResidentRuntimeClaimedBy(h.control().ResidentRuntime, session.Turn.Turn.Key) {
		t.Fatalf("the durable record is not claimed by turn N+1's lease: %#v", h.control().ResidentRuntime)
	}
}

func TestPersistentSessionRestartBetweenTurnsRecreatesAndRetiresTheRecord(t *testing.T) {
	turnN := persistentCompositeTask("turn-n", "22222222-0000-0000-0000-000000000001")
	turnN1 := persistentCompositeTask("turn-n1", "22222222-0000-0000-0000-000000000002")
	h := newPersistentCompositeHarness(t, turnN, turnN1)
	reuse := testControlDigestForDispatcher("reuse")
	kept := h.keep(turnN, reuse)
	before := h.fence()

	// A controller restart is an epoch takeover with an empty binding cache.
	h.startController("persistent-controller-b")
	fence := h.fence()
	if fence.Epoch <= before.Epoch {
		t.Fatalf("restart did not advance the controller epoch: %d -> %d", before.Epoch, fence.Epoch)
	}
	h.reserve(turnN1, fence)
	decision := &acpPersistentSessionReuse{attempted: true}
	session, err := h.prepare(turnN1, fence, h.claimInput(fence, reuse), decision)
	if err != nil {
		t.Fatalf("prepare turn N+1 after restart: %v", err)
	}
	if session.ResidentClaimed != nil || session.Reused || session.Bootstrap == nil ||
		session.Binding.Generation <= uint64(kept.RuntimeSessionGeneration) {
		t.Fatalf("a restart must cost one cold turn: claimed=%v reused=%v generation=%d", session.ResidentClaimed != nil, session.Reused, session.Binding.Generation)
	}
	if decision.reason != acpPersistentReasonEpochChanged {
		t.Fatalf("miss reason = %q, want %q", decision.reason, acpPersistentReasonEpochChanged)
	}
	if record := h.control().ResidentRuntime; record == nil || record.State != store.SessionResidentRuntimeRetiring {
		t.Fatalf("the ordinary acquisition must retire the old epoch's record: %#v", record)
	}
}

func TestPersistentSessionTakeoverFencesTheOldControllerOut(t *testing.T) {
	turnN := persistentCompositeTask("turn-n", "33333333-0000-0000-0000-000000000001")
	turnN1 := persistentCompositeTask("turn-n1", "33333333-0000-0000-0000-000000000002")
	h := newPersistentCompositeHarness(t, turnN, turnN1)
	reuse := testControlDigestForDispatcher("reuse")
	h.keep(turnN, reuse)
	old := h.fence()
	oldDispatcher := h.dispatcher

	// ADR 0058 takeover: a successor holds the epoch; the predecessor's fence
	// can no longer claim or write anything.
	h.startController("persistent-controller-successor")
	h.dispatcher = oldDispatcher
	h.reserve(turnN1, h.fence())
	if _, err := h.prepare(turnN1, old, h.claimInput(old, reuse), &acpPersistentSessionReuse{attempted: true}); err == nil {
		t.Fatal("the predecessor's fence must not acquire the Session lease after a takeover")
	}
	if record := h.control().ResidentRuntime; record == nil || record.State != store.SessionResidentRuntimeResident {
		t.Fatalf("a fenced-out attempt must not touch the record: %#v", record)
	}
}

func TestPersistentSessionConcurrentContinuationsClaimExactlyOnce(t *testing.T) {
	turnN := persistentCompositeTask("turn-n", "44444444-0000-0000-0000-000000000001")
	rivals := []*corev1alpha1.Task{
		persistentCompositeTask("turn-a", "44444444-0000-0000-0000-00000000000a"),
		persistentCompositeTask("turn-b", "44444444-0000-0000-0000-00000000000b"),
		persistentCompositeTask("turn-c", "44444444-0000-0000-0000-00000000000c"),
	}
	h := newPersistentCompositeHarness(t, append([]*corev1alpha1.Task{turnN}, rivals...)...)
	reuse := testControlDigestForDispatcher("reuse")
	h.keep(turnN, reuse)
	fence := h.fence()
	for _, task := range rivals {
		h.reserve(task, fence)
	}
	sessions := make([]*acpTaskSession, len(rivals))
	errs := make([]error, len(rivals))
	var wg sync.WaitGroup
	for i, task := range rivals {
		wg.Go(func() {
			sessions[i], errs[i] = h.prepare(task, fence, h.claimInput(fence, reuse), &acpPersistentSessionReuse{attempted: true})
		})
	}
	wg.Wait()
	claimed := 0
	for i := range rivals {
		if errs[i] == nil && sessions[i] != nil && sessions[i].ResidentClaimed != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("%d continuations claimed the runtime, want exactly one (errors %v)", claimed, errs)
	}
}

// A class opt-out (or a refused reuse) always plans a fresh RuntimeSession,
// even when the in-memory cache still names the resident process.
func TestPersistentSessionForcedFreshNeverReusesTheCachedBinding(t *testing.T) {
	turnN := persistentCompositeTask("turn-n", "55555555-0000-0000-0000-000000000001")
	turnN1 := persistentCompositeTask("turn-n1", "55555555-0000-0000-0000-000000000002")
	h := newPersistentCompositeHarness(t, turnN, turnN1)
	kept := h.keep(turnN, testControlDigestForDispatcher("reuse"))
	// Even a cache entry naming the resident process must not be reused.
	h.dispatcher.setRuntimeSessionBinding(acpRuntimeSessionBindingFromResidentRuntime(kept))
	fence := h.fence()
	h.reserve(turnN1, fence)
	session, err := h.prepare(turnN1, fence, &acpPersistentSessionClaimInput{forceFresh: true}, &acpPersistentSessionReuse{attempted: true})
	if err != nil {
		t.Fatal(err)
	}
	if session.Reused || session.ResidentClaimed != nil || session.Bootstrap == nil ||
		session.Binding.Generation <= uint64(kept.RuntimeSessionGeneration) {
		t.Fatalf("forced fresh turn reused=%v claimed=%v generation=%d", session.Reused, session.ResidentClaimed != nil, session.Binding.Generation)
	}
	if record := h.control().ResidentRuntime; record == nil || record.State != store.SessionResidentRuntimeRetiring {
		t.Fatalf("the unclaimed record must retire: %#v", record)
	}
}
