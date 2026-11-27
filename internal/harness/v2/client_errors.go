package v2

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrClientConfiguration      = errors.New("harness v2 client configuration error")
	ErrClientValidation         = errors.New("harness v2 client validation error")
	ErrClientTransport          = errors.New("harness v2 client transport error")
	ErrClientProtocol           = errors.New("harness v2 client protocol error")
	ErrClientStream             = errors.New("harness v2 client stream error")
	ErrResponseBodyTooLarge     = errors.New("harness v2 response body too large")
	ErrPromptStreamDisconnected = errors.New("harness v2 prompt stream disconnected")
)

type ClientErrorKind string

const (
	ClientErrorConfiguration ClientErrorKind = "configuration"
	ClientErrorValidation    ClientErrorKind = "validation"
	ClientErrorTransport     ClientErrorKind = "transport"
	ClientErrorHTTP          ClientErrorKind = "http"
	ClientErrorProtocol      ClientErrorKind = "protocol"
	ClientErrorStream        ClientErrorKind = "stream"
)

type RequestWriteState string

const (
	RequestWriteUnknown   RequestWriteState = "unknown"
	RequestWriteZeroBytes RequestWriteState = "zero_bytes_written"
	RequestWriteComplete  RequestWriteState = "request_written"
	RequestWriteAmbiguous RequestWriteState = "request_write_ambiguous"
)

type RequestWriteEvidence struct {
	State                RequestWriteState
	RequestBodyBytesRead int64
	WroteHeaders         bool
	WroteRequest         bool
	WroteRequestError    bool
	GotFirstResponseByte bool
}

func (e RequestWriteEvidence) SafeToResendSameIdentity() bool {
	return e.State == RequestWriteZeroBytes
}

type ClientError struct {
	Operation      string
	Kind           ClientErrorKind
	StatusCode     int
	Code           ErrorCode
	Classification *Classification
	Retryable      bool
	Message        string
	WriteEvidence  RequestWriteEvidence
	cause          error
}

func (e *ClientError) Error() string {
	if e == nil {
		return "<nil>"
	}
	parts := []string{"harness v2 " + e.Operation + " failed"}
	if e.Kind != "" {
		parts = append(parts, "kind="+string(e.Kind))
	}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if e.Code != "" {
		parts = append(parts, "code="+string(e.Code))
	}
	if e.Classification != nil {
		parts = append(parts, "classification="+string(e.Classification.Class))
	}
	if e.WriteEvidence.State != "" && e.WriteEvidence.State != RequestWriteUnknown {
		parts = append(parts, "write="+string(e.WriteEvidence.State))
	}
	prefix := strings.Join(parts, " ")
	if strings.TrimSpace(e.Message) == "" {
		return prefix
	}
	return prefix + ": " + e.Message
}

func (e *ClientError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *ClientError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch target {
	case ErrClientConfiguration:
		return e.Kind == ClientErrorConfiguration
	case ErrClientValidation:
		return e.Kind == ClientErrorValidation
	case ErrClientTransport:
		return e.Kind == ClientErrorTransport
	case ErrClientProtocol:
		return e.Kind == ClientErrorProtocol
	case ErrClientStream:
		return e.Kind == ClientErrorStream
	}
	return errors.Is(e.cause, target)
}

func clientError(operation string, kind ClientErrorKind, message string, cause error) *ClientError {
	return &ClientError{Operation: operation, Kind: kind, Message: message, cause: cause}
}

type retryablePreMutationError struct{ err error }

func (e *retryablePreMutationError) Error() string { return e.err.Error() }
func (e *retryablePreMutationError) Unwrap() error { return e.err }

func MarkPreMutationRetryable(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*retryablePreMutationError](err); ok {
		return err
	}
	return &retryablePreMutationError{err: err}
}

func preMutationRetryable(err error) bool {
	var marked *retryablePreMutationError
	return errors.As(err, &marked)
}

func classificationCode(class RequestClassification) ErrorCode {
	switch class {
	case RequestClassificationStaleFence:
		return ErrorCodeStaleFence
	case RequestClassificationExpired:
		return ErrorCodeExpired
	case RequestClassificationDigestConflict:
		return ErrorCodeDigestConflict
	case RequestClassificationAlreadyAccepted:
		return ErrorCodeAlreadyAccepted
	case RequestClassificationSettled:
		return ErrorCodeSettled
	default:
		return ErrorCodeInvalidRequest
	}
}
