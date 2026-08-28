/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSubstrateRuntimeActorVerifiedDataSnapshotFence(t *testing.T) {
	const (
		actorID  = "actor-1"
		actorUID = "private-actor-uid"
	)
	actor := &SubstrateRuntimeActor{
		ActorID:      actorID,
		ActorUID:     actorUID,
		ActorVersion: 7,
		DataSnapshot: &SubstrateDataSnapshotFence{
			ActorID:            actorID,
			ActorUID:           actorUID,
			ActorVersion:       7,
			SnapshotAtespace:   "private-atespace",
			SnapshotName:       "private-snapshot-name",
			SnapshotUID:        "private-snapshot-uid",
			SnapshotVersion:    3,
			SourceActorUID:     actorUID,
			SourceActorVersion: 5,
			ContentScope:       SubstrateSnapshotContentScopeData,
		},
	}
	fence, digest, err := actor.VerifiedDataSnapshotFence(actorID)
	if err != nil {
		t.Fatalf("verify data snapshot fence: %v", err)
	}
	if digest == "" {
		t.Fatal("snapshot digest is empty")
	}
	for _, private := range []string{fence.ActorUID, fence.SnapshotAtespace, fence.SnapshotName, fence.SnapshotUID} {
		if strings.Contains(digest, private) {
			t.Fatalf("snapshot digest exposed provider identifier material: %q", digest)
		}
	}
	identityDigest, err := fence.ImmutableIdentityDigest()
	if err != nil {
		t.Fatalf("snapshot identity digest: %v", err)
	}
	fence.ActorVersion++
	changedActor := *actor
	changedActor.ActorVersion = fence.ActorVersion
	changedActor.DataSnapshot = &fence
	_, changedDigest, err := changedActor.VerifiedDataSnapshotFence(actorID)
	if err != nil {
		t.Fatalf("verify changed Actor version: %v", err)
	}
	changedIdentityDigest, err := fence.ImmutableIdentityDigest()
	if err != nil {
		t.Fatalf("changed snapshot identity digest: %v", err)
	}
	if changedDigest == digest {
		t.Fatal("full snapshot fence digest ignored ActorVersion")
	}
	if changedIdentityDigest != identityDigest {
		t.Fatal("immutable snapshot identity digest changed with only ActorVersion")
	}

	actor.DataSnapshot.ContentScope = SubstrateSnapshotContentScopeFull
	if _, _, err := actor.VerifiedDataSnapshotFence(actorID); err == nil || !strings.Contains(err.Error(), "not Data") {
		t.Fatalf("Full snapshot verification error = %v, want Data-scope refusal", err)
	}
}

func TestSubstrateRuntimeActorControlConfirmsCreateRecoverySettlement(t *testing.T) {
	const actorID = "actor-1"
	for _, test := range []struct {
		name        string
		control     *recordingSubstrateControlClient
		wantSettled bool
		wantErr     bool
		wantGets    int
	}{
		{
			name:    "provider list still contains actor",
			control: &recordingSubstrateControlClient{actors: []substrateActor{{ActorID: actorID}}},
		},
		{
			name:     "actor appears during exact read",
			control:  &recordingSubstrateControlClient{},
			wantGets: 1,
		},
		{
			name: "provider list and exact read confirm absence",
			control: &recordingSubstrateControlClient{getErrs: []error{
				NewError("get actor", ErrorKindNotFound, "actor is absent", false, nil),
			}},
			wantSettled: true,
			wantGets:    1,
		},
		{
			name:    "provider list fails",
			control: &recordingSubstrateControlClient{listActorsErr: errors.New("list failed")},
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			control := &substrateRuntimeActorControl{control: test.control}
			if !control.ActorCreateRecoveryAttestationSupported() {
				t.Fatal("production actor control did not advertise create recovery attestation")
			}
			settled, err := control.ConfirmActorCreationSettled(context.Background(), actorID)
			if (err != nil) != test.wantErr {
				t.Fatalf("ConfirmActorCreationSettled() error = %v, wantErr %v", err, test.wantErr)
			}
			if settled != test.wantSettled {
				t.Fatalf("ConfirmActorCreationSettled() = %v, want %v", settled, test.wantSettled)
			}
			if test.control.listActorsCalls != 1 || test.control.getCalls != test.wantGets {
				t.Fatalf("provider reads = list:%d get:%d, want list:1 get:%d",
					test.control.listActorsCalls, test.control.getCalls, test.wantGets)
			}
		})
	}
}

func TestSubstrateRuntimeActorVerifiedDataResumeOperation(t *testing.T) {
	const (
		actorID     = "actor-1"
		actorUID    = "private-actor-uid"
		operationID = "resume-operation-1"
	)
	actor := &SubstrateRuntimeActor{
		ActorID: actorID, ActorUID: actorUID, ActorVersion: 9, LatestDataOperationID: operationID,
		DataResumeOperation: &SubstrateDataResumeOperationProof{
			OperationID: operationID, ActorID: actorID, ActorUID: actorUID, ActorVersion: 7,
		},
	}
	proof, digest, err := actor.VerifiedDataResumeOperation(actorID, operationID)
	if err != nil {
		t.Fatalf("verify data resume operation: %v", err)
	}
	if digest == "" || strings.Contains(digest, proof.ActorUID) || strings.Contains(digest, proof.OperationID) {
		t.Fatalf("resume operation digest exposed provider identity material: %q", digest)
	}

	replacement := *actor
	replacement.ActorUID = "replacement-actor-uid"
	if _, _, err := replacement.VerifiedDataResumeOperation(actorID, operationID); err == nil ||
		!strings.Contains(err.Error(), "exact actor lifetime") {
		t.Fatalf("replacement Actor verification error = %v, want exact-lifetime refusal", err)
	}

	intervening := *actor
	intervening.LatestDataOperationID = "later-data-operation"
	if _, _, err := intervening.VerifiedDataResumeOperation(actorID, operationID); err == nil ||
		!strings.Contains(err.Error(), "latest operation") {
		t.Fatalf("intervening operation verification error = %v, want stale-proof refusal", err)
	}
}

func TestSubstrateRuntimeActorVerifiedDataCheckpointOperation(t *testing.T) {
	const (
		actorID     = "actor-1"
		actorUID    = "private-actor-uid"
		operationID = "checkpoint-operation-1"
	)
	actor := &SubstrateRuntimeActor{
		ActorID: actorID, ActorUID: actorUID, ActorVersion: 9, LatestDataOperationID: operationID,
		DataCheckpointOperation: &SubstrateDataCheckpointOperationProof{
			OperationID: operationID, ActorID: actorID, ActorUID: actorUID, ActorVersion: 7,
		},
	}
	proof, digest, err := actor.VerifiedDataCheckpointOperation(actorID, operationID)
	if err != nil {
		t.Fatalf("verify data checkpoint operation: %v", err)
	}
	if digest == "" || strings.Contains(digest, proof.ActorUID) || strings.Contains(digest, proof.OperationID) {
		t.Fatalf("checkpoint operation digest exposed provider identity material: %q", digest)
	}

	intervening := *actor
	intervening.LatestDataOperationID = "later-data-operation"
	if _, _, err := intervening.VerifiedDataCheckpointOperation(actorID, operationID); err == nil ||
		!strings.Contains(err.Error(), "latest operation") {
		t.Fatalf("intervening operation verification error = %v, want stale-proof refusal", err)
	}
}
