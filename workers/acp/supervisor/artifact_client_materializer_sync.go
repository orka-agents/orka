package supervisor

import (
	"io/fs"
	"os"
	"path/filepath"
)

// syncMaterializedTree fsyncs every staged regular file and directory once
// extraction has written them all. Syncing each file right after writing it
// defeats journal batching and costs minutes for tens of thousands of files on
// network-attached node disks; syncfs(2) would also flush, and report errors
// from, unrelated writes on a shared node filesystem.
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
