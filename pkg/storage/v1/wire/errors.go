package wire

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const ErrorDomain = "storage.orka.ai"

const (
	ReasonNotFound                     = "NOT_FOUND"
	ReasonConflict                     = "CONFLICT"
	ReasonFenced                       = "FENCED"
	ReasonPreconditionFailed           = "PRECONDITION_FAILED"
	ReasonBindingConflict              = "BINDING_CONFLICT"
	ReasonSessionConfigurationMismatch = "SESSION_CONFIGURATION_MISMATCH"
	ReasonNotReady                     = "NOT_READY"
	ReasonDatasetDetached              = "DATASET_DETACHED"
	ReasonDatasetMigrated              = "DATASET_MIGRATED"
	ReasonDatasetMismatch              = "DATASET_MISMATCH"
	ReasonClockSkew                    = "CLOCK_SKEW"
	ReasonOperationExpired             = "OPERATION_EXPIRED"
	ReasonOperationAborted             = "OPERATION_ABORTED"
	ReasonDuplicateMismatch            = "DUPLICATE_MISMATCH"
	ReasonGatewayMessageReplayOnly     = "GATEWAY_MESSAGE_REPLAY_ONLY"
	ReasonCapacity                     = "CAPACITY"
	ReasonValidation                   = "VALIDATION"
	ReasonGatewayOwnedSession          = "GATEWAY_OWNED_SESSION"
	ReasonGatewaySessionCleanupPending = "GATEWAY_SESSION_CLEANUP_PENDING"
	ReasonTaskJobRevoked               = "TASK_JOB_REVOKED"
	ReasonTaskDataCleanupChanged       = "TASK_DATA_CLEANUP_CHANGED"
	ReasonUsageSelectionTooLarge       = "USAGE_SELECTION_TOO_LARGE"
	ReasonConnectorCustodyTombstoned   = "CONNECTOR_CUSTODY_TOMBSTONED"
	ReasonConnectorConsentSuperseded   = "CONNECTOR_CONSENT_SUPERSEDED"
	ReasonConnectorRetiredLimit        = "CONNECTOR_RETIRED_LIMIT"
	ReasonTooLarge                     = "TOO_LARGE"
	ReasonUnavailable                  = "UNAVAILABLE"
	ReasonOutcomeUnknown               = "OUTCOME_UNKNOWN"
	ReasonUnauthenticated              = "UNAUTHENTICATED"
	ReasonIncompatible                 = "INCOMPATIBLE"
	ReasonContractMismatch             = "CONTRACT_MISMATCH"
	ReasonUnsupportedField             = "UNSUPPORTED_FIELD"
	ReasonOverloaded                   = "OVERLOADED"
	ReasonProviderError                = "PROVIDER_ERROR"
)

const messageLimit = 1024

const messageKey = "message"

type sentinelReason struct {
	reason   string
	code     codes.Code
	sentinel error
}

var outcomeReasons = []sentinelReason{
	{ReasonOutcomeUnknown, codes.Unknown, storagev1.ErrOutcomeUnknown},
	{ReasonUnavailable, codes.Unavailable, storagev1.ErrUnavailable},
	{ReasonUnauthenticated, codes.Unauthenticated, storagev1.ErrUnauthenticated},
	{ReasonIncompatible, codes.FailedPrecondition, storagev1.ErrIncompatible},
}

