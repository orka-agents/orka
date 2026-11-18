/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// A Task a tool creates for an AHP turn carries the turn's work, so it is
// marked a turn and stays as private as the turn; other parents' children are
// not marked.
func TestInheritTaskProvenanceKeepsAHPTurnChildrenPrivate(t *testing.T) {
	ahpParent := parentTask()
	ahpParent.Labels = map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}
	for name, tc := range map[string]struct {
		parent *corev1alpha1.Task
		child  *corev1alpha1.Task
		want   bool
	}{
		"turn child without labels": {ahpParent, &corev1alpha1.Task{}, true},
		"turn child with labels": {ahpParent, &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{labels.LabelParentTask: parentTaskName},
		}}, true},
		"other parent's child": {parentTask(), &corev1alpha1.Task{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			inheritTaskProvenance(tc.child, tc.parent)
			if got := tc.child.Labels[labels.LabelAHPTurn] == labels.LabelAHPTurnValue; got != tc.want {
				t.Fatalf("child labels = %v, want AHP turn %v", tc.child.Labels, tc.want)
			}
			if tc.child.Spec.RequestedBy == nil || tc.child.Spec.RequestedBy.Subject != "parent-subject" {
				t.Fatalf("child requester = %#v, want the parent's", tc.child.Spec.RequestedBy)
			}
		})
	}
}

// delegate_task run by an AHP turn creates a child the API keeps private.
func TestDelegateTaskToolMarksAnAHPTurnsChildATurn(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaCoordinationDepth, "0")
	t.Setenv(envOrkaCoordinationAllowedAgents, testResearcherAgentName)
	t.Setenv(envOrkaCoordinationMaxDepth, "3")
	parent := parentTask()
	parent.Labels = map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}
	k8sClient := newFakeClient(parent, researcherAgent())
	if _, err := NewDelegateTaskTool(k8sClient).Execute(t.Context(), json.RawMessage(`{"agent": "researcher", "prompt": "Investigate this"}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	tasks := &corev1alpha1.TaskList{}
	if err := k8sClient.List(t.Context(), tasks); err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var children int
	for _, task := range tasks.Items {
		if task.Name == parentTaskName {
			continue
		}
		children++
		if task.Labels[labels.LabelAHPTurn] != labels.LabelAHPTurnValue {
			t.Fatalf("child %s labels = %v, want it marked an AHP turn", task.Name, task.Labels)
		}
	}
	if children != 1 {
		t.Fatalf("children = %d, want 1", children)
	}
}
