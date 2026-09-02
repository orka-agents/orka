package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspacedelta"
)

const testE2EPromptWriteAmbiguityMarker = "ORKA_E2E_WS_LC_AMBIGUOUS_OK"

func TestPromptStreamErrorDetailIsBoundedAndSingleLine(t *testing.T) {
	if got := promptStreamErrorDetail(fmt.Errorf("first\nsecond\rthird")); got != "first second third" {
		t.Fatalf("single-line detail = %q", got)
	}
	detail := promptStreamErrorDetail(fmt.Errorf("%s界", strings.Repeat("x", 512)))
	if !strings.HasSuffix(detail, "...") || len(strings.TrimSuffix(detail, "...")) > 512 || !utf8.ValidString(detail) {
		t.Fatalf("bounded detail = %q (len=%d)", detail, len(detail))
	}
}

func TestPromptContainsE2EWriteAmbiguityMarker(t *testing.T) {
	marker := testE2EPromptWriteAmbiguityMarker
	input := harnessv2.PromptInput{Content: []harnessv2.ContentBlock{
		{Type: harnessv2.ContentBlockResourceLink, URI: "https://example.com/" + testE2EPromptWriteAmbiguityMarker},
		{Type: harnessv2.ContentBlockText, Text: "Reply exactly: " + marker},
	}}
	if !promptContainsE2EWriteAmbiguityMarker(input, marker) {
		t.Fatal("configured text marker was not detected")
	}
	if promptContainsE2EWriteAmbiguityMarker(input, "") {
		t.Fatal("empty fault marker enabled the injection")
	}
	if promptContainsE2EWriteAmbiguityMarker(input, "ORKA_E2E_OTHER_OK") {
		t.Fatal("unrelated marker enabled the injection")
	}
}

func TestE2EPromptWriteAmbiguityIsOneShotPerOperation(t *testing.T) {
	request := harnessv2.StartPromptRequest{
		Metadata: harnessv2.MutationMetadata{OperationID: "operation-1"},
		Input: harnessv2.PromptInput{Content: []harnessv2.ContentBlock{{
			Type: harnessv2.ContentBlockText,
			Text: "Reply exactly: " + testE2EPromptWriteAmbiguityMarker,
		}}},
	}
	recorder := &sharedE2EPromptWriteFaultRecorder{consumed: make(map[harnessv2.OperationID]struct{})}
	server := &Server{e2ePromptWriteRecorder: recorder}
	consumed, err := server.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	if err != nil || !consumed {
		t.Fatal("first request did not consume the ambiguity fault")
	}
	consumed, err = server.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	if err != nil || consumed {
		t.Fatal("retry of the same operation consumed the ambiguity fault again")
	}
	request.Metadata.OperationID = "operation-2"
	consumed, err = server.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	if err != nil || !consumed {
		t.Fatal("distinct operation did not receive its own ambiguity fault")
	}
	// RuntimeSession state may be discarded and recreated inside the same
	// supervisor. The one-shot decision belongs to the external recorder.
	consumed, err = server.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	if err != nil || consumed {
		t.Fatal("runtime Session recreation reset the process-local ambiguity ledger")
	}

	restartedServer := &Server{e2ePromptWriteRecorder: recorder}
	consumed, err = restartedServer.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	if err != nil || consumed {
		t.Fatal("supervisor restart re-armed the external ambiguity ledger")
	}
}

func TestPromptStreamErrorClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "unexpected event", want: "unexpected-event"},
		{name: "rate", err: fmt.Errorf("wrapped: %w", harnessv2.ErrEventRateExceeded), want: "event-rate-exceeded"},
		{name: "byte rate", err: fmt.Errorf("wrapped: %w", harnessv2.ErrEventByteRateExceeded), want: "event-byte-rate-exceeded"},
		{name: "line", err: harnessv2.ErrEventLineTooLarge, want: "event-line-too-large"},
		{name: "malformed", err: harnessv2.ErrMalformedEvent, want: "malformed-event"},
		{name: "identity", err: harnessv2.ErrEventIdentityMismatch, want: "event-identity-mismatch"},
		{name: "sequence", err: harnessv2.ErrEventSequence, want: "event-sequence"},
		{name: "after terminal", err: harnessv2.ErrEventAfterTerminal, want: "event-after-terminal"},
		{name: "missing accepted", err: harnessv2.ErrMissingAcceptedEvent, want: "missing-accepted-event"},
		{name: "write", err: fmt.Errorf("write event: broken pipe"), want: "event-write"},
		{name: "tool call id", err: fmt.Errorf("ACP tool call update omitted toolCallId"), want: "missing-tool-call-id"},
		{name: "update decode", err: fmt.Errorf("decode ACP session update: invalid syntax"), want: "update-decode"},
		{name: "permission", err: fmt.Errorf("unsupported ACP permission option kind"), want: "permission-map"},
		{name: "runtime event", err: fmt.Errorf("unsupported runtime prompt event"), want: "unsupported-runtime-event"},
		{name: "MCP authority", err: fmt.Errorf("activate prompt-scoped MCP authority"), want: "mcp-authority"},
		{name: "terminal size", err: fmt.Errorf("bounded terminal failure event exceeds size"), want: "terminal-size"},
		{name: "other", err: fmt.Errorf("other failure"), want: "unclassified"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := promptStreamErrorClass(test.err); got != test.want {
				t.Fatalf("promptStreamErrorClass(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}

func TestPromptStreamMalformedValidationClassIsBounded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "not malformed", err: harnessv2.ErrEventRateExceeded},
		{name: "assistant empty", err: fmt.Errorf("%w: assistant message chunk is required", harnessv2.ErrMalformedEvent), want: "assistant-empty"},
		{name: "assistant too large", err: fmt.Errorf("%w: assistant message chunk exceeds 4096 bytes", harnessv2.ErrMalformedEvent), want: "assistant-too-large"},
		{name: "assistant UTF-8", err: fmt.Errorf("%w: assistant message chunk contains invalid UTF-8", harnessv2.ErrMalformedEvent), want: "assistant-invalid-utf8"},
		{name: "tool status", err: fmt.Errorf("%w: unsupported tool call status provider-secret", harnessv2.ErrMalformedEvent), want: "tool-call-status"},
		{name: "plan content", err: fmt.Errorf("%w: plan entry content exceeds 4096 bytes", harnessv2.ErrMalformedEvent), want: "plan-content"},
		{name: "other", err: fmt.Errorf("%w: provider-secret-must-not-leak", harnessv2.ErrMalformedEvent), want: "Other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := promptStreamMalformedValidationClass(test.err); got != test.want {
				t.Fatalf("validation class = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPromptDuplicateClassificationPrecedesCapacityAdmission(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	now := time.Now().UTC()

	tests := []struct {
		name           string
		phase          harnessv2.OperationPhase
		terminal       harnessv2.EventType
		wantCode       harnessv2.ErrorCode
		wantClass      harnessv2.RequestClassification
		withSettlement bool
	}{
		{
			name: "already accepted", phase: harnessv2.OperationPhaseAccepted,
			wantCode: harnessv2.ErrorCodeAlreadyAccepted, wantClass: harnessv2.RequestClassificationAlreadyAccepted,
		},
		{
			name: "settled", phase: harnessv2.OperationPhaseSettled, terminal: harnessv2.EventCompleted,
			wantCode: harnessv2.ErrorCodeSettled, wantClass: harnessv2.RequestClassificationSettled, withSettlement: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			state.operations[prompt.Metadata.OperationID] = operationRecord(prompt.Metadata, test.phase, test.terminal, now)
			state.prompt = &promptState{request: prompt}
			if test.withSettlement {
				settlement := harnessv2.PromptSettlement{
					TerminalEvent: harnessv2.EventCompleted, Outcome: harnessv2.PromptOutcomeSucceeded,
					StopReason: harnessv2.ACPStopReasonEndTurn, SettledAt: now,
				}
				state.prompt.settlement = &settlement
			}
			server.mu.Unlock()

			for len(server.promptSlots) < cap(server.promptSlots) {
				server.promptSlots <- struct{}{}
			}
			response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
			for len(server.promptSlots) > 0 {
				<-server.promptSlots
			}
			if response.Code != http.StatusOK {
				t.Fatalf("duplicate status = %d body=%s", response.Code, response.Body.String())
			}
			var decoded harnessv2.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Code != test.wantCode || decoded.Classification == nil || decoded.Classification.Class != test.wantClass {
				t.Fatalf("duplicate response = %#v", decoded)
			}
		})
	}
}

func TestSettledPromptDoesNotBlockNextIdleTurn(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	previous := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	settlement := harnessv2.PromptSettlement{
		TerminalEvent: harnessv2.EventCompleted,
		Outcome:       harnessv2.PromptOutcomeSucceeded,
		StopReason:    harnessv2.ACPStopReasonEndTurn,
		SettledAt:     time.Now().UTC(),
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	state.prompt = &promptState{request: previous, settlement: &settlement}
	server.mu.Unlock()

	next := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	next.Metadata.OperationID = testPromptOperationTwo
	next.Metadata.PromptID = testPromptTwoID
	next.MCPAuthorization.TaskUID = next.Metadata.TaskUID
	next.MCPAuthorization.TaskAttempt = next.Metadata.TaskAttempt
	next.MCPAuthorization.PromptID = next.Metadata.PromptID
	next.Metadata.RequestDigest = ""
	sealRequest(t, &next.Metadata.RequestDigest, next)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-2", next, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("continuation status = %d body=%s", response.Code, response.Body.String())
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if state.prompt == nil || state.prompt.request.Metadata.PromptID != next.Metadata.PromptID || state.prompt.settlement == nil {
		t.Fatalf("continuation prompt state = %#v", state.prompt)
	}
}

func TestWorkspaceDeltaDuplicateReplaysAfterContinuationStarts(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	oldPrompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	request := harnessv2.CreateWorkspaceDeltaRequest{
		Protocol:               harnessv2.ProtocolVersion,
		Metadata:               oldPrompt.Metadata,
		DeltaID:                "delta-old",
		Intent:                 create.Workspace.Intent,
		VerifiedBaseline:       create.Workspace.Baseline,
		PromptSettlementDigest: testDigest("old-settlement"),
		Limits:                 harnessv2.WorkspaceDeltaLimits{MaxBytes: 1 << 20, MaxEntries: 100},
	}
	request.Metadata.OperationID = "workspace-delta-old"
	request.Metadata.ExpiresAt = time.Now().UTC().Add(time.Minute)
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	stored := harnessv2.CreateWorkspaceDeltaResponse{
		Protocol:       harnessv2.ProtocolVersion,
		Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
		Delta:          harnessv2.WorkspaceDeltaDescriptor{DeltaID: request.DeltaID},
	}
	next := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	next.Metadata.OperationID = testPromptOperationTwo
	next.Metadata.PromptID = testPromptTwoID
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.prompt = &promptState{request: next}
	state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
	state.operations[request.Metadata.OperationID] = operationRecord(request.Metadata, harnessv2.OperationPhaseApplied, "", time.Now().UTC())
	state.deltas[request.DeltaID] = stored
	server.mu.Unlock()

	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/workspace-deltas/delta-old", request, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("duplicate workspace delta status = %d body=%s", response.Code, response.Body.String())
	}
	var decoded harnessv2.CreateWorkspaceDeltaResponse
	decodeResponse(t, response, &decoded)
	if decoded.Classification.Class != harnessv2.RequestClassificationDuplicate || decoded.Classification.Phase != harnessv2.OperationPhaseApplied {
		t.Fatalf("duplicate workspace delta response = %#v", decoded)
	}
}

func TestLateFreshCancellationCannotSettleContinuationPrompt(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	current := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	current.Metadata.OperationID = testPromptOperationTwo
	current.Metadata.PromptID = testPromptTwoID
	mutations := newBlockingPromptMutator()
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.prompt = &promptState{request: current}
	state.promptMutations = mutations
	state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
	server.mu.Unlock()

	request := harnessv2.CancelPromptRequest{
		Protocol:           harnessv2.ProtocolVersion,
		Metadata:           testMetadata(create.Metadata.Fence, "late-cancel-prompt-1", true),
		Reason:             harnessv2.CancelReasonUserRequested,
		SettlementDeadline: time.Now().UTC().Add(30 * time.Second),
	}
	request.Metadata.PromptID = testPromptOneID
	request.Metadata.ExpiresAt = request.SettlementDeadline
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel", request, cfg)
	if response.Code != http.StatusConflict {
		t.Fatalf("late cancellation status = %d body=%s", response.Code, response.Body.String())
	}
	var decoded harnessv2.ErrorResponse
	decodeResponse(t, response, &decoded)
	if decoded.Code != harnessv2.ErrorCodeDigestConflict {
		t.Fatalf("late cancellation error = %#v", decoded)
	}
	if got := mutations.cancelCalls.Load(); got != 0 {
		t.Fatalf("late cancellation side effects = %d, want 0", got)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if state.prompt.settlement != nil {
		t.Fatalf("continuation prompt was settled: %#v", state.prompt.settlement)
	}
	if _, ok := state.operations[request.Metadata.OperationID]; ok {
		t.Fatal("late cancellation recorded an operation")
	}
}

func TestPriorPromptLeaseDuplicateReplaysExactLeaseAfterContinuation(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	now := time.Now().UTC()
	oldPrompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	oldLease := harnessv2.PromptLease{Generation: 2, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	authorization := oldPrompt.MCPAuthorization
	authorization.LeaseGeneration = oldLease.Generation
	authorization.ExpiresAt = now.Add(30 * time.Second)
	request := harnessv2.RenewPromptLeaseRequest{
		Protocol:                harnessv2.ProtocolVersion,
		Metadata:                testMetadata(create.Metadata.Fence, "renew-old", true),
		ExpectedLeaseGeneration: 1,
		Lease:                   oldLease,
		MCPAuthorization:        authorization,
	}
	request.Metadata.PromptID = oldPrompt.Metadata.PromptID
	request.Metadata.ExpiresAt = authorization.ExpiresAt
	request.MCPAuthorization.TaskUID = request.Metadata.TaskUID
	request.MCPAuthorization.TaskAttempt = request.Metadata.TaskAttempt
	request.MCPAuthorization.PromptID = request.Metadata.PromptID
	sealRequest(t, &request.Metadata.RequestDigest, request)
	stored := harnessv2.PromptLeaseResponse{
		Protocol:       harnessv2.ProtocolVersion,
		Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
		Lease:          oldLease,
	}
	done := make(chan struct{})
	close(done)
	current := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	current.Metadata.OperationID = testPromptOperationTwo
	current.Metadata.PromptID = testPromptTwoID
	current.Lease.Generation = 9
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.prompt = &promptState{request: current, lease: current.Lease}
	state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
	state.operations[request.Metadata.OperationID] = operationRecord(request.Metadata, harnessv2.OperationPhaseApplied, "", now)
	state.operationReplays[request.Metadata.OperationID] = &operationReplay{done: done, lease: &stored}
	server.mu.Unlock()

	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1/lease", request, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("old lease replay status = %d body=%s", response.Code, response.Body.String())
	}
	var decoded harnessv2.PromptLeaseResponse
	decodeResponse(t, response, &decoded)
	if decoded.Classification.Class != harnessv2.RequestClassificationDuplicate || decoded.Lease.Generation != oldLease.Generation {
		t.Fatalf("old lease replay = %#v", decoded)
	}
}

func TestWorkspaceDeltaRouteValidatesSettledPromptWithoutPromptPathSegment(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	settlement := harnessv2.PromptSettlement{
		TerminalEvent: harnessv2.EventCompleted,
		Outcome:       harnessv2.PromptOutcomeSucceeded,
		StopReason:    harnessv2.ACPStopReasonEndTurn,
		SettledAt:     time.Now().UTC(),
	}
	settlementDigest := testDigest("settled-prompt")
	server.sessions[create.RuntimeSessionID] = &sessionState{
		descriptor: harnessv2.RuntimeSessionDescriptor{
			RuntimeSessionUID: create.Metadata.Fence.RuntimeSessionUID,
			Generation:        create.Metadata.Fence.RuntimeSessionGeneration,
			State:             harnessv2.RuntimeSessionStateValidating,
			WorkspaceBaseline: create.Workspace.Baseline,
		},
		operations:      make(map[harnessv2.OperationID]harnessv2.OperationRecord),
		permissions:     make(map[harnessv2.PermissionRequestID]permissionState),
		workspaceIntent: create.Workspace.Intent,
		prompt: &promptState{
			request:          prompt,
			settlement:       &settlement,
			settlementDigest: settlementDigest,
		},
	}
	request := harnessv2.CreateWorkspaceDeltaRequest{
		Protocol:               harnessv2.ProtocolVersion,
		Metadata:               prompt.Metadata,
		DeltaID:                "delta-1",
		Intent:                 create.Workspace.Intent,
		VerifiedBaseline:       create.Workspace.Baseline,
		PromptSettlementDigest: testDigest("different-settlement"),
		Limits:                 harnessv2.WorkspaceDeltaLimits{MaxBytes: 1 << 20, MaxEntries: 100},
	}
	request.Metadata.OperationID = "workspace-delta-1"
	request.Metadata.ExpiresAt = time.Now().UTC().Add(time.Minute)
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(
		t,
		server.Handler(),
		http.MethodPut,
		"/v2/runtime-sessions/session-1/workspace-deltas/delta-1",
		request,
		cfg,
	)
	if response.Code != http.StatusConflict {
		t.Fatalf("workspace delta status = %d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("workspace delta Content-Type = %q", got)
	}
	var decoded harnessv2.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Code != harnessv2.ErrorCodeDigestConflict || decoded.Message != "prompt settlement digest does not match" {
		t.Fatalf("workspace delta error = %#v", decoded)
	}
}

func TestRejectWorkspaceDeltaLimitUsesProtocolValidPoisonedStatus(t *testing.T) {
	server := &Server{}
	state := &sessionState{descriptor: harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStateValidating}}
	response := httptest.NewRecorder()
	server.rejectWorkspaceDeltaLimit(response, state)

	if response.Code != http.StatusConflict {
		t.Fatalf("workspace delta status = %d body=%s", response.Code, response.Body.String())
	}
	var decoded harnessv2.ErrorResponse
	decodeResponse(t, response, &decoded)
	if decoded.Code != harnessv2.ErrorCodeSessionPoisoned || decoded.Message != "workspace delta exceeds request limits" {
		t.Fatalf("workspace delta error = %#v", decoded)
	}
	if state.descriptor.State != harnessv2.RuntimeSessionStatePoisoned {
		t.Fatalf("runtime session state = %s, want Poisoned", state.descriptor.State)
	}
}

func TestPromptExecutionDiagnosticDoesNotExposeRPCMessage(t *testing.T) {
	stage, code, service, errorName := promptExecutionDiagnostic(&acp.RPCError{
		Code:    -32001,
		Message: "provider-secret-must-not-leak",
		Data:    json.RawMessage(`{"service":"session","errorName":"APIError","detail":"provider-secret-must-not-leak"}`),
	})
	if stage != promptExecutionStageJSONRPCError || code != -32001 || service != "session" || errorName != "APIError" {
		t.Fatalf("RPC diagnostic = %q/%d/%q/%q", stage, code, service, errorName)
	}
	stage, code, service, errorName = promptExecutionDiagnostic(&acp.RPCError{
		Code: -32603,
		Data: json.RawMessage(`{"service":"session\nsecret","errorName":"` + strings.Repeat("x", 65) + `"}`),
	})
	if stage != promptExecutionStageJSONRPCError || code != -32603 || service != "" || errorName != "" {
		t.Fatalf("unsafe RPC diagnostic = %q/%d/%q/%q", stage, code, service, errorName)
	}
	stage, code, service, errorName = promptExecutionDiagnostic(acp.ErrClosed)
	if stage != "transport-closed" || code != 0 || service != "" || errorName != "" {
		t.Fatalf("closed diagnostic = %q/%d/%q/%q", stage, code, service, errorName)
	}
}

func TestPromptTerminalDiagnosticAllowsOnlyProtocolEnums(t *testing.T) {
	tests := []struct {
		name           string
		result         acp.PromptResult
		wantOutcome    string
		wantStopReason string
	}{
		{
			name: "failed max turn requests",
			result: acp.PromptResult{
				Outcome: acp.PromptOutcomeFailed, StopReason: acp.StopReasonMaxTurnRequests,
			},
			wantOutcome: "failed", wantStopReason: "max_turn_requests",
		},
		{
			name:        "missing stop reason",
			result:      acp.PromptResult{Outcome: acp.PromptOutcomeFailed},
			wantOutcome: "failed", wantStopReason: "",
		},
		{
			name: "unrecognized values",
			result: acp.PromptResult{
				Outcome:    acp.PromptOutcome("provider-secret-outcome"),
				StopReason: acp.StopReason("provider-secret-reason"),
			},
			wantOutcome: "Other", wantStopReason: "Other",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome, stopReason := promptTerminalDiagnostic(test.result)
			if outcome != test.wantOutcome || stopReason != test.wantStopReason {
				t.Fatalf("terminal diagnostic = %q/%q, want %q/%q", outcome, stopReason, test.wantOutcome, test.wantStopReason)
			}
		})
	}
}

func TestConcurrentDuplicatePermissionResolutionExecutesOnce(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	mutations := newBlockingPromptMutator()

	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.prompt = &promptState{request: prompt}
	state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
	state.promptMutations = mutations
	state.permissions["permission-1"] = permissionState{
		requestID: "permission-1", requestedAt: time.Now().UTC(), expiresAt: time.Now().UTC().Add(time.Minute),
		options: map[string]harnessv2.PermissionOptionKind{"deny": harnessv2.PermissionOptionRejectOnce},
	}
	server.mu.Unlock()

	request := harnessv2.ResolvePermissionRequest{
		Protocol:  harnessv2.ProtocolVersion,
		Metadata:  testMetadata(create.Metadata.Fence, "permission-operation-1", true),
		RequestID: "permission-1",
		Decision:  harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionCancelled},
	}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	path := "/v2/runtime-sessions/session-1/prompts/prompt-1/permissions/permission-1"
	firstRequest := mutationHTTPRequest(t, http.MethodPut, path, request, cfg)
	secondRequest := mutationHTTPRequest(t, http.MethodPut, path, request, cfg)
	firstDone := serveMutationAsync(server.Handler(), firstRequest)
	awaitSignal(t, mutations.resolveEntered, "permission resolution did not start")
	secondDone := serveMutationAsync(server.Handler(), secondRequest)
	assertStillWaiting(t, secondDone, "duplicate permission resolution returned before the owner completed")
	if got := mutations.resolveCalls.Load(); got != 1 {
		t.Fatalf("permission side-effect calls = %d, want 1", got)
	}
	close(mutations.resolveRelease)

	first := awaitRecorder(t, firstDone, "first permission resolution did not complete")
	second := awaitRecorder(t, secondDone, "duplicate permission resolution did not replay")
	var firstResponse, secondResponse harnessv2.PermissionResolutionResponse
	decodeResponse(t, first, &firstResponse)
	decodeResponse(t, second, &secondResponse)
	if firstResponse.Classification.Class != harnessv2.RequestClassificationFresh || firstResponse.State != harnessv2.PermissionResolutionApplied {
		t.Fatalf("first permission response = %#v", firstResponse)
	}
	if secondResponse.Classification.Class != harnessv2.RequestClassificationDuplicate ||
		secondResponse.Classification.Phase != harnessv2.OperationPhaseApplied ||
		secondResponse.State != harnessv2.PermissionResolutionAlreadyResolved {
		t.Fatalf("duplicate permission response = %#v", secondResponse)
	}
	if got := mutations.resolveCalls.Load(); got != 1 {
		t.Fatalf("permission side-effect calls after replay = %d, want 1", got)
	}
	server.mu.Lock()
	_, stillPending := state.permissions[request.RequestID]
	server.mu.Unlock()
	if stillPending {
		t.Fatal("resolved permission remained in the pending-permission map")
	}
}

func TestSessionOperationRecordsPruneAfterCapabilityExpiry(t *testing.T) {
	now := time.Now().UTC()
	state := &sessionState{
		operations:       make(map[harnessv2.OperationID]harnessv2.OperationRecord),
		operationReplays: make(map[harnessv2.OperationID]*operationReplay),
	}
	add := func(operation string, expiresAt time.Time) harnessv2.OperationID {
		metadata := testMetadata(harnessv2.Fence{}, operation, false)
		metadata.ExpiresAt = expiresAt
		metadata.RequestDigest = harnessv2.RequestDigest(testDigest(operation))
		recordSessionOperationLocked(state, metadata, harnessv2.OperationPhaseApplied, "", now)
		state.operationReplays[metadata.OperationID] = &operationReplay{done: make(chan struct{})}
		return metadata.OperationID
	}

	expired := add("expired-operation", now.Add(-operationReplayRetentionSlack-time.Second))
	withinSkew := add("within-skew-operation", now.Add(-operationReplayRetentionSlack+time.Second))
	active := add("active-operation", now.Add(time.Minute))
	pruneSessionOperationsLocked(state, now)

	if _, ok := state.operations[expired]; ok {
		t.Fatal("expired operation record was retained")
	}
	if _, ok := state.operationReplays[expired]; ok {
		t.Fatal("expired operation replay was retained")
	}
	if _, ok := state.operationRetention[expired]; ok {
		t.Fatal("expired operation retention deadline was retained")
	}
	for _, operationID := range []harnessv2.OperationID{withinSkew, active} {
		if _, ok := state.operations[operationID]; !ok {
			t.Fatalf("operation %q was pruned before its retention deadline", operationID)
		}
		if _, ok := state.operationReplays[operationID]; !ok {
			t.Fatalf("operation replay %q was pruned before its retention deadline", operationID)
		}
	}
}

func TestSessionOperationCapacityReservesDeletionTombstoneSlot(t *testing.T) {
	now := time.Now().UTC()
	state := &sessionState{operations: make(map[harnessv2.OperationID]harnessv2.OperationRecord)}
	for index := range harnessv2.MaxRuntimeSessionTombstoneOperations - sessionDeletionOperationReserve {
		operationID := harnessv2.OperationID(fmt.Sprintf("operation-%04d", index))
		state.operations[operationID] = harnessv2.OperationRecord{
			OperationID: operationID, RequestDigest: harnessv2.RequestDigest(testDigest(string(operationID))),
			Phase: harnessv2.OperationPhaseApplied, RecordedAt: now, UpdatedAt: now,
		}
	}
	if err := ensureSessionOperationCapacityLocked(state, sessionDeletionOperationReserve); err == nil {
		t.Fatal("fresh non-deletion operation was accepted after consuming the deletion reserve")
	}
	if err := ensureSessionOperationCapacityLocked(state, 0); err != nil {
		t.Fatalf("reserved deletion operation was rejected: %v", err)
	}
	deleteID := harnessv2.OperationID("delete-session")
	state.operations[deleteID] = harnessv2.OperationRecord{
		OperationID: deleteID, RequestDigest: harnessv2.RequestDigest(testDigest(string(deleteID))),
		Phase: harnessv2.OperationPhaseDeleted, RecordedAt: now, UpdatedAt: now,
	}
	operations := make([]harnessv2.OperationRecord, 0, len(state.operations))
	for _, operation := range state.operations {
		operations = append(operations, operation)
	}
	tombstone := harnessv2.RuntimeSessionTombstone{
		RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 1,
		RuntimeProfileDigest: harnessv2.ProfileDigest(testDigest("profile")), DeletedAt: now, Operations: operations,
	}
	if err := tombstone.Validate(); err != nil {
		t.Fatalf("maximum-sized deletion tombstone is invalid: %v", err)
	}
	if err := ensureSessionOperationCapacityLocked(state, 0); err == nil {
		t.Fatal("operation journal accepted an entry beyond the tombstone protocol limit")
	}
}

func TestMapRuntimeEventEnforcesPendingPermissionLimit(t *testing.T) {
	limits := harnessv2.DefaultProtocolLimits()
	limits.MaxPendingPermissions = 1
	server := &Server{cfg: Config{Capabilities: harnessv2.CapabilitiesResponse{Limits: limits}}}
	now := time.Now().UTC()
	prompt := &promptState{request: harnessv2.StartPromptRequest{Metadata: harnessv2.MutationMetadata{PromptID: testPromptOneID}}}
	state := &sessionState{
		descriptor:  harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: "session-uid", Generation: 1},
		permissions: make(map[harnessv2.PermissionRequestID]permissionState),
	}
	permissionEvent := func(requestID string) acp.PromptEvent {
		toolCall, err := json.Marshal(map[string]any{
			"toolCallId": "tool-call-" + requestID,
			"name":       "Write",
			"title":      "Write file",
		})
		if err != nil {
			t.Fatal(err)
		}
		return acp.PromptEvent{
			Type:      acp.PromptEventPermissionRequested,
			Timestamp: now,
			Permission: &acp.PermissionRequestEvent{
				RequestID: requestID,
				Request: acp.RequestPermissionRequest{
					ToolCall: toolCall,
					Options:  []acp.PermissionOption{{OptionID: "deny", Name: "Deny", Kind: string(harnessv2.PermissionOptionRejectOnce)}},
				},
			},
		}
	}

	if _, err := server.mapRuntimeEvent(state, prompt, permissionEvent("permission-1")); err != nil {
		t.Fatalf("first permission event: %v", err)
	}
	if _, err := server.mapRuntimeEvent(state, prompt, permissionEvent("permission-2")); err == nil ||
		!strings.Contains(err.Error(), "pending permission limit 1 exceeded") {
		t.Fatalf("second permission event error = %v, want pending-limit rejection", err)
	}
	if len(state.permissions) != 1 {
		t.Fatalf("pending permission count = %d, want 1", len(state.permissions))
	}
	if _, ok := state.permissions["permission-2"]; ok {
		t.Fatal("over-limit permission was inserted")
	}
	delete(state.permissions, "permission-1")
	if _, err := server.mapRuntimeEvent(state, prompt, permissionEvent("permission-1")); err == nil ||
		!strings.Contains(err.Error(), "already used by this prompt") {
		t.Fatalf("reused resolved permission event error = %v, want request-ID reuse rejection", err)
	}
	if len(state.permissions) != 0 {
		t.Fatalf("reused permission request was reinserted: %#v", state.permissions)
	}
}

