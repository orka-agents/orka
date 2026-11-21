package nativesession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func normalizeLimit(maxBytes int) (int, error) {
	return harnessv2.NormalizeNativeSessionMaxBytes(maxBytes)
}

// expandedLimit bounds uncompressed conversation state, as SessionKit's
// budget does for Codex.
func expandedLimit(limit int) int { return 8 * limit }

func dataDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func absoluteCleanPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

// requireRealPath rejects a path with a symlink in any component, or whose
// final component is not a directory (dir) or regular file (!dir). Agent
// session trees are writable by the agent until it stops, so a stopped tree
// must not redirect a privileged reader elsewhere.
func requireRealPath(path string, dir bool) error {
	if !absoluteCleanPath(path) {
		return errors.New("native session paths must be absolute and clean")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("native session path %s contains a symlink", path)
		case current == path && !dir && !info.Mode().IsRegular():
			return fmt.Errorf("native session path %s is not a regular file", path)
		case (current != path || dir) && !info.IsDir():
			return fmt.Errorf("native session path %s is not a directory", path)
		}
		if parent := filepath.Dir(current); parent == current {
			return nil
		}
	}
}

func requireRealPathIfPresent(path string, dir bool) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return requireRealPath(path, dir)
}

func readBoundedFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(content) > limit {
		return nil, fmt.Errorf("%w: native session state exceeds %d bytes", ErrUnsupported, limit)
	}
	return content, nil
}

// writeNewFile creates path, which must not exist, and syncs its content. A
// failed write removes the file.
func writeNewFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(content)
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		_ = os.Remove(path)
	}
	return err
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// holdsFile reports whether path is a regular file, reached without a
// symlink, that holds exactly content.
func holdsFile(path string, content []byte) bool {
	if requireRealPath(path, false) != nil {
		return false
	}
	existing, err := readBoundedFile(path, len(content))
	return err == nil && bytes.Equal(existing, content)
}

// holdsFiles reports whether dir holds every named file with its content.
func holdsFiles(dir string, files map[string][]byte) bool {
	for name, content := range files {
		if !holdsFile(filepath.Join(dir, name), content) {
			return false
		}
	}
	return true
}

// lockInstallDestination holds an exclusive lock on the directory that holds
// state, an install's state directory, until the returned function first
// runs. Two installs into one destination never interleave, so staging that an
// install finds belongs to one that died, and the kernel released its lock.
func lockInstallDestination(ctx context.Context, state string) (func(), error) {
	if !absoluteCleanPath(state) {
		return nil, errors.New("the state directory must be an absolute, clean path")
	}
	parent := filepath.Dir(state)
	if err := requireRealPath(parent, true); err != nil {
		return nil, err
	}
	dir, err := os.OpenFile(parent, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return sync.OnceFunc(func() {
				_ = unix.Flock(int(dir.Fd()), unix.LOCK_UN)
				_ = dir.Close()
			}), nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = dir.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = dir.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// syncDirs syncs each directory in order. An install lists the directories it
// wrote into from the deepest up, so every new entry, including a directory
// it created, is durable before the install reports success.
func syncDirs(paths ...string) error {
	for _, path := range paths {
		if err := syncDir(path); err != nil {
			return err
		}
	}
	return nil
}

// privateTempDir returns a new directory only the current user can enter.
func privateTempDir() (string, error) {
	dir, err := os.MkdirTemp("", "orka-native-session-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
