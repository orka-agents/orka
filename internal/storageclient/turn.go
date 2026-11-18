package storageclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Turn finalization reads, decides and seals in the client, then commits
// conditionally in the provider. The client assigns the IDs of the
// transcript messages the finalization appends, so it knows the transcript
// boundary the finalization produces and can seal native state against it.
// The provider commits only if the transcript and stored native record still
// match what the client read.

// newTranscriptMessageID returns a transcript message ID in the format the
// embedded store has always used for messages it appends itself.
func newTranscriptMessageID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate session message ID: %w", err)
	}
	return "local:" + hex.EncodeToString(value[:]), nil
}

// validateNativeFinalization admits native state on exactly two terminal
// shapes: a fresh capture committed with a canonical assistant result, or the
// Session's existing checkpoint carried across a non-success outcome marker.
func validateNativeFinalization(request store.CommitSessionTurnFinalizationRequest) error {
	native, carried := request.NativeSession, request.NativeSessionCarried
	if native == nil {
		if carried {
			return store.ValidationErrorf("carried native checkpoint requires the existing record")
		}
		return nil
	}
	if request.SkipTranscriptAppend {
		return store.ValidationErrorf("native state cannot bind to a finalization that leaves the transcript unchanged")
	}
	if carried && request.TerminalKind != store.SessionTurnOutcomeMarker {
		return store.ValidationErrorf("carried native checkpoint requires an outcome-marker finalization")
	}
	if !carried && request.TerminalKind != store.SessionTurnAssistantResult {
		return store.ValidationErrorf("native capture requires canonical assistant-result finalization")
	}
	return nil
}

// commitSessionTurnFinalization assigns transcript message IDs, seals any
// native state against the boundary the finalization produces, and commits.
func (c *Client) commitSessionTurnFinalization(
	ctx context.Context, request store.CommitSessionTurnFinalizationRequest,
) (*store.SessionTurn, error) {
	if err := validateNativeFinalization(request); err != nil {
		return nil, err
	}
	finalization := storagev1.SealedSessionTurnFinalization{
		Key:                  request.Key,
		Namespace:            request.Namespace,
		SessionName:          request.SessionName,
		Fence:                request.Fence,
		ExpectedTurnVersion:  request.ExpectedTurnVersion,
		FinalizationDigest:   request.FinalizationDigest,
		TerminalKind:         request.TerminalKind,
		TerminalContent:      request.TerminalContent,
		SkipTranscriptAppend: request.SkipTranscriptAppend,
		SkipUserPromptAppend: request.SkipUserPromptAppend,
		PublicationID:        request.PublicationID,
		PublicationReceipt:   request.PublicationReceipt,
		Projection:           request.Projection,
		FinalizedAt:          request.FinalizedAt,
		NativeSessionDigest:  store.NativeSessionCaptureDigest(request.NativeSession),
		InputTokens:          request.InputTokens,
		OutputTokens:         request.OutputTokens,
	}
	appended := 0
	if !request.SkipTranscriptAppend {
		var err error
		if !request.SkipUserPromptAppend {
			if finalization.UserMessageID, err = newTranscriptMessageID(); err != nil {
				return nil, err
			}
			appended++
		}
		if finalization.TerminalMessageID, err = newTranscriptMessageID(); err != nil {
			return nil, err
		}
		appended++
	}
	if request.NativeSession != nil {
		native, err := c.sealTurnNativeSession(ctx, request, appended, finalization.TerminalMessageID)
		if err != nil {
			return nil, err
		}
		finalization.NativeSession = native
	}
	return c.CommitSealedSessionTurnFinalization(ctx, finalization)
}

// sealTurnNativeSession reads the turn, the Session's transcript boundary and
// its stored native record, then seals the finalization's native state
// against the boundary after the appended messages. It returns nil when the
// turn is already finalized: the provider then only compares the recorded
// finalization, as it would for a repeated request.
func (c *Client) sealTurnNativeSession(
	ctx context.Context, request store.CommitSessionTurnFinalizationRequest, appended int, terminalMessageID string,
) (*storagev1.SealedNativeSessionFinalization, error) {
	namespace := strings.TrimSpace(request.Namespace)
	sessionName := strings.TrimSpace(request.SessionName)
	native := *request.NativeSession
	if native.Namespace != namespace || native.SessionName != sessionName || native.SessionUID != request.Key.SessionUID {
		return nil, store.ConflictErrorf("native capture does not match finalized Session ownership")
	}
	if _, err := c.activeCustody(); err != nil {
		return nil, err
	}
	turnID, err := request.Key.CanonicalID()
	if err != nil {
		return nil, err
	}
	basis, err := c.ReadSessionTurnFinalizationBasis(ctx, turnID, namespace, sessionName)
	if err != nil {
		return nil, err
	}
	if basis == nil || basis.Turn.ID != turnID {
		return nil, fmt.Errorf("session turn provider returned another turn for %q", turnID)
	}
	if basis.Turn.State == store.SessionTurnFinalized {
		return nil, nil
	}
	sealed, err := c.SealNativeSessionFinalization(ctx, store.NativeSessionFinalizationSeal{
		Native:           native,
		Carried:          request.NativeSessionCarried,
		Stored:           basis.NativeSession,
		MessageCount:     basis.MessageCount + appended,
		ThroughMessageID: terminalMessageID,
	})
	if err != nil {
		return nil, err
	}
	sealed.ExpectedMessageCount = basis.MessageCount
	sealed.ExpectedLastMessageID = basis.LastMessageID
	return sealed, nil
}
