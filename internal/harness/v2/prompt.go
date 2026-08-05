package v2

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type ContentBlockType string

const (
	ContentBlockText         ContentBlockType = "text"
	ContentBlockResourceLink ContentBlockType = "resource_link"
	ContentBlockArtifactRef  ContentBlockType = "artifact_ref"
)

type ContentBlock struct {
	Type     ContentBlockType   `json:"type"`
	Text     string             `json:"text,omitempty"`
	URI      string             `json:"uri,omitempty"`
	Name     string             `json:"name,omitempty"`
	MimeType string             `json:"mimeType,omitempty"`
	Artifact *ArtifactReference `json:"artifact,omitempty"`
}

func (b ContentBlock) Validate() error {
	switch b.Type {
	case ContentBlockText:
		if err := validateBoundedString("content text", b.Text, true, MaxPromptContentBytes); err != nil {
			return err
		}
		if b.URI != "" || b.Artifact != nil {
			return fmt.Errorf("text content must not carry URI or artifact")
		}
	case ContentBlockResourceLink:
		if err := validateBoundedString("resource URI", b.URI, true, 4096); err != nil {
			return err
		}
		parsed, err := url.Parse(b.URI)
		if err != nil || !parsed.IsAbs() || parsed.User != nil {
			return fmt.Errorf("resource URI must be absolute and must not contain userinfo")
		}
		if b.Text != "" || b.Artifact != nil {
			return fmt.Errorf("resource link must not carry text or artifact")
		}
	case ContentBlockArtifactRef:
		if b.Artifact == nil {
			return fmt.Errorf("artifact content requires artifact reference")
		}
		if err := b.Artifact.Validate(); err != nil {
			return err
		}
		if b.Text != "" || b.URI != "" {
			return fmt.Errorf("artifact content must not carry text or URI")
		}
	default:
		return fmt.Errorf("unsupported content block type %q", b.Type)
	}
	if err := validateBoundedString("content name", b.Name, false, 1024); err != nil {
		return err
	}
	return validateBoundedString("content MIME type", b.MimeType, false, 256)
}

