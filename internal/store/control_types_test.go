package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func testControlDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPromptExecutionStateMachineIncludesAmbiguousSubmission(t *testing.T) {
	valid := [][2]PromptExecutionState{
		{PromptExecutionQueued, PromptExecutionReserved},
		{PromptExecutionReserved, PromptExecutionSessionStarting},
		{PromptExecutionSessionStarting, PromptExecutionPlanned},
		{PromptExecutionPlanned, PromptExecutionSubmitting},
		{PromptExecutionSubmitting, PromptExecutionSubmittedUnknown},
		{PromptExecutionSubmittedUnknown, PromptExecutionOutcomeUnknown},
		{PromptExecutionSubmitting, PromptExecutionAccepted},
		{PromptExecutionAccepted, PromptExecutionRunning},
		{PromptExecutionRunning, PromptExecutionSettling},
		{PromptExecutionSettling, PromptExecutionSucceeded},
	}
	for _, transition := range valid {
		if err := ValidatePromptExecutionTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("transition %s -> %s: %v", transition[0], transition[1], err)
		}
	}
	invalid := [][2]PromptExecutionState{
		{PromptExecutionSubmittedUnknown, PromptExecutionSubmitting},
		{PromptExecutionOutcomeUnknown, PromptExecutionQueued},
		{PromptExecutionSucceeded, PromptExecutionRunning},
		{PromptExecutionQueued, PromptExecutionAccepted},
	}
	for _, transition := range invalid {
		if err := ValidatePromptExecutionTransition(transition[0], transition[1]); !errors.Is(err, ErrValidation) {
			t.Fatalf("transition %s -> %s error = %v, want ErrValidation", transition[0], transition[1], err)
		}
	}
	if !IsTerminalPromptExecutionState(PromptExecutionOutcomeUnknown) {
		t.Fatalf("OutcomeUnknown must be terminal")
	}
}

func TestDeliveryAndPublicationCASRaceStateMachines(t *testing.T) {
	for _, transition := range [][2]PromptDeliveryState{
		{PromptDeliveryNotRequested, PromptDeliveryValidating},
		{PromptDeliveryValidating, PromptDeliveryPreparing},
		{PromptDeliveryPreparing, PromptDeliveryPrepared},
		{PromptDeliveryPrepared, PromptDeliveryPublishing},
		{PromptDeliveryPrepared, PromptDeliveryCancelledBeforePublish},
		{PromptDeliveryPublishing, PromptDeliveryVerifying},
		{PromptDeliveryVerifying, PromptDeliveryVerifiedExact},
		{PromptDeliveryVerifying, PromptDeliveryPublicationOutcomeUnknown},
	} {
		if err := ValidatePromptDeliveryTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("delivery %s -> %s: %v", transition[0], transition[1], err)
		}
	}
	if err := ValidatePromptDeliveryTransition(PromptDeliveryCancelledBeforePublish, PromptDeliveryPublishing); !errors.Is(err, ErrValidation) {
		t.Fatalf("cancelled delivery became publishable: %v", err)
	}

	for _, transition := range [][2]PublicationState{
		{PublicationPreparing, PublicationPrepared},
		{PublicationPrepared, PublicationPublishing},
		{PublicationPrepared, PublicationCancelledBeforePublish},
		{PublicationPublishing, PublicationVerifying},
		{PublicationVerifying, PublicationVerifiedExact},
		{PublicationVerifying, PublicationDeliveredSuperseded},
		{PublicationVerifying, PublicationDeliveryConflict},
		{PublicationVerifying, PublicationOutcomeUnknown},
	} {
		if err := ValidatePublicationTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("publication %s -> %s: %v", transition[0], transition[1], err)
		}
	}
	if err := ValidatePublicationTransition(PublicationCancelledBeforePublish, PublicationPublishing); !errors.Is(err, ErrValidation) {
		t.Fatalf("cancelled publication became publishable: %v", err)
	}
}

func TestCanonicalControlIdentifiersAreLengthDelimited(t *testing.T) {
	a := CanonicalControlID("kind", "ab", "c")
	b := CanonicalControlID("kind", "a", "bc")
	if a == b {
		t.Fatalf("length-delimited identities collided: %q", a)
	}
	key := PromptAttemptKey{Namespace: "ns", TaskUID: "task-uid", Attempt: 2, PromptID: "prompt"}
	id1, err := key.CanonicalID()
	if err != nil {
		t.Fatalf("CanonicalID: %v", err)
	}
	id2, err := key.CanonicalID()
	if err != nil || id1 != id2 {
		t.Fatalf("canonical prompt ID is unstable: %q %q %v", id1, id2, err)
	}
}

func TestCanonicalDigestAndRemoteRefValidation(t *testing.T) {
	if err := ValidateCanonicalDigest("request", testControlDigest("request")); err != nil {
		t.Fatalf("canonical digest: %v", err)
	}
	if err := ValidateCanonicalDigest("request", "sha256:ABC"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid digest error = %v", err)
	}
	if err := (RemoteRefState{Absent: true}).Validate("remote"); err != nil {
		t.Fatalf("absent remote: %v", err)
	}
	if err := (RemoteRefState{SHA: "0123456789012345678901234567890123456789"}).Validate("remote"); err != nil {
		t.Fatalf("SHA remote: %v", err)
	}
	if err := (RemoteRefState{}).Validate("remote"); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown remote accepted: %v", err)
	}
	if err := (RemoteRefState{Absent: true, SHA: "0123456789012345678901234567890123456789"}).Validate("remote"); !errors.Is(err, ErrValidation) {
		t.Fatalf("ambiguous remote accepted: %v", err)
	}
}
