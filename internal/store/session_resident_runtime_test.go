package store

import (
	"testing"
	"time"
)

func testResidentRuntime(t *testing.T, keptAt time.Time) SessionResidentRuntime {
	t.Helper()
	key := SessionTurnKey{SessionUID: "session-uid", LeaseGeneration: 3, TaskUID: "task-n", Attempt: 1, PromptID: "prompt-n"}
	turnID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewSessionResidentRuntime(SessionResidentRuntime{
		SourceTaskUID: key.TaskUID, SourceAttempt: 1, SourcePromptAttemptID: "attempt-n", SourceTurnID: turnID,
		SourceLeaseGeneration: key.LeaseGeneration, ControllerEpochName: "orka", ControllerEpoch: 4,
		WorkspaceName: "ws", WorkspaceUID: "ws-uid", WorkspaceAttachmentEpoch: 2,
		RuntimePoolName: "pool", RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1,
		RuntimeInstanceID: "instance", SupervisorBootID: "boot", ServedRecordDigest: testControlDigest("served"),
		RuntimeSessionUID: key.SessionUID, RuntimeSessionGeneration: 2,
		ProfileDigest: testControlDigest("profile"), MCPDigest: testControlDigest("mcp"),
		WorkspaceDigest: testControlDigest("workspace"), LineageConfigDigest: testControlDigest("lineage"),
		ReuseDigest: testControlDigest("reuse"), TranscriptHeadDigest: testControlDigest("head"),
		KeptAt: keptAt, ExpiresAt: keptAt.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("NewSessionResidentRuntime: %v", err)
	}
	return record
}

func TestSessionResidentRuntimeIdentityCoversEveryFieldAndSurvivesSecondPrecision(t *testing.T) {
	keptAt := time.Date(2026, 11, 20, 6, 0, 0, 987654321, time.UTC)
	record := testResidentRuntime(t, keptAt)
	if record.State != SessionResidentRuntimeResident || record.KeptAt.Nanosecond() != 0 {
		t.Fatalf("record = %#v, want Resident at whole-second precision", record)
	}
	if err := ValidateSessionResidentRuntime(record); err != nil {
		t.Fatalf("validate kept record: %v", err)
	}
	tampered := record
	tampered.RuntimeInstanceID = "instance-2"
	if err := ValidateSessionResidentRuntime(tampered); err == nil {
		t.Fatal("a changed field must invalidate the handoff identity")
	}
	claimed := record
	claimed.State = SessionResidentRuntimeClaimed
	claimed.Claim = &SessionResidentRuntimeClaim{TaskUID: "task-n1", Attempt: 1, PromptID: "prompt-n1", LeaseGeneration: 4}
	if err := ValidateSessionResidentRuntime(claimed); err != nil {
		t.Fatalf("a claim does not change the handoff identity: %v", err)
	}
	stale := claimed
	stale.Claim = &SessionResidentRuntimeClaim{TaskUID: "task-n1", Attempt: 1, PromptID: "prompt-n1", LeaseGeneration: 3}
	if err := ValidateSessionResidentRuntime(stale); err == nil {
		t.Fatal("a claim must come from a later lease generation")
	}
	expired := record
	expired.ExpiresAt = expired.KeptAt
	if _, err := NewSessionResidentRuntime(expired); err == nil {
		t.Fatal("a record must expire after it was kept")
	}
	rebased := record
	rebased.RebasedTreeDigest = testControlDigest("tree")
	if _, err := NewSessionResidentRuntime(rebased); err == nil {
		t.Fatal("a rebased record requires its verified baseline")
	}
}

func TestResolveResidentRuntimeOnAcquire(t *testing.T) {
	keptAt := time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)
	record := testResidentRuntime(t, keptAt)
	fence := ControllerEpochFence{Name: "orka", Epoch: 4, HolderID: "controller"}
	lease := SessionResidentRuntimeClaim{TaskUID: "task-n1", Attempt: 1, PromptID: "prompt-n1", LeaseGeneration: 4}
	claim := &ClaimSessionResidentRuntime{HandoffID: record.HandoffID}
	key := SessionTurnKey{SessionUID: record.RuntimeSessionUID, LeaseGeneration: 4, TaskUID: "task-n1", Attempt: 1, PromptID: "prompt-n1"}

	claimed := ResolveResidentRuntimeOnAcquire(&record, claim, fence, lease, keptAt.Add(time.Minute))
	if !ResidentRuntimeClaimedBy(claimed, key) {
		t.Fatalf("matching claim = %#v, want Claimed by the lease", claimed)
	}
	if record.State != SessionResidentRuntimeResident {
		t.Fatal("resolution must not mutate the stored record")
	}
	for name, tc := range map[string]struct {
		claim *ClaimSessionResidentRuntime
		fence ControllerEpochFence
		lease SessionResidentRuntimeClaim
		at    time.Time
		start *SessionResidentRuntime
	}{
		"ordinary acquisition":  {fence: fence, lease: lease, at: keptAt},
		"other handoff":         {claim: &ClaimSessionResidentRuntime{HandoffID: testControlDigest("other")}, fence: fence, lease: lease, at: keptAt},
		"controller epoch":      {claim: claim, fence: ControllerEpochFence{Name: "orka", Epoch: 5}, lease: lease, at: keptAt},
		"expired":               {claim: claim, fence: fence, lease: lease, at: record.ExpiresAt},
		"older lease":           {claim: claim, fence: fence, lease: SessionResidentRuntimeClaim{TaskUID: "x", Attempt: 1, PromptID: "p", LeaseGeneration: 3}, at: keptAt},
		"already claimed":       {claim: claim, fence: fence, lease: lease, at: keptAt, start: claimed},
		"already retiring once": {claim: claim, fence: fence, lease: lease, at: keptAt, start: ResolveResidentRuntimeOnAcquire(&record, nil, fence, lease, keptAt)},
	} {
		t.Run(name, func(t *testing.T) {
			start := tc.start
			if start == nil {
				copied := record
				start = &copied
			}
			next := ResolveResidentRuntimeOnAcquire(start, tc.claim, tc.fence, tc.lease, tc.at)
			if next == nil || next.State != SessionResidentRuntimeRetiring {
				t.Fatalf("resolution = %#v, want Retiring", next)
			}
		})
	}
	if ResolveResidentRuntimeOnAcquire(nil, claim, fence, lease, keptAt) != nil {
		t.Fatal("no record resolves to no record")
	}
}

func TestValidateResidentRuntimeForFinalizationNamesTheExactTurn(t *testing.T) {
	record := testResidentRuntime(t, time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC))
	key := SessionTurnKey{SessionUID: "session-uid", LeaseGeneration: 3, TaskUID: "task-n", Attempt: 1, PromptID: "prompt-n"}
	fence := ControllerEpochFence{Name: "orka", Epoch: 4}
	if err := ValidateResidentRuntimeForFinalization(record, key, "attempt-n", fence); err != nil {
		t.Fatalf("exact turn: %v", err)
	}
	other := key
	other.LeaseGeneration = 4
	if err := ValidateResidentRuntimeForFinalization(record, other, "attempt-n", fence); err == nil {
		t.Fatal("another turn must not keep this record")
	}
	if err := ValidateResidentRuntimeForFinalization(record, key, "attempt-other", fence); err == nil {
		t.Fatal("another PromptAttempt must not keep this record")
	}
	if err := ValidateResidentRuntimeForFinalization(record, key, "attempt-n", ControllerEpochFence{Name: "orka", Epoch: 5}); err == nil {
		t.Fatal("another controller epoch must not keep this record")
	}
}
