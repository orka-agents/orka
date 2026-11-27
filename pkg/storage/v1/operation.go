package v1

import (
	"context"
	"errors"
	"time"
)

var ErrOperationExpired = errors.New("operation expired")

var ErrOperationAborted = errors.New("operation aborted")

const (
	MaxOperationIDBytes   = 256
	MaxOperationAge       = 15 * time.Minute
	MaxClockSkew          = 30 * time.Second
	MinReceiptRetention   = time.Hour
	MaxReceiptResultBytes = 1 << 20
)

type OperationState string

const (
	OperationCommitted OperationState = "committed"
	OperationAborted   OperationState = "aborted"
)

func IsValidOperationState(state OperationState) bool {
	switch state {
	case OperationCommitted, OperationAborted:
		return true
	default:
		return false
	}
}

type OperationOutcome struct {
	OperationID   string
	Method        string
	RequestDigest string
	State         OperationState
	Fence         ControllerEpochFence
	IssuedAt      time.Time
	RecordedAt    time.Time
}

type ResolveOperationRequest struct {
	DatasetID     string
	OperationID   string
	Method        string
	RequestDigest string
	IssuedAt      time.Time
}

func (r ResolveOperationRequest) Validate() error {
	if err := ValidateControlIdentifier("resolve operation dataset ID", r.DatasetID); err != nil {
		return err
	}
	if err := ValidateOperationID(r.OperationID); err != nil {
		return err
	}
	if info, ok := LookupMethod(r.Method); !ok || info.Kind != MethodWrite {
		return ValidationErrorf("resolve operation method %q is not a contract write method", r.Method)
	}
	if err := ValidateCanonicalDigest("resolve operation request digest", r.RequestDigest); err != nil {
		return err
	}
	if r.IssuedAt.IsZero() {
		return ValidationErrorf("resolve operation issued-at time is required")
	}
	return nil
}

type OperationReceiptStore interface {
	ResolveOperation(ctx context.Context, request ResolveOperationRequest) (OperationOutcome, error)
	PruneOperationReceipts(ctx context.Context, limit int) (int, error)
}
