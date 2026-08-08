package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

type harnessV1SessionFenceStore struct {
	store.SessionControlStore
	controls        []store.SessionControl
	getErrors       map[int]error
	acquireErr      error
	getCalls        int
	acquireCalls    int
	createTurnCalls int
}

func (s *harnessV1SessionFenceStore) GetSessionControl(
	context.Context,
	string,
	string,
) (*store.SessionControl, error) {
	call := s.getCalls
	s.getCalls++
	if err := s.getErrors[call]; err != nil {
		return nil, err
	}
	if len(s.controls) == 0 {
		return nil, store.ErrNotFound
	}
	index := call
	if index >= len(s.controls) {
		index = len(s.controls) - 1
	}
	return copyHarnessV1SessionControl(s.controls[index]), nil
}

func (s *harnessV1SessionFenceStore) AcquireSessionMutationLease(
	_ context.Context,
	request store.AcquireSessionMutationLeaseRequest,
) (*store.SessionControl, error) {
	s.acquireCalls++
	if s.acquireErr != nil {
		return nil, s.acquireErr
	}
	control := *copyHarnessV1SessionControl(s.controls[len(s.controls)-1])
	if control.SessionUID != request.SessionUID {
		return nil, store.ErrConflict
	}
	if control.Lease == nil {
		if control.Version != request.ExpectedVersion ||
			control.LeaseGeneration != request.ExpectedLeaseGeneration {
			return nil, store.ErrConflict
		}
		control.Version++
		control.LeaseGeneration++
		control.Lease = &store.SessionMutationLease{
			Generation:    control.LeaseGeneration,
			TaskUID:       request.TaskUID,
			Attempt:       request.Attempt,
			PromptID:      request.PromptID,
			RequestDigest: request.RequestDigest,
			AcquiredAt:    request.AcquiredAt,
			ExpiresAt:     request.ExpiresAt,
		}
	}
	return &control, nil
}

func (s *harnessV1SessionFenceStore) CreateSessionTurn(
	_ context.Context,
	request store.CreateSessionTurnRequest,
) (*store.SessionTurn, error) {
	s.createTurnCalls++
	turn := request.Turn
	turn.State = store.SessionTurnOpen
	turn.Version = 1
	return &turn, nil
}

func copyHarnessV1SessionControl(control store.SessionControl) *store.SessionControl {
	copyControl := control
	if control.Lease != nil {
		copyLease := *control.Lease
		copyControl.Lease = &copyLease
	}
	return &copyControl
}

func TestPrepareHarnessV1TaskSessionRechecksFrozenControlBeforeAcquire(t *testing.T) {
	bootstrap, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	advanced := initial
	advanced.Version++
	controls := &harnessV1SessionFenceStore{controls: []store.SessionControl{initial, initial, advanced}}
	dispatcher, fence, closeStore := harnessV1SessionFenceDispatcher(t, controls, task)
	defer closeStore()

	err := dispatcher.prepareHarnessV1TaskSession(context.Background(), task, verified, attempt, fence)
	if err == nil || !errors.Is(err, store.ErrConflict) || !isPermanentHarnessV1PreSubmitSessionError(err) {
		t.Fatalf("stale frozen Session control error = %v, want permanent ErrConflict", err)
	}
	if bootstrap.ControlVersion != initial.Version || controls.acquireCalls != 0 {
		t.Fatalf("frozen version=%d initial=%d acquire calls=%d, want no acquire", bootstrap.ControlVersion, initial.Version, controls.acquireCalls)
	}
}

func TestPrepareHarnessV1TaskSessionRejectsRecreatedSessionUID(t *testing.T) {
	_, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	recreated := initial
	recreated.SessionUID = "recreated-session-uid"
	controls := &harnessV1SessionFenceStore{controls: []store.SessionControl{recreated}}
	dispatcher, fence, closeStore := harnessV1SessionFenceDispatcher(t, controls, task)
	defer closeStore()

	err := dispatcher.prepareHarnessV1TaskSession(context.Background(), task, verified, attempt, fence)
	if err == nil || !errors.Is(err, store.ErrConflict) || !isPermanentHarnessV1PreSubmitSessionError(err) {
		t.Fatalf("recreated Session UID error = %v, want permanent ErrConflict", err)
	}
	if controls.acquireCalls != 0 {
		t.Fatalf("acquire calls after Session recreation = %d, want 0", controls.acquireCalls)
	}
}

