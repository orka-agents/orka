/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import storagev1 "github.com/orka-agents/orka/pkg/storage/v1"

// The GitHub sign-in session record and store are part of the public storage
// contract; these aliases keep their internal names for Orka callers.
type (
	AuthSession      = storagev1.AuthSession
	AuthSessionStore = storagev1.AuthSessionStore
)
