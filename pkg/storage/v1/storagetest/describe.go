package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func runDescribe(t *testing.T, newProvider StateFactory) {
	t.Run("describes the provider and its dataset", func(t *testing.T) {
		info := requireValidProviderInfo(t, newProvider(t), storagev1.StateContract)
		require.GreaterOrEqual(t, info.Limits.MaxResultBytes, int64(storagev1.MaxResultBytes),
			"a state.v1 provider accepts results up to the contract limit")
	})
}
