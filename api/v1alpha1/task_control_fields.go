/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

// TaskControlFields lists, by JSON name, the TaskSpec fields that steer an
// already bound Task, such as pausing an autonomous loop. They are not
// execution inputs, so a soul binding leaves them mutable. Keep this list,
// ExecutionInputs, and the Task CEL immutability rule in sync; a test checks
// all three.
var TaskControlFields = []string{"suspend"}

// ExecutionInputs returns a copy of the spec without its control fields: the
// inputs a soul binding freezes. Every other field, including the difference
// between nil and empty values, is preserved.
func (s *TaskSpec) ExecutionInputs() TaskSpec {
	inputs := *s.DeepCopy()
	inputs.Suspend = nil
	return inputs
}
