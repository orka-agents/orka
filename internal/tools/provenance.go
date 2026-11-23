/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/taskmeta"
)

// inheritTaskProvenance gives a Task a tool creates for its parent the parent's
// requester and transaction. A child of an Agent Host Protocol turn is also
// marked a turn: it carries the turn's work, so the API shows it, as it shows
// the turn, only to the person who sent the turn.
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
	if parent.Labels[labels.LabelAHPTurn] == labels.LabelAHPTurnValue {
		if child.Labels == nil {
			child.Labels = map[string]string{}
		}
		child.Labels[labels.LabelAHPTurn] = labels.LabelAHPTurnValue
	}
}
