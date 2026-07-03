package ateompb_test

import (
	"testing"

	ateompb "github.com/orka-agents/orka/internal/substratepb/ateompb"
	"google.golang.org/grpc"
)

func TestAteomCheckpointRestoreClientCompiles(t *testing.T) {
	var conn grpc.ClientConnInterface
	client := ateompb.NewAteomClient(conn)
	if client == nil {
		t.Fatal("NewAteomClient returned nil")
	}

	req := &ateompb.RestoreWorkloadRequest{
		ActorTemplateNamespace: "ate-demo",
		ActorTemplateName:      "orka-codex",
		ActorId:                "actor-1",
		SnapshotUriPrefix:      "gs://bucket/actors/actor-1/snapshots/snapshot-1/",
	}
	if req.GetSnapshotUriPrefix() == "" {
		t.Fatal("RestoreWorkloadRequest snapshot URI was not retained")
	}
}
