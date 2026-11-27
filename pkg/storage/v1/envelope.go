package v1

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
)

var ErrDatasetMismatch = errors.New("dataset mismatch")

var ErrClockSkew = errors.New("clock skew")

type WriteEnvelope struct {
	DatasetID     string
	Fence         ControllerEpochFence
	OperationID   string
	RequestDigest string
	IssuedAt      time.Time
	Sequence      uint64
}

func (e WriteEnvelope) Validate() error {
	if err := ValidateControlIdentifier("write envelope dataset ID", e.DatasetID); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("write envelope fence name", e.Fence.Name); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("write envelope fence holder ID", e.Fence.HolderID); err != nil {
		return err
	}
	if e.Fence.Epoch < 1 {
		return ValidationErrorf("write envelope fence epoch must be at least 1")
	}
	if err := ValidateOperationID(e.OperationID); err != nil {
		return err
	}
	if err := ValidateCanonicalDigest("write envelope request digest", e.RequestDigest); err != nil {
		return err
	}
	if e.IssuedAt.IsZero() {
		return ValidationErrorf("write envelope issued-at time is required")
	}
	if e.Sequence < 1 {
		return ValidationErrorf("write envelope sequence must be at least 1")
	}
	return nil
}

func ValidateOperationID(id string) error {
	if id == "" {
		return ValidationErrorf("operation ID is required")
	}
	if len(id) > MaxOperationIDBytes {
		return ValidationErrorf("operation ID exceeds %d bytes", MaxOperationIDBytes)
	}
	if err := validateControlIdentifier("operation ID", id); err != nil {
		return err
	}
	if strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return ValidationErrorf("operation ID contains a control character")
	}
	return nil
}

type writeEnvelopeKey struct{}

func WithWriteEnvelope(ctx context.Context, envelope WriteEnvelope) context.Context {
	return context.WithValue(ctx, writeEnvelopeKey{}, envelope)
}

func WriteEnvelopeFrom(ctx context.Context) (WriteEnvelope, bool) {
	envelope, ok := ctx.Value(writeEnvelopeKey{}).(WriteEnvelope)
	return envelope, ok
}
