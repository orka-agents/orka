package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type SubstrateSnapshotContentScope string

const (
	SubstrateSnapshotContentScopeData SubstrateSnapshotContentScope = "Data"
	SubstrateSnapshotContentScopeFull SubstrateSnapshotContentScope = "Full"
)

type SubstrateDataSnapshotFence struct {
	ActorID            string
	ActorUID           string
	ActorVersion       int64
	SnapshotAtespace   string
	SnapshotName       string
	SnapshotUID        string
	SnapshotVersion    int64
	SourceActorUID     string
	SourceActorVersion int64
	ContentScope       SubstrateSnapshotContentScope
}

type SubstrateActorTemplateFence struct {
	Namespace       string
	Name            string
	UID             string
	ResourceVersion string
	Revision        string
}

type SubstrateDataCheckpointFence struct {
	OperationID  string
	ActorID      string
	ActorUID     string
	ActorVersion int64
	Template     SubstrateActorTemplateFence
}

type SubstrateDataResumeFence struct {
	OperationID string
	Snapshot    SubstrateDataSnapshotFence
	Template    SubstrateActorTemplateFence
}

type SubstrateDataCheckpointOperationProof struct {
	OperationID  string
	ActorID      string
	ActorUID     string
	ActorVersion int64
}

type SubstrateDataResumeOperationProof struct {
	OperationID  string
	ActorID      string
	ActorUID     string
	ActorVersion int64
}

type SubstrateWorkerPodFence struct {
	Namespace string
	Name      string
	UID       string
}

type SubstrateDataResumeCredentialFence struct {
	ResumeOperation SubstrateDataResumeOperationProof
	WorkerPod       SubstrateWorkerPodFence
}

type SubstrateCredentialBootstrapEnvelope struct {
	Nonce     string
	Signature string
	Body      []byte
}

type SubstrateCredentialBootstrapResult struct {
	AlreadyComplete bool
	PayloadConflict bool
	FenceConflict   bool
}

type SubstrateRuntimeActor struct {
	Atespace                string
	NativeTemplateName      string
	TemplateUID             string
	PodUID                  string
	ActorID                 string
	ActorUID                string
	ActorVersion            int64
	LatestDataOperationID   string
	TemplateNamespace       string
	TemplateName            string
	Status                  string
	PodNamespace            string
	PodName                 string
	PodIP                   string
	SnapshotObserved        bool
	DataSnapshot            *SubstrateDataSnapshotFence
	DataResumeOperation     *SubstrateDataResumeOperationProof
	DataCheckpointOperation *SubstrateDataCheckpointOperationProof
}

