package memorybackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/store"
)

const (
	memoryBackendLifecycleIntentAuditAction    = "backend.lifecycle.intent"
	memoryBackendLifecycleRequestedAuditAction = "backend.lifecycle.requested"
	memoryBackendRoutingFenceAckAuditAction    = "backend.routing_fence.acknowledged"
)

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

func canonicalMemoryBackendSpecDigest(spec corev1alpha1.MemoryBackendSpec) (string, error) {
	spec.LifecycleState = spec.RequestedLifecycle()
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
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

func lifecycleIntentDigestForValidation(snapshot controller.MemoryBackendValidationSnapshot) string {
	return memoryBackendLifecycleIntentDigest(
		snapshot.BackendUID,
		snapshot.BackendGeneration,
		snapshot.RequestedLifecycle,
		snapshot.SpecDigest,
	)
}

func lifecycleIntentDigestForBinding(snapshot controller.MemoryBackendBindingSnapshot) string {
	return memoryBackendLifecycleIntentDigest(
		snapshot.BackendUID,
		snapshot.BackendGeneration,
		snapshot.RequestedLifecycle,
		snapshot.SpecDigest,
	)
}

func (c *StoreCoordinator) requireLifecycleIntent(
	ctx context.Context,
	namespaceUID string,
	backendUID string,
	generation int64,
	target corev1alpha1.MemoryBackendLifecycleState,
	specDigest string,
	presentedDigest string,
) (string, error) {
	if !protectedMemoryBackendLifecycle(target) {
		return "", nil
	}
	if strings.TrimSpace(namespaceUID) == "" || strings.TrimSpace(backendUID) == "" || generation <= 0 || strings.TrimSpace(specDigest) == "" {
		return "", store.ValidationErrorf("protected MemoryBackend lifecycle requires complete intent identity")
	}
	expected := memoryBackendLifecycleIntentDigest(backendUID, generation, target, specDigest)
	if presentedDigest != expected {
		return "", fmt.Errorf("%w: protected MemoryBackend lifecycle intent does not match backend UID, generation, target, and spec", store.ErrConflict)
	}
	intentFound := false
	requestedFound := false
	if err := c.visitMemoryAudit(ctx, namespaceUID, func(record store.MemoryAuditRecord) bool {
		if record.RequestDigest != expected || record.NewState != string(target) {
			return true
		}
		switch record.Action {
		case memoryBackendLifecycleIntentAuditAction:
			intentFound = true
		case memoryBackendLifecycleRequestedAuditAction:
			requestedFound = true
		}
		return !intentFound || !requestedFound
	}); err != nil {
		return "", err
	}
	if !intentFound || !requestedFound {
		return "", fmt.Errorf("%w: protected MemoryBackend lifecycle lacks a committed durable intent", store.ErrConflict)
	}
	return expected, nil
}
