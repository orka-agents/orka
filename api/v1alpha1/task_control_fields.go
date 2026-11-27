/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

var TaskControlFields = []string{"suspend"}

func (s *TaskSpec) ExecutionInputs() TaskSpec {
	inputs := *s.DeepCopy()
	inputs.Suspend = nil
	return inputs
}
