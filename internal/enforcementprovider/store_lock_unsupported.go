//go:build !unix

package enforcementprovider

import (
	"errors"
	"io/fs"
	"os"
)

func validateStoreDirectory(string) error {
	return errors.New("enforcement file store ownership validation is unsupported on this platform")
}

func acquireStoreDirectoryLock(string) (*os.File, error) {
	return nil, errors.New("enforcement file store directory locking is unsupported on this platform")
}

func verifyLockedDirectoryPath(string, *os.File) error {
	return errors.New("enforcement file store directory verification is unsupported on this platform")
}

func verifyLockedPath(string, *os.File) error {
	return errors.New("enforcement file store lock verification is unsupported on this platform")
}

func acquireStoreLock(string) (*os.File, error) {
	return nil, errors.New("enforcement file store locking is unsupported on this platform")
}
func acquireStoreStateLock(string) (*os.File, error) {
	return nil, errors.New("enforcement file store locking is unsupported on this platform")
}
func lockStoreStateFile(*os.File) error {
	return errors.New("enforcement file store locking is unsupported on this platform")
}
func storeStateFileOwnedByCurrentUser(fs.FileInfo) bool { return false }

func storeStateFileHasSingleLink(fs.FileInfo) bool { return false }
func storeStateFileUnlinked(fs.FileInfo) bool      { return false }
func releaseStoreStateLock(*os.File) error         { return nil }
func releaseStoreStateLocks([]*os.File) error      { return nil }
func releaseStoreLock(*os.File) error              { return nil }
