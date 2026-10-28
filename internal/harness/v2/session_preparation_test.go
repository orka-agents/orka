package v2

import (
	"encoding/json"
	"testing"
)

func TestEnforcementProfileSeparatesCompatibleSessionsWithoutChangingOrdinaryIdentity(t *testing.T) {
	profile := testRuntimeProfile()
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, present := legacy["enforcementRequirementDigest"]; present {
		t.Fatal("ordinary profile added a field to the legacy canonical representation")
	}
	ordinary, err := CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest, err := CanonicalProfileDigest(legacy)
	if err != nil || ordinary != legacyDigest {
		t.Fatalf("ordinary profile changed legacy identity: %s != %s (%v)", ordinary, legacyDigest, err)
	}
	profile.EnforcementRequirementDigest = testSHA256("policy-one")
	first, err := CanonicalProfileDigest(profile)
	if err != nil || first == ordinary {
		t.Fatalf("enforcement did not separate profile identity: %s (%v)", first, err)
	}
	profile.EnforcementRequirementDigest = testSHA256("policy-two")
	second, err := CanonicalProfileDigest(profile)
	if err != nil || second == first {
		t.Fatalf("changed policy inherited compatible identity: %s (%v)", second, err)
	}
	profile.EnforcementRequirementDigest = "not-a-digest"
	if err := profile.Validate(); err == nil {
		t.Fatal("invalid enforcement digest accepted")
	}
}

func TestPrepareOnlySessionValidationAndLaunchIdentity(t *testing.T) {
	profile := testRuntimeProfile()
	profile.EnforcementRequirementDigest = testSHA256("policy")
	metadata := testMutationMetadata(t, false)
	digest, err := CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	metadata.Fence.RuntimeProfileDigest = digest
	request := CreateRuntimeSessionRequest{
		Protocol: ProtocolVersion, Metadata: metadata, PrepareOnly: true,
		RuntimeSessionID: "runtime-session-1", Profile: profile,
		AgentConfiguration: testAgentSessionConfigurationPointer(), MCPConfiguration: testMCPPolicyConfiguration(),
		Workspace: WorkspaceSpec{Intent: WorkspaceIntentWrite, Baseline: testWorkspaceBaseline()},
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatal(err)
	}
	immediate := request
	immediate.PrepareOnly = false
	sealRequest(t, immediate, &immediate.Metadata.RequestDigest)
	if err := immediate.ValidateAt(testNow); err == nil {
		t.Fatal("enforced profile allowed immediate adapter launch")
	}
	descriptor := RuntimeSessionDescriptor{
		RuntimeSessionID: request.RuntimeSessionID, RuntimeSessionUID: metadata.Fence.RuntimeSessionUID,
		Generation: metadata.Fence.RuntimeSessionGeneration, RuntimeInstanceID: metadata.Fence.RuntimeInstanceID,
		SupervisorBootID: metadata.Fence.SupervisorBootID, RuntimeProfileDigest: digest,
		State: RuntimeSessionStatePrepared, WorkspaceBaseline: request.Workspace.Baseline,
		CreatedAt: testNow, LastTransitionAt: testNow,
	}
	response := CreateRuntimeSessionResponse{Protocol: ProtocolVersion, Classification: Classification{Class: RequestClassificationFresh}, Session: descriptor}
	if err := response.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
	response.Session.ProviderSessionID = "fabricated-provider-session"
	if err := response.ValidateFor(request); err == nil {
		t.Fatal("prepared response fabricated provider initialization")
	}
	if descriptor.State.CanAdmitPrompt() || !descriptor.State.CanEvict() {
		t.Fatal("prepared session acquired prompt authority or lost safe eviction")
	}
	if next, err := DeletionTransition(descriptor.State); err != nil || next != RuntimeSessionStateDeleting {
		t.Fatalf("prepared deletion transition: %s (%v)", next, err)
	}
	launch := LaunchRuntimeSessionRequest{Protocol: ProtocolVersion, Metadata: metadata, PreparationOperationID: metadata.OperationID}
	launch.Metadata.OperationID = "launch-1"
	sealRequest(t, launch, &launch.Metadata.RequestDigest)
	if err := launch.ValidateAt(testNow); err != nil {
		t.Fatal(err)
	}
	descriptor.State = RuntimeSessionStateIdle
	descriptor.ProviderSessionID = "provider-session"
	launched := LaunchRuntimeSessionResponse{
		Protocol: ProtocolVersion, Classification: Classification{Class: RequestClassificationFresh},
		PreparationOperationID: launch.PreparationOperationID, Session: descriptor,
	}
	if err := launched.ValidateFor(request.RuntimeSessionID, launch); err != nil {
		t.Fatal(err)
	}
	launched.PreparationOperationID = "another-preparation"
	if err := launched.ValidateFor(request.RuntimeSessionID, launch); err == nil {
		t.Fatal("launch response acknowledged another preparation")
	}
	launch.PreparationOperationID = launch.Metadata.OperationID
	sealRequest(t, launch, &launch.Metadata.RequestDigest)
	if err := launch.ValidateAt(testNow); err == nil {
		t.Fatal("launch reused the preparation operation identity")
	}
}
