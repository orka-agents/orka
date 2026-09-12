package v2

import (
	"strings"
	"testing"
	"time"
)

func TestCreateRuntimeSessionRequestAndResponseValidation(t *testing.T) {
	profile := testRuntimeProfile()
	request := CreateRuntimeSessionRequest{
		Protocol:           ProtocolVersion,
		Metadata:           testMutationMetadata(t, false),
		RuntimeSessionID:   "runtime-session-1",
		Profile:            profile,
		AgentConfiguration: testAgentSessionConfigurationPointer(),
		MCPConfiguration:   testMCPPolicyConfiguration(),
		Workspace: WorkspaceSpec{
			Intent:   WorkspaceIntentWrite,
			Baseline: testWorkspaceBaseline(),
		},
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatalf("CreateRuntimeSessionRequest.ValidateAt() error = %v", err)
	}

	response := CreateRuntimeSessionResponse{
		Protocol:       ProtocolVersion,
		Classification: Classification{Class: RequestClassificationFresh},
		Session: RuntimeSessionDescriptor{
			RuntimeSessionID:     request.RuntimeSessionID,
			RuntimeSessionUID:    request.Metadata.Fence.RuntimeSessionUID,
			Generation:           request.Metadata.Fence.RuntimeSessionGeneration,
			RuntimeInstanceID:    request.Metadata.Fence.RuntimeInstanceID,
			SupervisorBootID:     request.Metadata.Fence.SupervisorBootID,
			RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest,
			State:                RuntimeSessionStateIdle,
			ProviderSessionID:    "provider-session-1",
			WorkspaceBaseline:    request.Workspace.Baseline,
			CreatedAt:            testNow,
			LastTransitionAt:     testNow,
		},
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("CreateRuntimeSessionResponse.ValidateFor() error = %v", err)
	}

	request.Profile.Model = "changed-after-seal"
	if err := request.ValidateAt(testNow); err == nil {
		t.Fatal("mutated sealed session request passed validation")
	}
}

