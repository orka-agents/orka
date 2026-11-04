package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

func runDescribe(t *testing.T, newProvider StateFactory) {
	t.Run("describes the provider and its dataset", func(t *testing.T) {
		requireValidProviderInfo(t, newProvider(t), storagev1.StateContract)
	})
}
