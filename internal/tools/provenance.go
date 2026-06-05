/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/taskmeta"
)

func inheritTaskProvenance(child, parent *corev1alpha1.Task) {
	if child == nil || parent == nil {
		return
	}
	if parent.Spec.RequestedBy != nil {
		child.Spec.RequestedBy = parent.Spec.RequestedBy.DeepCopy()
	}
	if parent.Spec.Transaction != nil {
		child.Spec.Transaction = parent.Spec.Transaction.DeepCopy()
		taskmeta.ApplyTransactionMetadata(&child.ObjectMeta, child.Spec.Transaction)
	}
}
