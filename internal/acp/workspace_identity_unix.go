//go:build unix

package acp

import (
	"fmt"
	"os"
	"syscall"
)

// directoryIdentity returns the device and inode behind info.
func directoryIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return 0, 0, fmt.Errorf("session workspace identity is unavailable")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil //nolint:unconvert // Dev and Ino widths differ across Unix platforms.
}