func TestConcurrentDuplicateCancellationExecutesOnceAndPreservesSettlement(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	mutations := newBlockingPromptMutator()
	mutations.cancelResult = acp.PromptResult{
		Outcome: acp.PromptOutcomeCancelled, StopReason: acp.StopReasonCancelled,
		Accepted: true, SettledAt: time.Now().UTC(),
	}

	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.prompt = &promptState{request: prompt}
	state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
	state.promptMutations = mutations
	state.permissions["permission-1"] = permissionState{requestID: "permission-1"}
	server.mu.Unlock()

	request := harnessv2.CancelPromptRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: testMetadata(create.Metadata.Fence, "cancel-operation-1", true),
		Reason:   harnessv2.CancelReasonUserRequested,
	}
	request.SettlementDeadline = time.Now().UTC().Add(30 * time.Second)
	sealRequest(t, &request.Metadata.RequestDigest, request)
	path := "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel"
	firstRequest := mutationHTTPRequest(t, http.MethodPut, path, request, cfg)
	secondRequest := mutationHTTPRequest(t, http.MethodPut, path, request, cfg)
	firstDone := serveMutationAsync(server.Handler(), firstRequest)
	awaitSignal(t, mutations.cancelEntered, "cancellation did not start")
	secondDone := serveMutationAsync(server.Handler(), secondRequest)
	assertStillWaiting(t, secondDone, "duplicate cancellation returned before the owner completed")
	if got := mutations.cancelCalls.Load(); got != 1 {
		t.Fatalf("cancellation side-effect calls = %d, want 1", got)
	}

	completed := harnessv2.PromptSettlement{
		TerminalEvent: harnessv2.EventCompleted, Outcome: harnessv2.PromptOutcomeSucceeded,
		StopReason: harnessv2.ACPStopReasonEndTurn, SettledAt: time.Now().UTC(),
	}
	server.mu.Lock()
	settlePromptLocked(state.prompt, completed)
	server.mu.Unlock()
	close(mutations.cancelRelease)

	first := awaitRecorder(t, firstDone, "first cancellation did not complete")
	second := awaitRecorder(t, secondDone, "duplicate cancellation did not replay")
	var firstResponse, secondResponse harnessv2.CancelPromptResponse
	decodeResponse(t, first, &firstResponse)
	decodeResponse(t, second, &secondResponse)
	if firstResponse.Classification.Class != harnessv2.RequestClassificationFresh || firstResponse.Settlement.TerminalEvent != harnessv2.EventCompleted {
		t.Fatalf("first cancellation response = %#v", firstResponse)
	}
	if secondResponse.Classification.Class != harnessv2.RequestClassificationSettled ||
		secondResponse.Classification.TerminalEvent != harnessv2.EventCompleted ||
		secondResponse.Settlement.TerminalEvent != harnessv2.EventCompleted {
		t.Fatalf("duplicate cancellation response = %#v", secondResponse)
	}
	if got := mutations.cancelCalls.Load(); got != 1 {
		t.Fatalf("cancellation side-effect calls after replay = %d, want 1", got)
	}
	server.mu.Lock()
	if state.prompt.settlement == nil || state.prompt.settlement.TerminalEvent != harnessv2.EventCompleted {
		server.mu.Unlock()
		t.Fatalf("terminal settlement was overwritten: %#v", state.prompt.settlement)
	}
	record := state.operations[request.Metadata.OperationID]
	server.mu.Unlock()
	if record.Phase != harnessv2.OperationPhaseSettled || record.TerminalEvent != harnessv2.EventCompleted {
		t.Fatalf("cancellation operation record = %#v", record)
	}

	terminal, settledResult, err := server.terminalEvent(state, state.prompt, mutations.cancelResult)
	if err != nil {
		t.Fatalf("terminal replay after cancellation race: %v", err)
	}
	if terminal.Type != harnessv2.EventCompleted || settledResult.Outcome != acp.PromptOutcomeCompleted {
		t.Fatalf("terminal replay changed settled outcome: event=%#v result=%#v", terminal, settledResult)
	}
	server.finishPrompt(state, state.prompt, mutations.cancelResult, mutations.cancelResult.SettledAt)
	server.mu.Lock()
	defer server.mu.Unlock()
	if state.prompt.settlement.TerminalEvent != harnessv2.EventCompleted || state.descriptor.State != harnessv2.RuntimeSessionStateValidating {
		t.Fatalf("terminal finisher overwrote settlement: state=%s settlement=%#v", state.descriptor.State, state.prompt.settlement)
	}
}

