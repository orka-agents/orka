package wire

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// contractSentinels lists every Err variable the contract declares. The
// coverage test keeps it complete.
var contractSentinels = map[string]error{
	"ErrNotFound":                     storagev1.ErrNotFound,
	"ErrConflict":                     storagev1.ErrConflict,
	"ErrSessionConfigurationMismatch": storagev1.ErrSessionConfigurationMismatch,
	"ErrNotReady":                     storagev1.ErrNotReady,
	"ErrDuplicateMismatch":            storagev1.ErrDuplicateMismatch,
	"ErrGatewayMessageReplayOnly":     storagev1.ErrGatewayMessageReplayOnly,
	"ErrCapacity":                     storagev1.ErrCapacity,
	"ErrValidation":                   storagev1.ErrValidation,
	"ErrGatewayOwnedSession":          storagev1.ErrGatewayOwnedSession,
	"ErrUnavailable":                  storagev1.ErrUnavailable,
	"ErrOutcomeUnknown":               storagev1.ErrOutcomeUnknown,
	"ErrFenced":                       storagev1.ErrFenced,
	"ErrTooLarge":                     storagev1.ErrTooLarge,
	"ErrPreconditionFailed":           storagev1.ErrPreconditionFailed,
	"ErrUnauthenticated":              storagev1.ErrUnauthenticated,
	"ErrIncompatible":                 storagev1.ErrIncompatible,
	"ErrGatewaySessionCleanupPending": storagev1.ErrGatewaySessionCleanupPending,
	"ErrTaskJobRevoked":               storagev1.ErrTaskJobRevoked,
	"ErrTaskDataCleanupChanged":       storagev1.ErrTaskDataCleanupChanged,
	"ErrUsageSelectionTooLarge":       storagev1.ErrUsageSelectionTooLarge,
	"ErrDatasetDetached":              storagev1.ErrDatasetDetached,
	"ErrDatasetMismatch":              storagev1.ErrDatasetMismatch,
	"ErrClockSkew":                    storagev1.ErrClockSkew,
	"ErrOperationExpired":             storagev1.ErrOperationExpired,
	"ErrOperationAborted":             storagev1.ErrOperationAborted,
	"ErrConnectorCustodyTombstoned":   storagev1.ErrConnectorCustodyTombstoned,
	"ErrConnectorConsentSuperseded":   storagev1.ErrConnectorConsentSuperseded,
	"ErrConnectorRetiredLimit":        storagev1.ErrConnectorRetiredLimit,
}

// alsoMatches lists the sentinels a decoded sentinel matches by the
// contract's own wrapping.
var alsoMatches = map[string][]string{
	"ErrFenced":             {"ErrConflict"},
	"ErrPreconditionFailed": {"ErrConflict"},
	"ErrDatasetDetached":    {"ErrNotReady"},
}

// A contract error the codec does not know would reach the client as a
// provider error, turning a refused write into an unknown outcome.
func TestEveryContractSentinelIsCovered(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	contractDir := filepath.Dir(filepath.Dir(file))
	names, err := filepath.Glob(filepath.Join(contractDir, "*.go"))
	require.NoError(t, err)
	var declared []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range source.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, ident := range spec.(*ast.ValueSpec).Names {
					if strings.HasPrefix(ident.Name, "Err") {
						declared = append(declared, ident.Name)
					}
				}
			}
		}
	}
	sort.Strings(declared)
	listed := make([]string, 0, len(contractSentinels))
	for name := range contractSentinels {
		listed = append(listed, name)
	}
	sort.Strings(listed)
	require.Equal(t, declared, listed, "contractSentinels lists every contract sentinel")
}

// serverToClient encodes err as the server library does and decodes it as
// the client does.
func serverToClient(t *testing.T, m *method, err error) error {
	t.Helper()
	encoded, ok := contractStatus(err)
	require.True(t, ok, "%v is a contract error", err)
	return decodeError(t.Context(), m, encoded, &sendEvidence{})
}

var (
	readMethod  = &method{name: "GetPlan", kind: storagev1.MethodRead, since: 1}
	writeMethod = &method{name: "SavePlan", kind: storagev1.MethodWrite, since: 1}
	adminMethod = &method{name: "ResolveOperation", kind: storagev1.MethodAdmin, since: 1}
)

