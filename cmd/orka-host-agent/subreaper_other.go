//go:build !linux

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

// becomeSubreaper is a no-op where child subreapers do not exist; there only
// PID 1 adopts orphans.
func becomeSubreaper() error { return nil }
