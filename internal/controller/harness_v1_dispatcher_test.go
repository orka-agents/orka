/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestBuildHarnessV1StartTurnRequestUsesStableCanonicalDigest(t *testing.T) {
	boundAt := time.Date(2026, 8, 5, 18, 0, 0, 123000000, time.UTC)
	bindingDigest := "sha256:" + strings.Repeat("a", 64)
	snapshotDigest := "sha256:" + strings.Repeat("b", 64)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "turn-task", UID: types.UID("task-uid"),
		CreationTimestamp: metav1.NewTime(boundAt.Add(-30 * time.Second)),
	}}
	verified := &verifiedHarnessV1Execution{
		binding: &corev1alpha1.AgentExecutionBinding{
			BindingDigest: bindingDigest, BoundAt: metav1.NewTime(boundAt),
			Snapshot: corev1alpha1.AgentExecutionSnapshotRef{Digest: snapshotDigest},
		},
		body: agentExecutionSnapshotBody{
			Prompt: "perform the work", Timeout: "2m",
			Configuration: agentExecutionSnapshotConfig{
				Model: "frozen-model", SystemPrompt: "frozen system prompt",
				MaxTurns: 7, ReasoningEffort: "high",
			},
			DefaultTools: &agentExecutionSnapshotToolPolicy{
				AllowedToolsOmitted: false, AllowedTools: []string{"Read"}, AllowBash: new(true),
			},
			RuntimeOverride: &corev1alpha1.AgentRuntimeSpec{
				AllowedTools: []string{}, DisallowedTools: []string{"Bash"}, AllowBash: new(false),
			},
			HarnessV1: &agentExecutionSnapshotHarnessV1{
				SessionName: "session-a", RuntimeName: string(corev1alpha1.AgentRuntimeCodex), RuntimeAuthOnly: true,
			},
		},
	}
	attempt := &store.HarnessV1Attempt{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), Attempt: 1,
		BindingDigest: bindingDigest, SnapshotDigest: snapshotDigest,
		RuntimeSessionID: "runtime-session-a", TurnID: "turn-a", CorrelationID: "correlation-a",
	}
	env := []harness.TurnEnvVar{{Name: "PROVIDER_TOKEN", Value: "test-value"}}

	request, err := buildHarnessV1StartTurnRequest(task, verified, attempt, env)
	if err != nil {
		t.Fatal(err)
	}
	wantDeadline := task.CreationTimestamp.Add(2 * time.Minute)
	if !request.Deadline.Equal(wantDeadline) {
		t.Fatalf("deadline = %s, want %s", request.Deadline, wantDeadline)
	}
	if request.Metadata["runtimeAuthOnly"] != "true" {
		t.Fatalf("runtimeAuthOnly metadata = %q, want true", request.Metadata["runtimeAuthOnly"])
	}
	for key, want := range map[string]string{
		harness.MetadataRuntimePolicyFrozen: "true",
		harness.MetadataAllowedToolsSet:     "true",
		"runtime":                           string(corev1alpha1.AgentRuntimeCodex),
		"orka.runtimeName":                  string(corev1alpha1.AgentRuntimeCodex),
		"model":                             "frozen-model",
		"systemPrompt":                      "frozen system prompt",
		"maxTurns":                          "7",
		"reasoningEffort":                   "high",
		"allowedTools":                      "",
		"disallowedTools":                   "Bash",
		"allowBash":                         "false",
		"readOnly":                          "true",
	} {
		if got := request.Metadata[key]; got != want {
			t.Fatalf("metadata[%q] = %q, want %q", key, got, want)
		}
	}
	digest, err := harness.CanonicalStartTurnRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	attempt.RequestDigest = digest

	reconstructed, err := buildHarnessV1StartTurnRequest(task, verified, attempt, env)
	if err != nil {
		t.Fatal(err)
	}
	if reconstructed.Metadata[harness.MetadataRequestDigest] != digest {
		t.Fatalf("request metadata digest = %q, want %q", reconstructed.Metadata[harness.MetadataRequestDigest], digest)
	}
	reconstructedDigest, err := harness.CanonicalStartTurnRequestDigest(reconstructed)
	if err != nil {
		t.Fatal(err)
	}
	if reconstructedDigest != digest {
		t.Fatalf("reconstructed digest = %q, want %q", reconstructedDigest, digest)
	}

	mutatedAuth := *verified
	mutatedAuth.body = verified.body
	mutatedHarnessV1 := *verified.body.HarnessV1
	mutatedHarnessV1.RuntimeAuthOnly = false
	mutatedAuth.body.HarnessV1 = &mutatedHarnessV1
	if _, err := buildHarnessV1StartTurnRequest(task, &mutatedAuth, attempt, env); err == nil ||
		!strings.Contains(err.Error(), "does not match the durable attempt") {
		t.Fatalf("mutated runtime-auth-only error = %v, want durable digest mismatch", err)
	}

	mutated := *verified
	mutated.body = verified.body
	mutated.body.Prompt = "different prompt"
	if _, err := buildHarnessV1StartTurnRequest(task, &mutated, attempt, env); err == nil ||
		!strings.Contains(err.Error(), "does not match the durable attempt") {
		t.Fatalf("mutated request error = %v, want durable digest mismatch", err)
	}
}

func TestContinueHarnessV1BrokeredToolCallReplaysDurableResult(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixture.task.Namespace, Name: "lookup", UID: types.UID("lookup-tool-uid"), Generation: 1,
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "look up a value", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP: &corev1alpha1.HTTPExecution{URL: "https://tools.example.test/lookup", Method: http.MethodPost},
		},
	}
	if err := fixture.dispatcher.Client.Create(fixture.ctx, tool); err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Tool{}
	if err := fixture.dispatcher.APIReader.Get(fixture.ctx, client.ObjectKeyFromObject(tool), current); err != nil {
		t.Fatal(err)
	}
	digest, err := harnessV1BrokeredToolDefinitionDigest(current)
	if err != nil {
		t.Fatal(err)
	}
	fixture.verified.body.HarnessV1.ToolExecutionMode = string(harness.ToolExecutionModeBrokered)
	fixture.verified.body.HarnessV1.BrokeredToolClasses = []corev1alpha1.AgentRuntimeBrokeredToolClass{
		corev1alpha1.AgentRuntimeBrokeredToolClassRead,
	}
	fixture.verified.body.HarnessV1.BrokeredTools = []agentExecutionSnapshotHarnessV1BrokeredTool{{
		Name: current.Name, Description: current.Spec.Description, BrokeredClass: current.Spec.BrokeredToolClass,
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":2}},"required":["query"],"additionalProperties":false}`),
		UID:        string(current.UID), Generation: current.Generation,
		DefinitionDigest: digest,
	}}
	call := harness.ToolCallRequest{
		Version: harness.ProtocolVersion, RuntimeSessionID: fixture.request.RuntimeSessionID,
		TurnID: fixture.request.TurnID, ToolCallID: "call-1", ToolName: current.Name,
		Input: json.RawMessage(`{"query":"value"}`),
	}
	call.IdempotencyKey = harness.ToolRequestIdempotencyKey(call.RuntimeSessionID, call.TurnID, call.ToolCallID)
	content, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	frame := harness.HarnessEventFrame{
		Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested,
		RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
		CorrelationID: fixture.request.CorrelationID, Seq: 2,
		ToolName: current.Name, ToolCallID: call.ToolCallID, Content: content,
	}
	executeCalls := 0
	fixture.dispatcher.ExternalEffects = fixture.durable
	fixture.dispatcher.BrokeredToolExecutor = HarnessV1BrokeredToolExecutorFunc(func(
		_ context.Context,
		namespace string,
		gotTool *corev1alpha1.Tool,
		request harness.ToolCallRequest,
	) (json.RawMessage, error) {
		executeCalls++
		if namespace != fixture.task.Namespace || gotTool.UID != current.UID || request.IdempotencyKey != call.IdempotencyKey {
			t.Fatalf("unexpected brokered execution: namespace=%q tool=%s request=%+v", namespace, gotTool.UID, request)
		}
		return json.RawMessage(`{"answer":42}`), nil
	})
	var continuedResults []harness.ToolCallResult
	protocolClient := &recordingHarnessV1RecoveryClient{continueTurn: func(
		_ context.Context,
		request harness.ContinueTurnRequest,
	) (*harness.ContinueTurnResponse, error) {
		if err := request.Validate(); err != nil {
			t.Fatalf("invalid ContinueTurn request: %v", err)
		}
		continuedResults = append(continuedResults, request.ToolResults[0])
		return &harness.ContinueTurnResponse{
			Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: request.RuntimeSessionID,
			TurnID: request.TurnID, CorrelationID: request.CorrelationID,
		}, nil
	}}

	for i := range 2 {
		if err := fixture.dispatcher.continueHarnessV1BrokeredToolCall(
			fixture.ctx, fixture.task, fixture.verified, protocolClient, fixture.request,
			fixture.attempt, fixture.fence, frame,
		); err != nil {
			t.Fatalf("continue brokered call %d: %v", i+1, err)
		}
	}
	if executeCalls != 1 {
		t.Fatalf("brokered executor calls = %d, want one durable execution", executeCalls)
	}
	if protocolClient.continueCalls != 2 || len(continuedResults) != 2 {
		t.Fatalf("ContinueTurn calls/results = %d/%d, want 2/2", protocolClient.continueCalls, len(continuedResults))
	}
	first, err := json.Marshal(continuedResults[0])
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(continuedResults[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || string(continuedResults[0].Output) != `{"answer":42}` {
		t.Fatalf("replayed tool result differs: first=%s second=%s", first, second)
	}

	invalidCall := call
	invalidCall.ToolCallID = "call-invalid"
	invalidCall.Input = json.RawMessage(`{"query":1}`)
	invalidCall.IdempotencyKey = harness.ToolRequestIdempotencyKey(
		invalidCall.RuntimeSessionID, invalidCall.TurnID, invalidCall.ToolCallID,
	)
	invalidContent, err := json.Marshal(invalidCall)
	if err != nil {
		t.Fatal(err)
	}
	invalidFrame := frame
	invalidFrame.Seq = 3
	invalidFrame.ToolCallID = invalidCall.ToolCallID
	invalidFrame.Content = invalidContent
	if err := fixture.dispatcher.continueHarnessV1BrokeredToolCall(
		fixture.ctx, fixture.task, fixture.verified, protocolClient, fixture.request,
		fixture.attempt, fixture.fence, invalidFrame,
	); err == nil || !strings.Contains(err.Error(), "input does not match the frozen parameters schema") {
		t.Fatalf("invalid brokered input error = %v, want frozen schema rejection", err)
	}
	if executeCalls != 1 || protocolClient.continueCalls != 2 {
		t.Fatalf("invalid input reached executor/continuation: execute=%d continue=%d", executeCalls, protocolClient.continueCalls)
	}
	invalidIdentity := store.ExternalEffectIdentity{
		Kind: "harness-v1-tool", Namespace: fixture.task.Namespace, AggregateID: string(fixture.task.UID),
		OperationID: store.CanonicalControlID(
			"harness-v1-tool-call", string(invalidCall.RuntimeSessionID), string(invalidCall.TurnID), invalidCall.ToolCallID,
		),
	}
	invalidEffectID, err := invalidIdentity.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.durable.GetExternalEffect(fixture.ctx, invalidEffectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid input external effect error = %v, want no durable reservation", err)
	}
}

func TestBuildHarnessV1StartTurnRequestRendersFrozenSessionBootstrap(t *testing.T) {
	bootstrap, err := buildACPBootstrapTranscript([]store.SessionMessage{
		{Role: "user", Content: "earlier request"},
		{Role: "assistant", Content: "earlier response"},
	}, ACPBootstrapLimits{})
	if err != nil {
		t.Fatal(err)
	}
	boundAt := time.Date(2026, 8, 6, 18, 0, 0, 0, time.UTC)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "continued-turn", UID: types.UID("continued-turn-uid"),
	}}
	verified := &verifiedHarnessV1Execution{
		binding: &corev1alpha1.AgentExecutionBinding{BoundAt: metav1.NewTime(boundAt)},
		body: agentExecutionSnapshotBody{
			Prompt:        "current request",
			Configuration: agentExecutionSnapshotConfig{MaxTurns: 5},
			DefaultTools: &agentExecutionSnapshotToolPolicy{
				AllowedToolsOmitted: false, AllowedTools: []string{}, AllowBash: new(false),
			},
			HarnessV1: &agentExecutionSnapshotHarnessV1{
				SessionName: "continued-session", RuntimeName: string(corev1alpha1.AgentRuntimeCodex),
				SessionBootstrap: &agentExecutionSnapshotHarnessV1SessionBootstrap{
					Artifact: string(bootstrap.Artifact), Digest: bootstrap.Digest,
					MessageCount: bootstrap.MessageCount, TotalMessages: len(bootstrap.Messages),
				},
			},
		},
	}
	attempt := &store.HarnessV1Attempt{
		TaskUID: string(task.UID), Attempt: 1, RuntimeSessionID: "continued-runtime-session",
		TurnID: "continued-turn", CorrelationID: "continued-correlation",
	}

	request, err := buildHarnessV1StartTurnRequest(task, verified, attempt, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPrompt := bootstrapPromptText(bootstrap) + "Orka current user prompt:\ncurrent request"
	if request.Input.Prompt != wantPrompt {
		t.Fatalf("rendered v1 prompt = %q, want %q", request.Input.Prompt, wantPrompt)
	}
	digest, err := harness.CanonicalStartTurnRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	attempt.RequestDigest = digest
	if _, err := buildHarnessV1StartTurnRequest(task, verified, attempt, nil); err != nil {
		t.Fatalf("reconstruct request from frozen Session input: %v", err)
	}

	mutated := *verified
	mutated.body = verified.body
	mutatedHarness := *verified.body.HarnessV1
	mutatedBootstrap := *verified.body.HarnessV1.SessionBootstrap
	mutatedBootstrap.Artifact = strings.ReplaceAll(mutatedBootstrap.Artifact, "earlier response", "later response")
	mutatedHarness.SessionBootstrap = &mutatedBootstrap
	mutated.body.HarnessV1 = &mutatedHarness
	if _, err := buildHarnessV1StartTurnRequest(task, &mutated, attempt, nil); err == nil ||
		!strings.Contains(err.Error(), "does not match the durable attempt") {
		t.Fatalf("mutated frozen Session bootstrap error = %v, want durable digest mismatch", err)
	}
}

