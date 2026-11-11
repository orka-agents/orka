package patchverification

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkerFrozenChecksRejectMutatedInventory(t *testing.T) {
	for _, scenario := range []string{"executable changed", "mode changed", "data changed", "missing", "extra file", "extra directory", "symlink", "FIFO"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			files := []FrozenFile{{Path: "run", Content: []byte("#!/bin/sh\nexit 0\n"), Executable: true},
				{Path: "data", Content: []byte("frozen input")}}
			for index := range files {
				files[index].Digest = Digest(files[index].Content)
				mode := os.FileMode(0444)
				if files[index].Executable {
					mode = 0555
				}
				require.NoError(t, os.WriteFile(filepath.Join(directory, files[index].Path), files[index].Content, mode))
			}
			inventory, err := validateWorkerCheckFiles(files, directory)
			require.NoError(t, err)
			manifest := Manifest{Files: files, Checks: []Check{{Command: []string{"/checks/run"}}}}
			require.NoError(t, validateWorkerCommands(manifest, inventory, directory))
			switch scenario {
			case "executable changed":
				require.NoError(t, os.Chmod(filepath.Join(directory, "run"), 0700))
				require.NoError(t, os.WriteFile(filepath.Join(directory, "run"), []byte("forged"), 0700))
			case "mode changed":
				require.NoError(t, os.Chmod(filepath.Join(directory, "run"), 0444))
			case "data changed":
				require.NoError(t, os.Chmod(filepath.Join(directory, "data"), 0600))
				require.NoError(t, os.WriteFile(filepath.Join(directory, "data"), []byte("forged input"), 0600))
			case "missing":
				require.NoError(t, os.Remove(filepath.Join(directory, "data")))
			case "extra file":
				require.NoError(t, os.WriteFile(filepath.Join(directory, "extra"), []byte("extra"), 0600))
			case "extra directory":
				require.NoError(t, os.Mkdir(filepath.Join(directory, "extra"), 0700))
			case "symlink":
				require.NoError(t, os.Remove(filepath.Join(directory, "data")))
				require.NoError(t, os.Symlink("run", filepath.Join(directory, "data")))
			case "FIFO":
				require.NoError(t, syscall.Mkfifo(filepath.Join(directory, "extra"), 0600))
			}
			_, err = validateWorkerCheckFiles(files, directory)
			require.Error(t, err)
		})
	}
}
