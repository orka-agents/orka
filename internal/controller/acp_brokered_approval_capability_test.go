package controller

import (
	"strings"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestValidateExternalRuntimeCapabilitiesRequiresBrokeredApprovalSupport(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	runtimeClient, err := harnessv2.NewClient(fixture.runtime.Spec.Deployment.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := runtimeClient.Capabilities(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime.Spec.Capabilities.MCPPolicy.ApprovalRequiredTools = []string{"lookup"}
	capabilities.Provider.SupportsPermissions = true
	capabilities.Provider.SupportsBrokeredToolApprovals = false
	if _, _, err := validateExternalRuntimeCapabilities(fixture.runtime, capabilities, false); err == nil ||
		!strings.Contains(err.Error(), "brokered tool approval capability") {
		t.Fatalf("approval capability validation = %v, want rejection despite native permission support", err)
	}
	capabilities.Provider.SupportsPermissions = false
	capabilities.Provider.SupportsBrokeredToolApprovals = true
	if _, _, err := validateExternalRuntimeCapabilities(fixture.runtime, capabilities, false); err != nil {
		t.Fatalf("controller-owned brokered approval capability was rejected: %v", err)
	}
}