func TestProviderProxyMaxTurnsMapsToTerminalFailure(t *testing.T) {
	server, cfg, _ := newTestServer(t, "immediate")
	fence := cfg.Fence
	fence.RuntimeSessionUID = "max-turn-session-uid"
	fence.RuntimeSessionGeneration = 1
	prompt := &promptState{request: testStartPromptRequest(t, cfg, fence)}
	promptID := string(prompt.request.Metadata.PromptID)
	now := time.Now().UTC()
	proxy := &providerProxySession{}
	if err := proxy.activateWithMaxTurns(promptID, 1, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	defer proxy.close()
	if err := proxy.consumeInferenceRequest(promptID, providerRequestInference, now); err != nil {
		t.Fatalf("first inference request: %v", err)
	}
	if err := proxy.consumeInferenceRequest(promptID, providerRequestInference, now); err != errProviderTurnLimitExceeded {
		t.Fatalf("N+1 inference request error = %v, want %v", err, errProviderTurnLimitExceeded)
	}
	proxy.deactivate(promptID)

	state := &sessionState{
		descriptor: harnessv2.RuntimeSessionDescriptor{
			RuntimeSessionUID: fence.RuntimeSessionUID,
			Generation:        fence.RuntimeSessionGeneration,
		},
		operations:    make(map[harnessv2.OperationID]harnessv2.OperationRecord),
		permissions:   make(map[harnessv2.PermissionRequestID]permissionState),
		providerProxy: proxy,
	}
	adapterResult := acp.PromptResult{
		Outcome: acp.PromptOutcomeCompleted, StopReason: acp.StopReasonEndTurn,
		Accepted: true, SettledAt: now,
	}
	terminal, settledResult, err := server.terminalEvent(state, prompt, adapterResult)
	if err != nil {
		t.Fatalf("build turn-limit terminal event: %v", err)
	}
	if terminal.Type != harnessv2.EventFailed || terminal.Failed == nil ||
		terminal.Failed.StopReason != harnessv2.ACPStopReasonMaxTurnRequests ||
		terminal.Failed.Code != "turn_limit" || terminal.Failed.Retryable {
		t.Fatalf("turn-limit terminal event = %#v", terminal)
	}
	if settledResult.Outcome != acp.PromptOutcomeFailed ||
		settledResult.StopReason != acp.StopReasonMaxTurnRequests || !settledResult.Accepted {
		t.Fatalf("turn-limit settled result = %#v", settledResult)
	}

	server.finishPrompt(state, prompt, settledResult, terminal.Identity.Timestamp)
	if state.descriptor.State != harnessv2.RuntimeSessionStatePoisoned || prompt.settlement == nil ||
		prompt.settlement.TerminalEvent != harnessv2.EventFailed ||
		prompt.settlement.StopReason != harnessv2.ACPStopReasonMaxTurnRequests {
		t.Fatalf("turn-limit settlement = state=%s settlement=%#v", state.descriptor.State, prompt.settlement)
	}
}

func TestTerminalResultLimitIncludesFullSerializedEvent(t *testing.T) {
	server, cfg, _ := newTestServer(t, "immediate")
	fence := cfg.Fence
	fence.RuntimeSessionUID = "session-uid"
	fence.RuntimeSessionGeneration = 1
	result := acp.PromptResult{
		Outcome: acp.PromptOutcomeCompleted, StopReason: acp.StopReasonEndTurn,
		Accepted: true, SettledAt: time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
	}
	text := strings.Repeat("x", 512)
	newStateAndPrompt := func() (*sessionState, *promptState) {
		prompt := &promptState{request: testStartPromptRequest(t, cfg, fence)}
		prompt.assistant.WriteString(text)
		return &sessionState{
			descriptor:  harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: fence.RuntimeSessionUID, Generation: fence.RuntimeSessionGeneration},
			operations:  make(map[harnessv2.OperationID]harnessv2.OperationRecord),
			permissions: make(map[harnessv2.PermissionRequestID]permissionState),
		}, prompt
	}

	state, prompt := newStateAndPrompt()
	prompt.sequence = 1
	completed := server.buildTerminalEventLocked(state, prompt, result, result.SettledAt)
	encodedCompleted, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	encodedPayload, err := json.Marshal(completed.Completed)
	if err != nil {
		t.Fatal(err)
	}
	limit := len(encodedCompleted) - 1
	if len(encodedPayload) > limit {
		t.Fatalf("test boundary does not isolate event overhead: payload=%d limit=%d event=%d", len(encodedPayload), limit, len(encodedCompleted))
	}

	state, prompt = newStateAndPrompt()
	server.cfg.Capabilities.Limits.MaxTerminalResultBytes = limit
	terminal, settledResult, err := server.terminalEvent(state, prompt, result)
	if err != nil {
		t.Fatalf("bounded terminal failure: %v", err)
	}
	if terminal.Type != harnessv2.EventFailed || terminal.Failed == nil || terminal.Failed.Code != "terminal_result_too_large" {
		t.Fatalf("oversized terminal event = %#v", terminal)
	}
	encodedFailure, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if len(encodedFailure) > limit {
		t.Fatalf("bounded failure size = %d, limit %d", len(encodedFailure), limit)
	}
	server.finishPrompt(state, prompt, settledResult, terminal.Identity.Timestamp)
	if state.descriptor.State != harnessv2.RuntimeSessionStatePoisoned || prompt.settlement == nil || prompt.settlement.TerminalEvent != harnessv2.EventFailed {
		t.Fatalf("oversized terminal did not poison before validation: state=%s settlement=%#v", state.descriptor.State, prompt.settlement)
	}

	state, prompt = newStateAndPrompt()
	server.cfg.Capabilities.Limits.MaxTerminalResultBytes = len(encodedCompleted)
	terminal, settledResult, err = server.terminalEvent(state, prompt, result)
	if err != nil {
		t.Fatalf("exact-boundary terminal event: %v", err)
	}
	if terminal.Type != harnessv2.EventCompleted || settledResult.Outcome != acp.PromptOutcomeCompleted {
		t.Fatalf("exact-boundary terminal event = %#v result=%#v", terminal, settledResult)
	}
	encodedExact, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if len(encodedExact) != len(encodedCompleted) {
		t.Fatalf("exact-boundary size = %d, want %d", len(encodedExact), len(encodedCompleted))
	}
}

