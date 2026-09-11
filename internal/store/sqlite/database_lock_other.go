//go:build !darwin && !linux

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"fmt"
	"io"
	"runtime"
)

func lockDatabaseFile(string) (io.Closer, error) {
	return nil, fmt.Errorf("process-lifetime SQLite locking is unsupported on %s", runtime.GOOS)
}
