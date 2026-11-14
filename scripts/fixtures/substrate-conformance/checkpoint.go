package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
)

// Verify actual provider file survival and independent fork data, in addition
// to the Orka checkpoint controller's journal and ownership unit tests.
func exerciseDataRestore(
	ctx context.Context, cfg workspace.SubstrateConfig, executor *workspace.SubstrateWorkspaceExecutor,
	ref workspace.WorkspaceRef, suffix string,
) error {
	api, err := workspace.NewSubstrateNativeClient(cfg)
	if err != nil {
		return err
	}
	defer api.Close() //nolint:errcheck
	name, _, _ := strings.Cut(ref.ID, ".")
	source := &ateapipb.ObjectRef{Atespace: conformanceAtespace, Name: name}
	actor, err := api.Control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: source})
	if err != nil {
		return err
	}
	if _, err := api.Control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
		Actor: source, Options: &ateapipb.SuspendOptions{Mode: ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
	}); err != nil {
		return err
	}
	var suspended *ateapipb.Actor
	if err := pollNative(ctx, func() (bool, error) {
		value, err := api.Control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: source})
		suspended = value
		return value.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED, err
	}); err != nil {
		return err
	}
	captured := latestSnapshot(suspended.GetStatus().GetSnapshots())
	if suspended.GetMetadata().GetUid() != actor.GetMetadata().GetUid() || !completedDataSnapshot(captured) {
		return fmt.Errorf("native direct suspension did not capture a completed VOLUMES snapshot of its source")
	}
	tagRef := &ateapipb.ObjectRef{Atespace: conformanceAtespace, Name: "orka-direct-data-" + suffix}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = api.Control.DeleteTag(cleanup, &ateapipb.DeleteTagRequest{Tag: tagRef})
	}()
	_, err = api.Control.CreateTag(ctx, &ateapipb.CreateTagRequest{Tag: &ateapipb.Tag{
		Metadata: &ateapipb.ResourceMetadata{Atespace: conformanceAtespace, Name: tagRef.Name},
		Scope:    ateapipb.TagScope_TAG_SCOPE_ATESPACE, SourceActor: source,
	}})
	if err != nil {
		return err
	}
	if err := pollNative(ctx, func() (bool, error) {
		tag, err := api.Control.GetTag(ctx, &ateapipb.GetTagRequest{Tag: tagRef})
		if err != nil {
			return false, err
		}
		snapshot := tag.GetStatus().GetSnapshot()
		if snapshot.GetDurableSnapshot().GetState() != ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
			return false, nil
		}
		if err := verifyTagProvenance(tag, source, captured); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return err
	}
	after, err := api.Control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: source})
	if err != nil || after.GetMetadata().GetUid() != actor.GetMetadata().GetUid() {
		return fmt.Errorf("native direct checkpoint source changed while tagging")
	}
	if _, err := executor.Delete(ctx, workspace.DeleteRequest{
		Ref: ref, SkipScrub: true, Timeout: 2 * time.Minute,
	}); err != nil {
		return err
	}
	// Each fork sees the immutable source even after another fork changes it.
	for index := range 2 {
		if err := restoreDataFork(ctx, cfg, api, actor.ActorTemplate, tagRef, suffix, index); err != nil {
			return err
		}
	}
	return nil
}

func restoreDataFork(
	ctx context.Context, cfg workspace.SubstrateConfig, api *workspace.SubstrateNativeClient,
	template, tag *ateapipb.ObjectRef, suffix string, index int,
) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	cfg.HandoffToken = token
	executor, err := workspace.NewSubstrateExecutor(cfg)
	if err != nil {
		return err
	}
	defer executor.Close() //nolint:errcheck
	name := fmt.Sprintf("orka-direct-fork-%s-%d", suffix, index)
	_, err = api.Control.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: conformanceAtespace, Name: name},
		ActorTemplate: template, SourceTag: tag,
	}})
	if err != nil {
		return err
	}
	ref := workspace.WorkspaceRef{ID: workspace.SubstrateActorKey(conformanceAtespace, name)}
	defer cleanupActor(executor, ref)
	if err := bootAndSeed(ctx, executor, ref, token, false); err != nil {
		return err
	}
	result, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref: ref, Command: []string{"sh", "-c", "cat /workspace/proof; printf changed > /workspace/proof"},
		WorkDir: "/workspace", Timeout: time.Minute,
	})
	if err != nil || result == nil || result.ExitCode != 0 || result.Stdout != proofContents {
		return commandFailure("native Data Tag did not restore independent workspace files", cfg, result, err)
	}
	_, err = executor.Delete(ctx, workspace.DeleteRequest{Ref: ref, Timeout: 2 * time.Minute})
	return err
}

func latestSnapshot(snapshots []*ateapipb.Snapshot) *ateapipb.Snapshot {
	var latest *ateapipb.Snapshot
	for _, snapshot := range snapshots {
		if snapshot != nil && (latest == nil || snapshot.GetGeneration() > latest.GetGeneration()) {
			latest = snapshot
		}
	}
	return latest
}

// completedDataSnapshot reports a durable data-only snapshot an Actor took
// under a known template.
func completedDataSnapshot(snapshot *ateapipb.Snapshot) bool {
	return completedVolumesSnapshot(snapshot) && snapshot.GetActorTemplateUid() != ""
}

func completedVolumesSnapshot(snapshot *ateapipb.Snapshot) bool {
	return snapshot.GetFidelity() == ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES &&
		snapshot.GetDurableSnapshot().GetState() == ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED
}

// verifyTagProvenance binds a completed Tag to the suspended source snapshot.
// v1 Tags keep only the source Actor name, and record the template the data
// was captured under in TagStatus, not on the Tag's own snapshot copy; the
// caller binds the exact Actor lifetime through its unchanged UID.
func verifyTagProvenance(tag *ateapipb.Tag, source *ateapipb.ObjectRef, captured *ateapipb.Snapshot) error {
	snapshot := tag.GetStatus().GetSnapshot()
	template := tag.GetStatus().GetActorTemplateUid()
	if !completedVolumesSnapshot(snapshot) || tag.GetSourceActor().GetName() != source.GetName() ||
		template == "" || template != captured.GetActorTemplateUid() {
		return fmt.Errorf("native direct checkpoint provenance differs from its source: "+
			"fidelity=%s state=%s tagTemplate=%q capturedTemplate=%q tagSource=%q source=%q",
			snapshot.GetFidelity(), snapshot.GetDurableSnapshot().GetState(),
			template, captured.GetActorTemplateUid(), tag.GetSourceActor().GetName(), source.GetName())
	}
	return nil
}

func pollNative(ctx context.Context, ready func() (bool, error)) error {
	for {
		done, err := ready()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
