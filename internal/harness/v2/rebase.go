package v2

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// RuntimeSessionRebasePathTemplate is the ADR 0059 rebase mutation route.
const RuntimeSessionRebasePathTemplate = "/v2/runtime-sessions/{sessionID}/rebase"

// RuntimeSessionRebaseHeader opts a capabilities or status request into the
// additive ADR 0059 fields (supportsRuntimeSessionRebase, quiescent). Older
// clients strictly decode both responses and must not receive unknown fields.
const RuntimeSessionRebaseHeader = "X-Orka-Runtime-Session-Rebase"

// RuntimeSessionRebasePath returns the escaped rebase route for one session.
func RuntimeSessionRebasePath(sessionID RuntimeSessionID) (string, error) {
	sessionPath, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	return sessionPath + "/rebase", nil
}

// RebasedWorkspaceTreeDigest is the workspace tree identity of a write
// session rebased onto its own exactly published delta (ADR 0059). Both the
// controller and the supervisor derive it from values they already hold, so
// a rebase never fetches or re-captures the tree.
func RebasedWorkspaceTreeDigest(previousTreeDigest, publishedManifestDigest, publishedRevision string) (string, error) {
	if err := validateSHA256Digest(previousTreeDigest); err != nil {
		return "", fmt.Errorf("previous workspace tree digest: %w", err)
	}
	if err := validateSHA256Digest(publishedManifestDigest); err != nil {
		return "", fmt.Errorf("published workspace delta manifest digest: %w", err)
	}
	if err := validateBoundedString("published workspace revision", publishedRevision, true, 512); err != nil {
		return "", err
	}
	canonical, err := CanonicalValue(map[string]string{
		"previousTreeDigest":      previousTreeDigest,
		"publishedManifestDigest": publishedManifestDigest,
		"publishedRevision":       publishedRevision,
	})
	if err != nil {
		return "", err
	}
	return hashCanonical("orka.harness.v2.acp-rebased-workspace-tree/v1", canonical), nil
}

// RebaseRuntimeSessionRequest returns a write session whose prepared delta was
// published and verified exactly to idle, with the published delta folded into
// its trusted baseline. It is valid only for the prompt that produced the
// delta, while the session is finalizing with the exact publication receipt.
type RebaseRuntimeSessionRequest struct {
	Protocol         string           `json:"protocol"`
	Metadata         MutationMetadata `json:"metadata"`
	WorkspaceDeltaID WorkspaceDeltaID `json:"workspaceDeltaID"`
	PublicationID    string           `json:"publicationID"`
	// TerminalReceiptDigest is the digest of the publication finalization
	// receipt the supervisor accepted.
	TerminalReceiptDigest string `json:"terminalReceiptDigest"`
	// PublishedManifestDigest is the manifest digest of the published delta.
	PublishedManifestDigest string `json:"publishedManifestDigest"`
	// NextBaseline is the session's next baseline identity: the verified
	// repository, the published commit, and the rebased tree digest. It
	// carries no artifact: nothing is downloaded.
	NextBaseline WorkspaceBaseline `json:"nextBaseline"`
}

func (r RebaseRuntimeSessionRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true, prompt: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := requireIdentifier("workspace delta ID", string(r.WorkspaceDeltaID)); err != nil {
		return err
	}
	if err := validateBoundedString("publication ID", r.PublicationID, true, 253); err != nil {
		return err
	}
	if err := validateSHA256Digest(r.TerminalReceiptDigest); err != nil {
		return fmt.Errorf("terminal publication receipt digest: %w", err)
	}
	if err := validateSHA256Digest(r.PublishedManifestDigest); err != nil {
		return fmt.Errorf("published manifest digest: %w", err)
	}
	if err := r.NextBaseline.Validate(); err != nil {
		return fmt.Errorf("next baseline: %w", err)
	}
	if r.NextBaseline.Artifact != nil {
		return fmt.Errorf("a rebased baseline carries no artifact")
	}
	return r.Metadata.ValidateDigest(r)
}

// RuntimeSessionRebaseReceipt is the supervisor's idempotent record of one
// applied rebase.
type RuntimeSessionRebaseReceipt struct {
	WorkspaceDeltaID        WorkspaceDeltaID  `json:"workspaceDeltaID"`
	PublicationID           string            `json:"publicationID"`
	TerminalReceiptDigest   string            `json:"terminalReceiptDigest"`
	PublishedManifestDigest string            `json:"publishedManifestDigest"`
	PreviousTreeDigest      string            `json:"previousTreeDigest"`
	NextBaseline            WorkspaceBaseline `json:"nextBaseline"`
	AppliedAt               time.Time         `json:"appliedAt"`
}