type PromptInput struct {
	Content  []ContentBlock    `json:"content"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func (i PromptInput) Validate() error {
	if len(i.Content) == 0 || len(i.Content) > MaxContentBlocks {
		return fmt.Errorf("prompt content block count must be in range 1..%d", MaxContentBlocks)
	}
	for index := range i.Content {
		if err := i.Content[index].Validate(); err != nil {
			return fmt.Errorf("content block %d: %w", index, err)
		}
	}
	encodedContent, err := json.Marshal(i.Content)
	if err != nil {
		return fmt.Errorf("marshal prompt content: %w", err)
	}
	if len(encodedContent) > MaxPromptContentBytes {
		return fmt.Errorf("prompt content exceeds %d bytes", MaxPromptContentBytes)
	}
	if len(i.Metadata) > MaxMetadataEntries {
		return fmt.Errorf("prompt metadata entry count exceeds %d", MaxMetadataEntries)
	}
	for key, value := range i.Metadata {
		if err := validateBoundedString("prompt metadata key", key, true, 256); err != nil {
			return err
		}
		if err := validateBoundedString("prompt metadata value", value, false, MaxProtocolStringBytes); err != nil {
			return err
		}
	}
	return nil
}

type PromptLease struct {
	Generation uint64    `json:"generation"`
	IssuedAt   time.Time `json:"issuedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

func (l PromptLease) ValidateAt(now time.Time, minTTL, maxTTL time.Duration) error {
	if l.Generation == 0 {
		return fmt.Errorf("prompt lease generation must be positive")
	}
	if err := validateTimestamp("prompt lease issue timestamp", l.IssuedAt); err != nil {
		return err
	}
	if err := validateTimestamp("prompt lease expiry", l.ExpiresAt); err != nil {
		return err
	}
	if !l.ExpiresAt.After(l.IssuedAt) {
		return fmt.Errorf("prompt lease expiry must follow issue timestamp")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if l.IssuedAt.After(now.Add(5 * time.Second)) {
		return fmt.Errorf("prompt lease issue timestamp is in the future")
	}
	if !l.ExpiresAt.After(now) {
		return fmt.Errorf("prompt lease is expired")
	}
	ttl := l.ExpiresAt.Sub(l.IssuedAt)
	if minTTL > 0 && ttl < minTTL {
		return fmt.Errorf("prompt lease duration %s is below minimum %s", ttl, minTTL)
	}
	if maxTTL > 0 && ttl > maxTTL {
		return fmt.Errorf("prompt lease duration %s exceeds maximum %s", ttl, maxTTL)
	}
	return nil
}

func (l PromptLease) ActiveAt(now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return l.Generation > 0 && !l.IssuedAt.IsZero() && l.ExpiresAt.After(now)
}

func ValidatePromptLeaseRenewal(current, proposed PromptLease, expectedGeneration uint64, now time.Time, maxExtension time.Duration) error {
	if current.Generation != expectedGeneration {
		return fmt.Errorf("stale prompt lease generation %d; current generation is %d", expectedGeneration, current.Generation)
	}
	if proposed.Generation != current.Generation+1 {
		return fmt.Errorf("renewed prompt lease generation must be %d", current.Generation+1)
	}
	if !proposed.ExpiresAt.After(current.ExpiresAt) {
		return fmt.Errorf("renewed prompt lease must extend expiry")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if proposed.IssuedAt.Before(now.Add(-time.Second)) || proposed.IssuedAt.After(now.Add(time.Second)) {
		return fmt.Errorf("renewed prompt lease issue timestamp is outside allowed clock skew")
	}
	if maxExtension > 0 && proposed.ExpiresAt.After(now.Add(maxExtension)) {
		return fmt.Errorf("renewed prompt lease exceeds maximum extension %s", maxExtension)
	}
	return proposed.ValidateAt(now, 0, maxExtension)
}

type PromptMCPAuthorization struct {
	RuntimeSessionUID      RuntimeSessionUID `json:"runtimeSessionUID"`
	SessionGeneration      uint64            `json:"sessionGeneration"`
	TaskUID                TaskUID           `json:"taskUID"`
	TaskAttempt            uint32            `json:"taskAttempt"`
	PromptID               PromptID          `json:"promptID"`
	LeaseGeneration        uint64            `json:"leaseGeneration"`
	ToolPolicyDigest       string            `json:"toolPolicyDigest"`
	ApprovalPolicyDigest   string            `json:"approvalPolicyDigest"`
	MCPConfigurationDigest string            `json:"mcpConfigurationDigest"`
	ToolPolicy             MCPToolPolicy     `json:"toolPolicy"`
	ApprovalPolicy         MCPApprovalPolicy `json:"approvalPolicy"`
	ExpiresAt              time.Time         `json:"expiresAt"`
}

func (a PromptMCPAuthorization) ValidateFor(metadata MutationMetadata, lease PromptLease) error {
	return a.ValidateForAt(metadata, lease, time.Now().UTC())
}

func (a PromptMCPAuthorization) ValidateForAt(metadata MutationMetadata, lease PromptLease, now time.Time) error {
	if a.RuntimeSessionUID != metadata.Fence.RuntimeSessionUID ||
		a.SessionGeneration != metadata.Fence.RuntimeSessionGeneration ||
		a.TaskUID != metadata.TaskUID || a.TaskAttempt != metadata.TaskAttempt ||
		a.PromptID != metadata.PromptID {
		return fmt.Errorf("prompt MCP authorization identity does not match request")
	}
	if a.LeaseGeneration != lease.Generation {
		return fmt.Errorf("prompt MCP authorization lease generation does not match prompt lease")
	}
	if err := a.Configuration().validate(); err != nil {
		return err
	}
	if err := validateTimestamp("MCP authorization expiry", a.ExpiresAt); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !a.ExpiresAt.After(now) {
		return fmt.Errorf("MCP authorization is expired")
	}
	if metadata.ExpiresAt.After(a.ExpiresAt) {
		return fmt.Errorf("request expiry must not outlive MCP authorization")
	}
	if a.ExpiresAt.After(lease.ExpiresAt) {
		return fmt.Errorf("MCP authorization expiry must not outlive prompt lease")
	}
	return nil
}

func (a PromptMCPAuthorization) Configuration() MCPPolicyConfiguration {
	return MCPPolicyConfiguration{
		ToolPolicyDigest: a.ToolPolicyDigest, ApprovalPolicyDigest: a.ApprovalPolicyDigest,
		MCPConfigurationDigest: a.MCPConfigurationDigest,
		ToolPolicy:             a.ToolPolicy, ApprovalPolicy: a.ApprovalPolicy,
	}
}

func (a PromptMCPAuthorization) ValidateProfile(profile RuntimeProfile) error {
	return a.Configuration().ValidateProfile(profile)
}

func (a PromptMCPAuthorization) AuthorizedAt(state RuntimeSessionState, lease PromptLease, now time.Time) bool {
	if !state.AllowsPromptScopedMCP() || a.LeaseGeneration != lease.Generation {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return lease.ActiveAt(now) && a.ExpiresAt.After(now)
}

type StartPromptRequest struct {
	Protocol         string                 `json:"protocol"`
	Metadata         MutationMetadata       `json:"metadata"`
	Lease            PromptLease            `json:"lease"`
	MCPAuthorization PromptMCPAuthorization `json:"mcpAuthorization"`
	Input            PromptInput            `json:"input"`
}

func (r StartPromptRequest) ValidateAt(now time.Time, minLease, maxLease time.Duration) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true, prompt: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := r.Lease.ValidateAt(now, minLease, maxLease); err != nil {
		return fmt.Errorf("prompt lease: %w", err)
	}
	if r.Metadata.ExpiresAt.After(r.Lease.ExpiresAt) {
		return fmt.Errorf("request expiry must not outlive prompt lease")
	}
	if err := r.MCPAuthorization.ValidateForAt(r.Metadata, r.Lease, now); err != nil {
		return err
	}
	if err := r.Input.Validate(); err != nil {
		return fmt.Errorf("prompt input: %w", err)
	}
	return r.Metadata.ValidateDigest(r)
}

type PromptSettlement struct {
	TerminalEvent EventType     `json:"terminalEvent"`
	Outcome       PromptOutcome `json:"outcome"`
	StopReason    ACPStopReason `json:"stopReason,omitempty"`
	SettledAt     time.Time     `json:"settledAt"`
}

func (s PromptSettlement) Validate() error {
	if !s.TerminalEvent.IsTerminal() {
		return fmt.Errorf("prompt settlement requires terminal event")
	}
	if err := s.Outcome.Validate(); err != nil {
		return err
	}
	if err := validateTimestamp("prompt settlement timestamp", s.SettledAt); err != nil {
		return err
	}
	if s.TerminalEvent == EventOutcomeUnknown {
		if s.Outcome != PromptOutcomeUnknown {
			return fmt.Errorf("outcome_unknown terminal event requires outcome_unknown outcome")
		}
		return nil
	}
	mapping := MapACPStopReason(s.StopReason, true)
	if mapping.EventType != s.TerminalEvent || mapping.Outcome != s.Outcome {
		return fmt.Errorf("terminal event/outcome %q/%q is inconsistent with stop reason %q", s.TerminalEvent, s.Outcome, s.StopReason)
	}
	return nil
}

type PromptAdmissionResponse struct {
	Protocol       string            `json:"protocol"`
	Classification Classification    `json:"classification"`
	AcceptedAt     time.Time         `json:"acceptedAt,omitempty"`
	Settlement     *PromptSettlement `json:"settlement,omitempty"`
}

func (r PromptAdmissionResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	switch r.Classification.Class {
	case RequestClassificationFresh:
		if err := validateTimestamp("prompt acceptance timestamp", r.AcceptedAt); err != nil {
			return err
		}
		if r.Settlement != nil {
			return fmt.Errorf("fresh prompt admission must not carry settlement")
		}
	case RequestClassificationAlreadyAccepted:
		if err := validateTimestamp("prompt acceptance timestamp", r.AcceptedAt); err != nil {
			return err
		}
		if r.Settlement != nil {
			return fmt.Errorf("already accepted prompt must not carry settlement")
		}
	case RequestClassificationSettled:
		if r.Settlement == nil {
			return fmt.Errorf("settled prompt admission requires settlement metadata")
		}
		return r.Settlement.Validate()
	default:
		return fmt.Errorf("unsupported prompt admission classification %q", r.Classification.Class)
	}
	return nil
}

type RenewPromptLeaseRequest struct {
	Protocol                string                 `json:"protocol"`
	Metadata                MutationMetadata       `json:"metadata"`
	ExpectedLeaseGeneration uint64                 `json:"expectedLeaseGeneration"`
	Lease                   PromptLease            `json:"lease"`
	MCPAuthorization        PromptMCPAuthorization `json:"mcpAuthorization"`
}

func (r RenewPromptLeaseRequest) ValidateAt(now time.Time, maxExtension time.Duration) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true, prompt: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if r.ExpectedLeaseGeneration == 0 {
		return fmt.Errorf("expected lease generation must be positive")
	}
	if r.Lease.Generation != r.ExpectedLeaseGeneration+1 {
		return fmt.Errorf("proposed lease generation must follow expected generation")
	}
	if err := r.Lease.ValidateAt(now, 0, maxExtension); err != nil {
		return err
	}
	if err := r.MCPAuthorization.ValidateForAt(r.Metadata, r.Lease, now); err != nil {
		return err
	}
	return r.Metadata.ValidateDigest(r)
}

type PromptLeaseResponse struct {
	Protocol       string         `json:"protocol"`
	Classification Classification `json:"classification"`
	Lease          PromptLease    `json:"lease"`
}

func (r PromptLeaseResponse) ValidateAt(now time.Time, maxLease time.Duration) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	return r.Lease.ValidateAt(now, 0, maxLease)
}

type PermissionOptionKind string

const (
	PermissionOptionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionOptionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionOptionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionOptionRejectAlways PermissionOptionKind = "reject_always"
)

type PermissionOption struct {
	OptionID string               `json:"optionID"`
	Name     string               `json:"name"`
	Kind     PermissionOptionKind `json:"kind"`
}

func (o PermissionOption) Validate() error {
	if err := requireIdentifier("permission option ID", o.OptionID); err != nil {
		return err
	}
	if err := validateBoundedString("permission option name", o.Name, true, 512); err != nil {
		return err
	}
	switch o.Kind {
	case PermissionOptionAllowOnce, PermissionOptionAllowAlways, PermissionOptionRejectOnce, PermissionOptionRejectAlways:
		return nil
	default:
		return fmt.Errorf("unsupported permission option kind %q", o.Kind)
	}
}

type PermissionDecisionOutcome string

const (
	PermissionDecisionSelected  PermissionDecisionOutcome = "selected"
	PermissionDecisionCancelled PermissionDecisionOutcome = "cancelled"
)

type PermissionDecision struct {
	Outcome  PermissionDecisionOutcome `json:"outcome"`
	OptionID string                    `json:"optionID,omitempty"`
}

func (d PermissionDecision) Validate() error {
	switch d.Outcome {
	case PermissionDecisionSelected:
		return requireIdentifier("selected permission option ID", d.OptionID)
	case PermissionDecisionCancelled:
		if strings.TrimSpace(d.OptionID) != "" {
			return fmt.Errorf("cancelled permission decision must not select an option")
		}
		return nil
	default:
		return fmt.Errorf("unsupported permission decision outcome %q", d.Outcome)
	}
}

type ResolvePermissionRequest struct {
	Protocol  string              `json:"protocol"`
	Metadata  MutationMetadata    `json:"metadata"`
	RequestID PermissionRequestID `json:"requestID"`
	Decision  PermissionDecision  `json:"decision"`
}

func (r ResolvePermissionRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true, prompt: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := requireIdentifier("permission request ID", string(r.RequestID)); err != nil {
		return err
	}
	if err := r.Decision.Validate(); err != nil {
		return err
	}
	return r.Metadata.ValidateDigest(r)
}

type PermissionResolutionState string

const (
	PermissionResolutionApplied           PermissionResolutionState = "applied"
	PermissionResolutionAlreadyResolved   PermissionResolutionState = "already_resolved"
	PermissionResolutionCancelledByPrompt PermissionResolutionState = "cancelled_by_prompt"
)

type PermissionResolutionResponse struct {
	Protocol       string                    `json:"protocol"`
	Classification Classification            `json:"classification"`
	State          PermissionResolutionState `json:"state"`
	Decision       PermissionDecision        `json:"decision"`
	ResolvedAt     time.Time                 `json:"resolvedAt"`
}

func (r PermissionResolutionResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	switch r.State {
	case PermissionResolutionApplied, PermissionResolutionAlreadyResolved, PermissionResolutionCancelledByPrompt:
	default:
		return fmt.Errorf("unsupported permission resolution state %q", r.State)
	}
	if err := r.Decision.Validate(); err != nil {
		return err
	}
	if r.State == PermissionResolutionCancelledByPrompt && r.Decision.Outcome != PermissionDecisionCancelled {
		return fmt.Errorf("permission invalidated by prompt cancellation must resolve as cancelled")
	}
	return validateTimestamp("permission resolution timestamp", r.ResolvedAt)
}

type CancelReason string

const (
	CancelReasonUserRequested      CancelReason = "user_requested"
	CancelReasonTaskTimeout        CancelReason = "task_timeout"
	CancelReasonLeaseExpired       CancelReason = "lease_expired"
	CancelReasonStreamDisconnected CancelReason = "stream_disconnected"
	CancelReasonControllerShutdown CancelReason = "controller_shutdown"
)

type CancelPromptRequest struct {
	Protocol           string           `json:"protocol"`
	Metadata           MutationMetadata `json:"metadata"`
	Reason             CancelReason     `json:"reason"`
	SettlementDeadline time.Time        `json:"settlementDeadline"`
}

func (r CancelPromptRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true, prompt: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	switch r.Reason {
	case CancelReasonUserRequested, CancelReasonTaskTimeout, CancelReasonLeaseExpired,
		CancelReasonStreamDisconnected, CancelReasonControllerShutdown:
	default:
		return fmt.Errorf("unsupported cancel reason %q", r.Reason)
	}
	if err := validateTimestamp("cancellation settlement deadline", r.SettlementDeadline); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !r.SettlementDeadline.After(now) {
		return fmt.Errorf("cancellation settlement deadline must be in the future")
	}
	if r.SettlementDeadline.After(r.Metadata.ExpiresAt) {
		return fmt.Errorf("cancellation settlement deadline must not outlive request expiry")
	}
	return r.Metadata.ValidateDigest(r)
}

type CancellationBarrierState string

const (
	CancellationBarrierSettled          CancellationBarrierState = "settled"
	CancellationBarrierForcedTerminated CancellationBarrierState = "forced_terminated"
	CancellationBarrierOutcomeUnknown   CancellationBarrierState = "outcome_unknown"
)

type CancelPromptResponse struct {
	Protocol                      string                   `json:"protocol"`
	Classification                Classification           `json:"classification"`
	BarrierState                  CancellationBarrierState `json:"barrierState"`
	SettlementProven              bool                     `json:"settlementProven"`
	Settlement                    PromptSettlement         `json:"settlement"`
	InvalidatedPermissionRequests uint32                   `json:"invalidatedPermissionRequests"`
	LiveDescendantCount           uint32                   `json:"liveDescendantCount"`
	ForcedTermination             bool                     `json:"forcedTermination"`
}

func (r CancelPromptResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	switch r.BarrierState {
	case CancellationBarrierSettled:
		if !r.SettlementProven || r.ForcedTermination {
			return fmt.Errorf("settled cancellation barrier requires proven settlement without forced termination")
		}
	case CancellationBarrierForcedTerminated:
		if !r.ForcedTermination {
			return fmt.Errorf("forced termination barrier must report forced termination")
		}
	case CancellationBarrierOutcomeUnknown:
		if r.SettlementProven || r.Settlement.TerminalEvent != EventOutcomeUnknown || r.Settlement.Outcome != PromptOutcomeUnknown {
			return fmt.Errorf("outcome unknown barrier must carry unproven outcome_unknown settlement")
		}
	default:
		return fmt.Errorf("unsupported cancellation barrier state %q", r.BarrierState)
	}
	if err := r.Settlement.Validate(); err != nil {
		return err
	}
	if r.SettlementProven && r.LiveDescendantCount != 0 {
		return fmt.Errorf("proven settlement must not leave live descendants")
	}
	if !r.SettlementProven && r.BarrierState != CancellationBarrierOutcomeUnknown {
		return fmt.Errorf("unproven settlement must be outcome_unknown")
	}
	return nil
}