var sentinelReasons = []sentinelReason{
	{ReasonOperationExpired, codes.FailedPrecondition, storagev1.ErrOperationExpired},
	{ReasonOperationAborted, codes.Aborted, storagev1.ErrOperationAborted},
	{ReasonDatasetDetached, codes.FailedPrecondition, storagev1.ErrDatasetDetached},
	{ReasonDatasetMigrated, codes.FailedPrecondition, storagev1.ErrDatasetMigrated},
	{ReasonDatasetMismatch, codes.FailedPrecondition, storagev1.ErrDatasetMismatch},
	{ReasonClockSkew, codes.Unavailable, storagev1.ErrClockSkew},
	{ReasonSessionConfigurationMismatch, codes.FailedPrecondition, storagev1.ErrSessionConfigurationMismatch},
	{ReasonDuplicateMismatch, codes.AlreadyExists, storagev1.ErrDuplicateMismatch},
	{ReasonGatewayMessageReplayOnly, codes.FailedPrecondition, storagev1.ErrGatewayMessageReplayOnly},
	{ReasonGatewayOwnedSession, codes.FailedPrecondition, storagev1.ErrGatewayOwnedSession},
	{ReasonGatewaySessionCleanupPending, codes.Unavailable, storagev1.ErrGatewaySessionCleanupPending},
	{ReasonTaskJobRevoked, codes.FailedPrecondition, storagev1.ErrTaskJobRevoked},
	{ReasonTaskDataCleanupChanged, codes.Aborted, storagev1.ErrTaskDataCleanupChanged},
	{ReasonUsageSelectionTooLarge, codes.ResourceExhausted, storagev1.ErrUsageSelectionTooLarge},
	{ReasonConnectorCustodyTombstoned, codes.FailedPrecondition, storagev1.ErrConnectorCustodyTombstoned},
	{ReasonConnectorConsentSuperseded, codes.FailedPrecondition, storagev1.ErrConnectorConsentSuperseded},
	{ReasonConnectorRetiredLimit, codes.ResourceExhausted, storagev1.ErrConnectorRetiredLimit},
	{ReasonNotFound, codes.NotFound, storagev1.ErrNotFound},
	{ReasonNotReady, codes.Unavailable, storagev1.ErrNotReady},
	{ReasonCapacity, codes.ResourceExhausted, storagev1.ErrCapacity},
}

func errorStatus(code codes.Code, reason, message string, metadata map[string]string) error {
	st := status.New(code, message)
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: ErrorDomain, Metadata: metadata})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}

func contractStatus(err error) (encoded error, ok bool) {
	for _, entry := range outcomeReasons {
		if errors.Is(err, entry.sentinel) {
			return errorStatus(entry.code, entry.reason, entry.sentinel.Error(), nil), true
		}
	}
	binding, isBinding := asTyped[storagev1.BindingConflictError](err)
	tooLarge, isTooLarge := asTyped[storagev1.TooLargeError](err)
	switch {
	case errors.Is(err, storagev1.ErrFenced):
		return messageStatus(codes.FailedPrecondition, ReasonFenced, typedMessage[storagev1.FencedError](err)), true
	case errors.Is(err, storagev1.ErrPreconditionFailed):
		return messageStatus(codes.FailedPrecondition, ReasonPreconditionFailed,
			typedMessage[storagev1.PreconditionFailedError](err)), true
	case isBinding:
		return bindingConflictStatus(binding), true
	case isTooLarge:
		bounded := storagev1.TooLargeError{What: sanitizeText(tooLarge.What, messageLimit), Size: tooLarge.Size, Limit: tooLarge.Limit}
		return errorStatus(codes.ResourceExhausted, ReasonTooLarge, bounded.Error(), map[string]string{
			"what":  bounded.What,
			"size":  strconv.FormatInt(bounded.Size, 10),
			"limit": strconv.FormatInt(bounded.Limit, 10),
		}), true
	case errors.Is(err, storagev1.ErrTooLarge):
		return errorStatus(codes.ResourceExhausted, ReasonTooLarge, storagev1.ErrTooLarge.Error(), nil), true
	case errors.Is(err, storagev1.ErrValidation):
		return messageStatus(codes.InvalidArgument, ReasonValidation, typedMessage[storagev1.ValidationError](err)), true
	}
	for _, entry := range sentinelReasons {
		if errors.Is(err, entry.sentinel) {
			return errorStatus(entry.code, entry.reason, entry.sentinel.Error(), nil), true
		}
	}
	if errors.Is(err, storagev1.ErrConflict) {
		return messageStatus(codes.Aborted, ReasonConflict, err.Error()), true
	}
	return nil, false
}

func asTyped[T error, P interface {
	*T
	error
}](err error) (T, bool) {
	if typed, ok := errors.AsType[T](err); ok {
		return typed, true
	}
	if typed, ok := errors.AsType[P](err); ok && typed != nil {
		return *typed, true
	}
	var zero T
	return zero, false
}

