package sqlite

import storagev1 "github.com/orka-agents/orka/pkg/storage/v1"

// The embedded SQLite store is the in-process provider for both storage
// contracts.
var (
	_ storagev1.StateProvider    = (*Store)(nil)
	_ storagev1.ArtifactProvider = (*Store)(nil)
)
