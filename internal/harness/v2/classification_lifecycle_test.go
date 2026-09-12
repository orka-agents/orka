package v2

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const testOtherFenceValue = "other"

func TestClassifyOperationReplayMatrix(t *testing.T) {
	metadata := testMutationMetadata(t, true)
	metadata.RequestDigest = RequestDigest(testSHA256("request-a"))
	expected := metadata.Fence
	recordedAt := testNow.Add(-time.Minute)

	tests := []struct {
		name       string
		mutate     func(*MutationMetadata)
		record     *OperationRecord
		want       RequestClassification
		wantStatus int
	}{
		{name: "fresh", want: RequestClassificationFresh, wantStatus: 0},
		{
			name:   "same digest recorded",
			record: &OperationRecord{OperationID: metadata.OperationID, RequestDigest: metadata.RequestDigest, Phase: OperationPhaseRecorded, RecordedAt: recordedAt},
			want:   RequestClassificationDuplicate, wantStatus: http.StatusOK,
		},
		{
			name:   "already accepted",
			record: &OperationRecord{OperationID: metadata.OperationID, RequestDigest: metadata.RequestDigest, Phase: OperationPhaseAccepted, RecordedAt: recordedAt},
			want:   RequestClassificationAlreadyAccepted, wantStatus: http.StatusOK,
		},
		{
			name:   "settled",
			record: &OperationRecord{OperationID: metadata.OperationID, RequestDigest: metadata.RequestDigest, Phase: OperationPhaseSettled, TerminalEvent: EventCompleted, RecordedAt: recordedAt},
			want:   RequestClassificationSettled, wantStatus: http.StatusOK,
		},
		{
			name:   "digest conflict",
			record: &OperationRecord{OperationID: metadata.OperationID, RequestDigest: RequestDigest(testSHA256("request-b")), Phase: OperationPhaseAccepted, RecordedAt: recordedAt},
			want:   RequestClassificationDigestConflict, wantStatus: http.StatusConflict,
		},
		{
			name:   "stale instance wins before duplicate",
			mutate: func(m *MutationMetadata) { m.Fence.RuntimeInstanceID = "old-instance" },
			record: &OperationRecord{OperationID: metadata.OperationID, RequestDigest: metadata.RequestDigest, Phase: OperationPhaseAccepted, RecordedAt: recordedAt},
			want:   RequestClassificationStaleFence, wantStatus: http.StatusGone,
		},
		{
			name:   "expired",
			mutate: func(m *MutationMetadata) { m.ExpiresAt = testNow },
			want:   RequestClassificationExpired, wantStatus: http.StatusGone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			incoming := metadata
			if tt.mutate != nil {
				tt.mutate(&incoming)
			}
			got, err := ClassifyOperation(expected, incoming, tt.record, true, testNow)
			if err != nil {
				t.Fatalf("ClassifyOperation() error = %v", err)
			}
			if got.Class != tt.want || got.HTTPStatus() != tt.wantStatus {
				t.Fatalf("ClassifyOperation() = %#v (HTTP %d), want %q (HTTP %d)", got, got.HTTPStatus(), tt.want, tt.wantStatus)
			}
		})
	}
}

func TestCompareFenceClassifiesEveryGenerationBoundaryAsStale(t *testing.T) {
	expected := testFence(t)
	tests := map[FenceMismatch]func(*Fence){
		FenceMismatchRuntimeInstance:          func(f *Fence) { f.RuntimeInstanceID = testOtherFenceValue },
		FenceMismatchSupervisorBoot:           func(f *Fence) { f.SupervisorBootID = testOtherFenceValue },
		FenceMismatchControllerEpoch:          func(f *Fence) { f.ControllerEpoch++ },
		FenceMismatchRuntimePoolUID:           func(f *Fence) { f.RuntimePoolUID = testOtherFenceValue },
		FenceMismatchRuntimePoolGeneration:    func(f *Fence) { f.RuntimePoolGeneration++ },
		FenceMismatchRuntimeSessionUID:        func(f *Fence) { f.RuntimeSessionUID = testOtherFenceValue },
		FenceMismatchRuntimeSessionGeneration: func(f *Fence) { f.RuntimeSessionGeneration++ },
		FenceMismatchRuntimeProfile:           func(f *Fence) { f.RuntimeProfileDigest = ProfileDigest(testSHA256(testOtherFenceValue)) },
		FenceMismatchProfileSchema:            func(f *Fence) { f.ProfileDigestSchemaVersion++ },
	}
	for want, mutate := range tests {
		t.Run(string(want), func(t *testing.T) {
			request := expected
			mutate(&request)
			if got := CompareFence(expected, request, true); got != want {
				t.Fatalf("CompareFence() = %q, want %q", got, want)
			}
		})
	}
}

