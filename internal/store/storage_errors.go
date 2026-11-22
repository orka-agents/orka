package store

import "errors"

func IsTemporaryStorageError(err error) bool {
	return errors.Is(err, ErrNotReady) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrOutcomeUnknown)
}
