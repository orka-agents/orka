package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/tools"
)

// testMCPBrokerForwardWriteRequest is testMCPBrokerRequest for an ungated
// forwarded write.
func testMCPBrokerForwardWriteRequest(t *testing.T) (harnessv2.MCPBrokerCallRequest, harnessv2.RuntimeProfile) {
	t.Helper()
	request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectConsequential)
	forward := &harnessv2.MCPToolForward{Protocol: harnessv2.MCPForwardProtocolAzureResourceManager, Origin: "https://management.azure.com", Write: true}
	descriptor := harnessv2.MCPToolDescriptor{
		Name: "azure-arm:write", Description: "Azure Resource Manager", InputSchema: forward.InputSchema(),
		Source: harnessv2.MCPToolSourceBrokeredCustom, Effect: harnessv2.MCPToolEffectConsequential,
		DefinitionDigest: testControllerMCPDigest("azure-arm"), Forward: forward,
	}
	names := []string{descriptor.Name}
	descriptorDigest, err := harnessv2.CanonicalMCPToolDescriptorDigest([]harnessv2.MCPToolDescriptor{descriptor})
	if err != nil {
		t.Fatal(err)
	}
	policy := &request.Authorization.ToolPolicy
	policy.AllowedToolNames, policy.Tools, policy.DescriptorDigest = names, []harnessv2.MCPToolDescriptor{descriptor}, descriptorDigest
	profile.ToolPolicyDigest, _ = harnessv2.CanonicalRuntimeToolPolicyDigest(names, nil, policy.AllowBash)
	profile.MCPConfigurationDigest, _ = harnessv2.CanonicalMCPConfigurationDigest(names)
	request.Authorization.ToolPolicyDigest = profile.ToolPolicyDigest
	request.Authorization.MCPConfigurationDigest = profile.MCPConfigurationDigest
	request.Call.ToolName = descriptor.Name
	request.Call.Arguments = json.RawMessage(`{"method":"PUT","path":"/subscriptions/s1/resourcegroups/rg1","body":{"location":"eastus"}}`)
	request.Metadata.RequestDigest = ""
	if request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(request); err != nil {
		t.Fatal(err)
	}
	return request, profile
}

// TestACPMCPBrokerForwardedWriteRefusedBeforeSendingIsADefiniteFailure
// covers an ungated forwarded write that fails before it is sent, such as
// on a readOnly link: it is a definite no-effect failure, not an unknown
// outcome, and a redelivery replays that answer without executing again.
func TestACPMCPBrokerForwardedWriteRefusedBeforeSendingIsADefiniteFailure(t *testing.T) {
	effects, fence := newMCPBrokerControlStore(t)
	request, profile := testMCPBrokerForwardWriteRequest(t)
	bearer := strings.Repeat("b", 32)
	capability := []byte(strings.Repeat("c", 32))
	var executions atomic.Int32
	broker := &ACPMCPBroker{
		Credentials: ACPMCPBrokerCredentialResolverFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
			return ACPMCPBrokerCredentials{
				ControllerBearerToken: bearer, CapabilitySecret: capability,
				ExpectedFence: got.Metadata.Fence, RuntimeProfile: profile, ControllerFence: fence,
			}, nil
		}),
		Prompts: ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error { return nil }),
		Executor: ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			executions.Add(1)
			return nil, &tools.NoEffectError{Summary: "The tool stopped before sending its request.", Err: errors.New("connection is readOnly")}
		}),
		Effects: effects,
	}
	for attempt := range 2 {
		recorder := performMCPBrokerCall(t, broker, request, bearer, capability)
		if recorder.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d body=%s", attempt, recorder.Code, recorder.Body.String())
		}
		var response harnessv2.MCPBrokerCallResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !response.IsError || !strings.Contains(string(response.Result), `"tool_failed_without_effect"`) {
			t.Fatalf("attempt %d result = %s", attempt, response.Result)
		}
	}
	if executions.Load() != 1 {
		t.Fatalf("executions = %d, want one", executions.Load())
	}
}