func TestDefaultHarnessV1DispatchWorkersMatchesShippedWrapperCapacity(t *testing.T) {
	if DefaultHarnessV1DispatchWorkers != 1 {
		t.Fatalf("default harness v1 dispatch workers = %d, want 1", DefaultHarnessV1DispatchWorkers)
	}
}

func TestHarnessV1DispatcherRejectsParallelWorkers(t *testing.T) {
	dispatcher := &HarnessV1Dispatcher{MaxConcurrent: 2}
	err := dispatcher.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "harness v1 dispatch workers must be exactly 1") {
		t.Fatalf("parallel worker startup error = %v, want exact-one rejection", err)
	}
}

func TestSortHarnessV1DispatchCandidatesPrioritizesRecoveryAndQueueRank(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	lowPriority, highPriority := int32(100), int32(900)
	queuedAt := metav1.NewTime(now.Add(-time.Minute))
	task := func(name string, priority int32) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: name, UID: types.UID(name + "-uid"),
				CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Minute)),
			},
			Spec: corev1alpha1.TaskSpec{Priority: &priority},
			Status: corev1alpha1.TaskStatus{HarnessRuntime: &corev1alpha1.HarnessRuntimeStatus{
				LastTransitionTime: &queuedAt,
			}},
		}
	}
	candidates := []harnessV1DispatchCandidate{
		{task: task("low-prepared", lowPriority), attempt: store.HarnessV1Attempt{State: store.HarnessV1AttemptPrepared}},
		{task: task("high-prepared", highPriority), attempt: store.HarnessV1Attempt{State: store.HarnessV1AttemptPrepared}},
		{task: task("recovery", lowPriority), attempt: store.HarnessV1Attempt{State: store.HarnessV1AttemptAccepted}},
	}

	sortHarnessV1DispatchCandidates(candidates, now)
	want := []string{"recovery", "high-prepared", "low-prepared"}
	for i := range want {
		if got := candidates[i].task.Name; got != want[i] {
			t.Fatalf("candidate[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestSortHarnessV1DispatchCandidatesPromotesMaximumWait(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	lowPriority, highPriority := int32(0), int32(1000)
	oldQueuedAt := metav1.NewTime(now.Add(-DefaultACPQueueMaximumWait))
	recentQueuedAt := metav1.NewTime(now.Add(-time.Minute))
	oldTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "old", UID: "old-uid"},
		Spec:       corev1alpha1.TaskSpec{Priority: &lowPriority},
		Status: corev1alpha1.TaskStatus{HarnessRuntime: &corev1alpha1.HarnessRuntimeStatus{
			LastTransitionTime: &oldQueuedAt,
		}},
	}
	recentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "recent", UID: "recent-uid"},
		Spec:       corev1alpha1.TaskSpec{Priority: &highPriority},
		Status: corev1alpha1.TaskStatus{HarnessRuntime: &corev1alpha1.HarnessRuntimeStatus{
			LastTransitionTime: &recentQueuedAt,
		}},
	}
	candidates := []harnessV1DispatchCandidate{
		{task: recentTask, attempt: store.HarnessV1Attempt{State: store.HarnessV1AttemptPrepared}},
		{task: oldTask, attempt: store.HarnessV1Attempt{State: store.HarnessV1AttemptPrepared}},
	}

	sortHarnessV1DispatchCandidates(candidates, now)
	if got := candidates[0].task.Name; got != "old" {
		t.Fatalf("first candidate = %q, want promoted old Task", got)
	}
}

func seedHarnessV1AttemptEpoch(t *testing.T, attemptStore *sqlite.Store) store.ControllerEpochFence {
	t.Helper()
	epoch, err := attemptStore.CompareAndSwapControllerEpoch(context.Background(), store.ControllerEpochCAS{
		ExpectedVersion: 0,
		ExpectedEpoch:   0,
		NewEpoch:        1,
		HolderID:        "harness-v1-test-controller",
		RequestDigest:   store.CanonicalAgentExecutionSnapshotDigest([]byte("harness-v1-test-epoch")),
		UpdatedAt:       time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("seed harness v1 controller epoch: %v", err)
	}
	return store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
}

func TestRecoverAmbiguousSubmissionUsesLedgerWithoutReplayingStartTurn(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "harness-v1-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	attemptStore := sqlite.NewStore(db, "harness-v1-test")
	fence := seedHarnessV1AttemptEpoch(t, attemptStore)
	attempt := &store.HarnessV1Attempt{
		Namespace: "default", TaskName: "turn-task", TaskUID: "task-uid", Attempt: 1,
		BindingDigest:  "sha256:" + strings.Repeat("a", 64),
		SnapshotDigest: "sha256:" + strings.Repeat("b", 64),
		RequestDigest:  "sha256:" + strings.Repeat("c", 64),
		TurnID:         "turn-a", RuntimeSessionID: "runtime-session-a", CorrelationID: "correlation-a",
		State: store.HarnessV1AttemptPrepared, RetryClass: store.HarnessV1RetryClassNone,
	}
	if err := attemptStore.CreateHarnessV1Attempt(ctx, attempt, fence); err != nil {
		t.Fatal(err)
	}
	attempt, err = attemptStore.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	evidenceErr := errors.New("stop after durable admission recovery")
	dispatcher := &HarnessV1Dispatcher{
		Attempts:   attemptStore,
		EventStore: failingExecutionEventStore{err: evidenceErr},
	}
	attempt, err = dispatcher.transitionAttempt(
		ctx, attempt, fence, store.HarnessV1AttemptSubmitting, "begin-submit", store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	protocolClient := &recordingHarnessV1RecoveryClient{status: &harness.DurableTurnStatus{
		TurnID: attempt.TurnID, TaskUID: attempt.TaskUID, Attempt: attempt.Attempt,
		RequestDigest: attempt.RequestDigest, State: harness.DurableTurnAdmitted,
	}}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: attempt.Namespace, Name: attempt.TaskName, UID: types.UID(attempt.TaskUID),
	}}
	request := harness.StartTurnRequest{TurnID: harness.HarnessTurnID(attempt.TurnID)}

	err = dispatcher.recoverAmbiguousSubmission(ctx, task, nil, protocolClient, request, attempt, fence)
	if !errors.Is(err, evidenceErr) {
		t.Fatalf("recover error = %v, want %v", err, evidenceErr)
	}
	if protocolClient.startCalls != 0 {
		t.Fatalf("StartTurn calls = %d, want zero during ambiguity recovery", protocolClient.startCalls)
	}
	if protocolClient.streamCalls != 0 {
		t.Fatalf("StreamFrames calls = %d, want zero before persisted evidence succeeds", protocolClient.streamCalls)
	}
	persisted, err := attemptStore.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != store.HarnessV1AttemptAccepted {
		t.Fatalf("attempt state = %s, want %s", persisted.State, store.HarnessV1AttemptAccepted)
	}
}

func TestRecoverAmbiguousSubmissionSettlesDurableTerminalReceiptWithoutStreaming(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "harness-v1-terminal-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	attemptStore := sqlite.NewStore(db, "harness-v1-terminal-test")
	fence := seedHarnessV1AttemptEpoch(t, attemptStore)
	attempt := &store.HarnessV1Attempt{
		Namespace: "default", TaskName: "turn-task", TaskUID: "task-uid", Attempt: 1,
		BindingDigest:  "sha256:" + strings.Repeat("a", 64),
		SnapshotDigest: "sha256:" + strings.Repeat("b", 64),
		RequestDigest:  "sha256:" + strings.Repeat("c", 64),
		TurnID:         "turn-a", RuntimeSessionID: "runtime-session-a", CorrelationID: "correlation-a",
		State: store.HarnessV1AttemptPrepared, RetryClass: store.HarnessV1RetryClassNone,
	}
	if err := attemptStore.CreateHarnessV1Attempt(ctx, attempt, fence); err != nil {
		t.Fatal(err)
	}
	attempt, err = attemptStore.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	saveErr := errors.New("stop after durable terminal settlement begins")
	dispatcher := &HarnessV1Dispatcher{
		Attempts:    attemptStore,
		EventStore:  failingExecutionEventStore{err: errors.New("frame evidence must not be read")},
		ResultStore: failingHarnessV1ResultStore{err: saveErr},
	}
	attempt, err = dispatcher.transitionAttempt(
		ctx, attempt, fence, store.HarnessV1AttemptSubmitting, "begin-submit", store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt := harness.DurableTurnTerminalReceipt{
		Version: harness.ProtocolVersion, Kind: harness.DurableTurnTerminalCompleted,
		RuntimeSessionID: harness.RuntimeSessionID(attempt.RuntimeSessionID),
		TurnID:           harness.HarnessTurnID(attempt.TurnID),
		CorrelationID:    attempt.CorrelationID, Seq: 2,
		Completed: &harness.DurableTurnCompletedReceipt{Result: "done", FinalEventSeq: 1},
	}
	receiptDigest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	protocolClient := &recordingHarnessV1RecoveryClient{status: &harness.DurableTurnStatus{
		TurnID: attempt.TurnID, TaskUID: attempt.TaskUID, Attempt: attempt.Attempt,
		RequestDigest: attempt.RequestDigest, State: harness.DurableTurnTerminal,
		TerminalReceiptDigest: receiptDigest, TerminalReceipt: &receipt,
	}}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: attempt.Namespace, Name: attempt.TaskName, UID: types.UID(attempt.TaskUID),
	}}
	request := harness.StartTurnRequest{
		RuntimeSessionID: harness.RuntimeSessionID(attempt.RuntimeSessionID),
		TurnID:           harness.HarnessTurnID(attempt.TurnID),
		CorrelationID:    attempt.CorrelationID,
	}

	err = dispatcher.recoverAmbiguousSubmission(ctx, task, nil, protocolClient, request, attempt, fence)
	if !errors.Is(err, saveErr) {
		t.Fatalf("recover error = %v, want %v", err, saveErr)
	}
	if protocolClient.startCalls != 0 || protocolClient.streamCalls != 0 {
		t.Fatalf("recovery replayed network effects: StartTurn=%d StreamFrames=%d", protocolClient.startCalls, protocolClient.streamCalls)
	}
	persisted, err := attemptStore.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != store.HarnessV1AttemptSettling {
		t.Fatalf("attempt state = %s, want %s", persisted.State, store.HarnessV1AttemptSettling)
	}
	if persisted.TerminalReceiptDigest != receiptDigest {
		t.Fatalf("terminal receipt digest = %q, want %q", persisted.TerminalReceiptDigest, receiptDigest)
	}

	// A controller crash after Settling is durable must recover through the
	// authenticated wrapper ledger again. It must not stream frames or downgrade
	// the already proven terminal result merely because result persistence still
	// needs to be retried.
	err = dispatcher.recoverSettlingAttempt(ctx, task, protocolClient, request, persisted, fence)
	if !errors.Is(err, saveErr) {
		t.Fatalf("second settling recovery error = %v, want %v", err, saveErr)
	}
	if protocolClient.statusCalls != 2 || protocolClient.startCalls != 0 || protocolClient.streamCalls != 0 {
		t.Fatalf(
			"second recovery calls: status=%d StartTurn=%d StreamFrames=%d, want 2/0/0",
			protocolClient.statusCalls, protocolClient.startCalls, protocolClient.streamCalls,
		)
	}
	persisted, err = attemptStore.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != store.HarnessV1AttemptSettling || persisted.TerminalReceiptDigest != receiptDigest {
		t.Fatalf("attempt after second recovery = %+v, want unchanged settling receipt", persisted)
	}

	conflictingReceipt := receipt
	conflictingReceipt.Completed = &harness.DurableTurnCompletedReceipt{Result: "different", FinalEventSeq: 1}
	conflictingDigest, err := harness.DurableTurnTerminalReceiptDigest(conflictingReceipt)
	if err != nil {
		t.Fatal(err)
	}
	protocolClient.status.TerminalReceipt = &conflictingReceipt
	protocolClient.status.TerminalReceiptDigest = conflictingDigest
	err = dispatcher.recoverSettlingAttempt(ctx, task, protocolClient, request, persisted, fence)
	if err == nil || !strings.Contains(err.Error(), "does not match the persisted receipt digest") {
		t.Fatalf("conflicting settling recovery error = %v, want persisted digest mismatch", err)
	}
	persisted, err = attemptStore.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != store.HarnessV1AttemptSettling || persisted.TerminalReceiptDigest != receiptDigest {
		t.Fatalf("attempt after conflicting recovery = %+v, want original settling receipt", persisted)
	}
}

