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
	"github.com/orka-agents/orka/internal/store"
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

// TestACPMCPBrokerForwardedActionAnswerIsNotKept covers an ungated Azure
// action (POST) whose answer carries a secret: the caller gets the answer,
// the effect record keeps only a note, and a redelivery gets that note.
func TestACPMCPBrokerForwardedActionAnswerIsNotKept(t *testing.T) {
	effects, fence := newMCPBrokerControlStore(t)
	request, profile := testMCPBrokerForwardWriteRequest(t)
	request.Call.Arguments = json.RawMessage(`{"method":"POST","path":"/subscriptions/s1/resourcegroups/rg1/providers/Microsoft.Storage/storageAccounts/a/listKeys"}`)
	var err error
	request.Metadata.RequestDigest = ""
	if request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(request); err != nil {
		t.Fatal(err)
	}
	bearer := strings.Repeat("b", 32)
	capability := []byte(strings.Repeat("c", 32))
	answer := json.RawMessage(`{"status":200,"headers":{"Content-Type":"application/json"},"body":"{\"keys\":[{\"value\":\"private-storage-key\"}]}"}`)
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
			return answer, nil
		}),
		Effects: effects,
	}
	results := make([]string, 0, 2)
	for range 2 {
		recorder := performMCPBrokerCall(t, broker, request, bearer, capability)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
		}
		var response harnessv2.MCPBrokerCallResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		results = append(results, string(response.Result))
	}
	if !strings.Contains(results[0], "private-storage-key") {
		t.Fatalf("the caller did not get the answer: %s", results[0])
	}
	if strings.Contains(results[1], "private-storage-key") || !strings.Contains(results[1], "OrkaResultNotKept") || executions.Load() != 1 {
		t.Fatalf("redelivery = %s after %d executions; want the kept note and one execution", results[1], executions.Load())
	}
	id, err := (store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: request.Namespace,
		AggregateID: string(request.Authorization.RuntimeSessionUID), OperationID: string(request.Metadata.OperationID),
	}).CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	effect, err := effects.GetExternalEffect(context.Background(), id)
	if err != nil || strings.Contains(string(effect.Response), "private-storage-key") {
		t.Fatalf("effect record = %s, %v; want no secret", effect.Response, err)
	}
}

// TestForwardedWriteKeptResultKeepsNoAnswer covers what an effect record
// keeps of a forwarded write: never its answer, which can carry a secret,
// only a note with its status; a tool error is kept as it is.
func TestForwardedWriteKeptResultKeepsNoAnswer(t *testing.T) {
	request, _ := testMCPBrokerForwardWriteRequest(t)
	descriptor := request.Authorization.ToolPolicy.Tools[0]
	failure := json.RawMessage(`{"isError":true,"code":"tool_execution_failed","error":"MCP tool execution failed"}`)
	if got := forwardedWriteKeptResult(descriptor, failure); string(got) != string(failure) {
		t.Fatalf("error kept as %s", got)
	}
	note, err := harnessv2.DecodeMCPForwardResponse(forwardedWriteKeptResult(descriptor,
		json.RawMessage(`{"status":200,"body":"{\"properties\":{\"value\":\"private-function-key\"}}"}`)))
	if err != nil || note.Status != http.StatusConflict || !strings.Contains(note.Body, "status 200") ||
		!strings.Contains(note.Body, "OrkaResultNotKept") || strings.Contains(note.Body, "private-function-key") {
		t.Fatalf("PUT note = %#v, %v", note, err)
	}
}

// TestMaskForwardedWriteArgumentsKeepsOnlyTheShape covers the approval
// preview of a forwarded write: its method, path, and body fields, with no
// body value, whatever the field is called.
func TestMaskForwardedWriteArgumentsKeepsOnlyTheShape(t *testing.T) {
	masked := string(maskForwardedWriteArguments(json.RawMessage(
		`{"method":"PUT","path":"/subscriptions/s1/x/functions/f/keys/k","body":{"properties":{"name":"k","value":"private-function-key","n":[1,true]}}}`)))
	for _, want := range []string{`"method":"PUT"`, `/functions/f/keys/k`, `"value":"[masked]"`, `"name":"[masked]"`} {
		if !strings.Contains(masked, want) {
			t.Errorf("masked preview %s lacks %s", masked, want)
		}
	}
	if strings.Contains(masked, "private-function-key") || strings.Contains(masked, "true") {
		t.Fatalf("masked preview kept a value: %s", masked)
	}
}
