package ateletpb_test

import (
	"testing"

	ateletpb "github.com/sozercan/orka/internal/substratepb/ateletpb"
	"google.golang.org/grpc"
)

func TestAteomHerderCheckpointRestoreClientCompiles(t *testing.T) {
	var conn grpc.ClientConnInterface
	client := ateletpb.NewAteomHerderClient(conn)
	if client == nil {
		t.Fatal("NewAteomHerderClient returned nil")
	}

	req := &ateletpb.RestoreRequest{
		TargetAteomNamespace:   "ate-demo",
		TargetAteomName:        "ateom-worker-1",
		ActorTemplateNamespace: "ate-demo",
		ActorTemplateName:      "orka-codex",
		ActorId:                "actor-1",
		SnapshotUriPrefix:      "gs://bucket/actors/actor-1/snapshots/snapshot-1/",
	}
	if req.GetSnapshotUriPrefix() == "" {
		t.Fatal("RestoreRequest snapshot URI was not retained")
	}
}