func TestRecoverActiveAttemptSettlesDurableTerminalReceiptWithoutStreaming(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "harness-v1-active-terminal-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	attemptStore := sqlite.NewStore(db, "harness-v1-active-terminal-test")
	fence := seedHarnessV1AttemptEpoch(t, attemptStore)
	attempt := &store.HarnessV1Attempt{
		Namespace: "default", TaskName: "turn-task", TaskUID: "task-uid", Attempt: 1,
		BindingDigest:  "sha256:" + strings.Repeat("a", 64),
		SnapshotDigest: "sha256:" + strings.Repeat("b", 64),
		RequestDigest:  "sha256:" + strings.Repeat("c", 64),
		TurnID:         "turn-a", RuntimeSessionID: "runtime-session-a", CorrelationID: "correlation-a",
		State: store.HarnessV1AttemptPrepared, RetryClass: store.HarnessV1RetryClassNone,
	}
	if err := attemptStore.CreateHarnessV1Attempt(ctx, attempt, fence); err != nil {
		t.Fatal(err)
	}
	attempt, err = attemptStore.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	saveErr := errors.New("stop after active durable terminal settlement begins")
	dispatcher := &HarnessV1Dispatcher{
		Attempts:    attemptStore,
		EventStore:  failingExecutionEventStore{err: errors.New("frame evidence must not be read")},
		ResultStore: failingHarnessV1ResultStore{err: saveErr},
	}
	attempt, err = dispatcher.transitionAttempt(
		ctx, attempt, fence, store.HarnessV1AttemptSubmitting, "begin-submit", store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = dispatcher.transitionAttempt(
		ctx, attempt, fence, store.HarnessV1AttemptAccepted, "accepted", store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt := harness.DurableTurnTerminalReceipt{
		Version: harness.ProtocolVersion, Kind: harness.DurableTurnTerminalCompleted,
		RuntimeSessionID: harness.RuntimeSessionID(attempt.RuntimeSessionID),
		TurnID:           harness.HarnessTurnID(attempt.TurnID),
		CorrelationID:    attempt.CorrelationID, Seq: 2,
		Completed: &harness.DurableTurnCompletedReceipt{Result: "done", FinalEventSeq: 1},
	}
	receiptDigest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	protocolClient := &recordingHarnessV1RecoveryClient{status: &harness.DurableTurnStatus{
		TurnID: attempt.TurnID, TaskUID: attempt.TaskUID, Attempt: attempt.Attempt,
		RequestDigest: attempt.RequestDigest, State: harness.DurableTurnTerminal,
		TerminalReceiptDigest: receiptDigest, TerminalReceipt: &receipt,
	}}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: attempt.Namespace, Name: attempt.TaskName, UID: types.UID(attempt.TaskUID),
	}}
	request := harness.StartTurnRequest{
		RuntimeSessionID: harness.RuntimeSessionID(attempt.RuntimeSessionID),
		TurnID:           harness.HarnessTurnID(attempt.TurnID),
		CorrelationID:    attempt.CorrelationID,
	}

	err = dispatcher.recoverActiveAttempt(ctx, task, nil, protocolClient, request, attempt, fence)
	if !errors.Is(err, saveErr) {
		t.Fatalf("recover error = %v, want %v", err, saveErr)
	}
	if protocolClient.statusCalls != 1 || protocolClient.startCalls != 0 || protocolClient.streamCalls != 0 {
		t.Fatalf(
			"active recovery calls: status=%d StartTurn=%d StreamFrames=%d, want 1/0/0",
			protocolClient.statusCalls, protocolClient.startCalls, protocolClient.streamCalls,
		)
	}
	persisted, err := attemptStore.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != store.HarnessV1AttemptSettling || persisted.TerminalReceiptDigest != receiptDigest {
		t.Fatalf("attempt after active recovery = %+v, want settling terminal receipt", persisted)
	}
}

