/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package executionmode

import (
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const NamespaceLabel = "orka.ai/controller-mode"

type Mode string

const (
	HarnessV2 Mode = "harness-v2"
)

func Parse(raw string) (Mode, error) {
	mode := Mode(strings.TrimSpace(raw))
	switch mode {
	case HarnessV2:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported execution mode %q; only %q is supported", raw, HarnessV2)
	}
}

func (m Mode) ContractVersion() corev1alpha1.AgentRuntimeContractVersion {
	if m == HarnessV2 {
		return corev1alpha1.AgentRuntimeContractHarnessV2
	}
	return ""
}

func DefaultBuiltInAgentContract(agent *corev1alpha1.Agent, mode Mode) error {
	if agent == nil || agent.Spec.Runtime == nil || agent.Spec.Runtime.Type == "" || agent.Spec.Runtime.RuntimeRef != nil {
		return nil
	}

	expected := mode.ContractVersion()
	if expected == "" {
		return fmt.Errorf("cannot default built-in Agent contract without a valid execution mode")
	}
	if agent.Spec.Runtime.ContractVersion == nil {
		agent.Spec.Runtime.ContractVersion = &expected
		return nil
	}
	if *agent.Spec.Runtime.ContractVersion != expected {
		return fmt.Errorf(
			"agent runtime contractVersion %q does not match execution mode %q",
			*agent.Spec.Runtime.ContractVersion,
			mode,
		)
	}
	return nil
}

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
