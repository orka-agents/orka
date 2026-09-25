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
	if _, err := api.Control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: source}); err != nil {
		return err
	}
	if err := pollNative(ctx, func() (bool, error) {
		value, err := api.Control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: source})
		return value.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED, err
	}); err != nil {
		return err
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
		if tag.GetStatus().GetSnapshot() == nil {
			return false, nil
		}
		if tag.GetStatus().GetSnapshot().GetContentScope() != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA ||
			tag.GetStatus().GetSourceActorUid() != actor.GetMetadata().GetUid() ||
			tag.GetStatus().GetActorTemplateUid() != actor.GetStatus().GetCurrentActorTemplateUid() {
			return false, fmt.Errorf("native direct checkpoint provenance differs from its source")
		}
		return true, nil
	}); err != nil {
		return err
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