func (a *SubstrateRuntimeActor) VerifiedDataSnapshotFence(actorID string) (SubstrateDataSnapshotFence, string, error) {
	if a == nil || strings.TrimSpace(a.ActorID) != strings.TrimSpace(actorID) {
		return SubstrateDataSnapshotFence{}, "", fmt.Errorf("provider snapshot proof does not identify the exact actor")
	}
	if a.DataSnapshot == nil {
		return SubstrateDataSnapshotFence{}, "", fmt.Errorf("provider did not return an immutable ActorSnapshot proof")
	}
	fence := *a.DataSnapshot
	fence.ActorID = strings.TrimSpace(fence.ActorID)
	fence.ActorUID = strings.TrimSpace(fence.ActorUID)
	fence.SnapshotAtespace = strings.TrimSpace(fence.SnapshotAtespace)
	fence.SnapshotName = strings.TrimSpace(fence.SnapshotName)
	fence.SnapshotUID = strings.TrimSpace(fence.SnapshotUID)
	fence.SourceActorUID = strings.TrimSpace(fence.SourceActorUID)
	if fence.ActorID != strings.TrimSpace(actorID) || fence.ActorUID == "" || fence.ActorUID != strings.TrimSpace(a.ActorUID) ||
		fence.ActorVersion <= 0 || a.ActorVersion <= 0 || fence.ActorVersion > a.ActorVersion {
		return SubstrateDataSnapshotFence{}, "", fmt.Errorf("provider snapshot proof is missing a valid actor UID/version lineage")
	}
	if fence.SnapshotAtespace == "" || fence.SnapshotName == "" || fence.SnapshotUID == "" || fence.SnapshotVersion <= 0 {
		return SubstrateDataSnapshotFence{}, "", fmt.Errorf("provider snapshot proof is missing immutable snapshot identity")
	}
	if fence.SourceActorUID == "" || fence.SourceActorUID != fence.ActorUID ||
		fence.SourceActorVersion <= 0 || fence.SourceActorVersion > a.ActorVersion {
		return SubstrateDataSnapshotFence{}, "", fmt.Errorf("provider snapshot proof does not bind the snapshot to this actor generation")
	}
	if fence.ContentScope != SubstrateSnapshotContentScopeData {
		return SubstrateDataSnapshotFence{}, "", fmt.Errorf("provider ActorSnapshot content scope is not Data")
	}
	payload, err := json.Marshal(struct {
		SchemaVersion      string                        `json:"schemaVersion"`
		ActorID            string                        `json:"actorID"`
		ActorUID           string                        `json:"actorUID"`
		SnapshotAtespace   string                        `json:"snapshotAtespace"`
		SnapshotName       string                        `json:"snapshotName"`
		SnapshotUID        string                        `json:"snapshotUID"`
		SnapshotVersion    int64                         `json:"snapshotVersion"`
		SourceActorUID     string                        `json:"sourceActorUID"`
		SourceActorVersion int64                         `json:"sourceActorVersion"`
		ContentScope       SubstrateSnapshotContentScope `json:"contentScope"`
	}{
		SchemaVersion:      "orka.substrate-data-snapshot-fence.v2",
		ActorID:            fence.ActorID,
		ActorUID:           fence.ActorUID,
		SnapshotAtespace:   fence.SnapshotAtespace,
		SnapshotName:       fence.SnapshotName,
		SnapshotUID:        fence.SnapshotUID,
		SnapshotVersion:    fence.SnapshotVersion,
		SourceActorUID:     fence.SourceActorUID,
		SourceActorVersion: fence.SourceActorVersion,
		ContentScope:       fence.ContentScope,
	})
	if err != nil {
		return SubstrateDataSnapshotFence{}, "", fmt.Errorf("encode provider snapshot proof: %w", err)
	}
	sum := sha256.Sum256(payload)
	fence.ActorVersion = a.ActorVersion
	return fence, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (f SubstrateDataSnapshotFence) ImmutableIdentityDigest() (string, error) {
	namespace := strings.TrimSpace(f.SnapshotAtespace)
	name := strings.TrimSpace(f.SnapshotName)
	uid := strings.TrimSpace(f.SnapshotUID)
	if namespace == "" || name == "" || uid == "" || f.SnapshotVersion <= 0 {
		return "", fmt.Errorf("provider snapshot proof is missing immutable snapshot identity")
	}
	payload, err := json.Marshal(struct {
		SchemaVersion   string `json:"schemaVersion"`
		Namespace       string `json:"namespace"`
		Name            string `json:"name"`
		UID             string `json:"uid"`
		SnapshotVersion int64  `json:"snapshotVersion"`
	}{
		SchemaVersion:   "orka.substrate-data-snapshot-identity.v1",
		Namespace:       namespace,
		Name:            name,
		UID:             uid,
		SnapshotVersion: f.SnapshotVersion,
	})
	if err != nil {
		return "", fmt.Errorf("encode provider snapshot identity: %w", err)
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (a *SubstrateRuntimeActor) VerifiedDataCheckpointOperation(
	actorID, operationID string,
	sourceActorVersion int64,
) (SubstrateDataCheckpointOperationProof, string, error) {
	actorID = strings.TrimSpace(actorID)
	operationID = strings.TrimSpace(operationID)
	if a == nil || actorID == "" || strings.TrimSpace(a.ActorID) != actorID || operationID == "" {
		return SubstrateDataCheckpointOperationProof{}, "", fmt.Errorf("provider checkpoint proof does not identify the requested actor and operation")
	}
	if a.DataCheckpointOperation == nil {
		return SubstrateDataCheckpointOperationProof{}, "", fmt.Errorf("provider did not return a durable data-checkpoint operation proof")
	}
	proof := *a.DataCheckpointOperation
	proof.OperationID = strings.TrimSpace(proof.OperationID)
	proof.ActorID = strings.TrimSpace(proof.ActorID)
	proof.ActorUID = strings.TrimSpace(proof.ActorUID)
	actorUID := strings.TrimSpace(a.ActorUID)
	if proof.OperationID != operationID || strings.TrimSpace(a.LatestDataOperationID) != operationID ||
		proof.ActorID != actorID || proof.ActorUID == "" || proof.ActorUID != actorUID ||
		sourceActorVersion <= 0 || proof.ActorVersion != sourceActorVersion || a.ActorVersion < sourceActorVersion {
		return SubstrateDataCheckpointOperationProof{}, "", fmt.Errorf("provider data-checkpoint operation proof is not the latest operation for the exact actor lifetime and requested source Actor version")
	}
	payload, err := json.Marshal(struct {
		SchemaVersion string `json:"schemaVersion"`
		OperationID   string `json:"operationID"`
		ActorID       string `json:"actorID"`
		ActorUID      string `json:"actorUID"`
		ActorVersion  int64  `json:"actorVersion"`
	}{
		SchemaVersion: "orka.substrate-data-checkpoint-operation.v1",
		OperationID:   proof.OperationID,
		ActorID:       proof.ActorID,
		ActorUID:      proof.ActorUID,
		ActorVersion:  proof.ActorVersion,
	})
	if err != nil {
		return SubstrateDataCheckpointOperationProof{}, "", fmt.Errorf("encode provider data-checkpoint operation proof: %w", err)
	}
	sum := sha256.Sum256(payload)
	return proof, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (a *SubstrateRuntimeActor) VerifiedDataResumeOperation(
	actorID, operationID string,
) (SubstrateDataResumeOperationProof, string, error) {
	actorID = strings.TrimSpace(actorID)
	operationID = strings.TrimSpace(operationID)
	if a == nil || actorID == "" || strings.TrimSpace(a.ActorID) != actorID || operationID == "" {
		return SubstrateDataResumeOperationProof{}, "", fmt.Errorf("provider resume proof does not identify the requested actor and operation")
	}
	if a.DataResumeOperation == nil {
		return SubstrateDataResumeOperationProof{}, "", fmt.Errorf("provider did not return a durable data-resume operation proof")
	}
	proof := *a.DataResumeOperation
	proof.OperationID = strings.TrimSpace(proof.OperationID)
	proof.ActorID = strings.TrimSpace(proof.ActorID)
	proof.ActorUID = strings.TrimSpace(proof.ActorUID)
	actorUID := strings.TrimSpace(a.ActorUID)
	if proof.OperationID != operationID || strings.TrimSpace(a.LatestDataOperationID) != operationID ||
		proof.ActorID != actorID || proof.ActorUID == "" ||
		proof.ActorUID != actorUID || proof.ActorVersion <= 0 || a.ActorVersion < proof.ActorVersion {
		return SubstrateDataResumeOperationProof{}, "", fmt.Errorf("provider data-resume operation proof is not the latest operation for the exact actor lifetime")
	}
	payload, err := json.Marshal(struct {
		SchemaVersion string `json:"schemaVersion"`
		OperationID   string `json:"operationID"`
		ActorID       string `json:"actorID"`
		ActorUID      string `json:"actorUID"`
		ActorVersion  int64  `json:"actorVersion"`
	}{
		SchemaVersion: "orka.substrate-data-resume-operation.v1",
		OperationID:   proof.OperationID,
		ActorID:       proof.ActorID,
		ActorUID:      proof.ActorUID,
		ActorVersion:  proof.ActorVersion,
	})
	if err != nil {
		return SubstrateDataResumeOperationProof{}, "", fmt.Errorf("encode provider data-resume operation proof: %w", err)
	}
	sum := sha256.Sum256(payload)
	return proof, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (a *SubstrateRuntimeActor) Running() bool {
	return a != nil && a.Status == substrateStatusRunning && strings.TrimSpace(a.PodIP) != ""
}

func (a *SubstrateRuntimeActor) SuspendedOrSuspending() bool {
	return a != nil && (a.Status == substrateStatusSuspended || a.Status == substrateStatusSuspending)
}

func (a *SubstrateRuntimeActor) Suspended() bool {
	return a != nil && a.Status == substrateStatusSuspended
}

func (a *SubstrateRuntimeActor) Suspending() bool {
	return a != nil && a.Status == substrateStatusSuspending
}

func (a *SubstrateRuntimeActor) RunningStatus() bool {
	return a != nil && a.Status == substrateStatusRunning
}

func (a *SubstrateRuntimeActor) Resuming() bool {
	return a != nil && a.Status == substrateStatusResuming
}

func (a *SubstrateRuntimeActor) Crashed() bool {
	return a != nil && a.Status == substrateStatusCrashed
}

type SubstrateRuntimeActorControl interface {
	GetActor(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error)
	CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*SubstrateRuntimeActor, error)
	ResumeActor(ctx context.Context, actorID string, boot bool) (*SubstrateRuntimeActor, error)
	SettleActor(ctx context.Context, actorID string) (*SubstrateRuntimeActor, error)
	DeleteActor(ctx context.Context, actorID string) error
	Close() error
}

type SubstrateRuntimeActorDataCheckpointControl interface {
	DataSnapshotCheckpointFencingSupported() bool
	SuspendActorForDataCheckpoint(
		ctx context.Context,
		actorID string,
		expected SubstrateDataCheckpointFence,
	) (*SubstrateRuntimeActor, error)
}

type SubstrateRuntimeActorDataResumeControl interface {
	DataSnapshotResumeFencingSupported() bool
	DataResumeCredentialBootstrapFencingSupported() bool
	ResumeActorFromDataCheckpoint(
		ctx context.Context,
		actorID string,
		expected SubstrateDataResumeFence,
	) (*SubstrateRuntimeActor, error)
	BootstrapActorCredentialsForDataResume(
		ctx context.Context,
		actorID string,
		expected SubstrateDataResumeCredentialFence,
		envelope SubstrateCredentialBootstrapEnvelope,
	) (SubstrateCredentialBootstrapResult, error)
}

type SubstrateRuntimeActorCreateRecoveryControl interface {
	ActorCreateRecoveryAttestationSupported() bool
	ConfirmActorCreationSettled(ctx context.Context, actorID string) (bool, error)
}

type substrateRuntimeActorControl struct {
	control substrateControlClient
}

func NewSubstrateRuntimeActorControl(cfg SubstrateConfig) (SubstrateRuntimeActorControl, error) {
	control, err := resolveSubstrateControlClient(cfg)
	if err != nil {
		return nil, err
	}
	return &substrateRuntimeActorControl{control: control}, nil
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

func (c *substrateRuntimeActorControl) DeleteActor(ctx context.Context, actorID string) error {
	if err := c.control.DeleteActor(ctx, actorID, ""); err != nil && !IsKind(err, ErrorKindNotFound) {
		return err
	}
	return nil
}

func (c *substrateRuntimeActorControl) Close() error {
	return closeSubstrateControlClient(c.control)
}

func substrateRuntimeActorView(actor *substrateActor) *SubstrateRuntimeActor {
	if actor == nil {
		return nil
	}
	return &SubstrateRuntimeActor{
		ActorID:  strings.TrimSpace(actor.ActorID),
		ActorUID: actor.ActorUID, ActorVersion: actor.ActorVersion,
		Atespace: actor.Atespace, TemplateUID: actor.TemplateUID, PodUID: actor.PodUID,
		TemplateNamespace: strings.TrimSpace(actor.TemplateNamespace),
		TemplateName:      strings.TrimSpace(actor.TemplateName),
		Status:            strings.TrimSpace(actor.Status),
		PodNamespace:      strings.TrimSpace(actor.PodNamespace),
		PodName:           strings.TrimSpace(actor.PodName),
		PodIP:             strings.TrimSpace(actor.PodIP),
		SnapshotObserved:  strings.TrimSpace(actor.LastSnapshot) != "" || strings.TrimSpace(actor.InProgressSnapshot) != "",
	}
}
