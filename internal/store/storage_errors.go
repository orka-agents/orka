package store

import "errors"

// IsTemporaryStorageError reports whether a storage write was not accepted
// for now, or may or may not have committed: ErrNotReady (the controller's
// fence is not open yet, an earlier write's outcome is still being resolved,
// or the store's clock is too far from the controller's), ErrUnavailable (nothing committed, including an operation the
// storage client resolved as aborted after only transient failures) or
// ErrOutcomeUnknown (neither success nor failure). A caller retries later,
// as a reconciler requeue or an HTTP 503; it never treats such an error as a
// definite failure of the work, and never reports success.
func IsTemporaryStorageError(err error) bool {
	return errors.Is(err, ErrNotReady) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrOutcomeUnknown)
}
