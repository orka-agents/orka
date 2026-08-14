// Package apierror defines client-safe structured HTTP errors shared by domain
// services and the REST surface without coupling those services to Fiber.
package apierror

import (
	"fmt"
	"time"
)

// Error carries an HTTP status, stable machine-readable reason, safe message,
// and optional retry hint.
type Error struct {
	Status     int
	Reason     string
	Message    string
	RetryAfter time.Duration
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return "API error"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// Unwrap returns the internal cause when one exists.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// New constructs a client-safe structured error.
func New(status int, reason, message string) *Error {
	return &Error{Status: status, Reason: reason, Message: message}
}

// WithRetryAfter adds a bounded retry hint.
func (e *Error) WithRetryAfter(delay time.Duration) *Error {
	if e != nil {
		e.RetryAfter = delay
	}
	return e
}
