package v2

import (
	"fmt"
	"strings"
	"time"
)

type WorkspaceIntent string

const (
	WorkspaceIntentRead  WorkspaceIntent = "read"
	WorkspaceIntentWrite WorkspaceIntent = "write"
)

func (i WorkspaceIntent) Validate() error {
	switch i {
	case WorkspaceIntentRead, WorkspaceIntentWrite:
		return nil
	default:
		return fmt.Errorf("unsupported workspace intent %q", i)
	}
}

type ModelTokenLimits struct {
	Context int64 `json:"context"`
	Output  int64 `json:"output"`
}

func (l ModelTokenLimits) Validate() error {
	if l.Context <= 0 {
		return fmt.Errorf("model context limit must be positive")
	}
	if l.Output <= 0 {
		return fmt.Errorf("model output limit must be positive")
	}
	if l.Context <= l.Output {
		return fmt.Errorf("model context limit must exceed output limit")
	}
	return nil
}

type RuntimeProfile struct {
	ACPProfile               string            `json:"acpProfile"`
	AdapterDigests           map[string]string `json:"adapterDigests"`
	ProviderKind             string            `json:"providerKind"`
	Model                    string            `json:"model"`
	ModelLimits              *ModelTokenLimits `json:"modelLimits,omitempty"`
	AgentConfigurationDigest string            `json:"agentConfigurationDigest"`
	ToolPolicyDigest         string            `json:"toolPolicyDigest"`
	ApprovalPolicyDigest     string            `json:"approvalPolicyDigest"`
	MCPConfigurationDigest   string            `json:"mcpConfigurationDigest"`
	WorkspaceIntent          WorkspaceIntent   `json:"workspaceIntent"`
	ProxyCredentialRole      string            `json:"proxyCredentialRole"`
	ProxyCredentialScope     string            `json:"proxyCredentialScope"`
	ResourceClass            string            `json:"resourceClass"`
}

