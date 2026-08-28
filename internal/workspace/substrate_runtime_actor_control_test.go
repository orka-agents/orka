/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"strings"
	"testing"
)

func TestSubstrateRuntimeActorVerifiedDataSnapshotFence(t *testing.T) {
	const (
		actorID  = "actor-1"
		actorUID = "private-actor-uid"
	)
	actor := &SubstrateRuntimeActor{
		ActorID:               actorID,
		ActorUID:              actorUID,
		ActorVersion:          7,
		LatestDataOperationID: "checkpoint-operation-1",
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
	if changedDigest != digest {
		t.Fatal("snapshot digest changed with only ActorVersion")
	}
	if changedIdentityDigest != identityDigest {
		t.Fatal("immutable snapshot identity digest changed with only ActorVersion")
	}
	changedOperation := changedActor
	changedOperation.LatestDataOperationID = "checkpoint-operation-2"
	_, operationDigest, err := changedOperation.VerifiedDataSnapshotFence(actorID)
	if err != nil {
		t.Fatalf("verify changed data operation: %v", err)
	}
	if operationDigest != digest {
		t.Fatal("snapshot digest changed with only the latest data operation")
	}

	actor.DataSnapshot.ContentScope = SubstrateSnapshotContentScopeFull
	if _, _, err := actor.VerifiedDataSnapshotFence(actorID); err == nil || !strings.Contains(err.Error(), "not Data") {
		t.Fatalf("Full snapshot verification error = %v, want Data-scope refusal", err)
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
	proof, digest, err := actor.VerifiedDataCheckpointOperation(actorID, operationID, 7)
	if err != nil {
		t.Fatalf("verify data checkpoint operation: %v", err)
	}
	if digest == "" || strings.Contains(digest, proof.ActorUID) || strings.Contains(digest, proof.OperationID) {
		t.Fatalf("checkpoint operation digest exposed provider identity material: %q", digest)
	}

	intervening := *actor
	intervening.LatestDataOperationID = "later-data-operation"
	if _, _, err := intervening.VerifiedDataCheckpointOperation(actorID, operationID, 7); err == nil ||
		!strings.Contains(err.Error(), "latest operation") {
		t.Fatalf("intervening operation verification error = %v, want stale-proof refusal", err)
	}
	if _, _, err := actor.VerifiedDataCheckpointOperation(actorID, operationID, 8); err == nil ||
		!strings.Contains(err.Error(), "source Actor version") {
		t.Fatalf("source-version verification error = %v, want exact requested source refusal", err)
	}
}
