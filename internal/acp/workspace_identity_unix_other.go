//go:build unix && !linux

package acp

import (
	"os"
	"syscall"
)

// openWorkspaceDirectory returns a descriptor that holds the directory at path
// without following a symbolic link.
func openWorkspaceDirectory(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
}
