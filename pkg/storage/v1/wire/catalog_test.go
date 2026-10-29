package wire

import (
	"testing"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Every RPC agrees with the contract's method catalog: a catalog read is a
// wire read, and a catalog write or administrative method is a wire write,
// whose transport failure after it may have been sent is an unknown outcome.
// Only catalog writes carry an envelope, and each RPC belongs to the contract
// part the catalog names.
func TestWireMethodsMatchTheMethodCatalog(t *testing.T) {
	require.NotEmpty(t, methodsByFullName)
	for _, m := range methodsByFullName {
		info, ok := storagev1.LookupMethod(m.name)
		require.True(t, ok, "%s is in the method catalog", m.name)
		switch info.Kind {
		case storagev1.MethodRead:
			require.Equal(t, KindRead, m.kind, "%s is a catalog read", m.name)
		case storagev1.MethodWrite, storagev1.MethodAdmin:
			require.Equal(t, KindWrite, m.kind, "%s may change stored state", m.name)
		default:
			t.Fatalf("%s has catalog kind %s", m.name, info.Kind)
		}
		require.Equal(t, info.Kind == storagev1.MethodWrite, m.takesEnvelope(), "%s carries an envelope only as a write", m.name)
		require.Equal(t, info.Contract, m.contract, "%s belongs to the catalog's contract part", m.name)
		switch {
		case m.name == "Describe" || m.name == "HealthCheck":
			require.Equal(t, ClassHealth, m.class(), m.name)
		case m.stream != streamNone:
			require.Equal(t, ClassBulk, m.class(), m.name)
		default:
			require.Equal(t, ClassControl, m.class(), m.name)
		}
		require.Equal(t, m.stream == streamSnapshot, m.batched != "", "%s names a batched list exactly when it is a batched read", m.name)
	}
}
