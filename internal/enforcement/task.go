package enforcement

import (
	"errors"
	"slices"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TaskRequirement(task *corev1alpha1.Task) (*enforcementv1.Requirement, error) {
	if task == nil || task.Spec.Enforcement == nil {
		return nil, nil
	}
	input := task.Spec.Enforcement
	if input.Mode != "enforce" {
		return nil, errors.New("enforcement mode must be explicitly enforce")
	}
	requirement := &enforcementv1.Requirement{
		Format: input.Format, Policy: slices.Clone(input.Policy),
		RequiredCapabilities: slices.Clone(input.RequiredCapabilities),
		Mode:                 enforcementv1.Requirement_MODE_ENFORCE,
	}
	if err := ValidateRequirement(requirement); err != nil {
		return nil, err
	}
	return requirement, nil
}
