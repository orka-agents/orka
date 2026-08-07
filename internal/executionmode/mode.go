/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package executionmode defines the immutable execution-plane identity shared
// by controllers and admission. A namespace belongs to exactly one mode.
package executionmode

import (
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// NamespaceLabel is the immutable namespace execution-mode claim.
const NamespaceLabel = "orka.ai/controller-mode"

// Mode identifies the only execution contract an installation may serve.
type Mode string

const (
	HarnessV1 Mode = "harness-v1"
	HarnessV2 Mode = "harness-v2"
)

// Parse returns a supported, canonical mode.
func Parse(raw string) (Mode, error) {
	mode := Mode(strings.TrimSpace(raw))
	switch mode {
	case HarnessV1, HarnessV2:
		return mode, nil
	default:
		return "", fmt.Errorf("execution mode must be %q or %q, got %q", HarnessV1, HarnessV2, raw)
	}
}

// ContractVersion returns the only harness contract admitted by the mode.
func (m Mode) ContractVersion() corev1alpha1.AgentRuntimeContractVersion {
	if m == HarnessV1 {
		return corev1alpha1.AgentRuntimeContractHarnessV1
	}
	if m == HarnessV2 {
		return corev1alpha1.AgentRuntimeContractHarnessV2
	}
	return ""
}

// FromNamespace reads and validates a namespace's immutable mode claim.
func FromNamespace(namespace *corev1.Namespace) (Mode, error) {
	if namespace == nil {
		return "", fmt.Errorf("execution-mode namespace is required")
	}
	raw := ""
	if namespace.Labels != nil {
		raw = namespace.Labels[NamespaceLabel]
	}
	mode, err := Parse(raw)
	if err != nil {
		return "", fmt.Errorf("namespace %q: %w", namespace.Name, err)
	}
	return mode, nil
}

// ValidateNamespace verifies the namespace is permanently claimed by mode.
func ValidateNamespace(namespace *corev1.Namespace, mode Mode) error {
	claimed, err := FromNamespace(namespace)
	if err != nil {
		return err
	}
	if claimed != mode {
		return fmt.Errorf("namespace %q is claimed by execution mode %q, not %q", namespace.Name, claimed, mode)
	}
	return nil
}