func TestRecoverAmbiguousSubmissionPrefersPersistedTerminalOverDurableOutcomeUnknown(t *testing.T) {
	fixture := newHarnessV1DispatcherPreparedFixture(t)
	var err error
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx,
		fixture.attempt,
		fixture.fence,
		store.HarnessV1AttemptSubmitting,
		"test-submitting",
		store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx,
		fixture.attempt,
		fixture.fence,
		store.HarnessV1AttemptSubmittedUnknown,
		"test-submission-unknown",
		store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	frame := harnessV1CompletedFrame(fixture.request, "persisted ambiguous result")
	appendPersistedHarnessV1Frame(t, fixture, frame)
	protocolClient := &recordingHarnessV1RecoveryClient{
		status: durableHarnessV1OutcomeUnknownStatus(t, fixture),
	}

	if err := fixture.dispatcher.recoverAmbiguousSubmission(
		fixture.ctx,
		fixture.task,
		fixture.verified,
		protocolClient,
		fixture.request,
		fixture.attempt,
		fixture.fence,
	); err != nil {
		t.Fatalf("recoverAmbiguousSubmission: %v", err)
	}
	persisted := assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	wantReceipt, err := harness.DurableTurnTerminalReceiptFromFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	wantReceiptDigest, err := harness.DurableTurnTerminalReceiptDigest(wantReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TerminalReceiptDigest != wantReceiptDigest {
		t.Fatalf("terminal receipt digest = %q, want persisted frame digest %q", persisted.TerminalReceiptDigest, wantReceiptDigest)
	}
	result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if string(result) != frame.Completed.Result {
		t.Fatalf("result = %q, want %q", result, frame.Completed.Result)
	}
	if protocolClient.statusCalls != 1 || protocolClient.startCalls != 0 || protocolClient.streamCalls != 0 {
		t.Fatalf(
			"ambiguous recovery calls: status=%d StartTurn=%d StreamFrames=%d, want 1/0/0",
			protocolClient.statusCalls, protocolClient.startCalls, protocolClient.streamCalls,
		)
	}
}

func TestRecoverActiveAttemptPrefersPersistedTerminalOverDurableOutcomeUnknown(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	frame := harnessV1CompletedFrame(fixture.request, "persisted active result")
	appendPersistedHarnessV1Frame(t, fixture, frame)
	protocolClient := &recordingHarnessV1RecoveryClient{
		status: durableHarnessV1OutcomeUnknownStatus(t, fixture),
	}

	if err := fixture.dispatcher.recoverActiveAttempt(
		fixture.ctx,
		fixture.task,
		fixture.verified,
		protocolClient,
		fixture.request,
		fixture.attempt,
		fixture.fence,
	); err != nil {
		t.Fatalf("recoverActiveAttempt: %v", err)
	}
	persisted := assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	wantReceipt, err := harness.DurableTurnTerminalReceiptFromFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	wantReceiptDigest, err := harness.DurableTurnTerminalReceiptDigest(wantReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TerminalReceiptDigest != wantReceiptDigest {
		t.Fatalf("terminal receipt digest = %q, want persisted frame digest %q", persisted.TerminalReceiptDigest, wantReceiptDigest)
	}
	result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if string(result) != frame.Completed.Result {
		t.Fatalf("result = %q, want %q", result, frame.Completed.Result)
	}
	if protocolClient.statusCalls != 1 || protocolClient.startCalls != 0 || protocolClient.streamCalls != 0 {
		t.Fatalf(
			"active recovery calls: status=%d StartTurn=%d StreamFrames=%d, want 1/0/0",
			protocolClient.statusCalls, protocolClient.startCalls, protocolClient.streamCalls,
		)
	}
}

func TestHarnessV1DispatcherSettlingRetryUsesPersistedTerminalEvidence(t *testing.T) {
	recoveries := []struct {
		name    string
		fixture func(*testing.T) *harnessV1DispatcherStateFixture
		recover func(*harnessV1DispatcherStateFixture, *recordingHarnessV1RecoveryClient) error
	}{
		{
			name:    "ambiguous",
			fixture: newHarnessV1DispatcherAmbiguousFixture,
			recover: func(f *harnessV1DispatcherStateFixture, c *recordingHarnessV1RecoveryClient) error {
				return f.dispatcher.recoverAmbiguousSubmission(
					f.ctx, f.task, f.verified, c, f.request, f.attempt, f.fence,
				)
			},
		},
		{
			name:    "active",
			fixture: newHarnessV1DispatcherStateFixture,
			recover: func(f *harnessV1DispatcherStateFixture, c *recordingHarnessV1RecoveryClient) error {
				return f.dispatcher.recoverActiveAttempt(
					f.ctx, f.task, f.verified, c, f.request, f.attempt, f.fence,
				)
			},
		},
	}
	failures := []struct {
		name    string
		install func(*harnessV1DispatcherStateFixture, error)
	}{
		{
			name: "save-result",
			install: func(f *harnessV1DispatcherStateFixture, transientErr error) {
				f.dispatcher.ResultStore = &failingOnceHarnessV1ResultStore{
					ResultStore: f.durable,
					err:         transientErr,
				}
			},
		},
		{
			name: "final-cas",
			install: func(f *harnessV1DispatcherStateFixture, transientErr error) {
				f.dispatcher.Attempts = &failingOnceHarnessV1AttemptStore{
					HarnessV1AttemptStore: f.durable,
					target:                store.HarnessV1AttemptSucceeded,
					err:                   transientErr,
				}
			},
		},
	}

	for _, recovery := range recoveries {
		for _, failure := range failures {
			t.Run(recovery.name+"/"+failure.name, func(t *testing.T) {
				fixture := recovery.fixture(t)
				frame := harnessV1CompletedFrame(fixture.request, recovery.name+" "+failure.name)
				appendPersistedHarnessV1Frame(t, fixture, frame)
				protocolClient := &recordingHarnessV1RecoveryClient{
					status: durableHarnessV1OutcomeUnknownStatus(t, fixture),
				}
				transientErr := errors.New("transient " + failure.name)
				failure.install(fixture, transientErr)

				if err := recovery.recover(fixture, protocolClient); !errors.Is(err, transientErr) {
					t.Fatalf("initial recovery error = %v, want %v", err, transientErr)
				}
				persisted := assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSettling)
				receipt, err := harness.DurableTurnTerminalReceiptFromFrame(frame)
				if err != nil {
					t.Fatal(err)
				}
				receiptDigest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if persisted.TerminalReceiptDigest != receiptDigest {
					t.Fatalf("settling digest = %q, want %q", persisted.TerminalReceiptDigest, receiptDigest)
				}

				if err := fixture.dispatcher.recoverSettlingAttempt(
					fixture.ctx,
					fixture.task,
					protocolClient,
					fixture.request,
					persisted,
					fixture.fence,
				); err != nil {
					t.Fatalf("recoverSettlingAttempt: %v", err)
				}
				assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
				result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
				if err != nil {
					t.Fatalf("GetResult: %v", err)
				}
				if string(result) != frame.Completed.Result {
					t.Fatalf("result = %q, want %q", result, frame.Completed.Result)
				}
				if protocolClient.statusCalls != 2 || protocolClient.startCalls != 0 || protocolClient.streamCalls != 0 {
					t.Fatalf(
						"recovery calls: status=%d StartTurn=%d StreamFrames=%d, want 2/0/0",
						protocolClient.statusCalls, protocolClient.startCalls, protocolClient.streamCalls,
					)
				}
			})
		}
	}
}

func TestHarnessV1DispatcherAdmissionClosedRecoveryEventuallyAccepts(t *testing.T) {
	fixture := newHarnessV1DispatcherPreparedFixture(t)
	markHarnessV1AdmissionClosedAttempt(t, fixture)
	protocolClient := &recordingHarnessV1RecoveryClient{
		statusErr: harness.ClientError{StatusCode: http.StatusNotFound, Message: "turn not found"},
	}
	protocolClient.start = func(
		_ context.Context,
		request harness.StartTurnRequest,
	) (*harness.StartTurnResponse, error) {
		if protocolClient.startCalls == 1 {
			return nil, harness.ClientError{StatusCode: http.StatusConflict, Message: "wrapper admission is closed"}
		}
		return &harness.StartTurnResponse{
			Version: harness.ProtocolVersion, Accepted: true,
			RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID,
			CorrelationID: request.CorrelationID,
		}, nil
	}
	protocolClient.stream = func(
		_ context.Context,
		_ harness.HarnessTurnID,
		_ int64,
		onFrame func(harness.HarnessEventFrame) error,
	) error {
		return onFrame(harnessV1CompletedFrame(fixture.request, "accepted after reopen"))
	}

	if err := fixture.dispatcher.recoverAmbiguousSubmission(
		fixture.ctx, fixture.task, fixture.verified, protocolClient, fixture.request, fixture.attempt, fixture.fence,
	); err != nil {
		t.Fatalf("recover while admission remains closed: %v", err)
	}
	persisted := assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSubmittedUnknown)
	if !harnessV1AttemptHasAdmissionClosedProof(persisted) || protocolClient.startCalls != 1 {
		t.Fatalf("closed recovery = attempt %#v StartTurn=%d", persisted, protocolClient.startCalls)
	}
	fixture.attempt = persisted

	if err := fixture.dispatcher.recoverAmbiguousSubmission(
		fixture.ctx, fixture.task, fixture.verified, protocolClient, fixture.request, fixture.attempt, fixture.fence,
	); err != nil {
		t.Fatalf("recover after admission reopened: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	if protocolClient.statusCalls != 2 || protocolClient.startCalls != 2 || protocolClient.streamCalls != 1 {
		t.Fatalf(
			"reopen calls: status=%d StartTurn=%d StreamFrames=%d, want 2/2/1",
			protocolClient.statusCalls, protocolClient.startCalls, protocolClient.streamCalls,
		)
	}
}

func TestHarnessV1DispatcherAdmissionClosedAmbiguousRetryWaitsForLedger(t *testing.T) {
	fixture := newHarnessV1DispatcherPreparedFixture(t)
	markHarnessV1AdmissionClosedAttempt(t, fixture)
	protocolClient := &recordingHarnessV1RecoveryClient{
		statusErr: harness.ClientError{StatusCode: http.StatusNotFound, Message: "turn not found"},
		start: func(context.Context, harness.StartTurnRequest) (*harness.StartTurnResponse, error) {
			return nil, harness.ClientError{RemoteAcceptanceUnknown: true, Message: "response lost"}
		},
	}
	ambiguousErr := fixture.dispatcher.recoverAmbiguousSubmission(
		fixture.ctx, fixture.task, fixture.verified, protocolClient, fixture.request, fixture.attempt, fixture.fence,
	)
	if ambiguousErr == nil {
		t.Fatal("ambiguous StartTurn retry error = nil")
	}
	persisted := assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSubmittedUnknown)
	if !harnessV1AttemptHasAdmissionClosedProof(persisted) || protocolClient.startCalls != 1 || protocolClient.streamCalls != 0 {
		t.Fatalf(
			"ambiguous retry = attempt %#v StartTurn=%d StreamFrames=%d",
			persisted, protocolClient.startCalls, protocolClient.streamCalls,
		)
	}
	fixture.attempt = persisted
	protocolClient.statusErr = nil
	protocolClient.status = &harness.DurableTurnStatus{
		TurnID: fixture.attempt.TurnID, TaskUID: fixture.attempt.TaskUID, Attempt: fixture.attempt.Attempt,
		RequestDigest: fixture.attempt.RequestDigest, State: harness.DurableTurnAccepted,
	}
	protocolClient.stream = func(
		_ context.Context,
		_ harness.HarnessTurnID,
		_ int64,
		onFrame func(harness.HarnessEventFrame) error,
	) error {
		return onFrame(harnessV1CompletedFrame(fixture.request, "accepted despite lost response"))
	}

	if err := fixture.dispatcher.recoverAmbiguousSubmission(
		fixture.ctx, fixture.task, fixture.verified, protocolClient, fixture.request, fixture.attempt, fixture.fence,
	); err != nil {
		t.Fatalf("recover accepted ambiguous retry: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	if protocolClient.startCalls != 1 || protocolClient.statusCalls != 2 || protocolClient.streamCalls != 1 {
		t.Fatalf(
			"ambiguous recovery calls: StartTurn=%d status=%d StreamFrames=%d, want 1/2/1",
			protocolClient.startCalls, protocolClient.statusCalls, protocolClient.streamCalls,
		)
	}
}

func TestHarnessV1DispatcherPreparedSnapshotDispatchSucceeds(t *testing.T) {
	fixture := newHarnessV1DispatcherPreparedFixture(t)
	fixture.dispatcher.Epochs = readyHarnessV1TestEpochManager(fixture.durable, fixture.fence)
	var acceptedRequest harness.StartTurnRequest
	protocolClient := &recordingHarnessV1RecoveryClient{}
	protocolClient.start = func(
		_ context.Context,
		request harness.StartTurnRequest,
	) (*harness.StartTurnResponse, error) {
		acceptedRequest = request
		return &harness.StartTurnResponse{
			Version: harness.ProtocolVersion, Accepted: true,
			RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID,
			CorrelationID: request.CorrelationID,
		}, nil
	}
	protocolClient.stream = func(
		_ context.Context,
		_ harness.HarnessTurnID,
		_ int64,
		onFrame func(harness.HarnessEventFrame) error,
	) error {
		return onFrame(harnessV1CompletedFrame(acceptedRequest, "full dispatch"))
	}
	fixture.dispatcher.clientFactory = func(string, string, *http.Client) (harnessV1ProtocolClient, error) {
		return protocolClient, nil
	}

	if err := fixture.dispatcher.reconcileAttempt(fixture.ctx, fixture.task, fixture.attempt); err != nil {
		t.Fatalf("reconcileAttempt: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if string(result) != "full dispatch" {
		t.Fatalf("result = %q, want full dispatch", result)
	}
	if protocolClient.startCalls != 1 || protocolClient.streamCalls != 1 {
		t.Fatalf("network calls: start=%d stream=%d, want 1/1", protocolClient.startCalls, protocolClient.streamCalls)
	}
}

func TestHarnessV1DispatcherRejectsNonIncreasingFrameSequences(t *testing.T) {
	for _, test := range []struct {
		name string
		seqs []int64
	}{
		{name: "decrease", seqs: []int64{1, 3, 2}},
		{name: "duplicate", seqs: []int64{1, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHarnessV1DispatcherStateFixture(t)
			protocolClient := &recordingHarnessV1RecoveryClient{
				stream: func(
					_ context.Context,
					_ harness.HarnessTurnID,
					_ int64,
					onFrame func(harness.HarnessEventFrame) error,
				) error {
					for _, seq := range test.seqs {
						if err := onFrame(harness.HarnessEventFrame{
							Version: harness.ProtocolVersion, Type: harness.FrameRuntimeOutput,
							RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
							CorrelationID: fixture.request.CorrelationID, Seq: seq, ContentText: "progress",
						}); err != nil {
							return err
						}
					}
					return nil
				},
			}

			if err := fixture.dispatcher.streamAcceptedAttempt(
				fixture.ctx,
				fixture.task,
				fixture.verified,
				protocolClient,
				fixture.request,
				fixture.attempt,
				fixture.fence,
			); err != nil {
				t.Fatalf("streamAcceptedAttempt: %v", err)
			}
			persisted := assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptOutcomeUnknown)
			if persisted.TerminalReason != harnessV1ReasonProtocolViolation {
				t.Fatalf("terminal reason = %q, want %q", persisted.TerminalReason, harnessV1ReasonProtocolViolation)
			}
		})
	}
}

func TestHarnessV1DispatcherClassifiesDeterministicFrameAuthorityViolations(t *testing.T) {
	tests := []struct {
		name  string
		frame func(harness.StartTurnRequest) harness.HarnessEventFrame
	}{
		{
			name: "identity mismatch",
			frame: func(request harness.StartTurnRequest) harness.HarnessEventFrame {
				return harness.HarnessEventFrame{
					Version: harness.ProtocolVersion, Type: harness.FrameRuntimeOutput,
					RuntimeSessionID: request.RuntimeSessionID, TurnID: "different-turn",
					CorrelationID: request.CorrelationID, Seq: 1, ContentText: "progress",
				}
			},
		},
		{
			name: "approval request",
			frame: func(request harness.StartTurnRequest) harness.HarnessEventFrame {
				return harness.HarnessEventFrame{
					Version: harness.ProtocolVersion, Type: harness.FrameApprovalRequested,
					RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID,
					CorrelationID: request.CorrelationID, Seq: 1,
				}
			},
		},
		{
			name: "unauthorized continuation",
			frame: func(request harness.StartTurnRequest) harness.HarnessEventFrame {
				return harness.HarnessEventFrame{
					Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested,
					RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID,
					CorrelationID: request.CorrelationID, Seq: 1,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHarnessV1DispatcherStateFixture(t)
			protocolClient := &recordingHarnessV1RecoveryClient{stream: func(
				_ context.Context,
				_ harness.HarnessTurnID,
				_ int64,
				onFrame func(harness.HarnessEventFrame) error,
			) error {
				return onFrame(test.frame(fixture.request))
			}}

			if err := fixture.dispatcher.streamAcceptedAttempt(
				fixture.ctx, fixture.task, fixture.verified, protocolClient,
				fixture.request, fixture.attempt, fixture.fence,
			); err != nil {
				t.Fatalf("streamAcceptedAttempt: %v", err)
			}
			persisted := assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptOutcomeUnknown)
			if persisted.TerminalReason != harnessV1ReasonProtocolViolation {
				t.Fatalf("terminal reason = %q, want %q", persisted.TerminalReason, harnessV1ReasonProtocolViolation)
			}
		})
	}
}

func TestHarnessV1DispatcherTerminalFrameWinsOverTrailingStreamError(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	trailingErr := errors.New("stream disconnected after terminal frame")
	protocolClient := &recordingHarnessV1RecoveryClient{
		stream: func(
			_ context.Context,
			_ harness.HarnessTurnID,
			_ int64,
			onFrame func(harness.HarnessEventFrame) error,
		) error {
			callbackErr := onFrame(harnessV1CompletedFrame(fixture.request, "done"))
			if !errors.Is(callbackErr, errHarnessV1TerminalFrame) {
				t.Fatalf("terminal frame callback error = %v, want %v", callbackErr, errHarnessV1TerminalFrame)
			}
			return trailingErr
		},
	}

	if err := fixture.dispatcher.streamAcceptedAttempt(
		fixture.ctx,
		fixture.task,
		fixture.verified,
		protocolClient,
		fixture.request,
		fixture.attempt,
		fixture.fence,
	); err != nil {
		t.Fatalf("streamAcceptedAttempt: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if string(result) != "done" {
		t.Fatalf("result = %q, want done", result)
	}
	if protocolClient.streamCalls != 1 || protocolClient.startCalls != 0 {
		t.Fatalf("network calls: stream=%d start=%d, want 1/0", protocolClient.streamCalls, protocolClient.startCalls)
	}
}

func TestHarnessV1DispatcherBoundedStreamPollAllowsCancellationFollowUp(t *testing.T) {
	tests := []struct {
		name       string
		deleteTask bool
		mutate     func(*corev1alpha1.Task)
	}{
		{
			name: "cancelled Task",
			mutate: func(task *corev1alpha1.Task) {
				task.Status.Phase = corev1alpha1.TaskPhaseCancelled
			},
		},
		{
			name:       "deleting Task",
			deleteTask: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHarnessV1DispatcherStateFixture(t)
			fixture.dispatcher.Interval = 10 * time.Millisecond
			fixture.dispatcher.Epochs = readyHarnessV1TestEpochManager(fixture.durable, fixture.fence)
			protocolClient := &recordingHarnessV1RecoveryClient{}
			protocolClient.cancel = func(ctx context.Context, request harness.CancelTurnRequest) (*harness.CancelTurnResponse, error) {
				if err := ctx.Err(); err != nil {
					t.Fatalf("CancelTurn context already ended: %v", err)
				}
				if request.TurnID != fixture.request.TurnID || request.RuntimeSessionID != fixture.request.RuntimeSessionID {
					t.Fatalf("CancelTurn request = %#v, want exact accepted turn", request)
				}
				return &harness.CancelTurnResponse{Accepted: true}, nil
			}
			protocolClient.stream = func(
				ctx context.Context,
				_ harness.HarnessTurnID,
				_ int64,
				onFrame func(harness.HarnessEventFrame) error,
			) error {
				if protocolClient.cancelCalls == 0 {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 100*time.Millisecond {
						t.Fatalf("accepted stream context deadline = %v, %t; want bounded poll", deadline, ok)
					}
					<-ctx.Done()
					return ctx.Err()
				}
				return onFrame(harness.HarnessEventFrame{
					Version: harness.ProtocolVersion, Type: harness.FrameTurnCancelled,
					RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
					CorrelationID: fixture.request.CorrelationID, Seq: 1,
				})
			}
			fixture.dispatcher.clientFactory = func(string, string, *http.Client) (harnessV1ProtocolClient, error) {
				return protocolClient, nil
			}

			managerCtx, cancelManager := context.WithTimeout(context.Background(), time.Second)
			defer cancelManager()
			if err := fixture.dispatcher.streamAcceptedAttempt(
				managerCtx,
				fixture.task,
				fixture.verified,
				protocolClient,
				fixture.request,
				fixture.attempt,
				fixture.fence,
			); err != nil {
				t.Fatalf("bounded accepted stream poll: %v", err)
			}
			if protocolClient.streamCalls != 1 || protocolClient.cancelCalls != 0 {
				t.Fatalf(
					"first pass calls: stream=%d cancel=%d, want 1/0",
					protocolClient.streamCalls,
					protocolClient.cancelCalls,
				)
			}

			current := &corev1alpha1.Task{}
			key := types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name}
			if err := fixture.dispatcher.Client.Get(fixture.ctx, key, current); err != nil {
				t.Fatal(err)
			}
			var err error
			if test.deleteTask {
				err = fixture.dispatcher.Client.Delete(fixture.ctx, current)
			} else {
				test.mutate(current)
				err = fixture.dispatcher.Client.Status().Update(fixture.ctx, current)
			}
			if err != nil {
				t.Fatalf("persist %s intent: %v", test.name, err)
			}
			if err := fixture.dispatcher.Client.Get(fixture.ctx, key, current); err != nil {
				t.Fatal(err)
			}
			fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptAccepted)

			if err := fixture.dispatcher.reconcileAttempt(fixture.ctx, current, fixture.attempt); err != nil {
				t.Fatalf("cancellation follow-up reconcile: %v", err)
			}
			assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptCancelled)
			if protocolClient.cancelCalls != 1 || protocolClient.streamCalls != 2 || protocolClient.startCalls != 0 {
				t.Fatalf(
					"follow-up calls: cancel=%d stream=%d start=%d, want 1/2/0",
					protocolClient.cancelCalls,
					protocolClient.streamCalls,
					protocolClient.startCalls,
				)
			}
		})
	}
}

