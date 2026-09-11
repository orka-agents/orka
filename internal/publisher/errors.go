package publisher

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidRequest          = errors.New("invalid publisher request")
	ErrInvalidRepository       = errors.New("invalid canonical repository")
	ErrInvalidRef              = errors.New("invalid canonical branch ref")
	ErrInvalidObjectID         = errors.New("invalid git object ID")
	ErrUnsafeDelta             = errors.New("unsafe workspace delta")
	ErrUnsupportedDelta        = errors.New("workspace delta cannot be represented by git")
	ErrSourceMoved             = errors.New("source ref moved from persisted baseline")
	ErrBranchMoved             = errors.New("publication branch moved from branch claim baseline")
	ErrBranchClaimMismatch     = errors.New("publication branch claim does not match request")
	ErrIdempotencyConflict     = errors.New("publisher operation identity was reused with different content")
	ErrPreparedArtifactMissing = errors.New("prepared publication artifact is missing")
	ErrPreparedArtifactCorrupt = errors.New("prepared publication artifact is corrupt")
	ErrCASRejected             = errors.New("exact publication compare-and-swap was rejected")
	ErrPublicationUnknown      = errors.New("publication outcome is unknown")
	ErrVerificationUnknown     = errors.New("remote publication state could not be observed")
)

// Error attaches a stable classification to a publisher failure.
type Error struct {
	Kind      error
	Operation string
	Detail    string
	Err       error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := e.Operation
	if e.Detail != "" {
		if message != "" {
			message += ": "
		}
		message += e.Detail
	}
	if e.Err != nil {
		if message != "" {
			message += ": "
		}
		message += e.Err.Error()
	}
	if message == "" && e.Kind != nil {
		message = e.Kind.Error()
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.Kind == nil {
		return e.Err
	}
	if e.Err == nil {
		return e.Kind
	}
	return errors.Join(e.Kind, e.Err)
}

func operationError(kind error, operation, detail string, err error) error {
	return &Error{Kind: kind, Operation: operation, Detail: detail, Err: err}
}

func invalid(field, format string, args ...any) error {
	return operationError(ErrInvalidRequest, "validate "+field, fmt.Sprintf(format, args...), nil)
}
