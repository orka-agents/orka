package controller

import (
	"context"
	"fmt"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
)

// Native template references are an explicit operator selection. Upstream
// templates have no Kubernetes labels, annotations or Ready condition. The
// Actor's native readiness probe gates the subsequent ResumeActor call.
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
	return nil
}

func validateNativeSubstrateRoute(template *ateapipb.ActorTemplate) error {
	if template.GetMetadata().GetUid() == "" || template.GetMetadata().GetAtespace() == "" {
		return fmt.Errorf("native Substrate template has no immutable identity")
	}
	for _, container := range template.GetContainers() {
		if container.GetReadyz().GetHttpGet().GetPort() == substrateActorListenPort {
			return nil
		}
	}
	return fmt.Errorf("substrate MCP template must probe its routable HTTP endpoint on port %d", substrateActorListenPort)
}
