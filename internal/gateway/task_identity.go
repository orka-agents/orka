/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package gateway

import (
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
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
