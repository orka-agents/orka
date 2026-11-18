/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import storagev1 "github.com/orka-agents/orka/pkg/storage/v1"

// The Agent Host Protocol records and store are part of the public storage
// contract; these aliases keep their internal names for Orka callers.
type (
	AHPToken         = storagev1.AHPToken
	AHPClient        = storagev1.AHPClient
	AHPJournalEntry  = storagev1.AHPJournalEntry
	AHPChannelState  = storagev1.AHPChannelState
	AHPSession       = storagev1.AHPSession
	AHPRecord        = storagev1.AHPRecord
	AHPJournalLimits = storagev1.AHPJournalLimits
	AHPStore         = storagev1.AHPStore
)
