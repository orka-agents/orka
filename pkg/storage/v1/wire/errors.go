package wire

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrorDomain is the google.rpc.ErrorInfo domain of every status the server
// library produces. A status without an ErrorInfo in this domain came from
// the gRPC library or a proxy, not from the server.
const ErrorDomain = "storage.orka.ai"

// ErrorInfo reasons. The reason, not the gRPC code, identifies the contract
// error: two errors can share a code.
const (
	ReasonNotFound                     = "NOT_FOUND"
	ReasonConflict                     = "CONFLICT"
	ReasonFenced                       = "FENCED"
	ReasonSessionConfigurationMismatch = "SESSION_CONFIGURATION_MISMATCH"
	ReasonNotReady                     = "NOT_READY"
	ReasonDuplicateMismatch            = "DUPLICATE_MISMATCH"
	ReasonGatewayMessageReplayOnly     = "GATEWAY_MESSAGE_REPLAY_ONLY"
	ReasonCapacity                     = "CAPACITY"
	ReasonValidation                   = "VALIDATION"
	ReasonGatewayOwnedSession          = "GATEWAY_OWNED_SESSION"
	ReasonGatewaySessionCleanupPending = "GATEWAY_SESSION_CLEANUP_PENDING"
	ReasonTaskJobRevoked               = "TASK_JOB_REVOKED"
	ReasonUsageSelectionTooLarge       = "USAGE_SELECTION_TOO_LARGE"
	ReasonTooLarge                     = "TOO_LARGE"
	ReasonUnavailable                  = "UNAVAILABLE"
	ReasonOutcomeUnknown               = "OUTCOME_UNKNOWN"
	// ReasonUnauthenticated refuses a call that failed authentication.
	ReasonUnauthenticated = "UNAUTHENTICATED"
	// ReasonContractMismatch refuses a call without the expected contract and
	// revision headers.
	ReasonContractMismatch = "CONTRACT_MISMATCH"
	// ReasonUnsupportedField refuses a request that carries a field the server
	// does not know, so an older server never ignores a newer precondition.
	ReasonUnsupportedField = "UNSUPPORTED_FIELD"
	// ReasonProviderError reports a provider error that is not a contract
	// error, or a provider panic. A write that fails this way may have
	// committed.
	ReasonProviderError = "PROVIDER_ERROR"
)

// Markers that a decoded error carries beside its contract error.
var (
	// ErrIncompatible marks a call the server refused because the client and
	// server do not speak a compatible protocol.
	ErrIncompatible = errors.New("storage server is incompatible")
	// ErrAuthentication marks a call the server refused because
	// authentication failed.
	ErrAuthentication = errors.New("storage authentication failed")
)

// messageLimit bounds the client-safe message a status forwards.
const messageLimit = 1024

// messageKey is the ErrorInfo metadata key of a client-safe message.
const messageKey = "message"

// sentinelReasons maps the contract sentinels that carry no detail. The
// order matters only for an error that matches several sentinels: the
// infrastructure outcomes come first, then family errors before the generic
// errors they resemble.
var sentinelReasons = []struct {
	reason   string
	code     codes.Code
	sentinel error
}{
	{ReasonOutcomeUnknown, codes.Unknown, storagev1.ErrOutcomeUnknown},
	{ReasonUnavailable, codes.Unavailable, storagev1.ErrUnavailable},
	{ReasonSessionConfigurationMismatch, codes.FailedPrecondition, storagev1.ErrSessionConfigurationMismatch},
	{ReasonDuplicateMismatch, codes.AlreadyExists, storagev1.ErrDuplicateMismatch},
	{ReasonGatewayMessageReplayOnly, codes.FailedPrecondition, storagev1.ErrGatewayMessageReplayOnly},
	{ReasonGatewayOwnedSession, codes.FailedPrecondition, storagev1.ErrGatewayOwnedSession},
	{ReasonGatewaySessionCleanupPending, codes.Unavailable, storagev1.ErrGatewaySessionCleanupPending},
	{ReasonTaskJobRevoked, codes.FailedPrecondition, storagev1.ErrTaskJobRevoked},
	{ReasonUsageSelectionTooLarge, codes.ResourceExhausted, storagev1.ErrUsageSelectionTooLarge},
	{ReasonNotFound, codes.NotFound, storagev1.ErrNotFound},
	{ReasonNotReady, codes.Unavailable, storagev1.ErrNotReady},
	{ReasonCapacity, codes.ResourceExhausted, storagev1.ErrCapacity},
}

