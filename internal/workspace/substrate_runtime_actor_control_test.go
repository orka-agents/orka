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
