package conformance

import (
	"context"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type WorkspaceGovernanceMode = harnessv2.WorkspaceGovernanceMode

const (
	WorkspaceGovernanceStrict  = harnessv2.WorkspaceGovernanceStrict
	WorkspaceGovernanceTrusted = harnessv2.WorkspaceGovernanceTrusted
)

type WorkspaceGovernanceClaims = harnessv2.WorkspaceGovernanceCapabilities

type CapabilitiesResponse struct {
	harnessv2.CapabilitiesResponse
}

func (r CapabilitiesResponse) Validate() error {
	return r.CapabilitiesResponse.Validate()
}

type Target struct {
	BaseURL                         string
	ControllerBearerToken           string
	OperationCapabilitySecret       []byte
	ControlTimeout                  time.Duration
	ProbeTimeout                    time.Duration
	ExpectedRuntimeInstanceID       harnessv2.RuntimeInstanceID
	ExpectedControllerEpoch         uint64
	ExpectedFence                   *harnessv2.Fence
	BeforeMutation                  func(context.Context) error
	Profile                         harnessv2.RuntimeProfile
	ToolPolicy                      harnessv2.MCPToolPolicy
	ApprovalPolicy                  harnessv2.MCPApprovalPolicy
	Limits                          harnessv2.ProtocolLimits
	SupportsDrain                   bool
	SupportsPublicationFinalization bool
	WorkspaceGovernance             WorkspaceGovernanceClaims
	ProbeLifecycle                  bool
	RequirePublicAddresses          bool
	PinnedBackendAddresses          []string
}

type Result struct {
	Passed                 bool
	Message                string
	ObservedCapabilities   *CapabilitiesResponse
	ObservedStatus         *harnessv2.StatusResponse
	LifecycleProbeExecuted bool
}
