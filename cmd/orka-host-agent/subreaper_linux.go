/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import "golang.org/x/sys/unix"

// becomeSubreaper makes orphaned descendants reparent to this process.
func becomeSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}