func TestHarnessV1DispatcherDeadlineDurablyRequestsAndRetriesCancellation(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	fixture.dispatcher.Interval = time.Second
	fixture.request.Deadline = time.Now().UTC().Add(100 * time.Millisecond)
	protocolClient := &recordingHarnessV1RecoveryClient{}
	protocolClient.cancel = func(_ context.Context, request harness.CancelTurnRequest) (*harness.CancelTurnResponse, error) {
		if request.Reason != "Task deadline exceeded" || request.TurnID != fixture.request.TurnID {
			t.Fatalf("deadline cancellation = %#v", request)
		}
		return &harness.CancelTurnResponse{Accepted: true}, nil
	}
	protocolClient.stream = func(
		ctx context.Context,
		_ harness.HarnessTurnID,
		_ int64,
		onFrame func(harness.HarnessEventFrame) error,
	) error {
		if protocolClient.streamCalls == 1 {
			deadline, ok := ctx.Deadline()
			if !ok || deadline.After(fixture.request.Deadline.Add(10*time.Millisecond)) {
				t.Fatalf("first stream deadline = %v, %t; want immutable request deadline %v", deadline, ok, fixture.request.Deadline)
			}
			<-ctx.Done()
			return ctx.Err()
		}
		return onFrame(harness.HarnessEventFrame{
			Version: harness.ProtocolVersion, Type: harness.FrameTurnCancelled,
			RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
			CorrelationID: fixture.request.CorrelationID, Seq: 1,
		})
	}

	if err := fixture.dispatcher.streamAcceptedAttempt(
		fixture.ctx, fixture.task, fixture.verified, protocolClient,
		fixture.request, fixture.attempt, fixture.fence,
	); err != nil {
		t.Fatalf("deadline stream poll: %v", err)
	}
	fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptCancelRequested)
	if fixture.attempt.CancelRequestedAt == nil || protocolClient.cancelCalls != 1 || protocolClient.streamCalls != 1 {
		t.Fatalf("deadline boundary = attempt %+v cancel=%d stream=%d", fixture.attempt, protocolClient.cancelCalls, protocolClient.streamCalls)
	}

	if err := fixture.dispatcher.streamAcceptedAttempt(
		fixture.ctx, fixture.task, fixture.verified, protocolClient,
		fixture.request, fixture.attempt, fixture.fence,
	); err != nil {
		t.Fatalf("deadline cancellation retry: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptCancelled)
	if protocolClient.cancelCalls != 2 || protocolClient.streamCalls != 2 {
		t.Fatalf("deadline cancellation calls = cancel=%d stream=%d, want 2/2", protocolClient.cancelCalls, protocolClient.streamCalls)
	}
}

func TestHarnessV1DispatcherExpiredDeadlineCancellationWindowBecomesOutcomeUnknown(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	now := time.Now().UTC()
	requestedAt := now.Add(-defaultHarnessV1CancellationSettlementTimeout - time.Second)
	fixture.request.Deadline = requestedAt.Add(-time.Second)
	var err error
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx, fixture.attempt, fixture.fence,
		store.HarnessV1AttemptCancelRequested, "test-expired-deadline-cancellation",
		store.HarnessV1AttemptUpdates{CancelRequestedAt: &requestedAt},
	)
	if err != nil {
		t.Fatal(err)
	}
	protocolClient := &recordingHarnessV1RecoveryClient{status: &harness.DurableTurnStatus{
		TurnID: fixture.attempt.TurnID, TaskUID: fixture.attempt.TaskUID,
		Attempt: fixture.attempt.Attempt, RequestDigest: fixture.attempt.RequestDigest,
		State: harness.DurableTurnAccepted,
	}}
	protocolClient.cancel = func(context.Context, harness.CancelTurnRequest) (*harness.CancelTurnResponse, error) {
		t.Fatal("CancelTurn was retried after the bounded settlement window")
		return nil, nil
	}
	protocolClient.stream = func(context.Context, harness.HarnessTurnID, int64, func(harness.HarnessEventFrame) error) error {
		t.Fatal("frames were streamed after the bounded settlement window")
		return nil
	}

	if err := fixture.dispatcher.recoverActiveAttempt(
		fixture.ctx, fixture.task, fixture.verified, protocolClient,
		fixture.request, fixture.attempt, fixture.fence,
	); err != nil {
		t.Fatalf("recover expired deadline cancellation: %v", err)
	}
	persisted := assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptOutcomeUnknown)
	if persisted.TerminalReason != harnessV1ReasonOutcomeUnknown {
		t.Fatalf("terminal reason = %q, want %q", persisted.TerminalReason, harnessV1ReasonOutcomeUnknown)
	}
	if protocolClient.statusCalls != 1 || protocolClient.cancelCalls != 0 || protocolClient.streamCalls != 0 {
		t.Fatalf("expired cancellation calls = status=%d cancel=%d stream=%d, want 1/0/0", protocolClient.statusCalls, protocolClient.cancelCalls, protocolClient.streamCalls)
	}
}

func TestHarnessV1DispatcherSettlingRecoveryFinalizesDurableDecision(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	frame := harnessV1CompletedFrame(fixture.request, "durably recovered")
	receipt, err := harness.DurableTurnTerminalReceiptFromFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	receiptDigest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx,
		fixture.attempt,
		fixture.fence,
		store.HarnessV1AttemptSettling,
		"test-settling",
		store.HarnessV1AttemptUpdates{TerminalReceiptDigest: &receiptDigest},
	)
	if err != nil {
		t.Fatal(err)
	}
	protocolClient := &recordingHarnessV1RecoveryClient{status: &harness.DurableTurnStatus{
		TurnID: fixture.attempt.TurnID, TaskUID: fixture.attempt.TaskUID, Attempt: fixture.attempt.Attempt,
		RequestDigest: fixture.attempt.RequestDigest, State: harness.DurableTurnTerminal,
		TerminalReceiptDigest: receiptDigest, TerminalReceipt: &receipt,
	}}

	if err := fixture.dispatcher.recoverSettlingAttempt(
		fixture.ctx,
		fixture.task,
		protocolClient,
		fixture.request,
		fixture.attempt,
		fixture.fence,
	); err != nil {
		t.Fatalf("recoverSettlingAttempt: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if string(result) != "durably recovered" {
		t.Fatalf("result = %q, want durably recovered", result)
	}
	if protocolClient.statusCalls != 1 || protocolClient.streamCalls != 0 || protocolClient.startCalls != 0 {
		t.Fatalf(
			"recovery calls: status=%d stream=%d start=%d, want 1/0/0",
			protocolClient.statusCalls,
			protocolClient.streamCalls,
			protocolClient.startCalls,
		)
	}
}

func TestHarnessV1DispatcherReconnectsAcceptedTurnAfterTransientStreamFailure(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	protocolClient := &recordingHarnessV1RecoveryClient{
		status: &harness.DurableTurnStatus{
			TurnID: fixture.attempt.TurnID, TaskUID: fixture.attempt.TaskUID,
			Attempt: fixture.attempt.Attempt, RequestDigest: fixture.attempt.RequestDigest,
			State: harness.DurableTurnAccepted,
		},
	}
	protocolClient.stream = func(
		_ context.Context,
		_ harness.HarnessTurnID,
		_ int64,
		onFrame func(harness.HarnessEventFrame) error,
	) error {
		if protocolClient.streamCalls == 1 {
			return errors.New("transient stream reset")
		}
		return onFrame(harnessV1CompletedFrame(fixture.request, "reconnected"))
	}

	err := fixture.dispatcher.recoverActiveAttempt(
		fixture.ctx,
		fixture.task,
		fixture.verified,
		protocolClient,
		fixture.request,
		fixture.attempt,
		fixture.fence,
	)
	if !errors.Is(err, errHarnessV1StreamEndedWithoutTerminal) {
		t.Fatalf("first recoverActiveAttempt error = %v, want retryable stream boundary", err)
	}
	fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptAccepted)

	if err := fixture.dispatcher.recoverActiveAttempt(
		fixture.ctx,
		fixture.task,
		fixture.verified,
		protocolClient,
		fixture.request,
		fixture.attempt,
		fixture.fence,
	); err != nil {
		t.Fatalf("second recoverActiveAttempt: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "reconnected" {
		t.Fatalf("result = %q, want reconnected", result)
	}
	if protocolClient.statusCalls != 2 || protocolClient.streamCalls != 2 || protocolClient.startCalls != 0 {
		t.Fatalf(
			"recovery calls: status=%d stream=%d start=%d, want 2/2/0",
			protocolClient.statusCalls,
			protocolClient.streamCalls,
			protocolClient.startCalls,
		)
	}
}

