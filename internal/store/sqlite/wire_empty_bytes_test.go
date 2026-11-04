package sqlite_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// Empty result and receipt bytes are valid values, as they are in process:
// over the wire, where nil and empty bytes look the same, SQLite still
// receives a non-nil empty value for a present record.
func TestEmptyResultBytesCrossTheWire(t *testing.T) {
	ctx := t.Context()
	provider := wiretest.State(t, openConformanceStore(t))

	receipt := storagev1.PromptResultReceipt{
		AttemptID: "attempt", Namespace: "tenant", TaskName: "task", OperationID: "operation",
		OperationDigest: storagev1.CanonicalBytesDigest([]byte("operation")), Data: []byte{},
	}
	require.NoError(t, provider.SavePromptResultReceipt(ctx, receipt))
	stored, err := provider.GetPromptResultReceipt(ctx, "attempt")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Empty(t, stored.Data)
	require.NoError(t, provider.SavePromptResultReceipt(ctx, receipt), "replaying the same empty receipt is accepted")

	require.NoError(t, provider.SaveResult(ctx, "tenant", "task", []byte{}))
	result, err := provider.GetResult(ctx, "tenant", "task")
	require.NoError(t, err)
	require.Empty(t, result)
}