// errorStatus builds a status carrying a storage ErrorInfo.
func errorStatus(code codes.Code, reason, message string, metadata map[string]string) error {
	st := status.New(code, message)
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: ErrorDomain, Metadata: metadata})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}

// contractStatus encodes a contract error. ok is false for an error that is
// not a contract error.
func contractStatus(err error) (encoded error, ok bool) {
	var fenced storagev1.FencedError
	var tooLarge storagev1.TooLargeError
	switch {
	case errors.Is(err, storagev1.ErrOutcomeUnknown):
		return errorStatus(codes.Unknown, ReasonOutcomeUnknown, storagev1.ErrOutcomeUnknown.Error(), nil), true
	case errors.Is(err, storagev1.ErrUnavailable):
		return errorStatus(codes.Unavailable, ReasonUnavailable, storagev1.ErrUnavailable.Error(), nil), true
	case errors.As(err, &fenced), errors.Is(err, storagev1.ErrFenced):
		message := sanitizeText(err.Error(), messageLimit)
		return errorStatus(codes.FailedPrecondition, ReasonFenced, message, map[string]string{messageKey: message}), true
	case errors.As(err, &tooLarge):
		// Only the typed fields cross, never the provider's wrapping text.
		bounded := storagev1.TooLargeError{What: sanitizeText(tooLarge.What, messageLimit), Size: tooLarge.Size, Limit: tooLarge.Limit}
		return errorStatus(codes.ResourceExhausted, ReasonTooLarge, bounded.Error(), map[string]string{
			"what":  bounded.What,
			"size":  strconv.FormatInt(bounded.Size, 10),
			"limit": strconv.FormatInt(bounded.Limit, 10),
		}), true
	case errors.Is(err, storagev1.ErrTooLarge):
		return errorStatus(codes.ResourceExhausted, ReasonTooLarge, storagev1.ErrTooLarge.Error(), nil), true
	case errors.Is(err, storagev1.ErrValidation):
		message := sanitizeText(err.Error(), messageLimit)
		return errorStatus(codes.InvalidArgument, ReasonValidation, message, map[string]string{messageKey: message}), true
	}
	for _, entry := range sentinelReasons {
		if errors.Is(err, entry.sentinel) {
			return errorStatus(entry.code, entry.reason, entry.sentinel.Error(), nil), true
		}
	}
	if errors.Is(err, storagev1.ErrConflict) {
		message := sanitizeText(err.Error(), messageLimit)
		return errorStatus(codes.Aborted, ReasonConflict, message, map[string]string{messageKey: message}), true
	}
	return nil, false
}

// providerErrorStatus reports an error that is not a contract error without
// its text, which can quote stored values. correlation ties it to the
// server's log entry.
func providerErrorStatus(correlation string) error {
	return errorStatus(codes.Internal, ReasonProviderError,
		"storage provider error (correlation "+correlation+")", map[string]string{"correlation": correlation})
}

func newCorrelationID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(raw[:])
}

// remoteError is a contract sentinel decoded from a status. Error returns the
// server's client-safe message when it sent one.
type remoteError struct {
	sentinel error
	message  string
}

func (e remoteError) Error() string {
	if e.message != "" {
		return e.message
	}
	return e.sentinel.Error()
}

func (e remoteError) Unwrap() error { return e.sentinel }

// markedError carries a marker such as ErrIncompatible beside its error.
type markedError struct {
	err    error
	marker error
}

func (e markedError) Error() string   { return e.err.Error() }
func (e markedError) Unwrap() []error { return []error{e.err, e.marker} }

