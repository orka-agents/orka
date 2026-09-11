//go:build !darwin && !linux

package supervisor

import (
	"fmt"
	"os"
)

func openMaterializedFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("materialized workspace target is a symlink")
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
}
