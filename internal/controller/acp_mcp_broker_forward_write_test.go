package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
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
	// A throttled answer keeps its status and delay, so a replay still
	// tells the client to retry.
	throttled, err := harnessv2.DecodeMCPForwardResponse(forwardedWriteKeptResult(descriptor,
		json.RawMessage(`{"status":429,"headers":{"Retry-After":"7"},"body":"{\"error\":{\"code\":\"TooManyRequests\"}}"}`)))
	if err != nil || throttled.Status != http.StatusTooManyRequests || throttled.Headers["Retry-After"] != "7" ||
		!strings.Contains(throttled.Body, "OrkaResultNotKept") {
		t.Fatalf("throttled note = %#v, %v", throttled, err)
	}
}

// TestMaskForwardedWriteArgumentsKeepsOnlyTheShape covers the approval
// preview of a forwarded write: its method, path, and body fields, with no
// body value, whatever the field is called.
func TestMaskForwardedWriteArgumentsKeepsOnlyTheShape(t *testing.T) {
	masked := string(maskForwardedWriteArguments(json.RawMessage(
		`{"method":"PUT","path":"/subscriptions/s1/x/functions/f/keys/k","body":{"location":"westus2","sku":{"name":"Base","tier":"Free"},` +
			`"properties":{"name":"k","value":"private-function-key","adminPassword":"private-password","n":[1,true],` +
			`"agentPoolProfiles":[{"count":1,"vmSize":"Standard_B2s","enableAutoScaling":false}]}}}`)))
	for _, want := range []string{
		`"method":"PUT"`, `/functions/f/keys/k`, `"value":"[masked]"`, `"name":"[masked]"`, `"adminPassword":"[masked]"`,
		`"location":"westus2"`, `"tier":"Free"`, `"count":1`, `"vmSize":"Standard_B2s"`, `"enableAutoScaling":false`,
		`"n":["[masked]","[masked]"]`,
	} {
		if !strings.Contains(masked, want) {
			t.Errorf("masked preview %s lacks %s", masked, want)
		}
	}
	if strings.Contains(masked, "private-") {
		t.Fatalf("masked preview kept a secret: %s", masked)
	}
	// A deployment's secure parameters stay masked, a number or a field
	// named like a sizing field included: only root-anchored paths show.
	deployment := string(maskForwardedWriteArguments(json.RawMessage(
		`{"method":"PUT","path":"/subscriptions/s1/resourcegroups/rg/providers/Microsoft.Resources/deployments/d","body":` +
			`{"properties":{"mode":"Incremental","parameters":{"credentials":{"value":{"pin":482913,"location":"private-decoy","sku":{"name":"private-sku"}}}}}}}`)))
	if strings.Contains(deployment, "482913") || strings.Contains(deployment, "private-") || !strings.Contains(deployment, `"pin":"[masked]"`) {
		t.Fatalf("deployment preview exposed a secure parameter: %s", deployment)
	}
	// A literal key that spells a nested path is not that path.
	dotted := string(maskForwardedWriteArguments(json.RawMessage(
		`{"method":"PUT","path":"/subscriptions/s1/x","body":{"properties":{"hardwareProfile.vmSize":"private-dotted","*":{"vmSize":"private-star"}}}}`)))
	if strings.Contains(dotted, "private-") {
		t.Fatalf("a dotted or wildcard key exposed a value: %s", dotted)
	}
}

// TestMCPApprovalForwardedWriteAnswerIsNotKept covers the default
// approval-required path for a forwarded write whose answer carries a
// secret: the approved caller gets the answer, the effect record and the
// approval events keep none of it, and a redelivery gets the kept note
// without another execution.
func TestMCPApprovalForwardedWriteAnswerIsNotKept(t *testing.T) {
	f := newMCPApprovalFixture(t)
	request, profile := testMCPBrokerForwardWriteRequest(t)
	request.Call.Arguments = json.RawMessage(`{"method":"POST","path":"/subscriptions/s1/resourcegroups/rg1/providers/Microsoft.Storage/storageAccounts/a/listKeys"}`)
	request.Authorization.ApprovalPolicy.RequiredTools = []string{request.Call.ToolName}
	var err error
	if request.Authorization.ApprovalPolicyDigest, err = harnessv2.CanonicalMCPApprovalPolicyDigest(request.Authorization.ApprovalPolicy); err != nil {
		t.Fatal(err)
	}
	profile.ApprovalPolicyDigest = request.Authorization.ApprovalPolicyDigest
	request.Metadata.RequestDigest = ""
	if request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(request); err != nil {
		t.Fatal(err)
	}
	f.request, f.profile = request, profile
	answer := json.RawMessage(`{"status":200,"headers":{"Content-Type":"application/json"},"body":"{\"keys\":[{\"value\":\"private-storage-key\"}]}"}`)
	f.broker.Executor = ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
		f.count.Add(1)
		return answer, nil
	})
	done := f.start(request)
	f.decide(f.pending().ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	if result.IsError || !strings.Contains(string(result.Result), "private-storage-key") || f.count.Load() != 1 {
		t.Fatalf("approved result = %s after %d executions; want the answer once", result.Result, f.count.Load())
	}
	id, err := (store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: request.Namespace,
		AggregateID: string(request.Authorization.RuntimeSessionUID), OperationID: string(request.Metadata.OperationID),
	}).CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	effect, err := f.events.GetExternalEffect(t.Context(), id)
	if err != nil || strings.Contains(string(effect.Response), "private-storage-key") || !strings.Contains(string(effect.Response), "OrkaResultNotKept") {
		t.Fatalf("effect record = %s, %v; want the note and no secret", effect.Response, err)
	}
	listed, err := approvals.ListEvents(t.Context(), f.events, request.Namespace, "approval-task")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range listed {
		if strings.Contains(string(event.Content), "private-storage-key") || strings.Contains(event.Summary, "private-storage-key") {
			t.Fatalf("approval event %s kept the secret", event.Type)
		}
	}
	replay := awaitMCPApprovalResult(t, f.start(request))
	if !replay.Replayed || strings.Contains(string(replay.Result), "private-storage-key") ||
		!strings.Contains(string(replay.Result), "OrkaResultNotKept") || f.count.Load() != 1 {
		t.Fatalf("redelivery = %s replayed=%t after %d executions", replay.Result, replay.Replayed, f.count.Load())
	}
}
