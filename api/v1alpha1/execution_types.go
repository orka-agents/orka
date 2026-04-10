/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import corev1 "k8s.io/api/core/v1"

// ExecutionSpec defines worker pod runtime and placement controls.
type ExecutionSpec struct {
	// RuntimeClassName routes worker pods through a specific RuntimeClass.
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// NodeSelector constrains worker pods to nodes with matching labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allows worker pods to schedule onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity defines Kubernetes affinity and anti-affinity rules for worker pods.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}