func TestPrepareHarnessV1TaskSessionNewUIDMustWinCreation(t *testing.T) {
	_, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, true)
	competing := initial
	competing.SessionUID = "competing-session-uid"
	competing.Version = 1
	controls := &harnessV1SessionFenceStore{controls: []store.SessionControl{competing}}
	dispatcher, fence, closeStore := harnessV1SessionFenceDispatcher(t, controls, task)
	defer closeStore()

	err := dispatcher.prepareHarnessV1TaskSession(context.Background(), task, verified, attempt, fence)
	if err == nil || !errors.Is(err, store.ErrConflict) || !isPermanentHarnessV1PreSubmitSessionError(err) {
		t.Fatalf("competing new Session UID error = %v, want permanent ErrConflict", err)
	}
	if controls.acquireCalls != 0 {
		t.Fatalf("acquire calls after losing Session creation = %d, want 0", controls.acquireCalls)
	}
}

func TestPrepareHarnessV1TaskSessionClassifiesAcquireWinnerAsPermanent(t *testing.T) {
	_, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	advanced := initial
	advanced.Version++
	controls := &harnessV1SessionFenceStore{
		controls:   []store.SessionControl{initial, initial, initial, advanced},
		acquireErr: fmt.Errorf("lease compare-and-swap: %w", store.ErrConflict),
	}
	dispatcher, fence, closeStore := harnessV1SessionFenceDispatcher(t, controls, task)
	defer closeStore()

	err := dispatcher.prepareHarnessV1TaskSession(context.Background(), task, verified, attempt, fence)
	if err == nil || !errors.Is(err, store.ErrConflict) || !isPermanentHarnessV1PreSubmitSessionError(err) {
		t.Fatalf("acquire winner error = %v, want permanent ErrConflict", err)
	}
	if controls.acquireCalls != 1 || controls.createTurnCalls != 0 {
		t.Fatalf("acquire winner calls acquire=%d createTurn=%d, want 1/0", controls.acquireCalls, controls.createTurnCalls)
	}
}

func TestPrepareHarnessV1TaskSessionKeepsUnchangedAcquireConflictRetryable(t *testing.T) {
	_, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	controls := &harnessV1SessionFenceStore{
		controls:   []store.SessionControl{initial},
		acquireErr: fmt.Errorf("stale controller epoch: %w", store.ErrConflict),
	}
	dispatcher, fence, closeStore := harnessV1SessionFenceDispatcher(t, controls, task)
	defer closeStore()

	err := dispatcher.prepareHarnessV1TaskSession(context.Background(), task, verified, attempt, fence)
	if err == nil || !errors.Is(err, store.ErrConflict) || isPermanentHarnessV1PreSubmitSessionError(err) {
		t.Fatalf("unchanged acquire conflict = %v, want retryable ErrConflict", err)
	}
	if controls.acquireCalls != 1 || controls.createTurnCalls != 0 {
		t.Fatalf("unchanged conflict calls acquire=%d createTurn=%d, want 1/0", controls.acquireCalls, controls.createTurnCalls)
	}
}

func TestPrepareHarnessV1TaskSessionKeepsFailedConflictRecheckRetryable(t *testing.T) {
	_, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	recheckErr := errors.New("temporary Session API read failure")
	controls := &harnessV1SessionFenceStore{
		controls:   []store.SessionControl{initial},
		getErrors:  map[int]error{3: recheckErr},
		acquireErr: fmt.Errorf("lease compare-and-swap: %w", store.ErrConflict),
	}
	dispatcher, fence, closeStore := harnessV1SessionFenceDispatcher(t, controls, task)
	defer closeStore()

	err := dispatcher.prepareHarnessV1TaskSession(context.Background(), task, verified, attempt, fence)
	if err == nil || !errors.Is(err, store.ErrConflict) || !errors.Is(err, recheckErr) ||
		isPermanentHarnessV1PreSubmitSessionError(err) {
		t.Fatalf("failed conflict recheck = %v, want retryable joined error", err)
	}
	if controls.acquireCalls != 1 || controls.createTurnCalls != 0 {
		t.Fatalf("failed recheck calls acquire=%d createTurn=%d, want 1/0", controls.acquireCalls, controls.createTurnCalls)
	}
}

