package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// in-progress snapshot for this actor. Pools without an operator-permitted
	// data-only suspension policy never request snapshots, so an observed
	// snapshot there is proof of a provider-initiated suspension and forces a
	// fail-closed recycle. Data-only pools expect snapshot records after a
	// requested suspension; their derived template's explicit Data policy is
	// what keeps process memory out of every checkpoint.
	SnapshotObserved bool
	// SnapshotDigest is a safe digest of the provider's current snapshot
	// identifier. It binds suspension consent to one exact provider snapshot
	// generation without exposing the provider URI outside this adapter.
	// In-progress snapshots take precedence over the prior completed snapshot
	// so the digest remains stable when the provider promotes that generation.
	SnapshotDigest string
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

// Suspended reports the settled provider state that permits DeleteActor.
func (a *SubstrateRuntimeActor) Suspended() bool {
	return a != nil && a.Status == substrateStatusSuspended
}

// Suspending reports an in-flight provider suspension transition.
func (a *SubstrateRuntimeActor) Suspending() bool {
	return a != nil && a.Status == substrateStatusSuspending
}

// RunningStatus reports provider-side liveness (STATUS_RUNNING) regardless of
// route readiness: a just-resumed actor can be Running before its Pod IP is
// populated, which is a transitional state, never a crash.
func (a *SubstrateRuntimeActor) RunningStatus() bool {
	return a != nil && a.Status == substrateStatusRunning
}

// Resuming reports an in-flight provider cold resume: ResumeActor accepted
// the request but the workload has not reached Running yet. The checkpoint is
// being consumed, not crashed.
func (a *SubstrateRuntimeActor) Resuming() bool {
	return a != nil && a.Status == substrateStatusResuming
}

// Crashed reports a provider state whose worker workload is no longer
// assigned. A crashed actor may be deleted only after Orka has durably proven
// the exact prior workload absent.
func (a *SubstrateRuntimeActor) Crashed() bool {
	return a != nil && a.Status == substrateStatusCrashed
}

// SubstrateRuntimeActorControl is the narrow Substrate control surface needed
// to host one ACP RuntimePool instance in an Actor. Suspending a live
// workload is prohibited: gVisor suspension checkpoints supervisor process
// memory — including live pool and provider-proxy credentials — into provider
// snapshot storage, which the ACP execution-workspace contract forbids.
// Because the provider deletes only suspended actors, teardown first destroys
// the workload's memory (deleting its single-workload worker Pod), then calls
// SettleActor purely to transition the memoryless actor into the deletable
// suspended state — with nothing left to checkpoint — and then DeleteActor.
type SubstrateRuntimeActorControl interface {
	// GetActor returns nil with no error when the actor does not exist.
	GetActor(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error)
	CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*SubstrateRuntimeActor, error)
	// ResumeActor with boot=true starts the workload from scratch. ACP hosting
	// always boots fresh so a supervisor lifetime is exactly one boot.
	ResumeActor(ctx context.Context, actorID string, boot bool) (*SubstrateRuntimeActor, error)
	// SettleActor transitions the actor toward the provider's deletable
	// suspended state. It must only be called after the actor's workload
	// memory has been destroyed and its absence confirmed — settling a live
	// supervisor would checkpoint credentials and is prohibited.
	SettleActor(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error)
	// SuspendActorForDataCheckpoint suspends a live actor whose derived
	// template renders the explicit data-only snapshot policy (onPause: Data,
	// onCommit: Data, onResume.fromData: ColdBoot). Under that policy the
	// checkpoint captures only the controller-owned DurableDir workspace
	// volume and can never contain process memory or credentials. Callers must
	// verify the deployed template's exact fence and rendered policy, and
	// settle prompt and workspace activity, before invoking it; every other
	// template policy keeps live suspension prohibited.
	SuspendActorForDataCheckpoint(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error)
	// DeleteActor returns nil when the actor is already absent. The provider
	// accepts deletion of suspended (settled) or crashed actors.
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

func (c *substrateRuntimeActorControl) SettleActor(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error) {
	actor, err := c.control.SuspendActor(ctx, actorID)
	if err != nil {
		return nil, err
	}
	return substrateRuntimeActorView(actor), nil
}

func (c *substrateRuntimeActorControl) SuspendActorForDataCheckpoint(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error) {
	actor, err := c.control.SuspendActor(ctx, actorID)
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
		SnapshotDigest:    substrateSnapshotDigest(actor),
	}
}

func substrateSnapshotDigest(actor *substrateActor) string {
	if actor == nil {
		return ""
	}
	identifier := strings.TrimSpace(actor.InProgressSnapshot)
	if identifier == "" {
		identifier = strings.TrimSpace(actor.LastSnapshot)
	}
	if identifier == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("orka-substrate-snapshot\x00" + identifier))
	return "sha256:" + hex.EncodeToString(sum[:])
}
