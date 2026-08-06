/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package harness

import (
	"strings"
)

// RuntimeSessionIdentityInput captures the backend-neutral facts needed to name
// and own a RuntimeSession without depending on Kubernetes Task or Agent types.
type RuntimeSessionIdentityInput struct {
	Namespace   string
	TaskName    string
	TaskUID     string
	SessionName string
	RuntimeName string
	ActiveTask  string
	AgentName   string
	Provider    ProviderKind
}

// RuntimeSessionIdentity is the canonical identity plus owner metadata for a
// RuntimeSession. The ID preserves the existing turn protocol identity shape;
// Owner is the persistence-facing ownership model.
type RuntimeSessionIdentity struct {
	ID          RuntimeSessionID
	Owner       RuntimeSessionOwner
	RuntimeName string
}

// ResolveRuntimeSessionIdentity derives the canonical RuntimeSession ID and owner
// metadata for a turn. Explicit SessionName makes the runtime reusable by that
// session; otherwise the owner session key is task-scoped using taskName:UID
// (falling back to taskName when UID is unavailable).
func ResolveRuntimeSessionIdentity(input RuntimeSessionIdentityInput) RuntimeSessionIdentity {
	namespace := strings.TrimSpace(input.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	runtimeName := strings.TrimSpace(input.RuntimeName)
	sessionName := strings.TrimSpace(input.SessionName)
	taskName := strings.TrimSpace(input.TaskName)
	if sessionName == "" {
		identity := strings.TrimSpace(input.TaskUID)
		if identity == "" {
			identity = taskName
		}
		switch {
		case taskName != "" && identity != "":
			sessionName = taskName + ":" + identity
		case taskName != "":
			sessionName = taskName
		case identity != "":
			sessionName = identity
		default:
			sessionName = "default"
		}
	}
	activeTask := strings.TrimSpace(input.ActiveTask)
	if activeTask == "" {
		activeTask = taskName
	}
	provider := ProviderKind(strings.TrimSpace(string(input.Provider)))
	return RuntimeSessionIdentity{
		ID:          RuntimeSessionID(strings.Join([]string{namespace, sessionName, runtimeName}, ":")),
		RuntimeName: runtimeName,
		Owner: RuntimeSessionOwner{
			Namespace:   namespace,
			SessionName: sessionName,
			ActiveTask:  activeTask,
			AgentName:   strings.TrimSpace(input.AgentName),
			Provider:    provider,
		},
	}
}
