//go:build !(linux || darwin || freebsd || netbsd || openbsd || windows)

/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

func processStartMark(int) string { return "" }

func portForwardProcessRunning(*portForwardCache) bool { return false }