// storageErrorInfo returns the status's storage ErrorInfo, if any.
func storageErrorInfo(st *status.Status) *errdetails.ErrorInfo {
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetDomain() == ErrorDomain {
			return info
		}
	}
	return nil
}

// decodeError turns a failed call into the error its caller receives.
func decodeError(ctx context.Context, m *method, err error, evidence *sendEvidence) error {
	st, ok := status.FromError(err)
	if ok {
		if info := storageErrorInfo(st); info != nil {
			return decodeReason(m, info)
		}
		if st.Code() == codes.Unimplemented {
			// The server has no such RPC, so nothing ran.
			return markedError{err: storagev1.ValidationErrorf("storage server does not serve %s", m.name), marker: ErrIncompatible}
		}
	}
	return transportError(ctx, m, err, evidence)
}

// decodeReason rebuilds the contract error a server reported.
func decodeReason(m *method, info *errdetails.ErrorInfo) error {
	metadata := info.GetMetadata()
	message := sanitizeText(metadata[messageKey], messageLimit)
	switch info.GetReason() {
	case ReasonFenced:
		return storagev1.FencedError{Message: message}
	case ReasonValidation:
		return storagev1.ValidationError{Message: message}
	case ReasonConflict:
		return remoteError{sentinel: storagev1.ErrConflict, message: message}
	case ReasonTooLarge:
		size, sizeErr := strconv.ParseInt(metadata["size"], 10, 64)
		limit, limitErr := strconv.ParseInt(metadata["limit"], 10, 64)
		if sizeErr != nil || limitErr != nil {
			return remoteError{sentinel: storagev1.ErrTooLarge}
		}
		return storagev1.TooLargeError{What: sanitizeText(metadata["what"], messageLimit), Size: size, Limit: limit}
	case ReasonUnauthenticated:
		return markedError{err: fmt.Errorf("%w: authentication failed", storagev1.ErrUnavailable), marker: ErrAuthentication}
	case ReasonContractMismatch, ReasonUnsupportedField:
		return markedError{
			err:    storagev1.ValidationErrorf("storage server refused %s: %s", m.name, info.GetReason()),
			marker: ErrIncompatible,
		}
	}
	for _, entry := range sentinelReasons {
		if info.GetReason() == entry.reason {
			return remoteError{sentinel: entry.sentinel}
		}
	}
	// PROVIDER_ERROR, or a reason this client does not know.
	correlation := sanitizeText(metadata["correlation"], 64)
	if m.kind == KindWrite {
		return fmt.Errorf("%w: %s failed in the storage provider (correlation %q)", storagev1.ErrOutcomeUnknown, m.name, correlation)
	}
	return fmt.Errorf("%s failed in the storage provider (correlation %q)", m.name, correlation)
}

// transportError classifies a failure that carries no storage ErrorInfo,
// from the gRPC library or a proxy, by whether the request may have left this
// process.
func transportError(ctx context.Context, m *method, err error, evidence *sendEvidence) error {
	detail := "transport failure"
	if st, ok := status.FromError(err); ok {
		detail = st.Code().String()
	}
	contextErr := ctx.Err()
	if !evidence.possiblySent() {
		if contextErr != nil {
			return fmt.Errorf("%w: %s was not sent: %w", storagev1.ErrUnavailable, m.name, contextErr)
		}
		return fmt.Errorf("%w: %s was not sent: %s", storagev1.ErrUnavailable, m.name, detail)
	}
	if m.kind == KindRead {
		if contextErr != nil {
			return fmt.Errorf("%s: %w", m.name, contextErr)
		}
		return fmt.Errorf("%w: %s: %s", storagev1.ErrUnavailable, m.name, detail)
	}
	if contextErr != nil {
		return fmt.Errorf("%w: %s may have been sent: %w", storagev1.ErrOutcomeUnknown, m.name, contextErr)
	}
	return fmt.Errorf("%w: %s may have been sent: %s", storagev1.ErrOutcomeUnknown, m.name, detail)
}
