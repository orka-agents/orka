package workspace

import (
	"context"
	"strings"
)

// SubstrateRuntimeActor is the sanitized Actor view used by workspace-backed
// ACP RuntimePools. It carries no snapshot URIs or provider credentials.
type SubstrateRuntimeActor struct {
	ActorID           string
	TemplateNamespace string
	TemplateName      string
	Status            string
	PodNamespace      string
	PodName           string
	PodIP             string
	// SnapshotObserved reports that the provider recorded a completed or
	// in-progress snapshot for this actor. ACP RuntimePool hosting never
	// requests snapshots, so an observed snapshot is proof of a
	// provider-initiated suspension and forces a fail-closed recycle.
	SnapshotObserved bool
}

// Running reports the exact provider running state; anything else refuses
// exact-instance admission.
func (a *SubstrateRuntimeActor) Running() bool {
	return a != nil && a.Status == substrateStatusRunning && strings.TrimSpace(a.PodIP) != ""
}

// SuspendedOrSuspending reports a provider-side suspension, which checkpoints
// supervisor memory and is prohibited for ACP RuntimePool actors.
func (a *SubstrateRuntimeActor) SuspendedOrSuspending() bool {
	return a != nil && (a.Status == substrateStatusSuspended || a.Status == substrateStatusSuspending)
}

// SubstrateRuntimeActorControl is the narrow Substrate control surface needed
// to host one ACP RuntimePool instance in an Actor. It intentionally excludes
// SuspendActor: gVisor suspension checkpoints supervisor process memory —
// including live pool and provider-proxy credentials — into provider snapshot
// storage, which the ACP execution-workspace contract prohibits. Teardown uses
// DeleteActor directly and never the legacy scrub-suspend-delete executor flow.
type SubstrateRuntimeActorControl interface {
	// GetActor returns nil with no error when the actor does not exist.
	GetActor(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error)
	CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*SubstrateRuntimeActor, error)
	// ResumeActor with boot=true starts the workload from scratch. ACP hosting
	// always boots fresh so a supervisor lifetime is exactly one boot.
	ResumeActor(ctx context.Context, actorID string, boot bool) (*SubstrateRuntimeActor, error)
	// DeleteActor returns nil when the actor is already absent.
	DeleteActor(ctx context.Context, actorID string) error
	Close() error
}

type substrateRuntimeActorControl struct {
	control substrateControlClient
}

// NewSubstrateRuntimeActorControl builds the control-only Substrate client for
// ACP RuntimePool hosting.
func NewSubstrateRuntimeActorControl(cfg SubstrateConfig, opts ...SubstrateOption) (SubstrateRuntimeActorControl, error) {
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.ControlClient == nil {
		client, err := newGRPCSubstrateControlClient(cfg)
		if err != nil {
			return nil, err
		}
		cfg.ControlClient = client
	}
	return &substrateRuntimeActorControl{control: cfg.ControlClient}, nil
}

func (c *substrateRuntimeActorControl) GetActor(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error) {
	actor, err := c.control.GetActor(ctx, actorID)
	if err != nil {
		if IsKind(err, ErrorKindNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return substrateRuntimeActorView(actor), nil
}

func (c *substrateRuntimeActorControl) CreateActor(
	ctx context.Context,
	actorID, templateNamespace, templateName string,
) (*SubstrateRuntimeActor, error) {
	actor, err := c.control.CreateActor(ctx, actorID, templateNamespace, templateName)
	if err != nil {
		if IsKind(err, ErrorKindAlreadyExists) {
			return c.GetActor(ctx, actorID)
		}
		return nil, err
	}
	return substrateRuntimeActorView(actor), nil
}

func (c *substrateRuntimeActorControl) ResumeActor(ctx context.Context, actorID string, boot bool) (*SubstrateRuntimeActor, error) {
	actor, err := c.control.ResumeActor(ctx, actorID, boot)
	if err != nil {
		return nil, err
	}
	return substrateRuntimeActorView(actor), nil
}

func (c *substrateRuntimeActorControl) DeleteActor(ctx context.Context, actorID string) error {
	if err := c.control.DeleteActor(ctx, actorID); err != nil && !IsKind(err, ErrorKindNotFound) {
		return err
	}
	return nil
}

func (c *substrateRuntimeActorControl) Close() error {
	if closer, ok := c.control.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func substrateRuntimeActorView(actor *substrateActor) *SubstrateRuntimeActor {
	if actor == nil {
		return nil
	}
	return &SubstrateRuntimeActor{
		ActorID:           strings.TrimSpace(actor.ActorID),
		TemplateNamespace: strings.TrimSpace(actor.TemplateNamespace),
		TemplateName:      strings.TrimSpace(actor.TemplateName),
		Status:            strings.TrimSpace(actor.Status),
		PodNamespace:      strings.TrimSpace(actor.PodNamespace),
		PodName:           strings.TrimSpace(actor.PodName),
		PodIP:             strings.TrimSpace(actor.PodIP),
		SnapshotObserved:  strings.TrimSpace(actor.LastSnapshot) != "" || strings.TrimSpace(actor.InProgressSnapshot) != "",
	}
}
