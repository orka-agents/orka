/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestSubstratePoolActorLeaseTaskHolderKeepsToolAnnotations(t *testing.T) {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "actor-00000",
			Annotations: map[string]string{
				substratePoolActorLeaseToolNSAnno:   "tool-ns",
				substratePoolActorLeaseToolNameAnno: "tool-name",
				substratePoolActorLeaseToolUIDAnno:  "tool-uid",
			},
		},
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "task", UID: types.UID("task-uid")}}

	setSubstratePoolActorLeaseHolder(lease, task, "actor-00000")

	if lease.Labels[labels.LabelPurpose] != substratePoolActorLeasePurpose {
		t.Fatalf("purpose label = %q, want %q", lease.Labels[labels.LabelPurpose], substratePoolActorLeasePurpose)
	}
	if !substratePoolActorLeaseHeldByTask(lease, task) {
		t.Fatalf("lease should be held by task")
	}
	if lease.Annotations[substratePoolActorLeaseToolNSAnno] != "tool-ns" ||
		lease.Annotations[substratePoolActorLeaseToolNameAnno] != "tool-name" ||
		lease.Annotations[substratePoolActorLeaseToolUIDAnno] != "tool-uid" {
		t.Fatalf("task holder update should preserve existing tool annotations, got %#v", lease.Annotations)
	}
}

func TestSubstrateMCPPoolActorLeaseToolHolderClearsTaskAnnotations(t *testing.T) {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "actor-00000",
			Annotations: map[string]string{
				substratePoolActorLeaseTaskNSAnno:   "task-ns",
				substratePoolActorLeaseTaskNameAnno: "task-name",
				substratePoolActorLeaseTaskUIDAnno:  "task-uid",
			},
		},
	}
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "tool", UID: types.UID("tool-uid")}}

	setSubstrateMCPPoolActorLeaseHolder(lease, tool, "actor-00000")

	if lease.Labels[labels.LabelPurpose] != substratePoolActorLeasePurpose {
		t.Fatalf("purpose label = %q, want %q", lease.Labels[labels.LabelPurpose], substratePoolActorLeasePurpose)
	}
	if !substratePoolActorLeaseHeldByTool(lease, tool) {
		t.Fatalf("lease should be held by tool")
	}
	if _, ok := lease.Annotations[substratePoolActorLeaseTaskNSAnno]; ok {
		t.Fatalf("tool holder update should clear task namespace annotation")
	}
	if _, ok := lease.Annotations[substratePoolActorLeaseTaskNameAnno]; ok {
		t.Fatalf("tool holder update should clear task name annotation")
	}
	if _, ok := lease.Annotations[substratePoolActorLeaseTaskUIDAnno]; ok {
		t.Fatalf("tool holder update should clear task uid annotation")
	}
}

func TestSubstratePoolActorLeaseIdentityHelpers(t *testing.T) {
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "fallback-actor"}}
	if got := substratePoolActorLeaseActorID(lease); got != "fallback-actor" {
		t.Fatalf("actor id fallback = %q, want fallback-actor", got)
	}

	lease.Labels = map[string]string{substratePoolActorLeaseActorIDLabel: " labeled-actor "}
	if got := substratePoolActorLeaseActorID(lease); got != "labeled-actor" {
		t.Fatalf("actor id label = %q, want labeled-actor", got)
	}

	prefix, ordinal, ok := substratePoolActorPrefixAndOrdinal("pool-prefix-00042")
	if !ok || prefix != "pool-prefix" || ordinal != 42 {
		t.Fatalf("prefix/ordinal = %q, %d, %t; want pool-prefix, 42, true", prefix, ordinal, ok)
	}
	if _, _, ok := substratePoolActorPrefixAndOrdinal("pool-prefix-42"); ok {
		t.Fatalf("expected non-five-digit ordinal to be rejected")
	}
}