type blockingPromptMutator struct {
	resolveCalls   atomic.Int32
	resolveOnce    sync.Once
	resolveEntered chan struct{}
	resolveRelease chan struct{}
	cancelCalls    atomic.Int32
	cancelOnce     sync.Once
	cancelEntered  chan struct{}
	cancelRelease  chan struct{}
	cancelResult   acp.PromptResult
}

func newBlockingPromptMutator() *blockingPromptMutator {
	return &blockingPromptMutator{
		resolveEntered: make(chan struct{}), resolveRelease: make(chan struct{}),
		cancelEntered: make(chan struct{}), cancelRelease: make(chan struct{}),
	}
}

func (m *blockingPromptMutator) ResolvePermission(string, string, acp.RequestPermissionOutcome) error {
	m.resolveCalls.Add(1)
	m.resolveOnce.Do(func() { close(m.resolveEntered) })
	<-m.resolveRelease
	return nil
}

func (m *blockingPromptMutator) CancelPrompt(context.Context, string) (acp.PromptResult, error) {
	m.cancelCalls.Add(1)
	m.cancelOnce.Do(func() { close(m.cancelEntered) })
	<-m.cancelRelease
	return m.cancelResult, nil
}

func serveMutationAsync(handler http.Handler, request *http.Request) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		done <- recorder
	}()
	return done
}

func awaitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

func assertStillWaiting(t *testing.T, done <-chan *httptest.ResponseRecorder, message string) {
	t.Helper()
	select {
	case response := <-done:
		t.Fatalf("%s: status=%d body=%s", message, response.Code, response.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
}

func awaitRecorder(t *testing.T, done <-chan *httptest.ResponseRecorder, message string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-done:
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", message, response.Code, response.Body.String())
		}
		return response
	case <-time.After(5 * time.Second):
		t.Fatal(message)
		return nil
	}
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
	}
}

func TestWorkspaceDeltaRepositoryControlPathAppliesWorkspaceRelativeRoot(t *testing.T) {
	if !workspaceDeltaRepositoryControlPathForWorkspace(".github/workflows", "ci.yml") {
		t.Fatal("workspace subpath change mapping to a repository-root workflow was allowed")
	}
	if workspaceDeltaRepositoryControlPathForWorkspace("website", ".github/workflows/ci.yml") {
		t.Fatal("nested workflow-like path outside the repository root was denied")
	}
}

func TestWorkspaceDeltaPathPolicy(t *testing.T) {
	result := workspacedelta.Result{
		Changes:   []workspacedelta.Change{{Path: "internal/controller/fix.go"}, {Path: "README.md"}},
		Deletions: []workspacedelta.Deletion{{Path: "internal/old.go"}},
	}
	paths := workspaceDeltaChangedPaths(result)
	if !slices.Equal(paths, []string{"README.md", "internal/controller/fix.go", "internal/old.go"}) {
		t.Fatalf("changed paths = %#v", paths)
	}
	for _, allowed := range []string{"internal/**", "README.md"} {
		if !workspaceDeltaPathAllowed(allowed, []string{"internal/**", "README.md"}) {
			t.Fatalf("expected %q to be allowed", allowed)
		}
	}
	if workspaceDeltaPathAllowed("website/sidebars.js", []string{"internal/**", "README.md"}) {
		t.Fatal("unexpected disallowed path match")
	}
}

