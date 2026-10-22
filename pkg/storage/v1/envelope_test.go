package v1_test

import (
	"strings"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func validEnvelope(t *testing.T) storagev1.WriteEnvelope {
	t.Helper()
	return storagev1.WriteEnvelope{
		DatasetID:     "3f0b8f9e-6f3c-4a59-9a43-1d2f5e7c9b10",
		Fence:         storagev1.ControllerEpochFence{Name: storagev1.DefaultControllerEpochName, Epoch: 2, HolderID: "holder"},
		OperationID:   "0192f1c4-7d3e-7a10-8b2c-5e6f7a8b9c0d",
		RequestDigest: requestDigest(t, "SaveResult", "tenant", "task", []byte("result")),
		IssuedAt:      time.Date(2026, time.October, 22, 1, 2, 3, 4, time.UTC),
		Sequence:      1,
	}
}

func TestWriteEnvelopeValidate(t *testing.T) {
	require.NoError(t, validEnvelope(t).Validate())

	for name, mutate := range map[string]func(*storagev1.WriteEnvelope){
		"missing dataset":         func(e *storagev1.WriteEnvelope) { e.DatasetID = "" },
		"padded dataset":          func(e *storagev1.WriteEnvelope) { e.DatasetID = " " + e.DatasetID },
		"missing fence name":      func(e *storagev1.WriteEnvelope) { e.Fence.Name = "" },
		"missing fence holder":    func(e *storagev1.WriteEnvelope) { e.Fence.HolderID = "" },
		"zero fence epoch":        func(e *storagev1.WriteEnvelope) { e.Fence.Epoch = 0 },
		"missing operation ID":    func(e *storagev1.WriteEnvelope) { e.OperationID = "" },
		"oversized operation ID":  func(e *storagev1.WriteEnvelope) { e.OperationID = strings.Repeat("o", storagev1.MaxOperationIDBytes+1) },
		"control in operation ID": func(e *storagev1.WriteEnvelope) { e.OperationID = "op\n1" },
		"C1 control in operation ID": func(e *storagev1.WriteEnvelope) {
			e.OperationID = "op" + string(rune(0x85)) + "1"
		},
		"missing request digest":   func(e *storagev1.WriteEnvelope) { e.RequestDigest = "" },
		"uppercase request digest": func(e *storagev1.WriteEnvelope) { e.RequestDigest = strings.ToUpper(e.RequestDigest) },
		"missing issued-at":        func(e *storagev1.WriteEnvelope) { e.IssuedAt = time.Time{} },
		"zero sequence":            func(e *storagev1.WriteEnvelope) { e.Sequence = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			envelope := validEnvelope(t)
			mutate(&envelope)
			require.ErrorIs(t, envelope.Validate(), storagev1.ErrValidation)
		})
	}

	envelope := validEnvelope(t)
	envelope.OperationID = strings.Repeat("o", storagev1.MaxOperationIDBytes)
	require.NoError(t, envelope.Validate(), "an operation ID of exactly the limit is accepted")
}

func TestEnvelopeErrorsAreDistinct(t *testing.T) {
	sentinels := []error{
		storagev1.ErrDatasetMismatch, storagev1.ErrClockSkew, storagev1.ErrOperationExpired,
		storagev1.ErrOperationAborted, storagev1.ErrDuplicateMismatch, storagev1.ErrFenced,
		storagev1.ErrConflict, storagev1.ErrValidation, storagev1.ErrNotReady,
		storagev1.ErrUnavailable, storagev1.ErrOutcomeUnknown, storagev1.ErrUnauthenticated, storagev1.ErrIncompatible,
	}
	for i, err := range sentinels {
		for j, other := range sentinels {
			if i != j {
				require.NotErrorIs(t, err, other, "%v must not match %v", err, other)
			}
		}
	}
}

func TestOperationLimitsCoverEveryAcceptedRetry(t *testing.T) {
	// A receipt recorded at provider time C may be retried by an operation the
	// provider still accepts until C + MaxClockSkew + MaxOperationAge.
	require.Greater(t, storagev1.MinReceiptRetention, storagev1.MaxOperationAge+storagev1.MaxClockSkew)
	require.Positive(t, storagev1.MaxReceiptResultBytes)
	require.True(t, storagev1.IsValidOperationState(storagev1.OperationCommitted))
	require.True(t, storagev1.IsValidOperationState(storagev1.OperationAborted))
	require.False(t, storagev1.IsValidOperationState("pending"))
}

