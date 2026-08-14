package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// These helpers intentionally mirror internal/memorybackend's durable intent
// digest. Keeping the controller-side verification local avoids an import cycle.
func protectedMemoryBackendLifecycle(lifecycle corev1alpha1.MemoryBackendLifecycleState) bool {
	switch lifecycle {
	case corev1alpha1.MemoryBackendLifecycleActive,
		corev1alpha1.MemoryBackendLifecycleReadOnly,
		corev1alpha1.MemoryBackendLifecycleDisabled,
		corev1alpha1.MemoryBackendLifecycleDecommissioning:
		return true
	default:
		return false
	}
}

func memoryBackendLifecycleIntentDigest(
	backendUID string,
	generation int64,
	target corev1alpha1.MemoryBackendLifecycleState,
	specDigest string,
) string {
	parts := []string{
		"orka.memorybackend.lifecycle-intent.v1",
		strings.TrimSpace(backendUID),
		fmt.Sprintf("%d", generation),
		string(target),
		strings.TrimSpace(specDigest),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
