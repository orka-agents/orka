/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"slices"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orka-agents/orka/internal/connectors"
)

func TestRegisterBrokeredGitHubToolsMatchesTheConnectorCatalog(t *testing.T) {
	registry := NewRegistry()
	k8sClient := fake.NewClientBuilder().Build()
	if err := RegisterBrokeredGitHubTools(registry, k8sClient); err != nil {
		t.Fatal(err)
	}
	if err := RegisterBrokeredGitHubTools(registry, k8sClient); err != nil {
		t.Fatalf("second registration: %v", err)
	}
	names := registry.Names()
	slices.Sort(names)
	if want := connectors.BuiltinConnectorToolNames(); !slices.Equal(names, want) {
		t.Fatalf("registered = %v, want the catalog %v", names, want)
	}
	if err := RegisterBrokeredGitHubTools(nil, k8sClient); err == nil {
		t.Fatal("nil registry must be refused")
	}
	if err := RegisterBrokeredGitHubTools(NewRegistry(), nil); err == nil {
		t.Fatal("nil client must be refused")
	}
}
