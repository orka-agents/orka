package controller

import (
	"context"
	"fmt"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
)

// Native template references are an explicit operator selection. Upstream
// templates have no Kubernetes labels, annotations or Ready condition. The
// Actor's native wakeup probe gates router ingress after each ResumeActor.
func validateNativeSubstrateRoutableTemplate(ctx context.Context, cfg SubstrateConfig, request *ExecutionWorkspaceRequest) error {
	cfg = cfg.WithDefaults()
	ctx, cancel := context.WithTimeout(ctx, cfg.ClaimTimeout)
	defer cancel()
	api, err := workspace.NewSubstrateNativeClient(cfg.WorkspaceClientConfig())
	if err != nil {
		return err
	}
	defer api.Close() //nolint:errcheck
	template, err := api.Control.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
		ActorTemplate: &ateapipb.ObjectRef{Atespace: request.TemplateNamespace, Name: request.TemplateName},
	})
	if err != nil {
		return fmt.Errorf("read native Substrate MCP template: %w", err)
	}
	if err := validateNativeSubstrateRoute(template); err != nil {
		return err
	}
	request.TemplateUID = template.GetMetadata().GetUid()
	request.TemplateSuspendUnsupported = nativeSubstrateTemplateSuspendUnsupported(template)
	return nil
}

// nativeSubstrateTemplateSuspendUnsupported reports whether Substrate cannot
// suspend an Actor of this template. A VOLUMES snapshot captures only
// durable-directory mounts; with none, the worker refuses the checkpoint before
// it stops the workload, and Substrate then records the Actor as crashed while
// its sandbox may keep running. Such an Actor holds no data a suspend would
// keep, so it is stopped by deleting it.
func nativeSubstrateTemplateSuspendUnsupported(template *ateapipb.ActorTemplate) bool {
	if template.GetSnapshotConfig().GetPreferredFidelity() != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
		return false
	}
	durable := map[string]bool{}
	for _, volume := range template.GetVolumes() {
		if volume.GetDurableDir() != nil {
			durable[volume.GetName()] = true
		}
	}
	for _, container := range template.GetContainers() {
		for _, mount := range container.GetVolumeMounts() {
			if durable[mount.GetName()] {
				return false
			}
		}
	}
	return true
}

func validateNativeSubstrateRoute(template *ateapipb.ActorTemplate) error {
	if template.GetMetadata().GetUid() == "" || template.GetMetadata().GetAtespace() == "" {
		return fmt.Errorf("native Substrate template has no immutable identity")
	}
	for _, container := range template.GetContainers() {
		if container.GetWakeupProbe().GetHttpGet().GetPort() == substrateActorListenPort {
			return nil
		}
	}
	return fmt.Errorf("substrate MCP template must set a wakeup probe on its routable HTTP endpoint port %d", substrateActorListenPort)
}
