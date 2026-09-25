package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

type failingTaskDataCleanup struct {
	store.ResultStore
	store.ArtifactStore
	store.MessageStore
	store.TaskJobAuthorityStore
	operation string
	err       error
}

func (s *failingTaskDataCleanup) DeleteResult(context.Context, string, string) error {
	if s.operation == "result" {
		return s.err
	}
	return nil
}

func (s *failingTaskDataCleanup) DeleteArtifacts(context.Context, string, string) error {
	if s.operation == "artifact" {
		return s.err
	}
	return nil
}

func (s *failingTaskDataCleanup) DeleteTaskMessages(context.Context, string, string) error {
	if s.operation == "task messages" {
		return s.err
	}
	return nil
}

func (s *failingTaskDataCleanup) DeleteParentMessages(context.Context, string, string) error {
	if s.operation == "parent messages" {
		return s.err
	}
	return nil
}

func TestTaskDataCleanupFailureRetainsFinalizer(t *testing.T) {
	for _, operation := range []string{"result", "artifact", "task messages", "parent messages"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "deleting-task", UID: "task-uid", Finalizers: []string{labels.TaskFinalizer},
			}}
			r := newUnitReconciler(newTestScheme(), task)
			failure := &failingTaskDataCleanup{
				TaskJobAuthorityStore: r.ResultStore.(store.TaskJobAuthorityStore),
				operation:             operation,
				err:                   errors.New("cleanup unavailable"),
			}
			r.ResultStore, r.ArtifactStore, r.MessageStore = failure, failure, failure
			_, err := r.handleDeletion(ctx, task)
			require.ErrorIs(t, err, failure.err)
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), current))
			require.Contains(t, current.Finalizers, labels.TaskFinalizer)
			failure.operation = ""
			_, err = r.handleDeletion(ctx, current)
			require.NoError(t, err)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), current))
			require.NotContains(t, current.Finalizers, labels.TaskFinalizer)
		})
	}
}
