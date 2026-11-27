//go:build !unix

package acp

import (
	"errors"
	"os"
)

var errWorkspaceIdentityUnsupported = errors.New("session workspace identity pinning is only supported on Unix platforms")

func directoryIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, errWorkspaceIdentityUnsupported
}

func openWorkspaceDirectory(string) (*os.File, error) {
	return nil, errWorkspaceIdentityUnsupported
}
