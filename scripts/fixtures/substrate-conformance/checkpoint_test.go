package main

import (
	"testing"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/stretchr/testify/require"
)

func dataSnapshot(template string) *ateapipb.Snapshot {
	return &ateapipb.Snapshot{
		Fidelity:         ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES,
		ActorTemplateUid: template,
		DurableSnapshot:  &ateapipb.SnapshotStorage{State: ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED},
	}
}

// Agent Substrate v1.0.0 leaves actor_template_uid unset on a Tag's own
// snapshot copy and records it in TagStatus, as observed on the pinned release.
func TestTagProvenanceUsesTheTagStatusTemplate(t *testing.T) {
	source := &ateapipb.ObjectRef{Atespace: conformanceAtespace, Name: "orka-direct-a"}
	captured := dataSnapshot("template-uid")
	tag := func(name, template string, snapshot *ateapipb.Snapshot) *ateapipb.Tag {
		return &ateapipb.Tag{SourceActor: &ateapipb.ObjectRef{Atespace: conformanceAtespace, Name: name},
			Status: &ateapipb.TagStatus{Snapshot: snapshot, ActorTemplateUid: template}}
	}
	require.NoError(t, verifyTagProvenance(tag(source.Name, "template-uid", dataSnapshot("")), source, captured))

	pending := &ateapipb.Snapshot{Fidelity: ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES}
	memory := dataSnapshot("")
	memory.Fidelity = ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
	for name, candidate := range map[string]*ateapipb.Tag{
		"other source":       tag("orka-direct-b", "template-uid", dataSnapshot("")),
		"other template":     tag(source.Name, "other-template", dataSnapshot("")),
		"missing template":   tag(source.Name, "", dataSnapshot("")),
		"process memory":     tag(source.Name, "template-uid", memory),
		"pending snapshot":   tag(source.Name, "template-uid", pending),
		"no snapshot at all": tag(source.Name, "template-uid", nil),
	} {
		require.Error(t, verifyTagProvenance(candidate, source, captured), name)
	}
}
