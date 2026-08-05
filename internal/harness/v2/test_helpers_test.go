package v2

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

func testSHA256(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func testRuntimeProfile() RuntimeProfile {
	toolPolicy := testMCPToolPolicy()
	toolDigest, _ := CanonicalRuntimeToolPolicyDigest(toolPolicy.AllowedToolNames, toolPolicy.DisallowedToolNames, toolPolicy.AllowBash)
	approvalPolicy := MCPApprovalPolicy{}
	approvalDigest, _ := CanonicalMCPApprovalPolicyDigest(approvalPolicy)
	mcpDigest, _ := CanonicalMCPConfigurationDigest(toolPolicy.AllowedToolNames)
	agentConfiguration := testAgentSessionConfiguration()
	agentConfigurationDigest, _ := CanonicalAgentConfigurationDigest(agentConfiguration)
	return RuntimeProfile{
		ACPProfile:               ACPProfileV1,
		AdapterDigests:           map[string]string{"codex-acp": testSHA256("adapter")},
		ProviderKind:             "codex",
		Model:                    "test-model",
		AgentConfigurationDigest: agentConfigurationDigest,
		ToolPolicyDigest:         toolDigest,
		ApprovalPolicyDigest:     approvalDigest,
		MCPConfigurationDigest:   mcpDigest,
		WorkspaceIntent:          WorkspaceIntentWrite,
		ProxyCredentialRole:      "provider-inference",
		ProxyCredentialScope:     "model:test-model",
		ResourceClass:            "standard",
	}
}

func testAgentSessionConfiguration() AgentSessionConfiguration {
	return AgentSessionConfiguration{
		AgentUID:        "agent-uid-1",
		AgentGeneration: 4,
		ProviderKind:    "codex",
		Model:           "test-model",
		MaxTurns:        50,
		ReasoningEffort: "high",
		SystemPrompt:    "Act as a careful test agent.",
	}
}

func testAgentSessionConfigurationPointer() *AgentSessionConfiguration {
	configuration := testAgentSessionConfiguration()
	return &configuration
}

func testMCPToolPolicy() MCPToolPolicy {
	tools := []MCPToolDescriptor{}
	digest, _ := CanonicalMCPToolDescriptorDigest(tools)
	return MCPToolPolicy{AllowedToolNames: []string{}, Tools: tools, DescriptorDigest: digest}
}

func testMCPPolicyConfiguration() MCPPolicyConfiguration {
	profile := testRuntimeProfile()
	return MCPPolicyConfiguration{
		ToolPolicyDigest: profile.ToolPolicyDigest, ApprovalPolicyDigest: profile.ApprovalPolicyDigest,
		MCPConfigurationDigest: profile.MCPConfigurationDigest,
		ToolPolicy:             testMCPToolPolicy(), ApprovalPolicy: MCPApprovalPolicy{},
	}
}

func testPromptMCPAuthorization(metadata MutationMetadata, lease PromptLease) PromptMCPAuthorization {
	return testPromptMCPAuthorizationAt(metadata, lease, testNow.Add(time.Minute))
}

func testPromptMCPAuthorizationAt(metadata MutationMetadata, lease PromptLease, expiresAt time.Time) PromptMCPAuthorization {
	profile := testRuntimeProfile()
	return PromptMCPAuthorization{
		RuntimeSessionUID: metadata.Fence.RuntimeSessionUID, SessionGeneration: metadata.Fence.RuntimeSessionGeneration,
		TaskUID: metadata.TaskUID, TaskAttempt: metadata.TaskAttempt, PromptID: metadata.PromptID,
		LeaseGeneration: lease.Generation, ToolPolicyDigest: profile.ToolPolicyDigest,
		ApprovalPolicyDigest: profile.ApprovalPolicyDigest, MCPConfigurationDigest: profile.MCPConfigurationDigest,
		ToolPolicy: testMCPToolPolicy(), ApprovalPolicy: MCPApprovalPolicy{}, ExpiresAt: expiresAt,
	}
}

func testFence(t *testing.T) Fence {
	t.Helper()
	profileDigest, err := CanonicalProfileDigest(testRuntimeProfile())
	if err != nil {
		t.Fatalf("CanonicalProfileDigest() error = %v", err)
	}
	return Fence{
		RuntimeInstanceID:          "runtime-instance-1",
		SupervisorBootID:           "boot-1",
		ControllerEpoch:            7,
		RuntimePoolUID:             "pool-uid-1",
		RuntimePoolGeneration:      3,
		RuntimeSessionUID:          "session-uid-1",
		RuntimeSessionGeneration:   5,
		RuntimeProfileDigest:       profileDigest,
		ProfileDigestSchemaVersion: ProfileDigestSchemaVersion,
	}
}

func testMutationMetadata(t *testing.T, prompt bool) MutationMetadata {
	t.Helper()
	metadata := MutationMetadata{
		Fence:                      testFence(t),
		TaskUID:                    "task-uid-1",
		TaskAttempt:                2,
		OperationID:                "operation-1",
		RequestDigestSchemaVersion: RequestDigestSchemaVersion,
		ExpiresAt:                  testNow.Add(time.Minute),
	}
	if prompt {
		metadata.PromptID = "prompt-1"
	}
	return metadata
}

func sealRequest(t *testing.T, request any, target *RequestDigest) {
	t.Helper()
	digest, err := CanonicalRequestDigest(request)
	if err != nil {
		t.Fatalf("CanonicalRequestDigest() error = %v", err)
	}
	*target = digest
}

func testWorkspaceBaseline() WorkspaceBaseline {
	return WorkspaceBaseline{
		RepositoryIdentity: "github.com/orka-agents/orka",
		Revision:           "0123456789abcdef",
		TreeDigest:         testSHA256("baseline-tree"),
	}
}

func testStartPromptRequest(t *testing.T) StartPromptRequest {
	t.Helper()
	metadata := testMutationMetadata(t, true)
	lease := PromptLease{
		Generation: 1,
		IssuedAt:   testNow,
		ExpiresAt:  testNow.Add(2 * time.Minute),
	}
	request := StartPromptRequest{
		Protocol:         ProtocolVersion,
		Metadata:         metadata,
		Lease:            lease,
		MCPAuthorization: testPromptMCPAuthorization(metadata, lease),
		Input:            PromptInput{Content: []ContentBlock{{Type: ContentBlockText, Text: "implement the change"}}},
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	return request
}

func testExpectation(t *testing.T) EventExpectation {
	t.Helper()
	request := testStartPromptRequest(t)
	return EventExpectationFromMetadata(request.Metadata)
}

func testEventIdentity(t *testing.T, sequence uint64, timestamp time.Time) EventIdentity {
	t.Helper()
	expectation := testExpectation(t)
	return EventIdentity{
		RuntimeInstanceID:        expectation.RuntimeInstanceID,
		SupervisorBootID:         expectation.SupervisorBootID,
		RuntimeSessionUID:        expectation.RuntimeSessionUID,
		RuntimeSessionGeneration: expectation.RuntimeSessionGeneration,
		TaskUID:                  expectation.TaskUID,
		TaskAttempt:              expectation.TaskAttempt,
		PromptID:                 expectation.PromptID,
		Sequence:                 sequence,
		RequestDigest:            expectation.RequestDigest,
		Timestamp:                timestamp,
	}
}

func testAcceptedEvent(t *testing.T) Event {
	t.Helper()
	return Event{
		Protocol: ProtocolVersion,
		Type:     EventAccepted,
		Identity: testEventIdentity(t, 1, testNow),
		Accepted: &AcceptedEvent{
			AcceptedAt: testNow,
			Lease: PromptLease{
				Generation: 1,
				IssuedAt:   testNow,
				ExpiresAt:  testNow.Add(2 * time.Minute),
			},
			ACPVersion: ACPProfileV1,
		},
	}
}

func testCompletedEvent(t *testing.T, sequence uint64, timestamp time.Time) Event {
	t.Helper()
	return Event{
		Protocol: ProtocolVersion,
		Type:     EventCompleted,
		Identity: testEventIdentity(t, sequence, timestamp),
		Completed: &CompletedEvent{
			StopReason: ACPStopReasonEndTurn,
			Result:     PromptResult{Content: []ContentBlock{{Type: ContentBlockText, Text: "done"}}},
		},
	}
}
