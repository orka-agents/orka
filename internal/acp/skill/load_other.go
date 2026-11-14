//go:build !linux

package skill

func loadTree(_, name string, _ Limits) (*Tree, error) {
	return nil, failf(ReasonUnsupported, "skill %s: skills are supported only in Linux runtime Pods", name)
}

// WriteTo is only implemented for Linux runtime Pods.
func (t *Tree) WriteTo(string) error {
	return failf(ReasonUnsupported, "skill %s: skills are supported only in Linux runtime Pods", t.Name)
}
