/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const gatewayRequestedByIssuerPrefix = "gateway.orka.ai/"

// TaskOwnerIdentity is the immutable Gateway ownership recorded on a Task.
type TaskOwnerIdentity struct {
	GatewayNamespace string
	NamespaceUID     string
	GatewayName      string
	GatewayUID       string
}

// TaskOwner reports immutable Gateway ownership. RequestedBy is immutable and
// therefore authoritative; metadata markers preserve compatibility for older Tasks.
func TaskOwner(task *corev1alpha1.Task) (TaskOwnerIdentity, bool) {
	if task == nil {
		return TaskOwnerIdentity{}, false
	}
	if task.Spec.RequestedBy != nil {
		issuer := strings.TrimSpace(task.Spec.RequestedBy.Issuer)
		if strings.HasPrefix(issuer, gatewayRequestedByIssuerPrefix) {
			parts := strings.Split(issuer, "/")
			identity := TaskOwnerIdentity{}
			switch {
			case len(parts) == 5 && parts[0] == "gateway.orka.ai" && strings.TrimSpace(parts[1]) != "":
				identity.GatewayNamespace = strings.TrimSpace(parts[1])
				identity.NamespaceUID = strings.TrimSpace(parts[2])
				identity.GatewayName = strings.TrimSpace(parts[3])
				identity.GatewayUID = strings.TrimSpace(parts[4])
			case len(parts) == 4 && parts[0] == "gateway.orka.ai" && strings.TrimSpace(parts[1]) != "":
				// Compatibility with pre-identity Tasks. The missing Namespace UID keeps
				// public authorization fail-closed unless the retained event supplies it.
				identity.GatewayNamespace = strings.TrimSpace(parts[1])
				identity.GatewayName = strings.TrimSpace(parts[2])
				identity.GatewayUID = strings.TrimSpace(parts[3])
			}
			return identity, true
		}
	}
	annotations := task.GetAnnotations()
	labels := task.GetLabels()
	identity := TaskOwnerIdentity{GatewayNamespace: task.Namespace, GatewayName: strings.TrimSpace(annotations[TaskGatewayNameAnnotation])}
	eventID := strings.TrimSpace(annotations[TaskGatewayEventAnnotation])
	if eventID == "" {
		eventID = strings.TrimSpace(labels[TaskGatewayEventLabel])
	}
	return identity, identity.GatewayName != "" || eventID != ""
}

// TaskIdentity reports only the Gateway name for callers that need classification.
func TaskIdentity(task *corev1alpha1.Task) (string, bool) {
	identity, owned := TaskOwner(task)
	return identity.GatewayName, owned
}

// ErrTaskOwnerMismatch reports that a Task's recorded Gateway ownership
// contradicts the retained GatewayEvent that created it.
var ErrTaskOwnerMismatch = errors.New("gateway task ownership does not match the retained gateway event")

// ResolveTaskOwner reports a Task's Gateway ownership, preferring the retained
// GatewayEvent linked to the Task over its metadata, so ownership survives
// removed or never-written metadata markers. A nil events store falls back to
// TaskOwner. A store failure or ErrTaskOwnerMismatch means ownership could not
// be verified; callers must fail closed.
func ResolveTaskOwner(ctx context.Context, events store.GatewayEventStore, task *corev1alpha1.Task) (TaskOwnerIdentity, bool, error) {
	if task == nil {
		return TaskOwnerIdentity{}, false, nil
	}
	annotations := task.GetAnnotations()
	labels := task.GetLabels()
	eventID := strings.TrimSpace(annotations[TaskGatewayEventAnnotation])
	if eventID == "" {
		eventID = strings.TrimSpace(labels[TaskGatewayEventLabel])
	}
	identity, gatewayOwned := TaskOwner(task)

	if events != nil {
		var durable *TaskOwnerIdentity
		if task.UID != "" {
			event, err := events.GetGatewayEventForTask(ctx, task.Namespace, task.Name, string(task.UID))
			if err == nil {
				durable = &TaskOwnerIdentity{
					GatewayNamespace: event.Namespace, NamespaceUID: event.NamespaceUID,
					GatewayName: event.GatewayName, GatewayUID: event.GatewayUID,
				}
			} else if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrValidation) {
				return TaskOwnerIdentity{}, gatewayOwned, fmt.Errorf("verify gateway task ownership: %w", err)
			}
		}
		if durable == nil && eventID != "" {
			event, err := events.GetGatewayEvent(ctx, task.Namespace, eventID)
			if err == nil && event.TaskName == task.Name && (event.TaskUID == "" || event.TaskUID == string(task.UID)) {
				durable = &TaskOwnerIdentity{
					GatewayNamespace: event.Namespace, NamespaceUID: event.NamespaceUID,
					GatewayName: event.GatewayName, GatewayUID: event.GatewayUID,
				}
			} else if err != nil && !errors.Is(err, store.ErrNotFound) {
				return TaskOwnerIdentity{}, true, fmt.Errorf("verify gateway task ownership: %w", err)
			}
		}
		if durable != nil {
			if gatewayOwned && taskOwnerIdentityComplete(identity) && identity != *durable {
				return TaskOwnerIdentity{}, true, ErrTaskOwnerMismatch
			}
			return *durable, true, nil
		}
	}
	return identity, gatewayOwned, nil
}

func taskOwnerIdentityComplete(identity TaskOwnerIdentity) bool {
	return strings.TrimSpace(identity.GatewayNamespace) != "" && strings.TrimSpace(identity.NamespaceUID) != "" &&
		strings.TrimSpace(identity.GatewayName) != "" && strings.TrimSpace(identity.GatewayUID) != ""
}