func TestResolveOperationRequestValidate(t *testing.T) {
	valid := storagev1.ResolveOperationRequest{
		DatasetID:     "dataset-1",
		OperationID:   "operation-1",
		Method:        "SendMessage",
		RequestDigest: requestDigest(t, "SendMessage", &storagev1.Message{Content: "hello"}),
		IssuedAt:      time.Date(2026, time.October, 22, 1, 2, 3, 4, time.UTC),
	}
	require.NoError(t, valid.Validate())
	for name, mutate := range map[string]func(*storagev1.ResolveOperationRequest){
		"missing issued-at":      func(r *storagev1.ResolveOperationRequest) { r.IssuedAt = time.Time{} },
		"missing dataset":        func(r *storagev1.ResolveOperationRequest) { r.DatasetID = "" },
		"missing operation ID":   func(r *storagev1.ResolveOperationRequest) { r.OperationID = "" },
		"unknown method":         func(r *storagev1.ResolveOperationRequest) { r.Method = "SendMessages" },
		"read method":            func(r *storagev1.ResolveOperationRequest) { r.Method = "GetResult" },
		"administrative method":  func(r *storagev1.ResolveOperationRequest) { r.Method = "ResolveOperation" },
		"malformed digest":       func(r *storagev1.ResolveOperationRequest) { r.RequestDigest = "sha256:abc" },
		"missing request digest": func(r *storagev1.ResolveOperationRequest) { r.RequestDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			mutate(&request)
			require.ErrorIs(t, request.Validate(), storagev1.ErrValidation)
		})
	}
}

func TestAdvanceControllerEpochRequestValidate(t *testing.T) {
	acquired := time.Date(2026, time.October, 22, 1, 2, 3, 4, time.UTC)
	valid := storagev1.AdvanceControllerEpochRequest{
		DatasetID: "dataset-1",
		Epoch: storagev1.ControllerEpoch{
			Name: storagev1.DefaultControllerEpochName, Epoch: 3, HolderID: "holder",
			RequestDigest: storagev1.CanonicalBytesDigest([]byte("acquire")), Version: 3,
			AcquiredAt: acquired, UpdatedAt: acquired,
		},
		InstallationID: "controller-epoch-uid",
	}
	require.NoError(t, valid.Validate())
	for name, mutate := range map[string]func(*storagev1.AdvanceControllerEpochRequest){
		"missing acquired time": func(r *storagev1.AdvanceControllerEpochRequest) { r.Epoch.AcquiredAt = time.Time{} },
		"missing updated time":  func(r *storagev1.AdvanceControllerEpochRequest) { r.Epoch.UpdatedAt = time.Time{} },
		"missing dataset":       func(r *storagev1.AdvanceControllerEpochRequest) { r.DatasetID = "" },
		"missing installation":  func(r *storagev1.AdvanceControllerEpochRequest) { r.InstallationID = "" },
		"missing epoch name":    func(r *storagev1.AdvanceControllerEpochRequest) { r.Epoch.Name = "" },
		"missing holder":        func(r *storagev1.AdvanceControllerEpochRequest) { r.Epoch.HolderID = "" },
		"malformed digest":      func(r *storagev1.AdvanceControllerEpochRequest) { r.Epoch.RequestDigest = "digest" },
		"zero epoch":            func(r *storagev1.AdvanceControllerEpochRequest) { r.Epoch.Epoch = 0 },
		"zero version":          func(r *storagev1.AdvanceControllerEpochRequest) { r.Epoch.Version = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			mutate(&request)
			require.ErrorIs(t, request.Validate(), storagev1.ErrValidation)
		})
	}
}

func TestDatasetStateVocabulary(t *testing.T) {
	for _, state := range []storagev1.DatasetState{
		storagev1.DatasetReady, storagev1.DatasetMigrating, storagev1.DatasetImporting, storagev1.DatasetIncompatible,
	} {
		require.True(t, storagev1.IsValidDatasetState(state), state)
	}
	require.False(t, storagev1.IsValidDatasetState(""))
	require.False(t, storagev1.IsValidDatasetState("detached"))
}
