//go:build windows

package common

import (
	"fmt"
	"os"
	"syscall"
)

func openNoFollow(path string) (*os.File, error) {
	utf16Path, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		utf16Path,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openAtNoFollow(_ *os.File, _ string) (*os.File, error) {
	return nil, fmt.Errorf("handle-relative artifact opens are not supported on windows")
}

func isNoFollowSkippable(err error) bool {
	return os.IsNotExist(err)
}
