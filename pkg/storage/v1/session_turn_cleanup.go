package v1

import (
	"encoding/json"
	"time"
)

type SessionTurnCleanupReceipt struct {
	Namespace          string                  `json:"namespace"`
	SessionName        string                  `json:"sessionName"`
	OperationID        string                  `json:"operationId"`
	OperationDigest    string                  `json:"operationDigest"`
	TurnID             string                  `json:"turnId"`
	Key                SessionTurnKey          `json:"key"`
	PromptAttemptID    string                  `json:"promptAttemptId"`
	TerminalKind       SessionTurnTerminalKind `json:"terminalKind"`
	FinalizedAt        time.Time               `json:"finalizedAt"`
	PublicationID      string                  `json:"publicationId,omitempty"`
	PublicationReceipt *PublicationReceipt     `json:"publicationReceipt,omitempty"`
	ProjectionID       string                  `json:"projectionId"`
	ProjectionKind     string                  `json:"projectionKind"`
	ProjectionDigest   string                  `json:"projectionDigest"`
	AggregateKind      string                  `json:"aggregateKind"`
	AggregateID        string                  `json:"aggregateId"`
	Payload            []byte                  `json:"payload"`
	PayloadDigest      string                  `json:"payloadDigest"`
	ProjectionState    OutboxProjectionState   `json:"projectionState"`
	DeliveryDigest     string                  `json:"deliveryDigest,omitempty"`
	DeliveredAt        *time.Time              `json:"deliveredAt,omitempty"`
}

func (r *SessionTurnCleanupReceipt) Validate(namespace, sessionName, turnID string) error {
	if r == nil || r.Namespace != namespace || r.SessionName != sessionName || r.TurnID != turnID {
		return ConflictErrorf("Session cleanup receipt does not match its requested identity")
	}
	for field, value := range map[string]string{
		"namespace": r.Namespace, "session name": r.SessionName, "cleanup operation ID": r.OperationID,
		"prompt attempt ID": r.PromptAttemptID,
	} {
		if err := ValidateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	if err := ValidateCanonicalDigest("cleanup operation digest", r.OperationDigest); err != nil {
		return err
	}
	if r.PublicationID != "" {
		if err := ValidateControlIdentifier("cleanup publication ID", r.PublicationID); err != nil {
			return err
		}
	}
	if r.PublicationReceipt != nil {
		if r.PublicationID == "" || r.PublicationReceipt.PublicationID != r.PublicationID ||
			r.PublicationReceipt.Generation < 1 || !IsTerminalPublicationState(r.PublicationReceipt.State) {
			return ConflictErrorf("Session cleanup receipt has inconsistent publication evidence")
		}
	}
	canonicalID, err := r.Key.CanonicalID()
	if err != nil || canonicalID != r.TurnID || r.FinalizedAt.IsZero() ||
		(r.TerminalKind != SessionTurnAssistantResult && r.TerminalKind != SessionTurnOutcomeMarker) {
		return ConflictErrorf("Session cleanup receipt has invalid finalized turn evidence")
	}
	if err := r.validateTerminalProjection(); err != nil {
		return err
	}
	switch r.ProjectionState {
	case OutboxProjectionDelivered:
		if r.DeliveredAt == nil || r.DeliveredAt.IsZero() {
			return ConflictErrorf("Session cleanup receipt has no projection delivery timestamp")
		}
		return ValidateCanonicalDigest("projection delivery digest", r.DeliveryDigest)
	case OutboxProjectionDeadLetter:
		if r.DeliveredAt != nil || r.DeliveryDigest != "" {
			return ConflictErrorf("dead-letter Session cleanup receipt contains delivery proof")
		}
		return nil
	default:
		return ConflictErrorf("Session cleanup receipt projection is not terminal")
	}
}

func (r *SessionTurnCleanupReceipt) validateTerminalProjection() error {
	if r.ProjectionKind != "TaskTerminalStatus" || r.AggregateKind != "SessionTurn" || r.AggregateID != r.TurnID ||
		r.ProjectionID != CanonicalControlID("outbox", r.TurnID, r.ProjectionKind) ||
		!json.Valid(r.Payload) || r.PayloadDigest != CanonicalBytesDigest(r.Payload) || r.ProjectionDigest != r.PayloadDigest {
		return ConflictErrorf("Session cleanup receipt does not pin its terminal projection")
	}
	return nil
}

func (r *SessionTurnCleanupReceipt) SessionTurn() *SessionTurn {
	return &SessionTurn{
		ID: r.TurnID, Key: r.Key, PromptAttemptID: r.PromptAttemptID,
		State: SessionTurnFinalized, TerminalKind: r.TerminalKind, FinalizedAt: &r.FinalizedAt,
		PublicationID: r.PublicationID, PublicationReceipt: r.PublicationReceipt,
		ProjectionID: r.ProjectionID, ProjectionKind: r.ProjectionKind, ProjectionDigest: r.ProjectionDigest,
	}
}

func (r *SessionTurnCleanupReceipt) OutboxProjection() *OutboxProjection {
	return &OutboxProjection{
		ID: r.ProjectionID, AggregateKind: r.AggregateKind, AggregateID: r.AggregateID,
		ProjectionKind: r.ProjectionKind, PayloadDigest: r.PayloadDigest,
		Payload: append([]byte(nil), r.Payload...), State: r.ProjectionState,
		DeliveryDigest: r.DeliveryDigest, DeliveredAt: r.DeliveredAt,
	}
}
