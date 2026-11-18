/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"fmt"

	"github.com/orka-agents/orka/internal/store"
	sqlite3 "modernc.org/sqlite/lib"
)

// uniqueConflict reports a unique or primary-key violation as
// store.ErrConflict. It keeps SQLite's message and error in the chain, so
// callers that inspect either keep working. Other errors pass through.
func uniqueConflict(err error) error {
	if err == nil {
		return nil
	}
	code, ok := sqliteErrorCode(err)
	if !ok || (code != sqlite3.SQLITE_CONSTRAINT_UNIQUE && code != sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
		return err
	}
	return fmt.Errorf("%w: %w", store.ErrConflict, err)
}