func TestHarnessV1DispatcherRetriesCancellationUntilAcknowledged(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	protocolClient := &recordingHarnessV1RecoveryClient{}
	protocolClient.cancel = func(context.Context, harness.CancelTurnRequest) (*harness.CancelTurnResponse, error) {
		if protocolClient.cancelCalls == 1 {
			return nil, errors.New("transient cancellation transport failure")
		}
		return &harness.CancelTurnResponse{Accepted: true}, nil
	}
	protocolClient.stream = func(
		_ context.Context,
		_ harness.HarnessTurnID,
		_ int64,
		onFrame func(harness.HarnessEventFrame) error,
	) error {
		persisted, err := fixture.durable.GetHarnessV1Attempt(fixture.ctx, harnessV1AttemptKey(fixture.attempt))
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != store.HarnessV1AttemptCancelRequested || persisted.CancelRequestedAt == nil {
			t.Fatalf("attempt at stream boundary = %+v, want nonterminal CancelRequested", persisted)
		}
		return onFrame(harness.HarnessEventFrame{
			Version: harness.ProtocolVersion, Type: harness.FrameTurnCancelled,
			RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
			CorrelationID: fixture.request.CorrelationID, Seq: 1,
		})
	}
	fixture.dispatcher.clientFactory = func(string, string, *http.Client) (harnessV1ProtocolClient, error) {
		return protocolClient, nil
	}

	err := fixture.dispatcher.cancelAttempt(
		fixture.ctx,
		fixture.task,
		fixture.verified,
		fixture.attempt,
		fixture.fence,
	)
	if err == nil || !strings.Contains(err.Error(), "transient cancellation transport failure") {
		t.Fatalf("first cancelAttempt error = %v, want transient transport failure", err)
	}
	fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptCancelRequested)
	if protocolClient.cancelCalls != 1 || protocolClient.streamCalls != 0 {
		t.Fatalf("first cancellation calls: cancel=%d stream=%d, want 1/0", protocolClient.cancelCalls, protocolClient.streamCalls)
	}

	if err := fixture.dispatcher.cancelAttempt(
		fixture.ctx,
		fixture.task,
		fixture.verified,
		fixture.attempt,
		fixture.fence,
	); err != nil {
		t.Fatalf("second cancelAttempt: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptCancelled)
	if protocolClient.cancelCalls != 2 || protocolClient.streamCalls != 1 || protocolClient.startCalls != 0 {
		t.Fatalf(
			"cancellation calls: cancel=%d stream=%d start=%d, want 2/1/0",
			protocolClient.cancelCalls,
			protocolClient.streamCalls,
			protocolClient.startCalls,
		)
	}
}

func TestHarnessV1DispatcherCancellationSkipsPersistedAndLiveBrokeredToolCalls(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixture.task.Namespace, Name: "lookup", UID: types.UID("lookup-tool-uid"), Generation: 1,
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "look up a value", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP: &corev1alpha1.HTTPExecution{URL: "https://tools.example.test/lookup", Method: http.MethodPost},
		},
	}
	if err := fixture.dispatcher.Client.Create(fixture.ctx, tool); err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Tool{}
	if err := fixture.dispatcher.APIReader.Get(fixture.ctx, client.ObjectKeyFromObject(tool), current); err != nil {
		t.Fatal(err)
	}
	digest, err := harnessV1BrokeredToolDefinitionDigest(current)
	if err != nil {
		t.Fatal(err)
	}
	fixture.verified.body.HarnessV1.ToolExecutionMode = string(harness.ToolExecutionModeBrokered)
	fixture.verified.body.HarnessV1.BrokeredToolClasses = []corev1alpha1.AgentRuntimeBrokeredToolClass{
		corev1alpha1.AgentRuntimeBrokeredToolClassRead,
	}
	fixture.verified.body.HarnessV1.BrokeredTools = []agentExecutionSnapshotHarnessV1BrokeredTool{{
		Name: current.Name, Description: current.Spec.Description, BrokeredClass: current.Spec.BrokeredToolClass,
		Parameters:       json.RawMessage(`{"type":"object","additionalProperties":true}`),
		UID:              string(current.UID),
		Generation:       current.Generation,
		DefinitionDigest: digest,
	}}
	brokeredFrame := func(seq int64, callID string) harness.HarnessEventFrame {
		t.Helper()
		call := harness.ToolCallRequest{
			Version: harness.ProtocolVersion, RuntimeSessionID: fixture.request.RuntimeSessionID,
			TurnID: fixture.request.TurnID, ToolCallID: callID, ToolName: current.Name,
			Input: json.RawMessage(`{"query":"value"}`),
		}
		call.IdempotencyKey = harness.ToolRequestIdempotencyKey(call.RuntimeSessionID, call.TurnID, call.ToolCallID)
		content, marshalErr := json.Marshal(call)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return harness.HarnessEventFrame{
			Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested,
			RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
			CorrelationID: fixture.request.CorrelationID, Seq: seq,
			ToolName: current.Name, ToolCallID: callID, Content: content,
		}
	}
	persistedFrame := brokeredFrame(1, "persisted-call")
	liveFrame := brokeredFrame(2, "live-call")
	appendPersistedHarnessV1Frame(t, fixture, persistedFrame)
	evidence, err := fixture.dispatcher.persistedEvidence(fixture.ctx, fixture.task, fixture.attempt)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.brokeredToolCalls) != 1 || evidence.brokeredToolCalls[0].Seq != persistedFrame.Seq {
		t.Fatalf("persisted brokered evidence = %#v, want sequence %d", evidence, persistedFrame.Seq)
	}

	now := time.Now().UTC()
	seededIdentity := harnessV1BrokeredToolEffectIdentity(
		fixture.task, fixture.request, persistedFrame.ToolCallID,
	)
	seededEffect, err := fixture.durable.ReserveExternalEffect(fixture.ctx, store.ReserveExternalEffectRequest{
		Identity: seededIdentity, RequestDigest: "sha256:" + strings.Repeat("c", 64),
		Fence: fixture.fence, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseExpiry := now.Add(time.Minute)
	seededEffect, err = fixture.durable.TransitionExternalEffect(fixture.ctx, store.ExternalEffectTransition{
		ID: seededEffect.ID, Fence: fixture.fence, ExpectedVersion: seededEffect.Version,
		ExpectedState: seededEffect.State, NewState: store.ExternalEffectInFlight,
		RequestDigest: seededEffect.RequestDigest, LeaseOwner: "seeded-harness-v1-tool-call",
		LeaseExpiresAt: &leaseExpiry, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx, fixture.attempt, fixture.fence,
		store.HarnessV1AttemptCancelRequested, "test-cancel-requested",
		store.HarnessV1AttemptUpdates{CancelRequestedAt: &now},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.dispatcher.Client.Delete(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}
	executeCalls := 0
	fixture.dispatcher.ExternalEffects = fixture.durable
	fixture.dispatcher.BrokeredToolExecutor = HarnessV1BrokeredToolExecutorFunc(func(
		context.Context, string, *corev1alpha1.Tool, harness.ToolCallRequest,
	) (json.RawMessage, error) {
		executeCalls++
		return json.RawMessage(`{"unexpected":true}`), nil
	})
	protocolClient := &recordingHarnessV1RecoveryClient{cancel: func(
		context.Context,
		harness.CancelTurnRequest,
	) (*harness.CancelTurnResponse, error) {
		return &harness.CancelTurnResponse{Accepted: true}, nil
	}, stream: func(
		_ context.Context,
		_ harness.HarnessTurnID,
		afterSeq int64,
		onFrame func(harness.HarnessEventFrame) error,
	) error {
		if afterSeq != 0 {
			t.Fatalf("stream after sequence = %d, want 0 while CancelRequested progress remains journal-backed", afterSeq)
		}
		if err := onFrame(persistedFrame); err != nil {
			return err
		}
		if err := onFrame(liveFrame); err != nil {
			return err
		}
		return onFrame(harness.HarnessEventFrame{
			Version: harness.ProtocolVersion, Type: harness.FrameTurnCancelled,
			RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
			CorrelationID: fixture.request.CorrelationID, Seq: 3,
		})
	}}

	if err := fixture.dispatcher.streamAcceptedAttempt(
		fixture.ctx, fixture.task, fixture.verified, protocolClient,
		fixture.request, fixture.attempt, fixture.fence,
	); err != nil {
		t.Fatalf("stream cancelled brokered attempt: %v", err)
	}
	assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptCancelled)
	if executeCalls != 0 || protocolClient.continueCalls != 0 {
		t.Fatalf(
			"cancelled brokered attempt calls = execute=%d continue=%d, want 0/0",
			executeCalls, protocolClient.continueCalls,
		)
	}
	settledEffect, err := fixture.durable.GetExternalEffect(fixture.ctx, seededEffect.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settledEffect.State != store.ExternalEffectOutcomeUnknown || settledEffect.LeaseOwner != "" ||
		settledEffect.LeaseExpiresAt != nil {
		t.Fatalf("settled cancelled brokered effect = %#v, want terminal OutcomeUnknown without a lease", settledEffect)
	}
	liveIdentity := harnessV1BrokeredToolEffectIdentity(fixture.task, fixture.request, liveFrame.ToolCallID)
	liveEffectID, err := liveIdentity.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.durable.GetExternalEffect(fixture.ctx, liveEffectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("live cancelled brokered call external effect error = %v, want ErrNotFound", err)
	}
}

func TestHarnessV1DispatcherRetriesOutputAcknowledgementBeforeProjection(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	frame := harnessV1CompletedFrame(fixture.request, "")
	frame.Completed.OutputRef = "durable-output-v1"
	appendPersistedHarnessV1Frame(t, fixture, frame)
	ackErr := errors.New("lost output acknowledgement response")
	protocolClient := &recordingHarnessV1RecoveryClient{
		fetch: func(context.Context, harness.HarnessTurnID, string) ([]byte, error) {
			return []byte("durable result"), nil
		},
	}
	protocolClient.acknowledge = func(_ context.Context, request harness.TurnOutputAcknowledgementRequest) error {
		persisted, err := fixture.durable.GetHarnessV1Attempt(fixture.ctx, harnessV1AttemptKey(fixture.attempt))
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != store.HarnessV1AttemptSucceeded || persisted.TerminalReceiptDigest != request.TerminalReceiptDigest {
			t.Fatalf("attempt at acknowledgement boundary = %+v, want durable Succeeded receipt", persisted)
		}
		result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
		if err != nil || string(result) != "durable result" {
			t.Fatalf("result at acknowledgement boundary = %q, %v", result, err)
		}
		if protocolClient.ackCalls == 1 {
			return ackErr
		}
		return nil
	}
	protocolClient.settle = func(_ context.Context, request harness.TurnSettlementAcknowledgementRequest) error {
		if request.TurnID != fixture.request.TurnID || request.RequestDigest != fixture.attempt.RequestDigest ||
			request.TerminalReceiptDigest == "" {
			t.Fatalf("settlement acknowledgement = %+v, want exact terminal attempt fence", request)
		}
		return nil
	}
	fixture.dispatcher.clientFactory = func(string, string, *http.Client) (harnessV1ProtocolClient, error) {
		return protocolClient, nil
	}

	err := fixture.dispatcher.settleTerminalFrame(
		fixture.ctx, fixture.task, protocolClient, fixture.request, fixture.attempt, fixture.fence, frame,
	)
	if !errors.Is(err, ackErr) {
		t.Fatalf("settleTerminalFrame error = %v, want lost acknowledgement response", err)
	}
	fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	latest := &corev1alpha1.Task{}
	if err := fixture.dispatcher.Client.Get(
		fixture.ctx,
		types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name},
		latest,
	); err != nil {
		t.Fatal(err)
	}
	if harnessV1AttemptProjectionMatches(latest, fixture.attempt) {
		t.Fatal("terminal attempt was projected before output acknowledgement")
	}

	fixture.dispatcher.Epochs = readyHarnessV1TestEpochManager(fixture.durable, fixture.fence)
	if err := fixture.dispatcher.reconcileAttempt(fixture.ctx, latest, fixture.attempt); err != nil {
		t.Fatalf("reconcile terminal acknowledgement retry: %v", err)
	}
	if err := fixture.dispatcher.Client.Get(
		fixture.ctx,
		types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name},
		latest,
	); err != nil {
		t.Fatal(err)
	}
	if !harnessV1AttemptProjectionMatches(latest, fixture.attempt) {
		t.Fatalf("terminal attempt was not projected after acknowledgement: %+v", latest.Status.HarnessRuntime)
	}
	if protocolClient.fetchCalls != 1 || protocolClient.ackCalls != 2 ||
		protocolClient.settleCalls != 1 || protocolClient.statusCalls != 0 {
		t.Fatalf(
			"output recovery calls: fetch=%d outputAck=%d settlementAck=%d status=%d, want 1/2/1/0",
			protocolClient.fetchCalls, protocolClient.ackCalls,
			protocolClient.settleCalls, protocolClient.statusCalls,
		)
	}
}

func TestHarnessV1DispatcherRetriesSettlementAcknowledgementBeforeProjection(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	frame := harnessV1CompletedFrame(fixture.request, "durable result")
	appendPersistedHarnessV1Frame(t, fixture, frame)
	ackErr := errors.New("lost settlement acknowledgement response")
	protocolClient := &recordingHarnessV1RecoveryClient{}
	protocolClient.settle = func(_ context.Context, request harness.TurnSettlementAcknowledgementRequest) error {
		persisted, err := fixture.durable.GetHarnessV1Attempt(fixture.ctx, harnessV1AttemptKey(fixture.attempt))
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State != store.HarnessV1AttemptSucceeded || persisted.TerminalReceiptDigest == "" ||
			request.TurnID != fixture.request.TurnID || request.RequestDigest != persisted.RequestDigest ||
			request.TerminalReceiptDigest != persisted.TerminalReceiptDigest {
			t.Fatalf("settlement acknowledgement boundary = attempt %+v request %+v", persisted, request)
		}
		if protocolClient.settleCalls == 1 {
			return ackErr
		}
		return nil
	}
	fixture.dispatcher.clientFactory = func(string, string, *http.Client) (harnessV1ProtocolClient, error) {
		return protocolClient, nil
	}

	err := fixture.dispatcher.settleTerminalFrame(
		fixture.ctx, fixture.task, protocolClient, fixture.request, fixture.attempt, fixture.fence, frame,
	)
	if !errors.Is(err, ackErr) {
		t.Fatalf("settleTerminalFrame error = %v, want lost settlement acknowledgement", err)
	}
	fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSucceeded)
	latest := &corev1alpha1.Task{}
	key := types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name}
	if err := fixture.dispatcher.Client.Get(fixture.ctx, key, latest); err != nil {
		t.Fatal(err)
	}
	if harnessV1AttemptProjectionMatches(latest, fixture.attempt) {
		t.Fatal("terminal attempt was projected before settlement acknowledgement")
	}

	fixture.dispatcher.Epochs = readyHarnessV1TestEpochManager(fixture.durable, fixture.fence)
	if err := fixture.dispatcher.reconcileAttempt(fixture.ctx, latest, fixture.attempt); err != nil {
		t.Fatalf("reconcile settlement acknowledgement retry: %v", err)
	}
	if err := fixture.dispatcher.Client.Get(fixture.ctx, key, latest); err != nil {
		t.Fatal(err)
	}
	if !harnessV1AttemptProjectionMatches(latest, fixture.attempt) {
		t.Fatalf("terminal attempt was not projected after settlement acknowledgement: %+v", latest.Status.HarnessRuntime)
	}
	if protocolClient.settleCalls != 2 || protocolClient.startCalls != 0 || protocolClient.statusCalls != 0 {
		t.Fatalf(
			"settlement recovery calls: settlementAck=%d start=%d status=%d, want 2/0/0",
			protocolClient.settleCalls, protocolClient.startCalls, protocolClient.statusCalls,
		)
	}
}

