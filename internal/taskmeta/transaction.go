/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskmeta

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

// ApplyTransactionMetadata copies safe transaction metadata to a Kubernetes object's
// labels and annotations so Tasks, Jobs, and Pods can be correlated by transaction.
func ApplyTransactionMetadata(meta *metav1.ObjectMeta, tx *corev1alpha1.TaskTransaction) {
	if meta == nil || tx == nil {
		return
	}
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}

	setLabel(meta.Labels, labels.LabelTransactionID, tx.ID)
	setLabel(meta.Labels, labels.LabelAuthProfile, tx.Profile)

	setAnnotation(meta.Annotations, labels.AnnotationTransactionID, tx.ID)
	setAnnotation(meta.Annotations, labels.AnnotationContextTokenProfile, tx.Profile)
	setAnnotation(meta.Annotations, labels.AnnotationTransactionIssuer, tx.Issuer)
	setAnnotation(meta.Annotations, labels.AnnotationTransactionSubject, tx.Subject)
	setAnnotation(meta.Annotations, labels.AnnotationTransactionRequestingWorkload, tx.RequestingWorkload)
	setAnnotation(meta.Annotations, labels.AnnotationTransactionScope, tx.Scope)
	setAnnotation(meta.Annotations, labels.AnnotationTransactionContextDigest, tx.ContextDigest)
	setAnnotation(meta.Annotations, labels.AnnotationRequesterContextDigest, tx.RequesterContextDigest)
}

func setLabel(out map[string]string, name, value string) {
	if value == "" {
		return
	}
	out[name] = labels.SelectorValue(value)
}

func setAnnotation(out map[string]string, name, value string) {
	if value == "" {
		return
	}
	out[name] = value
}
