package controller

import (
	"context"
	"fmt"

	"github.com/orka-agents/orka/internal/workspace"
)

// The RuntimePool keeps a stable logical template binding while every native
// revision has its own immutable name. Resolve through the durable ownership
// record, not a process-local cache. Provider-native fields remain available
// for lifecycle and provenance checks inside the controller.
type nativeSubstrateRuntimeActorControl struct {
	workspace.SubstrateRuntimeActorControl
	store           *nativeSubstrateTemplateStore
	atespace        string
	logicalTemplate string
}

func (c *nativeSubstrateRuntimeActorControl) mapActor(ctx context.Context, actor *workspace.SubstrateRuntimeActor) (*workspace.SubstrateRuntimeActor, error) {
	if actor == nil {
		return nil, nil
	}
	_, binding, err := c.store.read(ctx, c.atespace, c.logicalTemplate)
	if err != nil {
		return nil, err
	}
	copy := *actor
	copy.NativeTemplateName = actor.TemplateName
	if binding != nil && actor.TemplateNamespace == binding.Atespace {
		for _, revision := range binding.Revisions {
			if revision.Name == actor.TemplateName {
				copy.TemplateName = binding.Name
				return &copy, nil
			}
		}
	}
	return &copy, nil
}

func (c *nativeSubstrateRuntimeActorControl) GetActor(ctx context.Context, actorID string) (*workspace.SubstrateRuntimeActor, error) {
	actor, err := c.SubstrateRuntimeActorControl.GetActor(ctx, actorID)
	if err != nil {
		return nil, err
	}
	return c.mapActor(ctx, actor)
}

func (c *nativeSubstrateRuntimeActorControl) CreateActor(ctx context.Context, actorID, atespace, templateName string) (*workspace.SubstrateRuntimeActor, error) {
	if atespace != c.atespace || templateName != c.logicalTemplate {
		return nil, fmt.Errorf("substrate actor creation does not match the bound RuntimePool template")
	}
	template, err := c.store.Get(ctx, atespace, templateName)
	if err != nil {
		return nil, err
	}
	if template == nil {
		return nil, fmt.Errorf("substrate template revision is not committed")
	}
	provider, _ := template.Object["provider"].(map[string]any)
	nativeName, _ := provider["name"].(string)
	if nativeName == "" {
		return nil, fmt.Errorf("substrate native template identity is missing")
	}
	actor, err := c.SubstrateRuntimeActorControl.CreateActor(ctx, actorID, atespace, nativeName)
	if err != nil {
		return nil, err
	}
	return c.mapActor(ctx, actor)
}

func (c *nativeSubstrateRuntimeActorControl) ResumeActor(ctx context.Context, actorID string, boot bool) (*workspace.SubstrateRuntimeActor, error) {
	actor, err := c.SubstrateRuntimeActorControl.ResumeActor(ctx, actorID, boot)
	if err != nil {
		return nil, err
	}
	return c.mapActor(ctx, actor)
}

func (c *nativeSubstrateRuntimeActorControl) SettleActor(ctx context.Context, actorID string) (*workspace.SubstrateRuntimeActor, error) {
	actor, err := c.SubstrateRuntimeActorControl.SettleActor(ctx, actorID)
	if err != nil {
		return nil, err
	}
	return c.mapActor(ctx, actor)
}
