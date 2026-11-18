package verification

import "errors"

var (
	ErrRunNotFound = errors.New("verification run not found")
	ErrBinding     = errors.New("verification identity mismatch")
	ErrConflict    = errors.New("verification conflicting delivery")
	ErrClosed      = errors.New("verification run is closed")
	ErrIntegrity   = errors.New("verification evidence integrity failure")
	ErrLimit       = errors.New("verification storage limit exceeded")
)
