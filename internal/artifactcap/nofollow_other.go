//go:build !darwin && !linux

package artifactcap

import (
	"os"
)

func openFileNoFollow(path string, flags int, perm os.FileMode) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafePath
	}
	return os.OpenFile(path, flags, perm)
}
