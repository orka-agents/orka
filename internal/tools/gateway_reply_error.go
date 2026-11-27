package tools

import "errors"

type GatewayReplyRejection struct {
	message string
	cause   error
}

func (e *GatewayReplyRejection) Error() string { return e.message }
func (e *GatewayReplyRejection) Unwrap() error { return e.cause }

func NewGatewayReplyRejection(code string, cause error) *GatewayReplyRejection {
	var message string
	switch code {
	case "interim_delivery_unsupported":
		message = "gateway adapter does not support interim delivery"
	case "limit_reached":
		message = "gateway reply lifetime message limit reached"
	case "conflict":
		message = "gateway reply request conflicts with an existing request or inactive conversation"
	case "invalid_request", "too_large":
		message = "gateway reply requires only nonempty content of at most 16384 UTF-8 bytes"
	case "forbidden", "unauthorized":
		message = "gateway reply is not authorized for this task"
	case "unavailable":
		message = "gateway reply admission is temporarily unavailable"
	case "not_found":
		message = "gateway reply conversation no longer exists"
	default:
		return nil
	}
	return &GatewayReplyRejection{message: message, cause: cause}
}

func safeGatewayReplyError(err error, fallback string) error {
	if rejection, ok := errors.AsType[*GatewayReplyRejection](err); ok {
		return &gatewayReplyError{message: rejection.Error(), cause: err}
	}
	return &gatewayReplyError{message: fallback, cause: err}
}