func TestRuntimeSessionTombstoneRetirementAndDuplicateProtection(t *testing.T) {
	tombstone := RuntimeSessionTombstone{
		RuntimeSessionUID:        "session-uid-1",
		RuntimeSessionGeneration: 5,
		RuntimeProfileDigest:     testFence(t).RuntimeProfileDigest,
		DeletedAt:                testNow,
		Operations: []OperationRecord{{
			OperationID:   "delete-op",
			RequestDigest: RequestDigest(testSHA256("delete-request")),
			Phase:         OperationPhaseDeleted,
			RecordedAt:    testNow.Add(-time.Minute),
		}},
	}
	if err := tombstone.Validate(); err != nil {
		t.Fatalf("tombstone.Validate() error = %v", err)
	}
	if tombstone.CanRetire("other-session", 99) || tombstone.CanRetire(tombstone.RuntimeSessionUID, 4) {
		t.Fatal("tombstone retired before exact generation retirement")
	}
	if !tombstone.CanRetire(tombstone.RuntimeSessionUID, 5) {
		t.Fatal("tombstone did not retire at retired-through generation")
	}

	tombstone.Operations = append(tombstone.Operations, tombstone.Operations[0])
	if err := tombstone.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate tombstone operation validation = %v, want duplicate error", err)
	}
}

func TestRuntimeSessionLifecycleAndSafeDeletion(t *testing.T) {
	valid := [][2]RuntimeSessionState{
		{RuntimeSessionStateCreating, RuntimeSessionStateIdle},
		{RuntimeSessionStateIdle, RuntimeSessionStatePromptRunning},
		{RuntimeSessionStatePromptRunning, RuntimeSessionStateValidating},
		{RuntimeSessionStateValidating, RuntimeSessionStateIdle},
		{RuntimeSessionStateValidating, RuntimeSessionStatePreparingPublication},
		{RuntimeSessionStatePreparingPublication, RuntimeSessionStatePublicationPrepared},
		{RuntimeSessionStatePublicationPrepared, RuntimeSessionStatePublishing},
		{RuntimeSessionStatePublicationPrepared, RuntimeSessionStateFinalizing},
		{RuntimeSessionStatePublishing, RuntimeSessionStateVerifying},
		{RuntimeSessionStateFinalizing, RuntimeSessionStateDeleting},
		{RuntimeSessionStateIdle, RuntimeSessionStateDeleting},
		{RuntimeSessionStateDeleting, RuntimeSessionStateDeleted},
	}
	for _, transition := range valid {
		if err := ValidateRuntimeSessionTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("ValidateRuntimeSessionTransition(%s,%s) error = %v", transition[0], transition[1], err)
		}
	}

	for _, transition := range [][2]RuntimeSessionState{
		{RuntimeSessionStateCreating, RuntimeSessionStatePromptRunning},
		{RuntimeSessionStatePromptRunning, RuntimeSessionStateIdle},
		{RuntimeSessionStateValidating, RuntimeSessionStateFinalizing},
		{RuntimeSessionStatePublishing, RuntimeSessionStateCancelling},
		{RuntimeSessionStateVerifying, RuntimeSessionStateFinalizing},
		{RuntimeSessionStateFinalizing, RuntimeSessionStateIdle},
		{RuntimeSessionStateDeleted, RuntimeSessionStateIdle},
	} {
		if err := ValidateRuntimeSessionTransition(transition[0], transition[1]); err == nil {
			t.Fatalf("ValidateRuntimeSessionTransition(%s,%s) error = nil, want rejection", transition[0], transition[1])
		}
	}

	if !RuntimeSessionStateIdle.CanEvict() || RuntimeSessionStateValidating.CanEvict() {
		t.Fatal("only idle runtime sessions may be evicted")
	}
	if next, err := DeletionTransition(RuntimeSessionStatePromptRunning); err != nil || next != RuntimeSessionStateCancelling {
		t.Fatalf("DeletionTransition(prompt_running) = %q, %v", next, err)
	}
	if next, err := DeletionTransition(RuntimeSessionStatePublishing); err == nil || next != RuntimeSessionStatePublishing {
		t.Fatalf("DeletionTransition(publishing) = %q, %v, want blocked in publishing", next, err)
	}
	if next, err := DeletionTransition(RuntimeSessionStateFinalizing); err != nil || next != RuntimeSessionStateDeleting {
		t.Fatalf("DeletionTransition(finalizing) = %q, %v, want retirement deletion", next, err)
	}
}

func TestStopReasonMappingFailsClosed(t *testing.T) {
	tests := []struct {
		reason   ACPStopReason
		proven   bool
		event    EventType
		outcome  PromptOutcome
		validate bool
		poison   bool
	}{
		{ACPStopReasonEndTurn, true, EventCompleted, PromptOutcomeSucceeded, true, false},
		{ACPStopReasonCancelled, true, EventCancelled, PromptOutcomeCancelled, false, true},
		{ACPStopReasonMaxTokens, true, EventFailed, PromptOutcomeFailed, false, true},
		{ACPStopReasonMaxTurnRequests, true, EventFailed, PromptOutcomeFailed, false, true},
		{ACPStopReasonRefusal, true, EventFailed, PromptOutcomeFailed, false, true},
		{ACPStopReason("new_provider_reason"), true, EventFailed, PromptOutcomeFailed, false, true},
		{ACPStopReasonEndTurn, false, EventOutcomeUnknown, PromptOutcomeUnknown, false, true},
	}
	for _, tt := range tests {
		mapping := MapACPStopReason(tt.reason, tt.proven)
		if mapping.EventType != tt.event || mapping.Outcome != tt.outcome ||
			mapping.CanValidateWorkspace != tt.validate || mapping.PoisonSession != tt.poison {
			t.Fatalf("MapACPStopReason(%q,%v) = %#v", tt.reason, tt.proven, mapping)
		}
	}
	if !PromptOutcomeUnknown.RequiresExplicitNewTask() {
		t.Fatal("OutcomeUnknown must require an explicit new Task")
	}
}
