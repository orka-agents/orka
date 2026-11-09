//go:build !linux

package supervisor

import (
	"io/fs"
	"os"
	"path/filepath"
)

// syncMaterializedTree fsyncs every staged regular file and directory where
// syncfs(2) is unavailable.
func syncMaterializedTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
}
