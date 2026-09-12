package kube

// RuntimeSessionControlObjectName returns the deterministic Kubernetes object
// name for the control record that owns a user-visible Session name. Read-only
// API surfaces use this helper so they cannot drift from the control store's
// identity mapping.
func RuntimeSessionControlObjectName(sessionName string) string {
	return runtimeSessionObjectName(sessionName)
}
