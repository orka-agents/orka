//go:build !(linux || darwin || freebsd || netbsd || openbsd || windows)

/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

// processStartMark is not recorded where the forward's process cannot be
// identified.
func processStartMark(int) string { return "" }

// portForwardProcessRunning is false: nothing here identifies the process
// holding a cached port, so a cached forward is never reused.
func portForwardProcessRunning(*portForwardCache) bool { return false }
