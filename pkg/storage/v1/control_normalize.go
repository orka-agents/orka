package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// ConflictErrorf returns an error wrapping ErrConflict with a formatted detail.
func ConflictErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

// NotReadyErrorf returns an error wrapping ErrNotReady with a formatted detail.
func NotReadyErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotReady, fmt.Sprintf(format, args...))
}

// CanonicalBytesDigest returns the canonical "sha256:<hex>" digest of value.
func CanonicalBytesDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NormalizeControllerEpochName trims and validates a controller epoch name, defaulting to DefaultControllerEpochName.
func NormalizeControllerEpochName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultControllerEpochName
	}
	if err := ValidateControlIdentifier("controller epoch name", name); err != nil {
		return "", err
	}
	if name != DefaultControllerEpochName {
		return "", ValidationErrorf("first-release control store supports only controller epoch %q", DefaultControllerEpochName)
	}
	return name, nil
}

// NormalizeEpochFence trims and validates a controller epoch fence.
func NormalizeEpochFence(fence ControllerEpochFence) (ControllerEpochFence, error) {
	name, err := NormalizeControllerEpochName(fence.Name)
	if err != nil {
		return ControllerEpochFence{}, err
	}
	fence.Name = name
	fence.HolderID = strings.TrimSpace(fence.HolderID)
	if err := ValidateControlIdentifier("controller epoch holder ID", fence.HolderID); err != nil {
		return ControllerEpochFence{}, err
	}
	if fence.Epoch < 1 {
		return ControllerEpochFence{}, ValidationErrorf("controller epoch must be at least 1")
	}
	return fence, nil
}
