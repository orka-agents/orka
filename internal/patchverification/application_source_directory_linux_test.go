//go:build linux

package patchverification

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplicationSourceDirectoryAcceptsFreshExt4Metadata(t *testing.T) {
	for _, recoveryDirectory := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty volume", true: "ext4 lost+found"}[recoveryDirectory], func(t *testing.T) {
			volume := t.TempDir()
			if recoveryDirectory {
				require.NoError(t, os.Mkdir(filepath.Join(volume, "lost+found"), 0700))
			}
			require.NoError(t, ValidateApplicationVolumeRoot(volume))
			directory, err := applicationCreateSourceDirectory(volume)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(volume, "archives"), directory)
			info, err := os.Lstat(directory)
			require.NoError(t, err)
			require.True(t, info.IsDir())
			require.Equal(t, os.FileMode(0700), info.Mode().Perm())
			require.NoError(t, applicationEmptyDirectory(directory))
			if recoveryDirectory {
				info, err = os.Lstat(filepath.Join(volume, "lost+found"))
				require.NoError(t, err)
				require.True(t, info.IsDir(), "filesystem metadata must be left untouched")
			}
			_, err = applicationCreateSourceDirectory(volume)
			require.Error(t, err, "even an empty owned child cannot be adopted by another preparation")
		})
	}
}

func TestApplicationSourceDirectoryRejectsStaleForeignAndSymlinkedEntries(t *testing.T) {
	for name, prepare := range map[string]func(*testing.T, string){
		"stale empty child": func(t *testing.T, root string) {
			require.NoError(t, os.Mkdir(filepath.Join(root, "archives"), 0700))
		},
		"stale archive": func(t *testing.T, root string) {
			require.NoError(t, os.Mkdir(filepath.Join(root, "archives"), 0700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "archives", "original.tar"), []byte("stale"), 0600))
		},
		"foreign root file": func(t *testing.T, root string) {
			require.NoError(t, os.WriteFile(filepath.Join(root, "foreign"), []byte("do not remove"), 0600))
		},
		"stale artifact binary": func(t *testing.T, root string) {
			require.NoError(t, os.WriteFile(filepath.Join(root, "application"), []byte("previous binary"), 0500))
		},
		"foreign root directory": func(t *testing.T, root string) {
			require.NoError(t, os.Mkdir(filepath.Join(root, "foreign"), 0700))
		},
		"archive symlink": func(t *testing.T, root string) {
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "archives")))
		},
		"recovery symlink": func(t *testing.T, root string) {
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "lost+found")))
		},
		"recovery regular file": func(t *testing.T, root string) {
			require.NoError(t, os.WriteFile(filepath.Join(root, "lost+found"), []byte("not a directory"), 0600))
		},
	} {
		t.Run(name, func(t *testing.T) {
			volume := t.TempDir()
			prepare(t, volume)
			before, err := os.ReadDir(volume)
			require.NoError(t, err)
			require.Error(t, ValidateApplicationVolumeRoot(volume))
			_, err = applicationCreateSourceDirectory(volume)
			require.Error(t, err)
			after, err := os.ReadDir(volume)
			require.NoError(t, err)
			require.Len(t, after, len(before), "rejection must not clean up or adopt foreign content")
		})
	}
	volume := t.TempDir()
	link := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.Symlink(volume, link))
	_, err := applicationCreateSourceDirectory(link)
	require.Error(t, err)
	require.NoError(t, applicationEmptyDirectory(volume))
}

func TestApplicationSourceSidePathsStayInsideThePrivateDirectory(t *testing.T) {
	require.Equal(t, "/source", ApplicationSourceVolumePath)
	require.Equal(t, "/source/source.tar", ApplicationSourcePath, "the build worker's fixed input path does not change")
	require.Equal(t, "archives/original.tar", ApplicationOriginalSourceSubPath)
	require.Equal(t, "archives/patched.tar", ApplicationPatchedSourceSubPath)
	volume := t.TempDir()
	directory, err := applicationCreateSourceDirectory(volume)
	require.NoError(t, err)
	for _, name := range []string{ApplicationOriginalSourceName, ApplicationPatchedSourceName} {
		require.Equal(t, "archives", filepath.Dir(filepath.Join(ApplicationSourceDirectory, name)))
		require.Equal(t, filepath.Join(directory, name), filepath.Join(volume, ApplicationSourceDirectory, name))
	}
	require.NoError(t, os.WriteFile(filepath.Join(directory, "partial"), []byte("interrupted"), 0600))
	require.Error(t, applicationEmptyDirectory(directory))
	_, err = applicationCreateSourceDirectory(volume)
	require.Error(t, err)
}
