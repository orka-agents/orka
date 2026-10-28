package v2

import (
	"fmt"
	"time"
)

// SessionPreparationHeader opts into the additive capability for clients whose
// strict decoder understands prepare-only sessions.
const SessionPreparationHeader = "X-Orka-Session-Preparation"

type LaunchRuntimeSessionRequest struct {
	Protocol               string           `json:"protocol"`
	Metadata               MutationMetadata `json:"metadata"`
	PreparationOperationID OperationID      `json:"preparationOperationID"`
}

func (r LaunchRuntimeSessionRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true}); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	if err := requireIdentifier("preparation operation ID", string(r.PreparationOperationID)); err != nil {
		return err
	}
	if r.PreparationOperationID == r.Metadata.OperationID {
		return fmt.Errorf("launch requires an operation ID distinct from preparation")
	}
	return r.Metadata.ValidateDigest(r)
}

type LaunchRuntimeSessionResponse struct {
	Protocol               string                   `json:"protocol"`
	Classification         Classification           `json:"classification"`
	PreparationOperationID OperationID              `json:"preparationOperationID"`
	Session                RuntimeSessionDescriptor `json:"session"`
}

func (r LaunchRuntimeSessionResponse) ValidateFor(sessionID RuntimeSessionID, request LaunchRuntimeSessionRequest) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return err
	}
	if r.Classification.Class != RequestClassificationFresh && r.Classification.Class != RequestClassificationDuplicate {
		return fmt.Errorf("invalid launch response classification %q", r.Classification.Class)
	}
	if err := r.Session.Validate(); err != nil {
		return err
	}
	if r.PreparationOperationID != request.PreparationOperationID ||
		r.Session.RuntimeSessionID != sessionID ||
		r.Session.RuntimeSessionUID != request.Metadata.Fence.RuntimeSessionUID ||
		r.Session.Generation != request.Metadata.Fence.RuntimeSessionGeneration ||
		r.Session.RuntimeInstanceID != request.Metadata.Fence.RuntimeInstanceID ||
		r.Session.SupervisorBootID != request.Metadata.Fence.SupervisorBootID ||
		r.Session.RuntimeProfileDigest != request.Metadata.Fence.RuntimeProfileDigest ||
		r.Session.State != RuntimeSessionStateIdle {
		return fmt.Errorf("launch response does not confirm the prepared session identity and initialization")
	}
	return nil
}

func RuntimeSessionLaunchPath(sessionID RuntimeSessionID) (string, error) {
	path, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	return path + "/launch", nil
}