func TestAgentSessionConfigurationValidation(t *testing.T) {
	base := testAgentSessionConfiguration()
	if err := base.Validate(); err != nil {
		t.Fatalf("AgentSessionConfiguration.Validate() error = %v", err)
	}
	claudeMax := base
	claudeMax.ProviderKind = "claude"
	claudeMax.ReasoningEffort = "max"
	if err := claudeMax.Validate(); err != nil {
		t.Fatalf("Claude max reasoning effort validation error = %v", err)
	}
	maximumPrompt := base
	maximumPrompt.SystemPrompt = strings.Repeat("x", MaxAgentSystemPromptBytes)
	if err := maximumPrompt.Validate(); err != nil {
		t.Fatalf("maximum-size system prompt validation error = %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*AgentSessionConfiguration)
		wantErr string
	}{
		{name: "missing agent UID", mutate: func(configuration *AgentSessionConfiguration) { configuration.AgentUID = "" }, wantErr: "agent UID is required"},
		{name: "non-positive generation", mutate: func(configuration *AgentSessionConfiguration) { configuration.AgentGeneration = 0 }, wantErr: "generation must be positive"},
		{name: "missing provider", mutate: func(configuration *AgentSessionConfiguration) { configuration.ProviderKind = "" }, wantErr: "provider kind is required"},
		{name: "missing model", mutate: func(configuration *AgentSessionConfiguration) { configuration.Model = "" }, wantErr: "model is required"},
		{name: "zero max turns", mutate: func(configuration *AgentSessionConfiguration) { configuration.MaxTurns = 0 }, wantErr: "range 1..1000"},
		{name: "excess max turns", mutate: func(configuration *AgentSessionConfiguration) { configuration.MaxTurns = MaxAgentMaxTurns + 1 }, wantErr: "range 1..1000"},
		{name: "unsupported effort", mutate: func(configuration *AgentSessionConfiguration) { configuration.ReasoningEffort = "extreme" }, wantErr: "unsupported reasoning effort"},
		{name: "codex max effort", mutate: func(configuration *AgentSessionConfiguration) { configuration.ReasoningEffort = "max" }, wantErr: "codex provider does not support"},
		{name: "copilot effort", mutate: func(configuration *AgentSessionConfiguration) {
			configuration.ProviderKind = "copilot"
			configuration.ReasoningEffort = "high"
		}, wantErr: "copilot provider does not support"},
		{name: "custom provider effort", mutate: func(configuration *AgentSessionConfiguration) { configuration.ProviderKind = "custom" }, wantErr: "does not support reasoning effort"},
		{name: "oversized system prompt", mutate: func(configuration *AgentSessionConfiguration) {
			configuration.SystemPrompt = strings.Repeat("x", MaxAgentSystemPromptBytes+1)
		}, wantErr: "agent system prompt exceeds"},
		{name: "invalid UTF-8 system prompt", mutate: func(configuration *AgentSessionConfiguration) {
			configuration.SystemPrompt = string([]byte{0xff})
		}, wantErr: "invalid UTF-8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configuration := base
			test.mutate(&configuration)
			if err := configuration.Validate(); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("AgentSessionConfiguration.Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestCreateRuntimeSessionRequestRejectsAgentConfigurationDrift(t *testing.T) {
	newRequest := func() CreateRuntimeSessionRequest {
		request := CreateRuntimeSessionRequest{
			Protocol:           ProtocolVersion,
			Metadata:           testMutationMetadata(t, false),
			RuntimeSessionID:   "runtime-session-1",
			Profile:            testRuntimeProfile(),
			AgentConfiguration: testAgentSessionConfigurationPointer(),
			MCPConfiguration:   testMCPPolicyConfiguration(),
			Workspace: WorkspaceSpec{
				Intent:   WorkspaceIntentWrite,
				Baseline: testWorkspaceBaseline(),
			},
		}
		return request
	}

	tests := []struct {
		name    string
		mutate  func(*CreateRuntimeSessionRequest)
		wantErr string
	}{
		{name: "canonical digest", mutate: func(request *CreateRuntimeSessionRequest) {
			request.AgentConfiguration.SystemPrompt += " drift"
		}, wantErr: "agent configuration digest mismatch"},
		{name: "provider", mutate: func(request *CreateRuntimeSessionRequest) {
			request.AgentConfiguration.ProviderKind = "claude"
		}, wantErr: "does not match runtime profile provider kind"},
		{name: "model", mutate: func(request *CreateRuntimeSessionRequest) {
			request.AgentConfiguration.Model = "other-model"
		}, wantErr: "does not match runtime profile model"},
		{name: "max turns", mutate: func(request *CreateRuntimeSessionRequest) {
			request.AgentConfiguration.MaxTurns++
		}, wantErr: "agent configuration digest mismatch"},
		{name: "reasoning effort", mutate: func(request *CreateRuntimeSessionRequest) {
			request.AgentConfiguration.ReasoningEffort = reasoningEffortMedium
		}, wantErr: "agent configuration digest mismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newRequest()
			test.mutate(&request)
			sealRequest(t, request, &request.Metadata.RequestDigest)
			if err := request.ValidateAt(testNow); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("CreateRuntimeSessionRequest.ValidateAt() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestFinalizeRuntimeSessionPublicationValidation(t *testing.T) {
	request := FinalizeRuntimeSessionPublicationRequest{
		Protocol: ProtocolVersion, Metadata: testMutationMetadata(t, true), WorkspaceDeltaID: "delta-1",
		PublicationID: "publication-1", PublicationGeneration: 1, PublicationVersion: 7,
		TerminalState: PublicationTerminalVerifiedExact, TerminalReceiptDigest: testSHA256("publication-receipt"),
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatalf("FinalizeRuntimeSessionPublicationRequest.ValidateAt() error = %v", err)
	}
	response := FinalizeRuntimeSessionPublicationResponse{
		Protocol: ProtocolVersion, Classification: Classification{Class: RequestClassificationFresh},
		Session: RuntimeSessionDescriptor{
			RuntimeSessionID: "runtime-session-1", RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID,
			Generation: request.Metadata.Fence.RuntimeSessionGeneration, RuntimeInstanceID: request.Metadata.Fence.RuntimeInstanceID,
			SupervisorBootID: request.Metadata.Fence.SupervisorBootID, RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest,
			State: RuntimeSessionStateFinalizing, ProviderSessionID: "provider-session-1", WorkspaceBaseline: testWorkspaceBaseline(),
			CreatedAt: testNow, LastTransitionAt: testNow,
		},
		Finalization: PublicationFinalizationReceipt{
			WorkspaceDeltaID: request.WorkspaceDeltaID, PublicationID: request.PublicationID,
			PublicationGeneration: request.PublicationGeneration, PublicationVersion: request.PublicationVersion,
			TerminalState: request.TerminalState, TerminalReceiptDigest: request.TerminalReceiptDigest, AppliedAt: testNow,
		},
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("FinalizeRuntimeSessionPublicationResponse.ValidateFor() error = %v", err)
	}

	nonRetiring := response
	nonRetiring.Session.State = RuntimeSessionStateIdle
	if err := nonRetiring.ValidateFor(request); err == nil || !strings.Contains(err.Error(), "must return finalizing") {
		t.Fatalf("non-retiring publication finalization response validation = %v", err)
	}
	mismatchedReceipt := response
	mismatchedReceipt.Finalization.PublicationVersion++
	if err := mismatchedReceipt.ValidateFor(request); err == nil || !strings.Contains(err.Error(), "does not match request") {
		t.Fatalf("mismatched publication finalization receipt validation = %v", err)
	}

	request.TerminalState = PublicationTerminalState("publishing")
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err == nil || !strings.Contains(err.Error(), "terminal publication state") {
		t.Fatalf("nonterminal publication finalization validation = %v", err)
	}
}

func TestDeleteRuntimeSessionResponseValidationBindsRequest(t *testing.T) {
	request := DeleteRuntimeSessionRequest{
		Protocol: ProtocolVersion, Metadata: testMutationMetadata(t, false), Reason: "cleanup",
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	response := DeleteRuntimeSessionResponse{
		Protocol: ProtocolVersion, Classification: Classification{Class: RequestClassificationFresh},
		State: RuntimeSessionStateDeleted,
		Tombstone: RuntimeSessionTombstone{
			RuntimeSessionUID:        request.Metadata.Fence.RuntimeSessionUID,
			RuntimeSessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration,
			RuntimeProfileDigest:     request.Metadata.Fence.RuntimeProfileDigest,
			DeletedAt:                testNow,
			Operations: []OperationRecord{{
				OperationID: request.Metadata.OperationID, RequestDigest: request.Metadata.RequestDigest,
				Phase: OperationPhaseDeleted, RecordedAt: testNow, UpdatedAt: testNow,
			}},
		},
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("DeleteRuntimeSessionResponse.ValidateFor() error = %v", err)
	}

	wrongPhase := response
	wrongPhase.Classification = Classification{Class: RequestClassificationDuplicate, Phase: OperationPhaseApplied}
	if err := wrongPhase.ValidateFor(request); err == nil || !strings.Contains(err.Error(), "deleted phase") {
		t.Fatalf("duplicate non-deleted response validation = %v", err)
	}

	mismatched := response
	mismatched.Tombstone.RuntimeSessionGeneration++
	if err := mismatched.ValidateFor(request); err == nil || !strings.Contains(err.Error(), "does not match request fence") {
		t.Fatalf("mismatched tombstone validation = %v", err)
	}

	missingOperation := response
	missingOperation.Tombstone.Operations = nil
	if err := missingOperation.ValidateFor(request); err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("missing delete operation validation = %v", err)
	}

	mismatchedDigest := response
	mismatchedDigest.Tombstone.Operations = append([]OperationRecord(nil), response.Tombstone.Operations...)
	mismatchedDigest.Tombstone.Operations[0].RequestDigest = RequestDigest(testSHA256("other-delete-request"))
	if err := mismatchedDigest.ValidateFor(request); err == nil || !strings.Contains(err.Error(), "digest does not match") {
		t.Fatalf("mismatched delete operation digest validation = %v", err)
	}

	conflict := response
	conflict.Classification = Classification{Class: RequestClassificationDigestConflict, Phase: OperationPhaseDeleted}
	if err := conflict.ValidateFor(request); err == nil || !strings.Contains(err.Error(), "classification") {
		t.Fatalf("digest-conflict success response validation = %v", err)
	}
}

func TestRuntimeSessionStatusFinalizationReservationValidation(t *testing.T) {
	base := RuntimeSessionStatus{
		RuntimeSessionID: "runtime-session-1", RuntimeSessionUID: "session-uid-1", Generation: 1,
		State: RuntimeSessionStateIdle, LastTransitionAt: testNow,
	}
	tests := []struct {
		name     string
		state    RuntimeSessionState
		reserved bool
		wantErr  string
	}{
		{name: "idle", state: RuntimeSessionStateIdle},
		{name: "finalizing with accepted receipt", state: RuntimeSessionStateFinalizing, reserved: true},
		{name: "deleting finalized session", state: RuntimeSessionStateDeleting, reserved: true},
		{name: "deleting ordinary session", state: RuntimeSessionStateDeleting},
		{name: "poisoned finalized session", state: RuntimeSessionStatePoisoned, reserved: true},
		{name: "poisoned ordinary session", state: RuntimeSessionStatePoisoned},
		{name: "finalizing without accepted receipt", state: RuntimeSessionStateFinalizing, wantErr: "must be reserved"},
		{name: "prepared without accepted receipt", state: RuntimeSessionStatePublicationPrepared},
		{name: "prepared falsely reserved", state: RuntimeSessionStatePublicationPrepared, reserved: true, wantErr: "only valid while finalizing, deleting, or poisoned"},
		{name: "idle falsely reserved", state: RuntimeSessionStateIdle, reserved: true, wantErr: "only valid while finalizing, deleting, or poisoned"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := base
			status.State = tt.state
			status.ReservedForFinalization = tt.reserved
			err := status.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("RuntimeSessionStatus.Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RuntimeSessionStatus.Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestStartPromptLeaseAndPromptScopedMCPValidation(t *testing.T) {
	request := testStartPromptRequest(t)
	if err := request.ValidateAt(testNow, 5*time.Second, 3*time.Minute); err != nil {
		t.Fatalf("StartPromptRequest.ValidateAt() error = %v", err)
	}
	if !request.MCPAuthorization.AuthorizedAt(RuntimeSessionStatePromptRunning, request.Lease, testNow.Add(30*time.Second)) {
		t.Fatal("active prompt-scoped MCP authorization was denied")
	}
	for _, state := range []RuntimeSessionState{
		RuntimeSessionStateIdle,
		RuntimeSessionStateValidating,
		RuntimeSessionStatePublishing,
		RuntimeSessionStatePoisoned,
		RuntimeSessionStateDeleted,
	} {
		if request.MCPAuthorization.AuthorizedAt(state, request.Lease, testNow.Add(30*time.Second)) {
			t.Fatalf("MCP authorization unexpectedly active in state %s", state)
		}
	}
	if request.MCPAuthorization.AuthorizedAt(RuntimeSessionStatePromptRunning, request.Lease, request.MCPAuthorization.ExpiresAt) {
		t.Fatal("expired MCP authorization remained active")
	}

	request.Input.Content[0].Text = "changed after request acceptance"
	if err := request.ValidateAt(testNow, 5*time.Second, 3*time.Minute); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("mutated prompt request validation = %v, want digest error", err)
	}
}

func TestPromptLeaseRenewalRejectsStaleAndUnboundedExtension(t *testing.T) {
	current := PromptLease{Generation: 4, IssuedAt: testNow, ExpiresAt: testNow.Add(30 * time.Second)}
	renewAt := testNow.Add(10 * time.Second)
	proposed := PromptLease{Generation: 5, IssuedAt: renewAt, ExpiresAt: renewAt.Add(45 * time.Second)}
	if err := ValidatePromptLeaseRenewal(current, proposed, 4, renewAt, time.Minute); err != nil {
		t.Fatalf("ValidatePromptLeaseRenewal() error = %v", err)
	}
	if err := ValidatePromptLeaseRenewal(current, proposed, 3, renewAt, time.Minute); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale renewal error = %v", err)
	}
	proposed.ExpiresAt = renewAt.Add(2 * time.Minute)
	if err := ValidatePromptLeaseRenewal(current, proposed, 4, renewAt, time.Minute); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("unbounded renewal error = %v", err)
	}
}

func TestPermissionDecisionAndLateCancellationSemantics(t *testing.T) {
	if err := (PermissionDecision{Outcome: PermissionDecisionCancelled, OptionID: "allow"}).Validate(); err == nil {
		t.Fatal("cancelled permission decision selected an option")
	}
	if err := (PermissionDecision{Outcome: PermissionDecisionSelected, OptionID: "allow-once"}).Validate(); err != nil {
		t.Fatalf("selected permission decision error = %v", err)
	}

	request := ResolvePermissionRequest{
		Protocol:  ProtocolVersion,
		Metadata:  testMutationMetadata(t, true),
		RequestID: "permission-1",
		Decision:  PermissionDecision{Outcome: PermissionDecisionSelected, OptionID: "allow-once"},
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatalf("ResolvePermissionRequest.ValidateAt() error = %v", err)
	}

	response := PermissionResolutionResponse{
		Protocol:       ProtocolVersion,
		Classification: Classification{Class: RequestClassificationStaleFence, FenceMismatch: FenceMismatchRuntimeSessionGeneration},
		State:          PermissionResolutionCancelledByPrompt,
		Decision:       PermissionDecision{Outcome: PermissionDecisionCancelled},
		ResolvedAt:     testNow,
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("late cancelled permission response validation error = %v", err)
	}
	if response.Classification.HTTPStatus() != 410 {
		t.Fatalf("late permission HTTP status = %d, want 410", response.Classification.HTTPStatus())
	}
}

func TestCancellationBarrierRequiresOutcomeUnknownWhenSettlementUnproven(t *testing.T) {
	request := CancelPromptRequest{
		Protocol:           ProtocolVersion,
		Metadata:           testMutationMetadata(t, true),
		Reason:             CancelReasonStreamDisconnected,
		SettlementDeadline: testNow.Add(30 * time.Second),
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatalf("CancelPromptRequest.ValidateAt() error = %v", err)
	}

	unknown := CancelPromptResponse{
		Protocol:       ProtocolVersion,
		Classification: Classification{Class: RequestClassificationFresh},
		BarrierState:   CancellationBarrierOutcomeUnknown,
		Settlement: PromptSettlement{
			TerminalEvent: EventOutcomeUnknown,
			Outcome:       PromptOutcomeUnknown,
			SettledAt:     testNow.Add(20 * time.Second),
		},
		ForcedTermination: true,
	}
	if err := unknown.Validate(); err != nil {
		t.Fatalf("outcome-unknown cancellation response error = %v", err)
	}

	unknown.BarrierState = CancellationBarrierForcedTerminated
	if err := unknown.Validate(); err == nil || !strings.Contains(err.Error(), "unproven") {
		t.Fatalf("unproven forced termination validation = %v", err)
	}
}

func TestWorkspaceDeltaValidationEnforcesIntentAndPublicationSafety(t *testing.T) {
	request := CreateWorkspaceDeltaRequest{
		Protocol:               ProtocolVersion,
		Metadata:               testMutationMetadata(t, true),
		DeltaID:                "delta-1",
		Intent:                 WorkspaceIntentWrite,
		VerifiedBaseline:       testWorkspaceBaseline(),
		PromptSettlementDigest: testSHA256("settlement"),
		Limits:                 WorkspaceDeltaLimits{MaxBytes: 10 << 20, MaxEntries: 1000},
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatalf("CreateWorkspaceDeltaRequest.ValidateAt() error = %v", err)
	}

	response := CreateWorkspaceDeltaResponse{
		Protocol:       ProtocolVersion,
		Classification: Classification{Class: RequestClassificationFresh},
		Delta: WorkspaceDeltaDescriptor{
			DeltaID:           request.DeltaID,
			RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID,
			SessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration,
			State:             WorkspaceDeltaPrepared,
			Intent:            WorkspaceIntentWrite,
			VerifiedBaseline:  request.VerifiedBaseline,
			RelativeRoot:      "services/app",
			ManifestDigest:    testSHA256("manifest"),
			Artifact: &ArtifactReference{
				ArtifactID: "artifact-1",
				Digest:     testSHA256("artifact"),
				SizeBytes:  1024,
				MediaType:  "application/vnd.orka.workspace-delta+tar",
			},
			EntryCount:       2,
			ChangedFileCount: 2,
			NoFollowVerified: true,
			PublicationSafe:  true,
			FrozenAt:         testNow,
		},
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("CreateWorkspaceDeltaResponse.ValidateFor() error = %v", err)
	}

	response.Delta.RelativeRoot = "../services/app"
	if err := response.Delta.Validate(); err == nil || !strings.Contains(err.Error(), "unsafe segment") {
		t.Fatalf("unsafe workspace relative root validation = %v", err)
	}
	response.Delta.RelativeRoot = "services/app"

	response.Delta.Intent = WorkspaceIntentRead
	if err := response.Delta.Validate(); err == nil || !strings.Contains(err.Error(), "write intent") {
		t.Fatalf("prepared read-only delta validation = %v", err)
	}
}

func TestCapabilitiesStatusAndDrainContracts(t *testing.T) {
	limits := DefaultProtocolLimits()
	if limits.MaxResidentSessions != 10 || limits.MaxConcurrentPrompts != 4 {
		t.Fatalf("default pool limits = %d/%d, want 10/4", limits.MaxResidentSessions, limits.MaxConcurrentPrompts)
	}
	capabilities := CapabilitiesResponse{
		Protocol:                   ProtocolVersion,
		Transport:                  "http+ndjson",
		ACPVersion:                 ACPProfileV1,
		RuntimeProfileDigest:       testFence(t).RuntimeProfileDigest,
		ProfileDigestSchemaVersion: ProfileDigestSchemaVersion,
		AdapterDigests:             map[string]string{"codex-acp": testSHA256("adapter")},
		Limits:                     limits,
		Provider: ProviderCapabilities{
			ProviderKinds:       []string{"codex"},
			Models:              []string{"test-model"},
			SupportsPermissions: true,
			SupportsCancel:      true,
			SupportsTools:       true,
		},
		WorkspaceGovernance: StrictWorkspaceGovernanceCapabilities(),
		SupportsDrain:       true,
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("CapabilitiesResponse.Validate() error = %v", err)
	}

	poolFence := testFence(t)
	poolFence.RuntimeSessionUID = ""
	poolFence.RuntimeSessionGeneration = 0
	status := StatusResponse{
		Protocol:                ProtocolVersion,
		Fence:                   poolFence,
		Lifecycle:               SupervisorLifecycleReady,
		Drain:                   DrainStatus{AcceptingNewSessions: true},
		SessionIdentityCapacity: &SessionIdentityCapacity{Total: 10_000, Remaining: 9_999, ExhaustionReserve: 1},
		Sessions: []RuntimeSessionStatus{{
			RuntimeSessionID:  "runtime-session-1",
			RuntimeSessionUID: "session-uid-1",
			Generation:        5,
			State:             RuntimeSessionStateIdle,
			LastTransitionAt:  testNow,
		}},
		Pressure:  PressureMetadata{ResidentSessions: 1},
		Timestamp: testNow,
	}
	if err := status.Validate(); err != nil {
		t.Fatalf("StatusResponse.Validate() error = %v", err)
	}
	if status.SessionIdentityCapacity.RotationRequired() {
		t.Fatal("healthy identity capacity unexpectedly requires rotation")
	}
	status.SessionIdentityCapacity.Remaining = 1
	if !status.SessionIdentityCapacity.RotationRequired() {
		t.Fatal("identity reserve watermark did not require rotation")
	}
	status.SessionIdentityCapacity.Remaining = 9_999
	status.Pressure.ActivePrompts = 1
	if err := status.Validate(); err == nil || !strings.Contains(err.Error(), "active prompt count") {
		t.Fatalf("inconsistent status count validation = %v", err)
	}

	drain := DrainRequest{
		Protocol: ProtocolVersion,
		Metadata: MutationMetadata{
			Fence:                      poolFence,
			OperationID:                "drain-1",
			RequestDigestSchemaVersion: RequestDigestSchemaVersion,
			ExpiresAt:                  testNow.Add(time.Minute),
		},
		Reason: "rollout",
	}
	sealRequest(t, drain, &drain.Metadata.RequestDigest)
	if err := drain.ValidateAt(testNow); err != nil {
		t.Fatalf("DrainRequest.ValidateAt() error = %v", err)
	}
}

func TestTerminalErrorsAreNeverRetryable(t *testing.T) {
	for _, code := range []ErrorCode{ErrorCodeOutcomeUnknown, ErrorCodeWorkspaceResumeLost} {
		t.Run(string(code), func(t *testing.T) {
			response := ErrorResponse{
				Protocol:  ProtocolVersion,
				Code:      code,
				Message:   "terminal operation failure",
				Retryable: true,
			}
			if err := response.Validate(); err == nil || !strings.Contains(err.Error(), "never") {
				t.Fatalf("retryable %s validation = %v", code, err)
			}
		})
	}
}

func TestWorkspaceDeltaLimitsRejectUnsafePathPolicies(t *testing.T) {
	for _, pattern := range []string{"../outside", "/absolute", `bad\\path`, "["} {
		limits := WorkspaceDeltaLimits{MaxBytes: 1024, MaxEntries: 10, AllowedPaths: []string{pattern}}
		if err := limits.Validate(); err == nil {
			t.Fatalf("Validate() accepted unsafe pattern %q", pattern)
		}
	}
	limits := WorkspaceDeltaLimits{MaxBytes: 1024, MaxEntries: 10, MaxChangedFiles: 2, AllowedPaths: []string{"internal/**", "README.md"}}
	if err := limits.Validate(); err != nil {
		t.Fatalf("Validate() rejected valid policy: %v", err)
	}
}