func TestPrepareHarnessV1TaskSessionRecoversExactPreparedLease(t *testing.T) {
	bootstrap, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	expectedGeneration := bootstrap.LeaseGeneration + 1
	leaseDigest, err := acpSessionMutationLeaseDigest(
		bootstrap.SessionUID,
		expectedGeneration,
		string(task.UID),
		int64(attempt.Attempt),
		attempt.TurnID,
		attempt.RequestDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	leased := initial
	leased.Version++
	leased.LeaseGeneration = expectedGeneration
	leased.Lease = &store.SessionMutationLease{
		Generation: expectedGeneration,
		TaskUID:    string(task.UID), Attempt: int64(attempt.Attempt), PromptID: attempt.TurnID,
		RequestDigest: leaseDigest, AcquiredAt: time.Now().UTC(),
	}
	controls := &harnessV1SessionFenceStore{controls: []store.SessionControl{leased}}
	dispatcher, fence, closeStore := harnessV1SessionFenceDispatcher(t, controls, task)
	defer closeStore()

	if err := dispatcher.prepareHarnessV1TaskSession(context.Background(), task, verified, attempt, fence); err != nil {
		t.Fatalf("recover exact Prepared Session lease: %v", err)
	}
	if controls.acquireCalls != 1 || controls.createTurnCalls != 1 {
		t.Fatalf("Prepared lease recovery calls acquire=%d createTurn=%d, want 1/1", controls.acquireCalls, controls.createTurnCalls)
	}
}

func TestPrepareHarnessV1TaskSessionAllowsOwnFinalizedRetry(t *testing.T) {
	bootstrap, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	attempt.Attempt = 2
	attempt.TurnID = string(harnessV1TurnID(task, attempt.Attempt))
	retryReady := initial
	retryReady.Version += 2
	retryReady.LeaseGeneration++
	priorTurnID, err := store.SessionTurnKey{
		SessionUID:      bootstrap.SessionUID,
		LeaseGeneration: retryReady.LeaseGeneration,
		TaskUID:         string(task.UID),
		Attempt:         1,
		PromptID:        string(harnessV1TurnID(task, 1)),
	}.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	retryReady.LastOperationID = "finalize:" + priorTurnID
	controls := &harnessV1SessionFenceStore{controls: []store.SessionControl{retryReady, retryReady, retryReady}}
	dispatcher, fence, closeStore := harnessV1SessionFenceDispatcher(t, controls, task)
	defer closeStore()

	if err := dispatcher.prepareHarnessV1TaskSession(context.Background(), task, verified, attempt, fence); err != nil {
		t.Fatalf("prepare Session-backed safe retry: %v", err)
	}
	if controls.acquireCalls != 1 || controls.createTurnCalls != 1 {
		t.Fatalf("safe retry calls acquire=%d createTurn=%d, want 1/1", controls.acquireCalls, controls.createTurnCalls)
	}
}

func TestValidateFrozenHarnessV1SessionControlRejectsForeignRetryAdvance(t *testing.T) {
	_, initial, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	attempt.Attempt = 2
	attempt.TurnID = string(harnessV1TurnID(task, attempt.Attempt))
	advanced := initial
	advanced.Version += 2
	advanced.LeaseGeneration++

	err := validateFrozenHarnessV1SessionControl(
		verified.body.HarnessV1.SessionBootstrap,
		&advanced,
		task,
		attempt,
	)
	if err == nil || !errors.Is(err, store.ErrConflict) || !isPermanentHarnessV1PreSubmitSessionError(err) {
		t.Fatalf("foreign retry advance error = %v, want permanent ErrConflict", err)
	}
}

func TestFinalizeHarnessV1TaskSessionDoesNotRequireTurnAfterSessionConflict(t *testing.T) {
	_, _, task, verified, attempt := harnessV1SessionFenceFixture(t, false)
	attempt.State = store.HarnessV1AttemptRejected
	attempt.TerminalReason = harnessV1ReasonSessionConflict

	settled, err := (&HarnessV1Dispatcher{}).finalizeHarnessV1TaskSession(
		context.Background(), task, verified, attempt, store.ControllerEpochFence{}, false,
	)
	if err != nil || settled {
		t.Fatalf("finalize pre-submit Session conflict = settled:%t err:%v, want false/nil", settled, err)
	}
}

func harnessV1SessionFenceFixture(
	t *testing.T,
	newSession bool,
) (*agentExecutionSnapshotHarnessV1SessionBootstrap, store.SessionControl, *corev1alpha1.Task, *verifiedHarnessV1Execution, *store.HarnessV1Attempt) {
	t.Helper()
	controlVersion := int64(3)
	if newSession {
		controlVersion = 0
	}
	bootstrapTranscript, err := buildACPBootstrapTranscript(nil, ACPBootstrapLimits{
		MaxMessages:     harnessV1SessionBootstrapMaxMessages,
		MaxBytes:        harnessV1SessionBootstrapMaxBytes,
		MaxMessageBytes: harnessV1SessionBootstrapMaxMessageBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := &agentExecutionSnapshotHarnessV1SessionBootstrap{
		SchemaVersion: harnessV1SessionBootstrapSchemaVersion,
		SessionUID:    "frozen-session-uid", ControlVersion: controlVersion, LeaseGeneration: 4,
		Artifact: string(bootstrapTranscript.Artifact), Digest: bootstrapTranscript.Digest,
	}
	if newSession {
		bootstrap.LeaseGeneration = 0
	}
	initialVersion := controlVersion
	if newSession {
		initialVersion = 1
	}
	initial := store.SessionControl{
		Namespace: "ns", SessionName: "continued-session", SessionUID: bootstrap.SessionUID,
		Availability: store.SessionAvailable, Version: initialVersion,
		LeaseGeneration: bootstrap.LeaseGeneration,
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "continued-task", UID: types.UID("continued-task-uid")},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "continue the Session",
			SessionRef: &corev1alpha1.SessionReference{
				Name: initial.SessionName, Create: newSession, Append: true,
			},
		},
	}
	binding := &corev1alpha1.AgentExecutionBinding{
		ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
		Backend:         corev1alpha1.AgentExecutionBackendHarnessWrapper,
		RuntimeType:     corev1alpha1.AgentRuntimeCodex,
		Task:            corev1alpha1.AgentExecutionBindingTaskRef{NamespaceUID: types.UID("ns-uid"), UID: task.UID, BoundSpecGeneration: 1},
		Agent:           &corev1alpha1.AgentExecutionAgentRef{Namespace: "ns", Name: "agent", UID: types.UID("agent-uid"), Generation: 1},
	}
	verified := &verifiedHarnessV1Execution{
		binding: binding, frozenTask: task.DeepCopy(),
		body: agentExecutionSnapshotBody{HarnessV1: &agentExecutionSnapshotHarnessV1{
			SessionName: initial.SessionName, SessionBootstrap: bootstrap,
		}},
	}
	attempt := &store.HarnessV1Attempt{
		Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1,
		TurnID: "continued-turn", RequestDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte("continued-request")),
	}
	return bootstrap, initial, task, verified, attempt
}

func harnessV1SessionFenceDispatcher(
	t *testing.T,
	controls *harnessV1SessionFenceStore,
	task *corev1alpha1.Task,
) (*HarnessV1Dispatcher, store.ControllerEpochFence, func()) {
	t.Helper()
	transcripts, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "harness-v1-session-fence.db"))
	now := time.Now().UTC()
	if err := transcripts.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: task.Namespace, Name: task.Spec.SessionRef.Name, SessionType: defaultACPSessionType,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		closeStore()
		t.Fatal(err)
	}
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: controls, Transcripts: transcripts,
		Publications: transcripts, BranchClaims: transcripts,
	})
	if err != nil {
		closeStore()
		t.Fatal(err)
	}
	return &HarnessV1Dispatcher{Sessions: continuity}, fence, closeStore
}