func (p RuntimeProfile) Validate() error {
	if p.ACPProfile != ACPProfileV1 {
		return fmt.Errorf("ACP profile %q is unsupported; want %q", p.ACPProfile, ACPProfileV1)
	}
	if len(p.AdapterDigests) == 0 || len(p.AdapterDigests) > 32 {
		return fmt.Errorf("adapter digest count must be in range 1..32")
	}
	for name, digest := range p.AdapterDigests {
		if err := validateBoundedString("adapter name", name, true, 128); err != nil {
			return err
		}
		if err := validateSHA256Digest(digest); err != nil {
			return fmt.Errorf("adapter %q digest: %w", name, err)
		}
	}
	if err := validateBoundedString("provider kind", p.ProviderKind, true, 128); err != nil {
		return err
	}
	if err := validateBoundedString("model", p.Model, true, 256); err != nil {
		return err
	}
	if p.ModelLimits != nil {
		if err := p.ModelLimits.Validate(); err != nil {
			return err
		}
	}
	for name, digest := range map[string]string{
		"agent configuration digest": p.AgentConfigurationDigest,
		"tool policy digest":         p.ToolPolicyDigest,
		"approval policy digest":     p.ApprovalPolicyDigest,
		"MCP configuration digest":   p.MCPConfigurationDigest,
	} {
		if err := validateSHA256Digest(digest); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := p.WorkspaceIntent.Validate(); err != nil {
		return err
	}
	if err := validateBoundedString("proxy credential role", p.ProxyCredentialRole, true, 256); err != nil {
		return err
	}
	if err := validateBoundedString("proxy credential scope", p.ProxyCredentialScope, true, 1024); err != nil {
		return err
	}
	return validateBoundedString("resource class", p.ResourceClass, true, 128)
}

const (
	MinAgentMaxTurns          int32 = 1
	MaxAgentMaxTurns          int32 = 1000
	MaxAgentSystemPromptBytes       = 256 << 10

	providerKindCodex     = "codex"
	providerKindClaude    = "claude"
	providerKindCopilot   = "copilot"
	reasoningEffortLow    = "low"
	reasoningEffortMedium = "medium"
	reasoningEffortHigh   = "high"
	reasoningEffortXHigh  = "xhigh"
	reasoningEffortMax    = "max"
)

// AgentSessionConfiguration is the resolved, non-secret Agent configuration
// frozen into one RuntimeSession. Its canonical digest must match the
// RuntimeProfile AgentConfigurationDigest.
type AgentSessionConfiguration struct {
	AgentUID        string `json:"agentUID"`
	AgentGeneration int64  `json:"agentGeneration"`
	ProviderKind    string `json:"providerKind"`
	Model           string `json:"model"`
	MaxTurns        int32  `json:"maxTurns"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	SystemPrompt    string `json:"systemPrompt,omitempty"`
}

func (c AgentSessionConfiguration) Validate() error {
	if err := requireIdentifier("agent UID", c.AgentUID); err != nil {
		return err
	}
	if c.AgentGeneration <= 0 {
		return fmt.Errorf("agent generation must be positive")
	}
	if err := validateBoundedString("provider kind", c.ProviderKind, true, 128); err != nil {
		return err
	}
	if err := validateBoundedString("model", c.Model, true, 256); err != nil {
		return err
	}
	if err := validateAgentExecutionControls(c.ProviderKind, c.MaxTurns, c.ReasoningEffort); err != nil {
		return err
	}
	return validateBoundedString("agent system prompt", c.SystemPrompt, false, MaxAgentSystemPromptBytes)
}

func (c AgentSessionConfiguration) ValidateProfile(profile RuntimeProfile) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.ProviderKind != profile.ProviderKind {
		return fmt.Errorf("agent configuration provider kind %q does not match runtime profile provider kind %q", c.ProviderKind, profile.ProviderKind)
	}
	if c.Model != profile.Model {
		return fmt.Errorf("agent configuration model %q does not match runtime profile model %q", c.Model, profile.Model)
	}
	digest, err := CanonicalAgentConfigurationDigest(c)
	if err != nil {
		return fmt.Errorf("canonical agent configuration digest: %w", err)
	}
	if digest != profile.AgentConfigurationDigest {
		return fmt.Errorf("agent configuration digest mismatch: got %q, want %q", profile.AgentConfigurationDigest, digest)
	}
	return nil
}

func (c AgentSessionConfiguration) ValidateProfileOrLegacy(profile RuntimeProfile, allowBash bool) error {
	if c.ProviderKind != profile.ProviderKind {
		return fmt.Errorf("agent configuration provider kind %q does not match runtime profile provider kind %q", c.ProviderKind, profile.ProviderKind)
	}
	if c.Model != profile.Model {
		return fmt.Errorf("agent configuration model %q does not match runtime profile model %q", c.Model, profile.Model)
	}
	canonicalErr := c.ValidateProfile(profile)
	if canonicalErr == nil {
		return nil
	}
	legacyDigest, legacyErr := CanonicalLegacyAgentConfigurationDigest(c, allowBash)
	if legacyErr != nil {
		return canonicalErr
	}
	if legacyDigest != profile.AgentConfigurationDigest {
		return canonicalErr
	}
	return nil
}

func validateAgentExecutionControls(providerKind string, maxTurns int32, reasoningEffort string) error {
	if maxTurns < MinAgentMaxTurns || maxTurns > MaxAgentMaxTurns {
		return fmt.Errorf("max turns must be in range %d..%d", MinAgentMaxTurns, MaxAgentMaxTurns)
	}
	if err := validateBoundedString("reasoning effort", reasoningEffort, false, 16); err != nil {
		return err
	}
	switch reasoningEffort {
	case "", reasoningEffortLow, reasoningEffortMedium, reasoningEffortHigh, reasoningEffortXHigh, reasoningEffortMax:
	default:
		return fmt.Errorf("unsupported reasoning effort %q", reasoningEffort)
	}
	switch providerKind {
	case providerKindCodex:
		if reasoningEffort == reasoningEffortMax {
			return fmt.Errorf("codex provider does not support reasoning effort %q", reasoningEffort)
		}
	case providerKindClaude:
	case providerKindCopilot:
		if reasoningEffort != "" {
			return fmt.Errorf("copilot provider does not support reasoning effort")
		}
	default:
		if reasoningEffort != "" {
			return fmt.Errorf("provider %q does not support reasoning effort", providerKind)
		}
	}
	return nil
}

type ArtifactReference struct {
	ArtifactID ArtifactID `json:"artifactID"`
	Digest     string     `json:"digest"`
	SizeBytes  int64      `json:"sizeBytes"`
	MediaType  string     `json:"mediaType"`
}

func (a ArtifactReference) Validate() error {
	if err := requireIdentifier("artifact ID", string(a.ArtifactID)); err != nil {
		return err
	}
	if err := validateSHA256Digest(a.Digest); err != nil {
		return fmt.Errorf("artifact digest: %w", err)
	}
	if a.SizeBytes < 0 {
		return fmt.Errorf("artifact size must be non-negative")
	}
	return validateBoundedString("artifact media type", a.MediaType, true, 256)
}

type WorkspaceBaseline struct {
	RepositoryIdentity string             `json:"repositoryIdentity"`
	Revision           string             `json:"revision"`
	TreeDigest         string             `json:"treeDigest"`
	Artifact           *ArtifactReference `json:"artifact,omitempty"`
}

func (b WorkspaceBaseline) Validate() error {
	if err := validateBoundedString("repository identity", b.RepositoryIdentity, true, 1024); err != nil {
		return err
	}
	if err := validateBoundedString("workspace revision", b.Revision, true, 512); err != nil {
		return err
	}
	if err := validateSHA256Digest(b.TreeDigest); err != nil {
		return fmt.Errorf("workspace tree digest: %w", err)
	}
	if b.Artifact != nil {
		if err := b.Artifact.Validate(); err != nil {
			return fmt.Errorf("workspace artifact: %w", err)
		}
	}
	return nil
}

type WorkspaceSpec struct {
	Intent       WorkspaceIntent   `json:"intent"`
	Baseline     WorkspaceBaseline `json:"baseline"`
	RelativeRoot string            `json:"relativeRoot,omitempty"`
}

func (w WorkspaceSpec) Validate() error {
	if err := w.Intent.Validate(); err != nil {
		return err
	}
	if err := w.Baseline.Validate(); err != nil {
		return err
	}
	return validateWorkspaceRelativeRoot(w.RelativeRoot)
}

// ValidateWorkspaceRelativeRoot exposes the RuntimeSession workspace
// relative-root rule so the controller preflight can reject unsafe subPath
// values with exactly the semantics session creation enforces.
func ValidateWorkspaceRelativeRoot(value string) error {
	return validateWorkspaceRelativeRoot(value)
}

func validateWorkspaceRelativeRoot(value string) error {
	root := strings.TrimSpace(value)
	if root == "" || root == "." {
		return nil
	}
	if strings.HasPrefix(root, "/") || strings.Contains(root, "\\") {
		return fmt.Errorf("workspace relative root must be a relative slash-separated path")
	}
	for segment := range strings.SplitSeq(root, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("workspace relative root contains unsafe segment %q", segment)
		}
	}
	return validateBoundedString("workspace relative root", root, true, 1024)
}

type ArtifactAuthorization struct {
	Capability    string `json:"capability"`
	RequestDigest string `json:"requestDigest"`
}

func (a ArtifactAuthorization) Validate() error {
	if err := validateBoundedString("artifact capability", a.Capability, true, 16<<10); err != nil {
		return err
	}
	return validateSHA256Digest(a.RequestDigest)
}

type SessionBootstrap struct {
	TranscriptArtifact *ArtifactReference `json:"transcriptArtifact,omitempty"`
	TranscriptDigest   string             `json:"transcriptDigest,omitempty"`
	MessageCount       uint32             `json:"messageCount,omitempty"`
}

func (b SessionBootstrap) Validate() error {
	if b.TranscriptArtifact == nil {
		if b.TranscriptDigest != "" || b.MessageCount != 0 {
			return fmt.Errorf("transcript digest and message count require transcript artifact")
		}
		return nil
	}
	if err := b.TranscriptArtifact.Validate(); err != nil {
		return fmt.Errorf("transcript artifact: %w", err)
	}
	if err := validateSHA256Digest(b.TranscriptDigest); err != nil {
		return fmt.Errorf("transcript digest: %w", err)
	}
	return nil
}

type CreateRuntimeSessionRequest struct {
	Protocol                       string                     `json:"protocol"`
	Metadata                       MutationMetadata           `json:"metadata"`
	RuntimeSessionID               RuntimeSessionID           `json:"runtimeSessionID"`
	Profile                        RuntimeProfile             `json:"profile"`
	AgentConfiguration             *AgentSessionConfiguration `json:"agentConfiguration,omitempty"`
	MCPConfiguration               MCPPolicyConfiguration     `json:"mcpConfiguration"`
	Workspace                      WorkspaceSpec              `json:"workspace"`
	WorkspaceArtifactAuthorization *ArtifactAuthorization     `json:"workspaceArtifactAuthorization,omitempty"`
	Bootstrap                      *SessionBootstrap          `json:"bootstrap,omitempty"`
	BootstrapArtifactAuthorization *ArtifactAuthorization     `json:"bootstrapArtifactAuthorization,omitempty"`
}

func (r CreateRuntimeSessionRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := requireIdentifier("runtime session ID", string(r.RuntimeSessionID)); err != nil {
		return err
	}
	if err := r.Profile.Validate(); err != nil {
		return fmt.Errorf("runtime profile: %w", err)
	}
	profileDigest, err := CanonicalProfileDigest(r.Profile)
	if err != nil {
		return fmt.Errorf("runtime profile digest: %w", err)
	}
	if profileDigest != r.Metadata.Fence.RuntimeProfileDigest {
		return fmt.Errorf("runtime profile digest mismatch: got %q, want %q", r.Metadata.Fence.RuntimeProfileDigest, profileDigest)
	}
	if err := r.MCPConfiguration.ValidateProfile(r.Profile); err != nil {
		return fmt.Errorf("MCP configuration: %w", err)
	}
	if r.AgentConfiguration != nil {
		if err := r.AgentConfiguration.ValidateProfileOrLegacy(r.Profile, r.MCPConfiguration.ToolPolicy.AllowBash); err != nil {
			return fmt.Errorf("agent configuration: %w", err)
		}
	}
	if err := r.Workspace.Validate(); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if r.Workspace.Baseline.Artifact != nil {
		if r.WorkspaceArtifactAuthorization == nil {
			return fmt.Errorf("workspace artifact authorization is required")
		}
		if err := r.WorkspaceArtifactAuthorization.Validate(); err != nil {
			return fmt.Errorf("workspace artifact authorization: %w", err)
		}
	} else if r.WorkspaceArtifactAuthorization != nil {
		return fmt.Errorf("workspace artifact authorization requires a workspace artifact")
	}
	if r.Workspace.Intent != r.Profile.WorkspaceIntent {
		return fmt.Errorf("workspace intent %q does not match runtime profile intent %q", r.Workspace.Intent, r.Profile.WorkspaceIntent)
	}
	if r.Bootstrap != nil {
		if err := r.Bootstrap.Validate(); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
	if r.Bootstrap != nil && r.Bootstrap.TranscriptArtifact != nil {
		if r.BootstrapArtifactAuthorization == nil {
			return fmt.Errorf("bootstrap artifact authorization is required")
		}
		if err := r.BootstrapArtifactAuthorization.Validate(); err != nil {
			return fmt.Errorf("bootstrap artifact authorization: %w", err)
		}
	} else if r.BootstrapArtifactAuthorization != nil {
		return fmt.Errorf("bootstrap artifact authorization requires a transcript artifact")
	}
	return r.Metadata.ValidateDigest(r)
}

type RuntimeSessionDescriptor struct {
	RuntimeSessionID     RuntimeSessionID    `json:"runtimeSessionID"`
	RuntimeSessionUID    RuntimeSessionUID   `json:"runtimeSessionUID"`
	Generation           uint64              `json:"generation"`
	RuntimeInstanceID    RuntimeInstanceID   `json:"runtimeInstanceID"`
	SupervisorBootID     SupervisorBootID    `json:"supervisorBootID"`
	RuntimeProfileDigest ProfileDigest       `json:"runtimeProfileDigest"`
	State                RuntimeSessionState `json:"state"`
	ProviderSessionID    string              `json:"providerSessionID"`
	WorkspaceBaseline    WorkspaceBaseline   `json:"workspaceBaseline"`
	CreatedAt            time.Time           `json:"createdAt"`
	LastTransitionAt     time.Time           `json:"lastTransitionAt"`
}

func (d RuntimeSessionDescriptor) Validate() error {
	if err := requireIdentifier("runtime session ID", string(d.RuntimeSessionID)); err != nil {
		return err
	}
	if err := requireIdentifier("runtime session UID", string(d.RuntimeSessionUID)); err != nil {
		return err
	}
	if d.Generation == 0 {
		return fmt.Errorf("runtime session generation must be positive")
	}
	if err := requireIdentifier("runtime instance ID", string(d.RuntimeInstanceID)); err != nil {
		return err
	}
	if err := requireIdentifier("supervisor boot ID", string(d.SupervisorBootID)); err != nil {
		return err
	}
	if err := ValidateProfileDigest(d.RuntimeProfileDigest); err != nil {
		return fmt.Errorf("runtime profile digest: %w", err)
	}
	if !IsKnownRuntimeSessionState(d.State) {
		return fmt.Errorf("unsupported runtime session state %q", d.State)
	}
	if err := validateBoundedString("provider session ID", d.ProviderSessionID, true, 1024); err != nil {
		return err
	}
	if err := d.WorkspaceBaseline.Validate(); err != nil {
		return err
	}
	if err := validateTimestamp("created timestamp", d.CreatedAt); err != nil {
		return err
	}
	if err := validateTimestamp("last transition timestamp", d.LastTransitionAt); err != nil {
		return err
	}
	if d.LastTransitionAt.Before(d.CreatedAt) {
		return fmt.Errorf("last transition timestamp precedes creation")
	}
	return nil
}

type CreateRuntimeSessionResponse struct {
	Protocol       string                   `json:"protocol"`
	Classification Classification           `json:"classification"`
	Session        RuntimeSessionDescriptor `json:"session"`
}

func (r CreateRuntimeSessionResponse) ValidateFor(request CreateRuntimeSessionRequest) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	switch r.Classification.Class {
	case RequestClassificationFresh, RequestClassificationDuplicate:
	default:
		return fmt.Errorf("session create response classification %q is invalid", r.Classification.Class)
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	if err := r.Session.Validate(); err != nil {
		return fmt.Errorf("session descriptor: %w", err)
	}
	if r.Session.State != RuntimeSessionStateIdle {
		return fmt.Errorf("session create must return initialized idle session, got %q", r.Session.State)
	}
	if r.Session.RuntimeSessionID != request.RuntimeSessionID ||
		r.Session.RuntimeSessionUID != request.Metadata.Fence.RuntimeSessionUID ||
		r.Session.Generation != request.Metadata.Fence.RuntimeSessionGeneration ||
		r.Session.RuntimeProfileDigest != request.Metadata.Fence.RuntimeProfileDigest {
		return fmt.Errorf("session create response identity does not match request")
	}
	return nil
}

type PublicationTerminalState string

const (
	PublicationTerminalVerifiedExact          PublicationTerminalState = "verified_exact"
	PublicationTerminalDeliveredSuperseded    PublicationTerminalState = "delivered_superseded"
	PublicationTerminalCancelledBeforePublish PublicationTerminalState = "cancelled_before_publish"
	PublicationTerminalDeliveryConflict       PublicationTerminalState = "delivery_conflict"
	PublicationTerminalCredentialBlocked      PublicationTerminalState = "credential_blocked"
	PublicationTerminalPreparationFailed      PublicationTerminalState = "preparation_failed"
	PublicationTerminalOutcomeUnknown         PublicationTerminalState = "outcome_unknown"
)

func (s PublicationTerminalState) Validate() error {
	switch s {
	case PublicationTerminalVerifiedExact, PublicationTerminalDeliveredSuperseded,
		PublicationTerminalCancelledBeforePublish, PublicationTerminalDeliveryConflict,
		PublicationTerminalCredentialBlocked, PublicationTerminalPreparationFailed, PublicationTerminalOutcomeUnknown:
		return nil
	default:
		return fmt.Errorf("unsupported terminal publication state %q", s)
	}
}

type FinalizeRuntimeSessionPublicationRequest struct {
	Protocol              string                   `json:"protocol"`
	Metadata              MutationMetadata         `json:"metadata"`
	WorkspaceDeltaID      WorkspaceDeltaID         `json:"workspaceDeltaID"`
	PublicationID         string                   `json:"publicationID"`
	PublicationGeneration uint64                   `json:"publicationGeneration"`
	PublicationVersion    uint64                   `json:"publicationVersion"`
	TerminalState         PublicationTerminalState `json:"terminalState"`
	TerminalReceiptDigest string                   `json:"terminalReceiptDigest"`
}

func (r FinalizeRuntimeSessionPublicationRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true, prompt: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := requireIdentifier("workspace delta ID", string(r.WorkspaceDeltaID)); err != nil {
		return err
	}
	if err := validateBoundedString("publication ID", r.PublicationID, true, 253); err != nil {
		return err
	}
	if r.PublicationGeneration == 0 || r.PublicationVersion == 0 {
		return fmt.Errorf("publication generation and version must be positive")
	}
	if err := r.TerminalState.Validate(); err != nil {
		return err
	}
	if err := validateSHA256Digest(r.TerminalReceiptDigest); err != nil {
		return fmt.Errorf("terminal publication receipt digest: %w", err)
	}
	return r.Metadata.ValidateDigest(r)
}

type PublicationFinalizationReceipt struct {
	WorkspaceDeltaID      WorkspaceDeltaID         `json:"workspaceDeltaID"`
	PublicationID         string                   `json:"publicationID"`
	PublicationGeneration uint64                   `json:"publicationGeneration"`
	PublicationVersion    uint64                   `json:"publicationVersion"`
	TerminalState         PublicationTerminalState `json:"terminalState"`
	TerminalReceiptDigest string                   `json:"terminalReceiptDigest"`
	AppliedAt             time.Time                `json:"appliedAt"`
}

func (r PublicationFinalizationReceipt) Validate() error {
	request := FinalizeRuntimeSessionPublicationRequest{
		Protocol: ProtocolVersion, WorkspaceDeltaID: r.WorkspaceDeltaID, PublicationID: r.PublicationID,
		PublicationGeneration: r.PublicationGeneration, PublicationVersion: r.PublicationVersion,
		TerminalState: r.TerminalState, TerminalReceiptDigest: r.TerminalReceiptDigest,
	}
	if err := requireIdentifier("workspace delta ID", string(request.WorkspaceDeltaID)); err != nil {
		return err
	}
	if err := validateBoundedString("publication ID", request.PublicationID, true, 253); err != nil {
		return err
	}
	if request.PublicationGeneration == 0 || request.PublicationVersion == 0 {
		return fmt.Errorf("publication generation and version must be positive")
	}
	if err := request.TerminalState.Validate(); err != nil {
		return err
	}
	if err := validateSHA256Digest(request.TerminalReceiptDigest); err != nil {
		return fmt.Errorf("terminal publication receipt digest: %w", err)
	}
	return validateTimestamp("publication finalization applied timestamp", r.AppliedAt)
}

type FinalizeRuntimeSessionPublicationResponse struct {
	Protocol       string                         `json:"protocol"`
	Classification Classification                 `json:"classification"`
	Session        RuntimeSessionDescriptor       `json:"session"`
	Finalization   PublicationFinalizationReceipt `json:"finalization"`
}

func (r FinalizeRuntimeSessionPublicationResponse) ValidateFor(request FinalizeRuntimeSessionPublicationRequest) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	switch r.Classification.Class {
	case RequestClassificationFresh, RequestClassificationDuplicate:
	default:
		return fmt.Errorf("publication finalization response classification %q is invalid", r.Classification.Class)
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	if err := r.Session.Validate(); err != nil {
		return fmt.Errorf("session descriptor: %w", err)
	}
	if r.Session.State != RuntimeSessionStateFinalizing {
		return fmt.Errorf("publication finalization must return finalizing session, got %q", r.Session.State)
	}
	if r.Session.RuntimeSessionUID != request.Metadata.Fence.RuntimeSessionUID ||
		r.Session.Generation != request.Metadata.Fence.RuntimeSessionGeneration ||
		r.Session.RuntimeProfileDigest != request.Metadata.Fence.RuntimeProfileDigest {
		return fmt.Errorf("publication finalization response identity does not match request")
	}
	if err := r.Finalization.Validate(); err != nil {
		return fmt.Errorf("publication finalization receipt: %w", err)
	}
	if r.Finalization.WorkspaceDeltaID != request.WorkspaceDeltaID || r.Finalization.PublicationID != request.PublicationID ||
		r.Finalization.PublicationGeneration != request.PublicationGeneration || r.Finalization.PublicationVersion != request.PublicationVersion ||
		r.Finalization.TerminalState != request.TerminalState || r.Finalization.TerminalReceiptDigest != request.TerminalReceiptDigest {
		return fmt.Errorf("publication finalization receipt does not match request")
	}
	return nil
}

type DeleteRuntimeSessionRequest struct {
	Protocol string           `json:"protocol"`
	Metadata MutationMetadata `json:"metadata"`
	Reason   string           `json:"reason,omitempty"`
}

func (r DeleteRuntimeSessionRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := validateBoundedString("delete reason", r.Reason, false, MaxDiagnosticBytes); err != nil {
		return err
	}
	return r.Metadata.ValidateDigest(r)
}

type DeleteRuntimeSessionResponse struct {
	Protocol       string                  `json:"protocol"`
	Classification Classification          `json:"classification"`
	State          RuntimeSessionState     `json:"state"`
	Tombstone      RuntimeSessionTombstone `json:"tombstone"`
}

func (r DeleteRuntimeSessionResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	if r.State != RuntimeSessionStateDeleted {
		return fmt.Errorf("delete response state must be deleted")
	}
	if err := r.Tombstone.Validate(); err != nil {
		return fmt.Errorf("tombstone: %w", err)
	}
	return nil
}

func (r DeleteRuntimeSessionResponse) ValidateFor(request DeleteRuntimeSessionRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	switch r.Classification.Class {
	case RequestClassificationFresh:
	case RequestClassificationDuplicate:
		if r.Classification.Phase != OperationPhaseDeleted {
			return fmt.Errorf("duplicate delete response must replay deleted phase, got %q", r.Classification.Phase)
		}
	default:
		return fmt.Errorf("delete response classification %q is invalid", r.Classification.Class)
	}
	if r.Tombstone.RuntimeSessionUID != request.Metadata.Fence.RuntimeSessionUID ||
		r.Tombstone.RuntimeSessionGeneration != request.Metadata.Fence.RuntimeSessionGeneration ||
		r.Tombstone.RuntimeProfileDigest != request.Metadata.Fence.RuntimeProfileDigest {
		return fmt.Errorf("delete tombstone identity does not match request fence")
	}
	for i := range r.Tombstone.Operations {
		operation := r.Tombstone.Operations[i]
		if operation.OperationID != request.Metadata.OperationID {
			continue
		}
		if operation.RequestDigest != request.Metadata.RequestDigest {
			return fmt.Errorf("delete tombstone operation digest does not match request")
		}
		if operation.Phase != OperationPhaseDeleted {
			return fmt.Errorf("delete tombstone operation phase must be deleted, got %q", operation.Phase)
		}
		return nil
	}
	return fmt.Errorf("delete tombstone does not contain the request operation")
}

type DrainRequest struct {
	Protocol string           `json:"protocol"`
	Metadata MutationMetadata `json:"metadata"`
	Reason   string           `json:"reason,omitempty"`
}

func (r DrainRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if r.Metadata.Fence.RuntimeSessionUID != "" || r.Metadata.Fence.RuntimeSessionGeneration != 0 {
		return fmt.Errorf("drain request must use a pool-wide fence without runtime session identity")
	}
	if r.Metadata.TaskUID != "" || r.Metadata.PromptID != "" {
		return fmt.Errorf("drain request must not carry task or prompt identity")
	}
	if err := validateBoundedString("drain reason", r.Reason, false, MaxDiagnosticBytes); err != nil {
		return err
	}
	return r.Metadata.ValidateDigest(r)
}

type DrainResponse struct {
	Protocol       string         `json:"protocol"`
	Classification Classification `json:"classification"`
	Drain          DrainStatus    `json:"drain"`
}

func (r DrainResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	if !r.Drain.Requested || r.Drain.AcceptingNewSessions {
		return fmt.Errorf("drain response must report requested drain with admission disabled")
	}
	return validateTimestamp("drain request timestamp", r.Drain.RequestedAt)
}
