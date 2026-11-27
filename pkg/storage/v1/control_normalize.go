package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

func ConflictErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

func NotReadyErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotReady, fmt.Sprintf(format, args...))
}

func CanonicalBytesDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

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