func TestEveryContractSentinelCrossesTheWire(t *testing.T) {
	for name, sentinel := range contractSentinels {
		t.Run(name, func(t *testing.T) {
			for _, m := range []*method{readMethod, writeMethod} {
				decoded := serverToClient(t, m, fmt.Errorf("provider detail: %w", sentinel))
				require.ErrorIs(t, decoded, sentinel)
				for otherName, other := range contractSentinels {
					if otherName == name || slices.Contains(alsoMatches[name], otherName) {
						continue
					}
					require.False(t, errors.Is(decoded, other), "%s also matches %s", name, otherName)
				}
			}
		})
	}
}

func TestTypedContractErrorsKeepTheirDetail(t *testing.T) {
	fenced := serverToClient(t, writeMethod, storagev1.FencedErrorf("epoch %d is stale", 4))
	var fencedErr storagev1.FencedError
	require.ErrorAs(t, fenced, &fencedErr)
	require.Equal(t, "epoch 4 is stale", fencedErr.Message)
	require.ErrorIs(t, fenced, storagev1.ErrConflict, "a fence rejection still matches ErrConflict")

	validation := serverToClient(t, writeMethod, storagev1.ValidationErrorf("namespace is required"))
	var validationErr storagev1.ValidationError
	require.ErrorAs(t, validation, &validationErr)
	require.Equal(t, "namespace is required", validationErr.Message)

	precondition := serverToClient(t, writeMethod, storagev1.PreconditionFailedErrorf("generation %d moved", 5))
	var preconditionErr storagev1.PreconditionFailedError
	require.ErrorAs(t, precondition, &preconditionErr)
	require.Equal(t, "generation 5 moved", preconditionErr.Message)
	require.ErrorIs(t, precondition, storagev1.ErrConflict)

	// The provider's wrapping text stays on the server: the client finds the
	// typed message an in-process caller finds, whether the provider returned
	// the typed error's value or a pointer to it.
	for _, typed := range []struct{ err, sentinel error }{
		{storagev1.ValidationErrorf("x"), storagev1.ErrValidation},
		{&storagev1.ValidationError{Message: "x"}, storagev1.ErrValidation},
		{storagev1.FencedErrorf("x"), storagev1.ErrFenced},
		{&storagev1.FencedError{Message: "x"}, storagev1.ErrFenced},
		{storagev1.PreconditionFailedErrorf("x"), storagev1.ErrPreconditionFailed},
		{&storagev1.PreconditionFailedError{Message: "x"}, storagev1.ErrPreconditionFailed},
	} {
		wrapped := serverToClient(t, writeMethod, fmt.Errorf("save plan %q: %w", "secret-value", typed.err))
		require.Equal(t, "x", wrapped.Error(), "%T", typed.err)
		require.ErrorIs(t, wrapped, typed.sentinel)
	}
	bare := serverToClient(t, writeMethod, fmt.Errorf("plan: %w", storagev1.ErrValidation))
	require.ErrorIs(t, bare, storagev1.ErrValidation)
	require.Equal(t, "plan: validation error", bare.Error(), "a bare sentinel sends the provider's text")

	bound := storagev1.BindingConflictError{
		DatasetID: "dataset-1",
		Binding: &storagev1.DatasetBinding{
			InstallationID: "installation-2", RecordUID: "uid", SwitchGeneration: 3, KeyFingerprint: "sha256:ab",
			State: storagev1.DatasetBound, BoundAt: time.Date(2026, 11, 17, 1, 2, 3, 4, time.UTC),
		},
		Reason: "bound to another installation",
	}
	binding := serverToClient(t, writeMethod, fmt.Errorf("bind: %w", bound))
	var bindingErr storagev1.BindingConflictError
	require.ErrorAs(t, binding, &bindingErr)
	require.Equal(t, bound, bindingErr, "the binding conflict keeps its stored binding")
	require.ErrorIs(t, binding, storagev1.ErrConflict)
	binding = serverToClient(t, writeMethod, fmt.Errorf("bind: %w", &bound))
	bindingErr = storagev1.BindingConflictError{}
	require.ErrorAs(t, binding, &bindingErr)
	require.Equal(t, bound, bindingErr, "a pointer to a binding conflict keeps its stored binding")

	tooLarge := serverToClient(t, writeMethod, storagev1.TooLargeError{What: "artifact \"a\"", Size: 11, Limit: 10})
	require.ErrorIs(t, tooLarge, storagev1.ErrTooLarge)
	require.Equal(t, storagev1.TooLargeError{What: "artifact \"a\"", Size: 11, Limit: 10}, tooLarge)
	tooLarge = serverToClient(t, writeMethod, fmt.Errorf("save: %w", &storagev1.TooLargeError{What: "plan", Size: 20, Limit: 10}))
	require.Equal(t, storagev1.TooLargeError{What: "plan", Size: 20, Limit: 10}, tooLarge,
		"a pointer to a size refusal keeps its fields")

	conflict := serverToClient(t, writeMethod, storagev1.ConflictErrorf("version %d changed", 3))
	require.ErrorIs(t, conflict, storagev1.ErrConflict)
	require.Equal(t, "conflict: version 3 changed", conflict.Error())

	notFound := serverToClient(t, readMethod, fmt.Errorf("row 7 for %q: %w", "secret-value", storagev1.ErrNotFound))
	require.NotContains(t, notFound.Error(), "secret-value", "only VALIDATION, FENCED and CONFLICT forward their text")

	wrappedTooLarge, ok := contractStatus(fmt.Errorf("row %q: %w", "secret-value",
		storagev1.TooLargeError{What: "result", Size: 11, Limit: 10}))
	require.True(t, ok)
	require.NotContains(t, status.Convert(wrappedTooLarge).Message(), "secret-value",
		"a size refusal carries only its typed fields")
}

