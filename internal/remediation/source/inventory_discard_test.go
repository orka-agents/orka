package source

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInventoryDiscardedBatchBudgetIsBounded(t *testing.T) {
	reader := inventoryReader{}
	for discarded := 1; discarded <= 4; discarded++ {
		handled, err := reader.useNonrecursiveInventory()
		require.NoError(t, err)
		require.False(t, handled)
		require.Equal(t, discarded, reader.recursiveDiscards)
		require.Equal(t, discarded == 4, reader.recursiveDisabled)
		require.Zero(t, reader.treeEntries)
		require.Zero(t, reader.treeBytes)
	}
}
