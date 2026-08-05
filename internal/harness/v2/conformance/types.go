package conformance

import (
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// WorkspaceGovernanceMode and WorkspaceGovernanceClaims share the portable
// v2 capability wire type; external registration adds policy around the same
// canonical field rather than defining a duplicate JSON member.
type WorkspaceGovernanceMode = harnessv2.WorkspaceGovernanceMode

const (
	WorkspaceGovernanceStrict  = harnessv2.WorkspaceGovernanceStrict
	WorkspaceGovernanceTrusted = harnessv2.WorkspaceGovernanceTrusted
)

type WorkspaceGovernanceClaims = harnessv2.WorkspaceGovernanceCapabilities

// CapabilitiesResponse is the external-runtime v2 capability envelope.
type CapabilitiesResponse struct {
	harnessv2.CapabilitiesResponse
}

func (r CapabilitiesResponse) Validate() error {
	return r.CapabilitiesResponse.Validate()
}

// Target is one exact external runtime registration to probe. Check never
// automatically retries mutations, reconnects prompt streams, or allocates a
// replacement prompt identity after ambiguous acceptance. A lifecycle probe
// deliberately resends exact operation identities to verify a claimed
// duplicate-safe replay matrix.
type Target struct {
	BaseURL                         string
	ControllerBearerToken           string
	OperationCapabilitySecret       []byte
	ControlTimeout                  time.Duration
	ExpectedRuntimeInstanceID       harnessv2.RuntimeInstanceID
	Profile                         harnessv2.RuntimeProfile
	ToolPolicy                      harnessv2.MCPToolPolicy
	ApprovalPolicy                  harnessv2.MCPApprovalPolicy
	Limits                          harnessv2.ProtocolLimits
	SupportsDrain                   bool
	SupportsPublicationFinalization bool
	WorkspaceGovernance             WorkspaceGovernanceClaims
	ProbeLifecycle                  bool
}

// Result contains only sanitized protocol observations. Authentication values,
// operation capabilities, prompt content, and provider output are never stored.
type Result struct {
	Passed                 bool
	Message                string
	ObservedCapabilities   *CapabilitiesResponse
	ObservedStatus         *harnessv2.StatusResponse
	LifecycleProbeExecuted bool
}
