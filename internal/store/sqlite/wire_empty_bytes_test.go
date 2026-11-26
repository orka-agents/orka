package sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// Empty result and receipt bytes are valid values, as they are in process:
// over the wire, where nil and empty bytes look the same, SQLite still
// receives a non-nil empty value for a present record.
func TestEmptyResultBytesCrossTheWire(t *testing.T) {
	ctx := t.Context()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	provider := wiretest.State(t, s)

	receipt := storagev1.PromptResultReceipt{
		AttemptID: "attempt", Namespace: "tenant", TaskName: "task", OperationID: "operation",
		OperationDigest: storagev1.CanonicalBytesDigest([]byte("operation")), Data: []byte{},
	}
	receiptCtx, _ := testEnvelope(t, ctx, dataset, "SavePromptResultReceipt", time.Now().UTC(), receipt)
	require.NoError(t, provider.SavePromptResultReceipt(receiptCtx, receipt))
	stored, err := provider.GetPromptResultReceipt(ctx, "attempt")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Empty(t, stored.Data)
	require.NoError(t, provider.SavePromptResultReceipt(receiptCtx, receipt), "replaying the same empty receipt is accepted")

	resultCtx, _ := testEnvelope(t, ctx, dataset, "SaveResult", time.Now().UTC(), "tenant", "task", []byte{})
	require.NoError(t, provider.SaveResult(resultCtx, "tenant", "task", []byte{}))
	result, err := provider.GetResult(ctx, "tenant", "task")
	require.NoError(t, err)
	require.NotNil(t, result, "a present empty result is not nil")
	require.Empty(t, result)
}