func (r RuntimeSessionRebaseReceipt) Validate() error {
	if err := requireIdentifier("workspace delta ID", string(r.WorkspaceDeltaID)); err != nil {
		return err
	}
	if err := validateBoundedString("publication ID", r.PublicationID, true, 253); err != nil {
		return err
	}
	for name, digest := range map[string]string{
		"terminal publication receipt digest": r.TerminalReceiptDigest,
		"published manifest digest":           r.PublishedManifestDigest,
		"previous tree digest":                r.PreviousTreeDigest,
	} {
		if err := validateSHA256Digest(digest); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := r.NextBaseline.Validate(); err != nil {
		return fmt.Errorf("next baseline: %w", err)
	}
	return validateTimestamp("rebase applied timestamp", r.AppliedAt)
}

type RebaseRuntimeSessionResponse struct {
	Protocol       string                      `json:"protocol"`
	Classification Classification              `json:"classification"`
	Session        RuntimeSessionDescriptor    `json:"session"`
	Rebase         RuntimeSessionRebaseReceipt `json:"rebase"`
}

func (r RebaseRuntimeSessionResponse) ValidateFor(request RebaseRuntimeSessionRequest) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	switch r.Classification.Class {
	case RequestClassificationFresh, RequestClassificationDuplicate:
	default:
		return fmt.Errorf("rebase response classification %q is invalid", r.Classification.Class)
	}
	if err := r.Classification.Validate(); err != nil {
		return fmt.Errorf("classification: %w", err)
	}
	if err := r.Session.Validate(); err != nil {
		return fmt.Errorf("session descriptor: %w", err)
	}
	if r.Session.RuntimeSessionUID != request.Metadata.Fence.RuntimeSessionUID ||
		r.Session.Generation != request.Metadata.Fence.RuntimeSessionGeneration ||
		r.Session.RuntimeProfileDigest != request.Metadata.Fence.RuntimeProfileDigest {
		return fmt.Errorf("rebase response identity does not match request")
	}
	if err := r.Rebase.Validate(); err != nil {
		return fmt.Errorf("rebase receipt: %w", err)
	}
	if r.Rebase.WorkspaceDeltaID != request.WorkspaceDeltaID || r.Rebase.PublicationID != request.PublicationID ||
		r.Rebase.TerminalReceiptDigest != request.TerminalReceiptDigest ||
		r.Rebase.PublishedManifestDigest != request.PublishedManifestDigest ||
		r.Rebase.NextBaseline != request.NextBaseline {
		return fmt.Errorf("rebase receipt does not match request")
	}
	if r.Session.State != RuntimeSessionStateIdle {
		return fmt.Errorf("rebase must return an idle session, got %q", r.Session.State)
	}
	if r.Session.WorkspaceBaseline != request.NextBaseline {
		return fmt.Errorf("rebased session does not report the requested baseline")
	}
	return nil
}

// RebaseRuntimeSession applies an ADR 0059 rebase. Callers must first check
// CapabilitiesResponse.SupportsRuntimeSessionRebase.
func (c *Client) RebaseRuntimeSession(ctx context.Context, sessionID RuntimeSessionID, request RebaseRuntimeSessionRequest) (*RebaseRuntimeSessionResponse, error) {
	const operation = "rebase_runtime_session"
	if err := request.ValidateAt(time.Now().UTC()); err != nil {
		return nil, c.validationError(operation, err)
	}
	relative, err := RuntimeSessionRebasePath(sessionID)
	if err != nil {
		return nil, c.validationError(operation, err)
	}
	var response RebaseRuntimeSessionResponse
	if err := c.mutateJSON(ctx, operation, http.MethodPut, relative, request.Metadata, request, &response); err != nil {
		return nil, err
	}
	if err := response.ValidateFor(request); err != nil {
		return nil, c.protocolError(operation, 0, err)
	}
	if err := validateSuccessClassification(operation, response.Classification, RequestClassificationFresh, RequestClassificationDuplicate); err != nil {
		return nil, c.protocolError(operation, 0, err)
	}
	return &response, nil
}