func TestWorkspaceDeltaPathAllowedRecursiveGlobs(t *testing.T) {
	allowed := []struct {
		path     string
		patterns []string
	}{
		// `**` must span multiple directory levels, gitignore-style.
		{path: "src/pkg/util/helpers.go", patterns: []string{"src/**/*.go"}},
		{path: "src/one.go", patterns: []string{"src/**/*.go"}},
		{path: "main.go", patterns: []string{"**/*.go"}},
		{path: "a/b/c/d.txt", patterns: []string{"a/**/d.txt"}},
		{path: "a/d.txt", patterns: []string{"a/**/d.txt"}},
		{path: "internal/deep/nested/file.go", patterns: []string{"internal/**"}},
		{path: "docs/guide/intro.md", patterns: []string{"docs/*/intro.md"}},
	}
	for _, tt := range allowed {
		if !workspaceDeltaPathAllowed(tt.path, tt.patterns) {
			t.Errorf("expected %q to match %v", tt.path, tt.patterns)
		}
	}
	denied := []struct {
		path     string
		patterns []string
	}{
		{path: "src/pkg/util/helpers.txt", patterns: []string{"src/**/*.go"}},
		{path: "other/one.go", patterns: []string{"src/**/*.go"}},
		{path: "docs/guide/deep/intro.md", patterns: []string{"docs/*/intro.md"}},
		{path: "srcfile.go", patterns: []string{"src/**"}},
	}
	for _, tt := range denied {
		if workspaceDeltaPathAllowed(tt.path, tt.patterns) {
			t.Errorf("expected %q to not match %v", tt.path, tt.patterns)
		}
	}
	// Multiple ** segments over a long non-matching path must complete without
	// backtracking blowup.
	hostile := strings.Repeat("a/", 512) + "z.txt"
	if workspaceDeltaPathAllowed(hostile, []string{"**/b/**/c/**/d/**/e/**/*.go"}) {
		t.Fatal("hostile path unexpectedly matched")
	}
	if !workspaceDeltaPathAllowed("a/x/b/y/c/z/d/w/e/final.go", []string{"a/**/b/**/c/**/d/**/e/**/*.go"}) {
		t.Fatal("interleaved multi-star pattern failed to match")
	}
}

