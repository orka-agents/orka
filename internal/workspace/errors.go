/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"errors"
	"fmt"
)

// ErrorKind classifies executor failures without exposing backend-specific error types.
type ErrorKind string

const (
	ErrorKindUnknown            ErrorKind = "Unknown"
	ErrorKindInvalidArgument    ErrorKind = "InvalidArgument"
	ErrorKindNotFound           ErrorKind = "NotFound"
	ErrorKindAlreadyExists      ErrorKind = "AlreadyExists"
	ErrorKindTimeout            ErrorKind = "Timeout"
	ErrorKindCanceled           ErrorKind = "Canceled"
	ErrorKindFailedPrecondition ErrorKind = "FailedPrecondition"
	ErrorKindCommandFailed      ErrorKind = "CommandFailed"
)

// Error is the structured error type returned by WorkspaceExecutor implementations.
type Error struct {
	Op        string
	Kind      ErrorKind
	Message   string
	Retryable bool
	Err       error
}

// Error renders a compact human-readable error.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	kind := e.Kind
	if kind == "" {
		kind = ErrorKindUnknown
	}
	prefix := string(kind)
	if e.Op != "" {
		prefix = e.Op + ": " + prefix
	}
	if e.Message != "" && e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", prefix, e.Message, e.Err)
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", prefix, e.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", prefix, e.Err)
	}
	return prefix
}

// Unwrap returns the underlying backend or context error.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewError constructs a structured executor error.
func NewError(op string, kind ErrorKind, message string, retryable bool, err error) *Error {
	if kind == "" {
		kind = ErrorKindUnknown
	}
	return &Error{Op: op, Kind: kind, Message: message, Retryable: retryable, Err: err}
}

// KindOf returns the structured kind for err, or ErrorKindUnknown when err is
// not a workspace Error.
func KindOf(err error) ErrorKind {
	var workspaceErr *Error
	if errors.As(err, &workspaceErr) && workspaceErr.Kind != "" {
		return workspaceErr.Kind
	}
	return ErrorKindUnknown
}

// IsKind reports whether err contains a workspace Error with kind.
func IsKind(err error, kind ErrorKind) bool {
	return KindOf(err) == kind
}

func contextError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewError(op, ErrorKindTimeout, "operation timed out", true, err)
	}
	if errors.Is(err, context.Canceled) {
		return NewError(op, ErrorKindCanceled, "operation canceled", true, err)
	}
	return NewError(op, ErrorKindUnknown, "context failed", true, err)
}
