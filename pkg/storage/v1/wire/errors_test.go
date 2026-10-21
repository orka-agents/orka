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
	"sort"
	"strings"
	"testing"

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
	"ErrGatewaySessionCleanupPending": storagev1.ErrGatewaySessionCleanupPending,
	"ErrTaskJobRevoked":               storagev1.ErrTaskJobRevoked,
	"ErrUsageSelectionTooLarge":       storagev1.ErrUsageSelectionTooLarge,
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
	readMethod  = &method{name: "GetPlan", kind: KindRead, since: 1}
	writeMethod = &method{name: "SavePlan", kind: KindWrite, since: 1}
)

func TestEveryContractSentinelCrossesTheWire(t *testing.T) {
	for name, sentinel := range contractSentinels {
		t.Run(name, func(t *testing.T) {
			for _, m := range []*method{readMethod, writeMethod} {
				decoded := serverToClient(t, m, fmt.Errorf("provider detail: %w", sentinel))
				require.ErrorIs(t, decoded, sentinel)
				for otherName, other := range contractSentinels {
					if otherName == name || (name == "ErrFenced" && otherName == "ErrConflict") {
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

	tooLarge := serverToClient(t, writeMethod, storagev1.TooLargeError{What: "artifact \"a\"", Size: 11, Limit: 10})
	require.ErrorIs(t, tooLarge, storagev1.ErrTooLarge)
	require.Equal(t, storagev1.TooLargeError{What: "artifact \"a\"", Size: 11, Limit: 10}, tooLarge)

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

func TestServerRefusalsDecodeToTheirMarkers(t *testing.T) {
	unauthenticated := decodeError(t.Context(), writeMethod,
		errorStatus(codes.Unauthenticated, ReasonUnauthenticated, "no", nil), &sendEvidence{})
	require.ErrorIs(t, unauthenticated, storagev1.ErrUnavailable)
	require.ErrorIs(t, unauthenticated, ErrAuthentication)
	require.False(t, errors.Is(unauthenticated, storagev1.ErrOutcomeUnknown))

	for _, reason := range []string{ReasonContractMismatch, ReasonUnsupportedField} {
		refused := decodeError(t.Context(), writeMethod, errorStatus(codes.InvalidArgument, reason, "no", nil), &sendEvidence{})
		require.ErrorIs(t, refused, storagev1.ErrValidation)
		require.ErrorIs(t, refused, ErrIncompatible)
	}

	unimplemented := decodeError(t.Context(), writeMethod, status.Error(codes.Unimplemented, "unknown method"), &sendEvidence{})
	require.ErrorIs(t, unimplemented, storagev1.ErrValidation)
	require.ErrorIs(t, unimplemented, ErrIncompatible)
}

// A status without the storage ErrorInfo came from the gRPC library or a
// proxy, so send evidence decides it.
func TestTransportFailuresAreClassifiedBySendEvidence(t *testing.T) {
	failure := status.Error(codes.Unavailable, "connection reset")
	notSent := &sendEvidence{}
	notSent.attempted.Store(true)
	sent := &sendEvidence{}
	sent.attempted.Store(true)
	sent.sent.Store(true)
	noHandler := &sendEvidence{}

	for _, m := range []*method{readMethod, writeMethod} {
		err := decodeError(t.Context(), m, failure, notSent)
		require.ErrorIs(t, err, storagev1.ErrUnavailable, "%s never sent", m.name)
		require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
	}
	require.ErrorIs(t, decodeError(t.Context(), readMethod, failure, sent), storagev1.ErrUnavailable)
	require.ErrorIs(t, decodeError(t.Context(), writeMethod, failure, sent), storagev1.ErrOutcomeUnknown)
	require.ErrorIs(t, decodeError(t.Context(), writeMethod, failure, noHandler), storagev1.ErrOutcomeUnknown,
		"without the evidence handler a write's transport failure is an unknown outcome")

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
