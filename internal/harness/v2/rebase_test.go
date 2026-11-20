package v2

import (
	"testing"
	"time"
)

func testRebaseRequest(t *testing.T) RebaseRuntimeSessionRequest {
	t.Helper()
	tree, err := RebasedWorkspaceTreeDigest(testSHA256("previous-tree"), testSHA256("manifest"), "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	request := RebaseRuntimeSessionRequest{
		Protocol: ProtocolVersion, Metadata: testMutationMetadata(t, true), WorkspaceDeltaID: "delta-1",
		PublicationID: "publication-1", TerminalReceiptDigest: testSHA256("receipt"), PublishedManifestDigest: testSHA256("manifest"),
		NextBaseline: WorkspaceBaseline{
			RepositoryIdentity: "github.com/orka-agents/orka", Revision: "0123456789abcdef0123456789abcdef01234567", TreeDigest: tree,
		},
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	return request
}

func TestRebasedWorkspaceTreeDigestBindsEveryInput(t *testing.T) {
	base, err := RebasedWorkspaceTreeDigest(testSHA256("tree"), testSHA256("manifest"), "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := RebasedWorkspaceTreeDigest(testSHA256("tree"), testSHA256("manifest"), "rev-1")
	if base != again {
		t.Fatal("rebased tree digest must be deterministic")
	}
	for name, inputs := range map[string][3]string{
		"previous tree": {testSHA256("other-tree"), testSHA256("manifest"), "rev-1"},
		"manifest":      {testSHA256("tree"), testSHA256("other-manifest"), "rev-1"},
		"revision":      {testSHA256("tree"), testSHA256("manifest"), "rev-2"},
	} {
		changed, err := RebasedWorkspaceTreeDigest(inputs[0], inputs[1], inputs[2])
		if err != nil || changed == base {
			t.Fatalf("%s change did not change the digest: %v", name, err)
		}
	}
	if _, err := RebasedWorkspaceTreeDigest("not-a-digest", testSHA256("manifest"), "rev"); err == nil {
		t.Fatal("an invalid previous tree digest must be rejected")
	}
}

func TestRebaseRuntimeSessionRequestValidation(t *testing.T) {
	request := testRebaseRequest(t)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatalf("valid rebase request: %v", err)
	}
	withArtifact := request
	withArtifact.NextBaseline.Artifact = &ArtifactReference{}
	sealRequest(t, withArtifact, &withArtifact.Metadata.RequestDigest)
	if err := withArtifact.ValidateAt(testNow); err == nil {
		t.Fatal("a rebased baseline must carry no artifact")
	}
	noPrompt := request
	noPrompt.Metadata.PromptID = ""
	sealRequest(t, noPrompt, &noPrompt.Metadata.RequestDigest)
	if err := noPrompt.ValidateAt(testNow); err == nil {
		t.Fatal("a rebase is bound to the prompt that produced the delta")
	}
	unsealed := request
	unsealed.PublishedManifestDigest = testSHA256("other")
	if err := unsealed.ValidateAt(testNow); err == nil {
		t.Fatal("a request changed after sealing must be rejected")
	}
	if err := request.ValidateAt(testNow.Add(time.Hour)); err == nil {
		t.Fatal("an expired rebase must be rejected")
	}
}

func TestRebaseRuntimeSessionResponseMustReportTheRequestedIdleBaseline(t *testing.T) {
	request := testRebaseRequest(t)
	descriptor := RuntimeSessionDescriptor{
		RuntimeSessionID: "session-1", RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID,
		Generation: request.Metadata.Fence.RuntimeSessionGeneration, RuntimeInstanceID: request.Metadata.Fence.RuntimeInstanceID,
		SupervisorBootID: request.Metadata.Fence.SupervisorBootID, RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest,
		State: RuntimeSessionStateIdle, ProviderSessionID: "provider-session", WorkspaceBaseline: request.NextBaseline,
		CreatedAt: testNow, LastTransitionAt: testNow,
	}
	receipt := RuntimeSessionRebaseReceipt{
		WorkspaceDeltaID: request.WorkspaceDeltaID, PublicationID: request.PublicationID,
		TerminalReceiptDigest: request.TerminalReceiptDigest, PublishedManifestDigest: request.PublishedManifestDigest,
		PreviousTreeDigest: testSHA256("previous-tree"), NextBaseline: request.NextBaseline, AppliedAt: testNow,
	}
	response := RebaseRuntimeSessionResponse{
		Protocol: ProtocolVersion, Classification: Classification{Class: RequestClassificationFresh},
		Session: descriptor, Rebase: receipt,
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("valid rebase response: %v", err)
	}
	finalizing := response
	finalizing.Session.State = RuntimeSessionStateFinalizing
	if err := finalizing.ValidateFor(request); err == nil {
		t.Fatal("a rebase must return an idle session")
	}
	stale := response
	stale.Session.WorkspaceBaseline.TreeDigest = testSHA256("stale")
	if err := stale.ValidateFor(request); err == nil {
		t.Fatal("a rebase must report the requested baseline")
	}
	otherReceipt := response
	otherReceipt.Rebase.TerminalReceiptDigest = testSHA256("other")
	if err := otherReceipt.ValidateFor(request); err == nil {
		t.Fatal("a rebase receipt must match the request")
	}
}

func TestQuiescentRuntimeSessionStatusMustBeIdle(t *testing.T) {
	status := RuntimeSessionStatus{
		RuntimeSessionID: "session-1", RuntimeSessionUID: "uid", Generation: 1, State: RuntimeSessionStateIdle,
		Quiescent: true, LiveDescendantCount: 1, LastTransitionAt: testNow,
	}
	if err := status.Validate(); err != nil {
		t.Fatalf("quiescent idle status: %v", err)
	}
	running := status
	running.State = RuntimeSessionStatePromptRunning
	running.ActivePromptID = "prompt-1"
	if err := running.Validate(); err == nil {
		t.Fatal("a running session cannot be quiescent")
	}
	pending := status
	pending.PendingPermissionCount = 1
	if err := pending.Validate(); err == nil {
		t.Fatal("a session with a pending permission cannot be quiescent")
	}
}
