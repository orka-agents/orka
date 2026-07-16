package cliwrapper

import (
	"slices"
	"testing"
)

func TestMultiAdapterRoutesOpencode(t *testing.T) {
	adapter := NewMultiAdapter(DefaultConfig())
	selected, runtime, err := adapter.adapterFor(TurnContext{Metadata: map[string]string{"runtime": RuntimeOpencode}})
	if err != nil {
		t.Fatalf("adapterFor() error = %v", err)
	}
	if runtime != RuntimeOpencode {
		t.Fatalf("runtime = %q, want %q", runtime, RuntimeOpencode)
	}
	if selected.Name() != RuntimeOpencode {
		t.Fatalf("adapter name = %q, want %q", selected.Name(), RuntimeOpencode)
	}
	if !slices.Contains(adapter.SupportedRuntimes(), RuntimeOpencode) {
		t.Fatalf("SupportedRuntimes() = %#v, want %q", adapter.SupportedRuntimes(), RuntimeOpencode)
	}
}