func TestForwardedMessagesAreBoundedAndStripped(t *testing.T) {
	long := strings.Repeat("x", 4096)
	decoded := serverToClient(t, writeMethod, storagev1.ValidationErrorf("bad\x00 value\n%s", long))
	var validationErr storagev1.ValidationError
	require.ErrorAs(t, decoded, &validationErr)
	require.LessOrEqual(t, len(validationErr.Message), messageLimit)
	require.True(t, strings.HasPrefix(validationErr.Message, "bad valuexxx"))
}

func TestProviderErrorsAreUnknownOutcomesForWritesOnly(t *testing.T) {
	encoded := providerErrorStatus("abc123")
	write := decodeError(t.Context(), writeMethod, encoded, &sendEvidence{})
	require.ErrorIs(t, write, storagev1.ErrOutcomeUnknown)
	require.Contains(t, write.Error(), "abc123")
	read := decodeError(t.Context(), readMethod, encoded, &sendEvidence{})
	require.False(t, errors.Is(read, storagev1.ErrOutcomeUnknown))
	require.False(t, errors.Is(read, storagev1.ErrUnavailable))

	_, ok := contractStatus(errors.New("disk I/O error near 'secret-value'"))
	require.False(t, ok, "an untyped provider error is not a contract error")

	unknownReason := errorStatus(codes.FailedPrecondition, "SOMETHING_NEW", "new", nil)
	require.ErrorIs(t, decodeError(t.Context(), writeMethod, unknownReason, &sendEvidence{}), storagev1.ErrOutcomeUnknown,
		"an unknown reason on a write is an unknown outcome")
}

// An older server would ignore a parameter newer than its revision, so the
// client refuses a call that sets one instead of sending it without it.
func TestNewerParametersAreNeverDroppedSilently(t *testing.T) {
	older := &Client{serverRevision: 1}
	for _, unset := range []any{"", 0, []string{}, map[string]string{}, (*storagev1.PlanState)(nil), nil} {
		require.NoError(t, older.checkNewerParameter(readMethod, "filter", 2, unset), "%#v is not sent", unset)
	}
	for _, set := range []any{"x", 1, []string{"x"}, &storagev1.PlanState{}} {
		require.ErrorIs(t, older.checkNewerParameter(readMethod, "filter", 2, set), storagev1.ErrIncompatible)
	}
	newer := &Client{serverRevision: 2}
	require.NoError(t, newer.checkNewerParameter(readMethod, "filter", 2, "x"))
}

