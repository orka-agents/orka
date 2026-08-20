package service

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	ErrUnauthorized      = errors.New("workspace publisher authorization failed")
	ErrInvalidRequest    = errors.New("invalid workspace publisher request")
	ErrOperationConflict = errors.New("workspace publisher operation conflicts with durable journal")
	ErrJournalFull       = errors.New("workspace publisher durable journal is full")
	ErrCredential        = errors.New("workspace publisher credential reference is invalid")
	ErrArtifactTransport = errors.New("workspace publisher artifact transport failed")
	ErrSCMTransport      = errors.New("workspace publisher SCM transport failed")
	ErrNotConfigured     = errors.New("workspace publisher feature is not configured")
	ErrBusy              = errors.New("workspace publisher is at its concurrency limit")
)

type operationError struct {
	kind      error
	code      string
	message   string
	status    int
	retryable bool
	cause     error
}

func (e *operationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.message, e.cause)
	}
	return e.message
}

func (e *operationError) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.kind == nil {
		return e.cause
	}
	if e.cause == nil {
		return e.kind
	}
	return errors.Join(e.kind, e.cause)
}

func apiError(kind error, code, message string, status int, retryable bool, cause error) error {
	return &operationError{kind: kind, code: code, message: message, status: status, retryable: retryable, cause: cause}
}

func invalidRequest(message string, cause error) error {
	return apiError(ErrInvalidRequest, "invalid_request", message, http.StatusBadRequest, false, cause)
}

func errorResponse(err error, metadata OperationMetadata, requestDigest string) (int, ErrorResponse, bool) {
	var typed *operationError
	if errors.As(err, &typed) {
		return typed.status, ErrorResponse{
			Code: typed.code, Message: typed.message, Retryable: typed.retryable,
			OperationID: metadata.OperationID, RequestDigest: requestDigest,
		}, !typed.retryable
	}
	return http.StatusBadGateway, ErrorResponse{
		Code: "upstream_failure", Message: "the clean-room operation did not complete", Retryable: true,
		OperationID: metadata.OperationID, RequestDigest: requestDigest,
	}, false
}
