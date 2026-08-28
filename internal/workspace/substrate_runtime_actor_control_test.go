/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"strings"
	"testing"
)

func TestSubstrateRuntimeActorViewDigestsCurrentSnapshotGeneration(t *testing.T) {
	actor := &substrateActor{
		LastSnapshot:       "gs://private-bucket/actor/previous",
		InProgressSnapshot: "gs://private-bucket/actor/current",
	}
	view := substrateRuntimeActorView(actor)
	if view.SnapshotDigest == "" {
		t.Fatal("snapshot digest is empty")
	}
	if view.SnapshotDigest != substrateSnapshotDigest(actor) {
		t.Fatal("actor view did not use the current in-progress snapshot generation")
	}
	if strings.Contains(view.SnapshotDigest, "private-bucket") || strings.Contains(view.SnapshotDigest, "current") {
		t.Fatalf("snapshot digest exposed provider identifier material: %q", view.SnapshotDigest)
	}

	actor.InProgressSnapshot = ""
	if completed := substrateRuntimeActorView(actor).SnapshotDigest; completed == "" || completed == view.SnapshotDigest {
		t.Fatalf("completed snapshot digest = %q, want the distinct last-snapshot generation", completed)
	}
}