func TestServerRefusalsDecodeToContractErrors(t *testing.T) {
	unauthenticated := decodeError(t.Context(), writeMethod,
		errorStatus(codes.Unauthenticated, ReasonUnauthenticated, "no", nil), &sendEvidence{})
	require.ErrorIs(t, unauthenticated, storagev1.ErrUnauthenticated)
	require.False(t, errors.Is(unauthenticated, storagev1.ErrOutcomeUnknown))

	for _, reason := range []string{ReasonContractMismatch, ReasonUnsupportedField} {
		refused := decodeError(t.Context(), writeMethod, errorStatus(codes.InvalidArgument, reason, "no", nil), &sendEvidence{})
		require.ErrorIs(t, refused, storagev1.ErrIncompatible)
		require.False(t, errors.Is(refused, storagev1.ErrOutcomeUnknown))
	}

	unimplemented := decodeError(t.Context(), writeMethod, status.Error(codes.Unimplemented, "unknown method"), &sendEvidence{})
	require.ErrorIs(t, unimplemented, storagev1.ErrIncompatible)

	binding := decodeError(t.Context(), adminMethod,
		errorStatus(codes.Aborted, ReasonBindingConflict, "x", map[string]string{messageKey: "detail lost"}), &sendEvidence{})
	var bindingErr storagev1.BindingConflictError
	require.ErrorAs(t, binding, &bindingErr, "a binding conflict without its detail keeps its type")
	require.Equal(t, "detail lost", bindingErr.Reason)
}

// A status without the storage ErrorInfo came from the gRPC library or a
// proxy, so send evidence decides it.
func TestTransportFailuresAreClassifiedBySendEvidence(t *testing.T) {
	failure := status.Error(codes.Unavailable, "connection reset")
	notSent := &sendEvidence{}
	notSent.covered.Store(true)
	sent := &sendEvidence{}
	sent.covered.Store(true)
	sent.sent.Store(true)
	uncovered := &sendEvidence{}

	for _, m := range []*method{readMethod, writeMethod, adminMethod} {
		err := decodeError(t.Context(), m, failure, notSent)
		require.ErrorIs(t, err, storagev1.ErrUnavailable, "%s never sent", m.name)
		require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
	}
	require.ErrorIs(t, decodeError(t.Context(), readMethod, failure, sent), storagev1.ErrUnavailable)
	require.ErrorIs(t, decodeError(t.Context(), writeMethod, failure, sent), storagev1.ErrOutcomeUnknown)
	require.ErrorIs(t, decodeError(t.Context(), adminMethod, failure, sent), storagev1.ErrOutcomeUnknown,
		"an administrative call may change state, so its outcome is unknown")
	require.ErrorIs(t, decodeError(t.Context(), writeMethod, failure, uncovered), storagev1.ErrOutcomeUnknown,
		"without Dial's coverage a write's transport failure is an unknown outcome")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := status.Error(codes.Canceled, "context canceled")
	readErr := decodeError(ctx, readMethod, canceled, sent)
	require.ErrorIs(t, readErr, context.Canceled)
	require.False(t, errors.Is(readErr, storagev1.ErrOutcomeUnknown))
	writeErr := decodeError(ctx, writeMethod, canceled, sent)
	require.ErrorIs(t, writeErr, storagev1.ErrOutcomeUnknown)
	require.ErrorIs(t, writeErr, context.Canceled)
	notSentErr := decodeError(ctx, writeMethod, canceled, notSent)
	require.ErrorIs(t, notSentErr, storagev1.ErrUnavailable)
	require.ErrorIs(t, notSentErr, context.Canceled)
}

func TestStatusesCarryTheStorageErrorInfo(t *testing.T) {
	encoded, ok := contractStatus(storagev1.ErrNotFound)
	require.True(t, ok)
	st := status.Convert(encoded)
	require.Equal(t, codes.NotFound, st.Code())
	info := storageErrorInfo(st)
	require.NotNil(t, info)
	require.Equal(t, ErrorDomain, info.GetDomain())
	require.Equal(t, ReasonNotFound, info.GetReason())

	foreign, err := status.New(codes.NotFound, "x").WithDetails(&errdetails.ErrorInfo{Domain: "example.com", Reason: ReasonNotFound})
	require.NoError(t, err)
	require.Nil(t, storageErrorInfo(foreign), "another domain's ErrorInfo is not authoritative")
}
