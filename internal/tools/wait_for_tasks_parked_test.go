/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func runWaitForTasks(t *testing.T, objs ...*corev1alpha1.Task) WaitForTasksResult {
	t.Helper()
	t.Setenv(envOrkaTaskNamespace, testNamespace)
	clientObjs := make([]client.Object, 0, len(objs))
	for _, obj := range objs {
		clientObjs = append(clientObjs, obj)
	}
	c := newFakeClient(clientObjs...)
	out, err := NewWaitForTasksTool(c).Execute(context.Background(), json.RawMessage(`{"tasks":["child"],"timeout":"1s"}`))
	if err != nil {
		t.Fatalf("wait_for_tasks: %v", err)
	}
	var result WaitForTasksResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWaitForTasksTreatsAParkedChildAsUnfinished(t *testing.T) {
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: testNamespace, UID: "uid-child"},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			Wait:  &corev1alpha1.TaskWaitStatus{State: corev1alpha1.TaskWaitStateWaiting, Kind: corev1alpha1.TaskWaitKindTimer},
		},
	}
	result := runWaitForTasks(t, child)
	if result.Completed {
		t.Fatal("a child whose turn parked must not be reported complete")
	}
	if result.Results[0].WaitState != string(corev1alpha1.TaskWaitStateWaiting) {
		t.Fatalf("WaitState = %q", result.Results[0].WaitState)
	}
}

func TestWaitForTasksReportsTheContinuationChainTail(t *testing.T) {
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: testNamespace, UID: "uid-child"},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			Wait: &corev1alpha1.TaskWaitStatus{
				State: corev1alpha1.TaskWaitStateResumed, Kind: corev1alpha1.TaskWaitKindTimer,
				ContinuationTaskName: "child-next", ContinuationTaskUID: "uid-next",
			},
		},
	}
	next := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-next", Namespace: testNamespace, UID: "uid-next",
			Annotations: map[string]string{labels.AnnotationContinuationOfUID: "uid-child"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed, Message: "tests failed"},
	}
	result := runWaitForTasks(t, child, next)
	if !result.Completed {
		t.Fatal("a settled continuation chain completes the child")
	}
	got := result.Results[0]
	if got.Task != "child" || got.ContinuationTask != "child-next" || got.Phase != string(corev1alpha1.TaskPhaseFailed) || got.Result != "tests failed" {
		t.Fatalf("unexpected result %+v", got)
	}
}
