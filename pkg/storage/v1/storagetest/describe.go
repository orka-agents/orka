package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func runDescribe(t *testing.T, newProvider StateFactory) {
	t.Run("describes the provider and its dataset", func(t *testing.T) {
		provider := newProvider(t)
		requireValidProviderInfo(t, provider, storagev1.StateContract)
		require.NoError(t, provider.HealthCheck(t.Context()), "a fresh dataset is healthy")
	})

	t.Run("a dataset holding a record is not empty", func(t *testing.T) {
		provider := newProvider(t)
		require.NoError(t, provider.SaveResult(t.Context(), "tenant-a", "task-1", []byte("result")))
		info := requireValidProviderInfo(t, provider, storagev1.StateContract)
		require.False(t, info.Bootstrap.Empty, "a dataset with a stored record never reports itself empty")
	})
}
