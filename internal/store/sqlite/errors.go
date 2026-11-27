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