func TestHarnessV1DispatcherPersistsInlineFailedResultBeforeSettlement(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	frame := harness.HarnessEventFrame{
		Version: harness.ProtocolVersion, Type: harness.FrameTurnFailed,
		RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
		CorrelationID: fixture.request.CorrelationID, Seq: 1,
		Failed: &harness.TurnFailed{Reason: "provider_failed", Result: "partial result"},
	}
	saveErr := errors.New("result persistence unavailable")
	fixture.dispatcher.ResultStore = &failingOnceHarnessV1ResultStore{
		ResultStore: fixture.durable,
		err:         saveErr,
	}
	protocolClient := &recordingHarnessV1RecoveryClient{}

	err := fixture.dispatcher.settleTerminalFrame(
		fixture.ctx, fixture.task, protocolClient, fixture.request, fixture.attempt, fixture.fence, frame,
	)
	if !errors.Is(err, saveErr) {
		t.Fatalf("settleTerminalFrame first error = %v, want %v", err, saveErr)
	}
	fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSettling)
	if protocolClient.settleCalls != 0 {
		t.Fatalf("settlement calls before result persistence = %d, want zero", protocolClient.settleCalls)
	}
	if _, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetResult() before persistence error = %v, want ErrNotFound", err)
	}

	if err := fixture.dispatcher.settleTerminalFrame(
		fixture.ctx, fixture.task, protocolClient, fixture.request, fixture.attempt, fixture.fence, frame,
	); err != nil {
		t.Fatalf("settleTerminalFrame retry: %v", err)
	}
	fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptFailed)
	result, err := fixture.durable.GetResult(fixture.ctx, fixture.task.Namespace, fixture.task.Name)
	if err != nil {
		t.Fatalf("GetResult() after settlement: %v", err)
	}
	if string(result) != frame.Failed.Result {
		t.Fatalf("result = %q, want %q", result, frame.Failed.Result)
	}
	if protocolClient.fetchCalls != 0 || protocolClient.ackCalls != 0 || protocolClient.settleCalls != 1 {
		t.Fatalf(
			"inline failure settlement calls: fetch=%d outputAck=%d settlementAck=%d, want 0/0/1",
			protocolClient.fetchCalls, protocolClient.ackCalls, protocolClient.settleCalls,
		)
	}
}

func TestHarnessV1DispatcherSettlementAcknowledgementDoesNotResolveProviderCredentials(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	receipt, err := harness.DurableTurnTerminalReceiptFromFrame(
		harnessV1CompletedFrame(fixture.request, "settled"),
	)
	if err != nil {
		t.Fatal(err)
	}
	receiptDigest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := fixture.dispatcher.finishAttemptDurably(
		fixture.ctx, fixture.attempt, fixture.fence,
		store.HarnessV1AttemptSucceeded, "Succeeded", receiptDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.verified.body.HarnessV1.CredentialRefs = []agentExecutionSnapshotSecretRef{{
		Role: "provider", Namespace: fixture.task.Namespace, Name: "deleted-provider-credentials",
		UID: "deleted-provider-uid", ResourceVersion: "1", Keys: []string{"OPENAI_API_KEY"},
	}}
	protocolClient := &recordingHarnessV1RecoveryClient{settle: func(
		_ context.Context,
		request harness.TurnSettlementAcknowledgementRequest,
	) error {
		if request.TurnID != fixture.request.TurnID || request.RequestDigest != terminal.RequestDigest ||
			request.TerminalReceiptDigest != terminal.TerminalReceiptDigest {
			t.Fatalf("settlement acknowledgement = %+v, want durable attempt fences", request)
		}
		return nil
	}}
	fixture.dispatcher.clientFactory = func(endpoint, bearer string, _ *http.Client) (harnessV1ProtocolClient, error) {
		if endpoint != fixture.verified.body.HarnessV1.Endpoint || strings.TrimSpace(bearer) == "" {
			t.Fatalf("wrapper client transport = endpoint %q bearerPresent=%t", endpoint, strings.TrimSpace(bearer) != "")
		}
		return protocolClient, nil
	}

	if err := fixture.dispatcher.acknowledgeDurableHarnessV1Settlement(
		fixture.ctx, fixture.task, fixture.verified, terminal, nil, nil,
	); err != nil {
		t.Fatalf("acknowledge without provider credentials: %v", err)
	}
	if protocolClient.settleCalls != 1 || protocolClient.startCalls != 0 || protocolClient.statusCalls != 0 {
		t.Fatalf("settlement calls = settle=%d start=%d status=%d, want 1/0/0", protocolClient.settleCalls, protocolClient.startCalls, protocolClient.statusCalls)
	}

	fixture.verified.body.HarnessV1.AuthSecretResourceVersion = "rotated-wrapper-auth"
	err = fixture.dispatcher.acknowledgeDurableHarnessV1Settlement(
		fixture.ctx, fixture.task, fixture.verified, terminal, nil, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "frozen harness auth Secret identity changed") {
		t.Fatalf("wrapper auth rotation error = %v, want frozen identity rejection", err)
	}
	if protocolClient.settleCalls != 1 {
		t.Fatalf("settlement was attempted after wrapper auth rotation: %d calls", protocolClient.settleCalls)
	}
}

func TestHarnessV1AttemptProjectionMatchesRequiresCompleteTerminalProjection(t *testing.T) {
	completedAt := metav1.NewTime(time.Date(2026, 8, 7, 1, 0, 0, 0, time.UTC))
	attempt := &store.HarnessV1Attempt{
		Attempt: 2, TurnID: "turn-terminal", RuntimeSessionID: "runtime-terminal",
		State: store.HarnessV1AttemptSucceeded, TerminalReason: "Succeeded",
		TerminalReceiptDigest: "sha256:" + strings.Repeat("a", 64),
		RequestDigest:         "sha256:" + strings.Repeat("b", 64),
		ControllerEpoch:       3, LastEventSeq: 7,
	}
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{
		Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: attempt.Attempt,
		CompletionTime: &completedAt,
		HarnessRuntime: &corev1alpha1.HarnessRuntimeStatus{
			Attempt: attempt.Attempt, TurnID: attempt.TurnID, RuntimeSessionID: attempt.RuntimeSessionID,
			State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
			Reason: attempt.TerminalReason, TerminalReceiptDigest: attempt.TerminalReceiptDigest,
			RequestDigest: attempt.RequestDigest, ControllerEpoch: attempt.ControllerEpoch,
			LastEventSeq: attempt.LastEventSeq, LastTransitionTime: &completedAt,
		},
	}}
	if !harnessV1AttemptProjectionMatches(task, attempt) {
		t.Fatal("complete terminal projection did not match")
	}
	task.Status.CompletionTime = nil
	if harnessV1AttemptProjectionMatches(task, attempt) {
		t.Fatal("terminal projection without completion time matched")
	}
}

type harnessV1DispatcherStateFixture struct {
	ctx        context.Context
	dispatcher *HarnessV1Dispatcher
	durable    *sqlite.Store
	task       *corev1alpha1.Task
	verified   *verifiedHarnessV1Execution
	attempt    *store.HarnessV1Attempt
	request    harness.StartTurnRequest
	fence      store.ControllerEpochFence
}