func TestWorkspaceDeltaRejectsSessionCredentials(t *testing.T) {
	state := &sessionState{
		providerProxy: &providerProxySession{credential: []byte("provider-session-secret")},
		mcpProxy:      &mcpProxySession{credential: []byte("mcp-session-secret")},
	}
	if !workspaceDeltaContainsSessionCredential([]byte("prefix provider-session-secret suffix"), state) {
		t.Fatal("provider credential was not detected")
	}
	if !workspaceDeltaContainsSessionCredential([]byte("prefix mcp-session-secret suffix"), state) {
		t.Fatal("MCP credential was not detected")
	}
	if workspaceDeltaContainsSessionCredential([]byte("safe workspace content"), state) {
		t.Fatal("safe artifact was rejected")
	}
}

func TestWorkspaceDeltaRepositoryControlAndContentPolicies(t *testing.T) {
	for _, path := range []string{".github/workflows/ci.yml", "config/rbac/role.yaml", "charts/orka/templates/publisher-secret.yaml"} {
		if !workspaceDeltaRepositoryControlPath(path) {
			t.Fatalf("protected path %q was allowed", path)
		}
	}
	if workspaceDeltaRepositoryControlPath("internal/controller/task.go") {
		t.Fatal("ordinary source path was denied")
	}

	artifact := tarBytes(t, tarEntry{name: "files/config.txt", body: []byte("api_key=0123456789abcdef")})
	violation, err := workspaceDeltaContentPolicyViolation(artifact, harnessv2.WorkspaceDeltaLimits{MaxBytes: 1 << 20, RejectSecretLikeContent: true})
	if err != nil || !strings.Contains(violation, "secret-like") {
		t.Fatalf("secret policy = %q, %v", violation, err)
	}
	artifact = tarBytes(t, tarEntry{name: "files/binary.bin", body: []byte{'a', 0, 'b'}})
	violation, err = workspaceDeltaContentPolicyViolation(artifact, harnessv2.WorkspaceDeltaLimits{MaxBytes: 1 << 20, RejectBinaryFiles: true})
	if err != nil || !strings.Contains(violation, "binary") {
		t.Fatalf("binary policy = %q, %v", violation, err)
	}
	artifact = tarBytes(t, tarEntry{name: "meta/symlinks.json", body: []byte(`{"symlinks":[{"path":"link","target":"api_key=0123456789abcdef"}]}`)})
	violation, err = workspaceDeltaContentPolicyViolation(artifact, harnessv2.WorkspaceDeltaLimits{MaxBytes: 1 << 20, RejectSecretLikeContent: true})
	if err != nil || !strings.Contains(violation, "secret-like") {
		t.Fatalf("symlink secret policy = %q, %v", violation, err)
	}
}