func typedMessage[T error, P interface {
	*T
	error
}](err error) string {
	if typed, ok := asTyped[T, P](err); ok {
		return typed.Error()
	}
	return err.Error()
}

func messageStatus(code codes.Code, reason, text string) error {
	message := sanitizeText(text, messageLimit)
	return errorStatus(code, reason, message, map[string]string{messageKey: message})
}

func bindingConflictStatus(conflict storagev1.BindingConflictError) error {
	conflict.DatasetID = sanitizeText(conflict.DatasetID, messageLimit)
	conflict.Reason = sanitizeText(conflict.Reason, messageLimit)
	message := sanitizeText(conflict.Error(), messageLimit)
	info := &errdetails.ErrorInfo{Reason: ReasonBindingConflict, Domain: ErrorDomain, Metadata: map[string]string{messageKey: message}}
	st := status.New(codes.Aborted, message)
	if detail, err := toWireBindingConflictError(conflict); err == nil {
		if detailed, err := st.WithDetails(info, detail); err == nil {
			return detailed.Err()
		}
	}
	return errorStatus(codes.Aborted, ReasonBindingConflict, message, info.GetMetadata())
}

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

func storageErrorInfo(st *status.Status) *errdetails.ErrorInfo {
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetDomain() == ErrorDomain {
			return info
		}
	}
	return nil
}

var refusedBeforeDataset = map[string]bool{
	ReasonUnauthenticated:  true,
	ReasonContractMismatch: true,
	ReasonUnsupportedField: true,
	ReasonDatasetMismatch:  true,
	ReasonUnavailable:      true,
	ReasonIncompatible:     true,
	ReasonOverloaded:       true,
}

func answersForDataset(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	info := storageErrorInfo(st)
	return info != nil && !refusedBeforeDataset[info.GetReason()]
}

func decodeError(ctx context.Context, m *method, err error, evidence *sendEvidence) error {
	st, ok := status.FromError(err)
	if ok {
		if info := storageErrorInfo(st); info != nil {
			return decodeReason(m, st, info)
		}
		if st.Code() == codes.Unimplemented {
			return fmt.Errorf("%w: storage server does not serve %s", storagev1.ErrIncompatible, m.name)
		}
	}
	return transportError(ctx, m, err, evidence)
}

func decodeReason(m *method, st *status.Status, info *errdetails.ErrorInfo) error {
	metadata := info.GetMetadata()
	message := sanitizeText(metadata[messageKey], messageLimit)
	switch info.GetReason() {
	case ReasonFenced:
		return storagev1.FencedError{Message: message}
	case ReasonPreconditionFailed:
		return storagev1.PreconditionFailedError{Message: message}
	case ReasonBindingConflict:
		return decodeBindingConflict(st, message)
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
	case ReasonOverloaded:
		return fmt.Errorf("%w: the storage server is overloaded", storagev1.ErrUnavailable)
	case ReasonContractMismatch, ReasonUnsupportedField:
		return fmt.Errorf("%w: storage server refused %s: %s", storagev1.ErrIncompatible, m.name, info.GetReason())
	}
	for _, table := range [][]sentinelReason{outcomeReasons, sentinelReasons} {
		for _, entry := range table {
			if info.GetReason() == entry.reason {
				return remoteError{sentinel: entry.sentinel}
			}
		}
	}
	correlation := sanitizeText(metadata["correlation"], 64)
	if m.mayChange() {
		return fmt.Errorf("%w: %s failed in the storage provider (correlation %q)", storagev1.ErrOutcomeUnknown, m.name, correlation)
	}
	return fmt.Errorf("%s failed in the storage provider (correlation %q)", m.name, correlation)
}

func decodeBindingConflict(st *status.Status, message string) error {
	for _, detail := range st.Details() {
		encoded, ok := detail.(*storagepb.BindingConflictError)
		if !ok {
			continue
		}
		conflict, err := fromWireBindingConflictError(encoded, false)
		if err != nil {
			break
		}
		conflict.DatasetID = sanitizeText(conflict.DatasetID, messageLimit)
		conflict.Reason = sanitizeText(conflict.Reason, messageLimit)
		return conflict
	}
	return storagev1.BindingConflictError{Reason: message}
}

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
	if !m.mayChange() {
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
