package memory

import (
	"context"
	"fmt"

	"github.com/orka-agents/orka/internal/store"
)

const memoryBindingPageSize = 200

func forEachMemoryBackendBinding(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	filter store.MemoryBackendBindingFilter,
	visit func(store.MemoryBackendBinding) error,
) error {
	if governed == nil {
		return nil
	}
	cursor := filter.BeforeNamespaceUID
	for {
		filter.BeforeNamespaceUID = cursor
		filter.Limit = memoryBindingPageSize
		bindings, err := governed.ListMemoryBackendBindings(ctx, filter)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			if err := visit(binding); err != nil {
				return err
			}
		}
		if len(bindings) < memoryBindingPageSize {
			return nil
		}
		next := bindings[len(bindings)-1].NamespaceUID
		if next == "" || next <= cursor {
			return fmt.Errorf("memory backend binding pagination did not advance")
		}
		cursor = next
	}
}

// CheckFeatureEpochReadiness fails closed when an existing remote binding is
// unsupported by this replica or remote-memory serving has been disabled.
func CheckFeatureEpochReadiness(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	supported int64,
	enabled bool,
) error {
	return forEachMemoryBackendBinding(ctx, governed, store.MemoryBackendBindingFilter{
		Modes: []store.MemoryBackendMode{store.MemoryBackendModeRemote},
	}, func(binding store.MemoryBackendBinding) error {
		if !enabled {
			return fmt.Errorf("memory backend %s is active while MemoryBackend support is disabled", binding.NamespaceUID)
		}
		if binding.MinimumFeatureEpoch > supported {
			return fmt.Errorf("memory backend requires feature epoch %d; this replica supports %d", binding.MinimumFeatureEpoch, supported)
		}
		return nil
	})
}