func newHarnessV1DispatcherAmbiguousFixture(t *testing.T) *harnessV1DispatcherStateFixture {
	t.Helper()
	fixture := newHarnessV1DispatcherPreparedFixture(t)
	var err error
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx,
		fixture.attempt,
		fixture.fence,
		store.HarnessV1AttemptSubmitting,
		"test-submitting",
		store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx,
		fixture.attempt,
		fixture.fence,
		store.HarnessV1AttemptSubmittedUnknown,
		"test-submission-unknown",
		store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func markHarnessV1AdmissionClosedAttempt(t *testing.T, fixture *harnessV1DispatcherStateFixture) {
	t.Helper()
	var err error
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx,
		fixture.attempt,
		fixture.fence,
		store.HarnessV1AttemptSubmitting,
		"test-submitting",
		store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	protocolClient := &recordingHarnessV1RecoveryClient{}
	err = fixture.dispatcher.handleStartTurnError(
		fixture.ctx,
		fixture.task,
		fixture.verified,
		protocolClient,
		fixture.request,
		fixture.attempt,
		fixture.fence,
		harness.ClientError{StatusCode: http.StatusConflict, Message: "wrapper admission is closed"},
	)
	if err != nil {
		t.Fatalf("handleStartTurnError(admission closed): %v", err)
	}
	fixture.attempt = assertHarnessV1AttemptState(t, fixture, store.HarnessV1AttemptSubmittedUnknown)
	if !harnessV1AttemptHasAdmissionClosedProof(fixture.attempt) {
		t.Fatalf("attempt operation = %q, want durable admission-closed proof", fixture.attempt.LastOperationID)
	}
}

func newHarnessV1DispatcherStateFixture(t *testing.T) *harnessV1DispatcherStateFixture {
	t.Helper()
	fixture := newHarnessV1DispatcherPreparedFixture(t)
	var err error
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx,
		fixture.attempt,
		fixture.fence,
		store.HarnessV1AttemptSubmitting,
		"test-submitting",
		store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.attempt, err = fixture.dispatcher.transitionAttempt(
		fixture.ctx,
		fixture.attempt,
		fixture.fence,
		store.HarnessV1AttemptAccepted,
		"test-accepted",
		store.HarnessV1AttemptUpdates{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func newHarnessV1DispatcherPreparedFixture(t *testing.T) *harnessV1DispatcherStateFixture {
	t.Helper()
	ctx := context.Background()
	reconciler, task := newHarnessV1RecoveryBindingFixture(t, ctx)
	durable, ok := reconciler.AgentExecutionSnapshots.(*sqlite.Store)
	if !ok {
		t.Fatalf("snapshot store type = %T, want *sqlite.Store", reconciler.AgentExecutionSnapshots)
	}
	fence := seedHarnessV1AttemptEpoch(t, durable)
	verified, err := reconciler.loadVerifiedHarnessV1ExecutionForRecovery(
		ctx,
		task,
		task.Status.AgentExecutionBinding,
		false,
	)
	if err != nil {
		t.Fatalf("load verified harness v1 execution: %v", err)
	}
	attempt := &store.HarnessV1Attempt{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID), Attempt: 1,
		BindingDigest:  task.Status.AgentExecutionBinding.BindingDigest,
		SnapshotDigest: task.Status.AgentExecutionBinding.Snapshot.Digest,
		TurnID:         "turn-state-machine", RuntimeSessionID: "runtime-session-state-machine",
		CorrelationID: string(task.UID), Backend: string(task.Status.AgentExecutionBinding.Backend),
		BackendEndpoint:           verified.body.HarnessV1.Endpoint,
		AuthSecretNamespace:       verified.body.HarnessV1.AuthSecretNamespace,
		AuthSecretName:            verified.body.HarnessV1.AuthSecretName,
		AuthSecretKey:             verified.body.HarnessV1.AuthSecretKey,
		AuthSecretUID:             verified.body.HarnessV1.AuthSecretUID,
		AuthSecretResourceVersion: verified.body.HarnessV1.AuthSecretResourceVersion,
		State:                     store.HarnessV1AttemptPrepared, RetryClass: store.HarnessV1RetryClassNone,
		ControllerEpochName: fence.Name, ControllerEpoch: fence.Epoch,
	}
	request, err := buildHarnessV1StartTurnRequest(task, verified, attempt, nil)
	if err != nil {
		t.Fatalf("build harness v1 request: %v", err)
	}
	attempt.RequestDigest, err = harness.CanonicalStartTurnRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	request, err = buildHarnessV1StartTurnRequest(task, verified, attempt, nil)
	if err != nil {
		t.Fatalf("rebuild harness v1 request: %v", err)
	}
	if err := durable.CreateHarnessV1Attempt(ctx, attempt, fence); err != nil {
		t.Fatalf("create harness v1 attempt: %v", err)
	}
	attempt, err = durable.GetHarnessV1Attempt(ctx, harnessV1AttemptKey(attempt))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &HarnessV1Dispatcher{
		Client: reconciler.Client, APIReader: reconciler.Client,
		Attempts: durable, Snapshots: durable,
		ResultStore: durable, EventStore: store.NewFakeExecutionEventStore(),
	}
	return &harnessV1DispatcherStateFixture{
		ctx: ctx, dispatcher: dispatcher, durable: durable, task: task,
		verified: verified, attempt: attempt, request: request, fence: fence,
	}
}

func readyHarnessV1TestEpochManager(
	durable *sqlite.Store,
	fence store.ControllerEpochFence,
) *ControllerEpochManager {
	epochs := NewControllerEpochManager(durable, fence.HolderID)
	epochs.current = &store.ControllerEpoch{Name: fence.Name, Epoch: fence.Epoch, HolderID: fence.HolderID}
	close(epochs.ready)
	return epochs
}

func harnessV1CompletedFrame(request harness.StartTurnRequest, result string) harness.HarnessEventFrame {
	return harness.HarnessEventFrame{
		Version: harness.ProtocolVersion, Type: harness.FrameTurnCompleted,
		RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID,
		CorrelationID: request.CorrelationID, Seq: 1,
		Completed: &harness.TurnCompleted{Result: result, FinalEventSeq: 1},
	}
}

func durableHarnessV1OutcomeUnknownStatus(
	t *testing.T,
	fixture *harnessV1DispatcherStateFixture,
) *harness.DurableTurnStatus {
	t.Helper()
	receipt := harness.DurableTurnTerminalReceipt{
		Version:          harness.ProtocolVersion,
		Kind:             harness.DurableTurnTerminalOutcomeUnknown,
		RuntimeSessionID: fixture.request.RuntimeSessionID,
		TurnID:           fixture.request.TurnID,
		CorrelationID:    fixture.request.CorrelationID,
		OutcomeUnknown:   &harness.DurableTurnOutcomeUnknownReceipt{Reason: "wrapper outcome unknown"},
	}
	digest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return &harness.DurableTurnStatus{
		TurnID:                fixture.attempt.TurnID,
		TaskUID:               fixture.attempt.TaskUID,
		Attempt:               fixture.attempt.Attempt,
		RequestDigest:         fixture.attempt.RequestDigest,
		State:                 harness.DurableTurnOutcomeUnknown,
		TerminalReceiptDigest: digest,
		TerminalReceipt:       &receipt,
	}
}

func appendPersistedHarnessV1Frame(
	t *testing.T,
	fixture *harnessV1DispatcherStateFixture,
	frame harness.HarnessEventFrame,
) {
	t.Helper()
	journalState, err := (harness.TurnJournal{
		EventStore: fixture.dispatcher.EventStore,
		MapContext: harness.EventMapContext{
			Namespace:   fixture.task.Namespace,
			TaskName:    fixture.task.Name,
			SessionName: fixture.request.SessionName,
			AgentName:   fixture.task.Status.AgentExecutionBinding.Agent.Name,
			StreamID:    fixture.task.Name,
		},
	}).Open(fixture.ctx)
	if err != nil {
		t.Fatalf("open harness v1 turn journal: %v", err)
	}
	if _, appended, err := journalState.AppendFrameIfNew(fixture.ctx, frame); err != nil {
		t.Fatalf("append persisted harness v1 frame: %v", err)
	} else if !appended {
		t.Fatal("persisted harness v1 frame was not appended")
	}
}

func assertHarnessV1AttemptState(
	t *testing.T,
	fixture *harnessV1DispatcherStateFixture,
	want store.HarnessV1AttemptState,
) *store.HarnessV1Attempt {
	t.Helper()
	persisted, err := fixture.durable.GetHarnessV1Attempt(fixture.ctx, harnessV1AttemptKey(fixture.attempt))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != want {
		t.Fatalf("attempt state = %s, want %s", persisted.State, want)
	}
	return persisted
}

type recordingHarnessV1RecoveryClient struct {
	status        *harness.DurableTurnStatus
	statusErr     error
	start         func(context.Context, harness.StartTurnRequest) (*harness.StartTurnResponse, error)
	stream        func(context.Context, harness.HarnessTurnID, int64, func(harness.HarnessEventFrame) error) error
	continueTurn  func(context.Context, harness.ContinueTurnRequest) (*harness.ContinueTurnResponse, error)
	fetch         func(context.Context, harness.HarnessTurnID, string) ([]byte, error)
	acknowledge   func(context.Context, harness.TurnOutputAcknowledgementRequest) error
	settle        func(context.Context, harness.TurnSettlementAcknowledgementRequest) error
	cancel        func(context.Context, harness.CancelTurnRequest) (*harness.CancelTurnResponse, error)
	statusCalls   int
	startCalls    int
	streamCalls   int
	continueCalls int
	fetchCalls    int
	ackCalls      int
	settleCalls   int
	cancelCalls   int
}

type failingHarnessV1ResultStore struct{ err error }

type failingOnceHarnessV1ResultStore struct {
	store.ResultStore
	err    error
	failed bool
}

func (s *failingOnceHarnessV1ResultStore) SaveResult(
	ctx context.Context,
	namespace, taskName string,
	result []byte,
) error {
	if !s.failed {
		s.failed = true
		return s.err
	}
	return s.ResultStore.SaveResult(ctx, namespace, taskName, result)
}

type failingOnceHarnessV1AttemptStore struct {
	store.HarnessV1AttemptStore
	target store.HarnessV1AttemptState
	err    error
	failed bool
}

func (s *failingOnceHarnessV1AttemptStore) TransitionHarnessV1Attempt(
	ctx context.Context,
	transition store.HarnessV1AttemptTransition,
) (*store.HarnessV1Attempt, error) {
	if !s.failed && transition.TargetState == s.target {
		s.failed = true
		return nil, s.err
	}
	return s.HarnessV1AttemptStore.TransitionHarnessV1Attempt(ctx, transition)
}

func (s failingHarnessV1ResultStore) SaveResult(context.Context, string, string, []byte) error {
	return s.err
}

func (s failingHarnessV1ResultStore) GetResult(context.Context, string, string) ([]byte, error) {
	return nil, s.err
}

func (s failingHarnessV1ResultStore) DeleteResult(context.Context, string, string) error {
	return s.err
}

func (c *recordingHarnessV1RecoveryClient) StartTurn(
	ctx context.Context,
	request harness.StartTurnRequest,
) (*harness.StartTurnResponse, error) {
	c.startCalls++
	if c.start != nil {
		return c.start(ctx, request)
	}
	return nil, errors.New("StartTurn must not be called during recovery")
}

func (c *recordingHarnessV1RecoveryClient) DurableTurnStatus(context.Context, harness.HarnessTurnID) (*harness.DurableTurnStatus, error) {
	c.statusCalls++
	return c.status, c.statusErr
}

func (c *recordingHarnessV1RecoveryClient) StreamFrames(
	ctx context.Context,
	turnID harness.HarnessTurnID,
	afterSeq int64,
	onFrame func(harness.HarnessEventFrame) error,
) error {
	c.streamCalls++
	if c.stream != nil {
		return c.stream(ctx, turnID, afterSeq, onFrame)
	}
	return errors.New("unexpected frame stream")
}

func (c *recordingHarnessV1RecoveryClient) ContinueTurn(
	ctx context.Context,
	request harness.ContinueTurnRequest,
) (*harness.ContinueTurnResponse, error) {
	c.continueCalls++
	if c.continueTurn != nil {
		return c.continueTurn(ctx, request)
	}
	return nil, errors.New("unexpected continuation")
}

func (c *recordingHarnessV1RecoveryClient) FetchTurnOutput(
	ctx context.Context,
	turnID harness.HarnessTurnID,
	outputRef string,
) ([]byte, error) {
	c.fetchCalls++
	if c.fetch != nil {
		return c.fetch(ctx, turnID, outputRef)
	}
	return nil, errors.New("unexpected output fetch")
}

func (c *recordingHarnessV1RecoveryClient) AcknowledgeTurnOutput(
	ctx context.Context,
	request harness.TurnOutputAcknowledgementRequest,
) error {
	c.ackCalls++
	if c.acknowledge != nil {
		return c.acknowledge(ctx, request)
	}
	return errors.New("unexpected output acknowledgement")
}

func (c *recordingHarnessV1RecoveryClient) AcknowledgeTurnSettlement(
	ctx context.Context,
	request harness.TurnSettlementAcknowledgementRequest,
) error {
	c.settleCalls++
	if c.settle != nil {
		return c.settle(ctx, request)
	}
	return nil
}

func (c *recordingHarnessV1RecoveryClient) CancelTurn(
	ctx context.Context,
	request harness.CancelTurnRequest,
) (*harness.CancelTurnResponse, error) {
	c.cancelCalls++
	if c.cancel != nil {
		return c.cancel(ctx, request)
	}
	return nil, errors.New("unexpected cancellation")
}

var _ harnessV1ProtocolClient = (*recordingHarnessV1RecoveryClient)(nil)
var _ store.ResultStore = failingHarnessV1ResultStore{}
var _ store.ResultStore = (*failingOnceHarnessV1ResultStore)(nil)
var _ store.HarnessV1AttemptStore = (*failingOnceHarnessV1AttemptStore)(nil)
