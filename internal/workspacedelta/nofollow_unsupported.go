//go:build !darwin && !linux

package workspacedelta

import "os"

type fileIdentity struct{}

func identityAndLinks(os.FileInfo) (fileIdentity, uint64, error) {
	return fileIdentity{}, 0, ErrUnsupportedFilesystem
}

func openRegularNoFollow(string) (*os.File, error) {
	return nil, ErrUnsupportedFilesystem
}
